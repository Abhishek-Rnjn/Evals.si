package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Block an actual Open at the backend boundary, after manager admission but
// before a session can be registered. No timing or host sandbox is involved.
type admissionDriver struct {
	driver
	entered chan struct{}
	release chan struct{}
}

func (d *admissionDriver) name() string         { return "admission" }
func (d *admissionDriver) level() Level         { return LevelNamespaced }
func (d *admissionDriver) available() error     { return nil }
func (d *admissionDriver) supports(*Spec) error { return nil }
func (d *admissionDriver) open(ctx context.Context, _ *Spec, _, _ string) (backend, error) {
	d.entered <- struct{}{}
	select {
	case <-d.release:
		return &admissionBackend{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type admissionBackend struct{ backend }

func (*admissionBackend) close() error { return nil }

func admissionManager(t *testing.T, limit int) (*Manager, *admissionDriver) {
	t.Helper()
	d := &admissionDriver{entered: make(chan struct{}, 16), release: make(chan struct{})}
	sb := &Sandbox{cfg: Config{MaxSandboxes: limit, WorkDir: t.TempDir()}, drivers: []driver{d}, probes: map[string]error{d.name(): nil}}
	m := NewManager(sb)
	t.Cleanup(m.Close)
	return m, d
}

func TestAdmissionIncludesPendingCreatesAndRestores(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "create"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			const limit, requests = 2, 8
			m, d := admissionManager(t, limit)
			if restore {
				m.snapshots["snapshot"] = &snapshot{id: "snapshot", dir: t.TempDir(), driver: d}
			}
			type result struct {
				s   *Session
				err error
			}
			results := make(chan result, requests)
			for range requests {
				go func() {
					var s *Session
					var err error
					if restore {
						s, err = m.Restore(t.Context(), "snapshot", nil)
					} else {
						s, err = m.Create(t.Context(), &Spec{})
					}
					results <- result{s, err}
				}()
			}
			started, rejected := 0, 0
			timeout := time.NewTimer(5 * time.Second)
			defer timeout.Stop()
			for started+rejected < requests {
				select {
				case <-d.entered:
					started++
				case r := <-results:
					if r.err == nil {
						t.Fatal("open finished before backend was released")
					}
					rejected++
				case <-timeout.C:
					close(d.release)
					t.Fatal("requests did not reach admission")
				}
			}
			close(d.release)
			for range started {
				r := <-results
				if r.err != nil {
					t.Fatal(r.err)
				}
				defer r.s.Close()
			}
			if started != limit || rejected != requests-limit {
				t.Fatalf("limit %d admitted %d concurrent opens, rejected %d", limit, started, rejected)
			}
		})
	}
}

func TestCancelledOpenReturnsItsAdmissionSlot(t *testing.T) {
	m, d := admissionManager(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := m.Create(ctx, &Spec{}); result <- err }()
	<-d.entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	close(d.release)
	s, err := m.Create(t.Context(), &Spec{})
	if err != nil {
		t.Fatalf("slot leaked after cancellation: %v", err)
	}
	defer s.Close()
}

func TestManagerCloseCancelsPendingOpen(t *testing.T) {
	m, d := admissionManager(t, 1)
	result := make(chan error, 1)
	go func() {
		s, err := m.Create(t.Context(), &Spec{})
		if s != nil {
			_ = s.Close()
		}
		result <- err
	}()
	<-d.entered
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		close(d.release)
		<-closed
		t.Fatal("close did not cancel a pending backend open")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("pending open survived manager shutdown")
		}
	case <-time.After(time.Second):
		close(d.release)
		t.Fatal("pending open was not cancelled")
	}
	if _, err := m.Create(t.Context(), &Spec{}); err == nil {
		t.Fatal("manager admitted work after close")
	}
}

func TestCancelledResultIsClosedBeforeRegistration(t *testing.T) {
	m, d := admissionManager(t, 1)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	_, err := m.openSession(ctx, func(context.Context) (*Session, error) {
		cancel()
		return &Session{ID: "cancelled", b: &admissionBackend{}, dir: dir}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled result admitted: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled backend was not closed: %v", err)
	}
	if m.Created() != 0 {
		t.Fatal("cancelled session registered")
	}
	close(d.release)
	s, err := m.Create(t.Context(), &Spec{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
}

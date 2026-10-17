package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound means no sandbox or snapshot has the given ID.
var ErrNotFound = errors.New("no such sandbox or snapshot")

// Manager owns live sessions and snapshots by ID, enforces the sandbox
// limit and destroys idle sessions.
type Manager struct {
	sb   *Sandbox
	max  int
	idle time.Duration

	mu        sync.Mutex
	pending   int
	closed    bool
	sessions  map[string]*Session
	snapshots map[string]*snapshot
	stop      chan struct{}
	wg        sync.WaitGroup
	opening   sync.WaitGroup
	closeOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	// Sandboxes created since start (metrics, tests).
	created atomic.Int64
}

// Created reports how many sandboxes this manager has created.
func (m *Manager) Created() int64 { return m.created.Load() }

type snapshot struct {
	id     string
	dir    string
	spec   Spec
	driver driver
}

// NewManager starts a manager over sb; Close destroys everything it holds.
func NewManager(sb *Sandbox) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		sb: sb, max: sb.cfg.MaxSandboxes, idle: time.Duration(sb.cfg.IdleTimeoutS * float64(time.Second)),
		sessions: map[string]*Session{}, snapshots: map[string]*snapshot{}, stop: make(chan struct{}),
		ctx: ctx, cancel: cancel,
	}
	if m.max <= 0 {
		m.max = 64
	}
	if m.idle <= 0 {
		m.idle = 30 * time.Minute
	}
	m.wg.Add(1)
	go m.reap()
	return m
}

// Sandbox is the ladder the manager creates sessions on.
func (m *Manager) Sandbox() *Sandbox { return m.sb }

func (m *Manager) reap() {
	defer m.wg.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-t.C:
			m.reapOnce(now)
		}
	}
}

func (m *Manager) reapOnce(now time.Time) {
	m.mu.Lock()
	var stale []*Session
	for id, s := range m.sessions {
		idle := m.idle
		if s.Spec.IdleTimeout > 0 {
			idle = s.Spec.IdleTimeout
		}
		if last, quiet := s.idleSince(); quiet && now.Sub(last) > idle {
			stale = append(stale, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range stale {
		_ = s.Close()
	}
}

func (m *Manager) reserve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("sandbox manager is closed")
	}
	if len(m.sessions)+m.pending >= m.max {
		return fmt.Errorf("too many sandboxes (max_sandboxes is %d); destroy some first", m.max)
	}
	m.pending++
	m.opening.Add(1)
	return nil
}

// openSession owns one admission slot through backend startup. Registering the
// session and releasing its pending slot happen atomically, so concurrent opens
// cannot bypass the cap. Shutdown cancels and joins pending backend creation.
func (m *Manager) openSession(ctx context.Context, open func(context.Context) (*Session, error)) (*Session, error) {
	if err := m.reserve(); err != nil {
		return nil, err
	}
	defer m.opening.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	s, err := open(ctx)
	if err == nil && ctx.Err() != nil {
		// A backend may finish just as its caller disconnects. Do not register
		// a session that the cancelled request can no longer receive.
		_ = s.Close()
		err = ctx.Err()
	}
	m.mu.Lock()
	m.pending--
	closed := m.closed
	if err == nil && !closed {
		m.created.Add(1)
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if closed {
		_ = s.Close()
		return nil, errors.New("sandbox manager closed during creation")
	}
	return s, nil
}

// Create opens a sandbox for sp.
func (m *Manager) Create(ctx context.Context, sp *Spec) (*Session, error) {
	return m.openSession(ctx, func(ctx context.Context) (*Session, error) {
		return m.sb.Open(ctx, sp)
	})
}

// Get returns a live session.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s, nil
}

// Destroy closes and forgets a session.
func (m *Manager) Destroy(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.Close()
}

// Snapshot saves a session's state; it must not be running a command.
func (m *Manager) Snapshot(ctx context.Context, id string) (string, error) {
	s, err := m.Get(id)
	if err != nil {
		return "", err
	}
	if _, quiet := s.idleSince(); !quiet {
		return "", errors.New("cannot snapshot a sandbox while a command is running in it")
	}
	var d driver
	for _, cand := range m.sb.drivers {
		if cand.name() == s.Driver {
			d = cand
		}
	}
	dir, err := os.MkdirTemp(m.sb.cfg.WorkDir, "evalsi-snap-")
	if err != nil {
		return "", err
	}
	// The snapshot's own contents go one level down, so copies can create it.
	if err := s.b.snapshot(ctx, filepath.Join(dir, "state")); err != nil {
		_ = removeAll(dir)
		return "", err
	}
	snap := &snapshot{id: "snap_" + newSessionID()[4:], dir: dir, spec: s.Spec, driver: d}
	m.mu.Lock()
	m.snapshots[snap.id] = snap
	m.mu.Unlock()
	return snap.id, nil
}

// Restore makes a new session from a snapshot, on the rung it was taken on,
// optionally with a different network policy.
func (m *Manager) Restore(ctx context.Context, snapID string, network *Network) (*Session, error) {
	m.mu.Lock()
	snap, ok := m.snapshots[snapID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, snapID)
	}
	sp := snap.spec
	if network != nil {
		sp.Network = *network
		if err := sp.Validate(); err != nil {
			return nil, err
		}
		if err := snap.driver.supports(&sp); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, snap.driver.name(), err)
		}
	}
	return m.openSession(ctx, func(ctx context.Context) (*Session, error) {
		return m.sb.openWith(ctx, snap.driver, &sp, filepath.Join(snap.dir, "state"))
	})
}

// DeleteSnapshot removes a snapshot.
func (m *Manager) DeleteSnapshot(id string) error {
	m.mu.Lock()
	snap, ok := m.snapshots[id]
	delete(m.snapshots, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return removeAll(snap.dir)
}

// Close destroys every session and snapshot.
func (m *Manager) Close() {
	m.closeOnce.Do(m.close)
}

func (m *Manager) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	close(m.stop)
	m.wg.Wait()
	m.opening.Wait()
	m.mu.Lock()
	sessions, snaps := m.sessions, m.snapshots
	m.sessions, m.snapshots = map[string]*Session{}, map[string]*snapshot{}
	m.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
	for _, s := range snaps {
		_ = removeAll(s.dir)
	}
	m.sb.Close()
}

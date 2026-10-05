package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	sessions  map[string]*Session
	snapshots map[string]*snapshot
	stop      chan struct{}
	wg        sync.WaitGroup
}

type snapshot struct {
	id     string
	dir    string
	spec   Spec
	driver driver
}

// NewManager starts a manager over sb; Close destroys everything it holds.
func NewManager(sb *Sandbox) *Manager {
	m := &Manager{
		sb: sb, max: sb.cfg.MaxSandboxes, idle: time.Duration(sb.cfg.IdleTimeoutS * float64(time.Second)),
		sessions: map[string]*Session{}, snapshots: map[string]*snapshot{}, stop: make(chan struct{}),
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
	if len(m.sessions) >= m.max {
		return fmt.Errorf("too many sandboxes (max_sandboxes is %d); destroy some first", m.max)
	}
	return nil
}

func (m *Manager) add(s *Session) {
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
}

// Create opens a sandbox for sp.
func (m *Manager) Create(ctx context.Context, sp *Spec) (*Session, error) {
	if err := m.reserve(); err != nil {
		return nil, err
	}
	s, err := m.sb.Open(ctx, sp)
	if err != nil {
		return nil, err
	}
	m.add(s)
	return s, nil
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
	if err := m.reserve(); err != nil {
		return nil, err
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
	s, err := m.sb.openWith(ctx, snap.driver, &sp, filepath.Join(snap.dir, "state"))
	if err != nil {
		return nil, err
	}
	m.add(s)
	return s, nil
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
	close(m.stop)
	m.wg.Wait()
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
}

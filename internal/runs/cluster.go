package runs

import (
	"context"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func leaseName(id string) string { return "run:" + id }

func (m *Manager) leaseTTL() time.Duration {
	if m.opts.LeaseTTL > 0 {
		return m.opts.LeaseTTL
	}
	return 30 * time.Second
}

// joinCluster starts adopting orphaned runs and listening for cancellations.
func (m *Manager) joinCluster() error {
	ctx, cancel := context.WithCancel(context.Background())
	stopCancels, err := m.opts.Cluster.OnCancel(func(id string) {
		m.mu.Lock()
		if a := m.active[id]; a != nil {
			a.cancelled = true
			a.cancel()
		}
		m.mu.Unlock()
	})
	if err != nil {
		cancel()
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := m.opts.AdoptInterval
		if interval <= 0 {
			interval = 10 * time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			m.adopt(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	m.stopCluster = func() {
		stopCancels()
		cancel()
		<-done
	}
	return nil
}

// adopt starts the unfinished runs nobody holds: their replica died (its
// lease expired) or handed them off at shutdown. Executing resumes from the
// stored results.
func (m *Manager) adopt(ctx context.Context) {
	runs, err := m.store.RunsWithStatus(ctx, evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING)
	if err != nil {
		m.log.Warn("listing unfinished runs", "err", err)
		return
	}
	for _, run := range runs {
		m.mu.Lock()
		_, mine := m.active[run.GetId()]
		m.mu.Unlock()
		if mine {
			continue
		}
		holder, err := m.store.LeaseHolder(ctx, leaseName(run.GetId()))
		if err != nil || holder != "" {
			continue
		}
		m.log.Info("adopting run", "run", run.GetId())
		m.start(run)
	}
}

// holdLease takes the run's lease and keeps renewing it while the run
// executes. If the lease is lost (this replica stalled past its TTL and
// another adopted the run), the run stops here without writing its state.
func (m *Manager) holdLease(ctx context.Context, a *activeRun) bool {
	owner, ttl := m.opts.Cluster.Owner(), m.leaseTTL()
	ok, err := m.store.AcquireLease(ctx, leaseName(a.id), owner, ttl)
	if err != nil || !ok {
		if err != nil {
			m.log.Warn("taking a run's lease", "run", a.id, "err", err)
		}
		return false
	}
	go func() {
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-a.done:
				// Released whether the run finished or was handed off: an
				// unfinished run is then adopted by another replica.
				_ = m.store.ReleaseLease(context.Background(), leaseName(a.id), owner)
				return
			case <-t.C:
				ok, err := m.store.AcquireLease(context.Background(), leaseName(a.id), owner, ttl)
				if err == nil && ok {
					continue
				}
				m.log.Warn("lost a run's lease; another replica continues it", "run", a.id, "err", err)
				m.mu.Lock()
				a.handoff = true
				m.mu.Unlock()
				a.cancel()
			}
		}
	}()
	return true
}

// watchRemote follows a run executing on another replica: its events over
// the cluster, and the stored state as a fallback (a replica can die and
// another adopt the run).
func (m *Manager) watchRemote(ctx context.Context, id string, includeResults bool, send func(*evalsiv1alpha1.WatchRunResponse) error) error {
	events, stop, err := m.opts.Cluster.SubscribeRunEvents(id)
	if err != nil {
		return err
	}
	defer stop()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-events:
			if ev.GetResult() != nil && !includeResults {
				continue
			}
			if err := send(ev); err != nil {
				return err
			}
			if r := ev.GetRun(); r != nil && terminal(r.GetStatus()) {
				return nil
			}
		case <-poll.C:
			run, err := m.get(ctx, id)
			if err != nil {
				return err
			}
			if terminal(run.GetStatus()) {
				return send(&evalsiv1alpha1.WatchRunResponse{Event: &evalsiv1alpha1.WatchRunResponse_Run{Run: run}})
			}
		}
	}
}

// cancelRemote asks the executing replica to cancel and waits (bounded) for
// the run to stop.
func (m *Manager) cancelRemote(ctx context.Context, id string) (*evalsiv1alpha1.Run, error) {
	if err := m.opts.Cluster.RequestCancel(id); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		run, err := m.get(ctx, id)
		if err != nil || terminal(run.GetStatus()) || time.Now().After(deadline) {
			return run, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

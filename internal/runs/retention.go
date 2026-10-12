package runs

import (
	"context"
	"errors"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// retentionPage is how many candidate runs one query reads.
const retentionPage = 200

// Retain deletes finished runs older than their project's retention (runs.retention
// and runs.project_retention), every interval until ctx is done. A run counts from
// when it finished, else when it was created. Deletion is DeleteRun's: records,
// outputs, results and webhook deliveries go, and the project's stored-run quota
// is freed. A run that is not finished, or is labelled evalsi.keep=true, stays.
// Every replica may run it; deleting a run twice is harmless.
func (m *Manager) Retain(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := m.retainOnce(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("run retention", "err", err)
		} else if n > 0 {
			m.log.Info("run retention", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// retainOnce makes one pass and returns how many runs it deleted.
func (m *Manager) retainOnce(ctx context.Context) (int, error) {
	projects, err := m.store.RunProjects(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, project := range projects {
		keep := m.opts.Retention.RetentionFor(project)
		if keep <= 0 {
			continue
		}
		n, err := m.retainProject(ctx, project, m.opts.Now().Add(-keep))
		deleted += n
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func (m *Manager) retainProject(ctx context.Context, project string, cutoff time.Time) (int, error) {
	deleted := 0
	var afterCreated int64
	afterID := ""
	for {
		page, err := m.store.RunsCreatedBefore(ctx, project, cutoff, afterCreated, afterID, retentionPage)
		if err != nil {
			return deleted, err
		}
		for _, run := range page {
			afterCreated, afterID = run.GetCreatedAt().AsTime().UnixNano(), run.GetId()
			if !expired(run, cutoff) {
				continue
			}
			ok, err := m.retire(ctx, run.GetId())
			if err != nil {
				return deleted, err
			}
			if ok {
				deleted++
			}
		}
		if len(page) < retentionPage {
			return deleted, nil
		}
	}
}

// expired reports whether a run is finished, not kept, and older than the cutoff.
func expired(run *evalsiv1alpha1.Run, cutoff time.Time) bool {
	switch run.GetStatus() {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED:
	default:
		return false
	}
	if run.GetLabels()[config.KeepLabel] == "true" {
		return false
	}
	at := run.GetFinishedAt()
	if at == nil {
		at = run.GetCreatedAt()
	}
	return at.AsTime().Before(cutoff)
}

// retire deletes one run unless it is active (here or on another replica) or already gone.
func (m *Manager) retire(ctx context.Context, id string) (bool, error) {
	m.admit.Lock()
	defer m.admit.Unlock()
	m.mu.Lock()
	_, active := m.active[id]
	m.mu.Unlock()
	if active {
		return false, nil
	}
	switch err := m.store.DeleteRun(ctx, id); {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrRunActive), errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

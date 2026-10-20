package runs

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// retentionPage is how many candidate runs one query reads.
const retentionPage = 200

// Retain deletes finished runs older than their project's retention (runs.retention
// and runs.project_retention) and archives those older than runs.archive_after
// (and runs.project_archive_after), every interval until ctx is done. Each
// automatic delete or archive leaves an audit event. A run counts from
// when it finished, else when it was created. Deletion is DeleteRun's: records,
// outputs, results and webhook deliveries go, and the project's stored-run quota
// is freed. A run that is not finished, or is labelled evalsi.keep=true, stays.
// Every replica may run it; deleting a run twice is harmless.
func (m *Manager) Retain(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if deleted, archived, err := m.retainOnce(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("run retention", "err", err)
		} else if deleted+archived > 0 {
			m.log.Info("run retention", "deleted", deleted, "archived", archived)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// retainOnce makes one pass over every project and returns how many runs it
// deleted and archived. Deleting comes first, so a run due for both is deleted.
func (m *Manager) retainOnce(ctx context.Context) (deleted, archived int, err error) {
	projects, err := m.store.RunProjects(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, project := range projects {
		if keep := m.opts.Retention.RetentionFor(project); keep > 0 {
			n, err := m.retainProject(ctx, project, m.opts.Now().Add(-keep), m.retire)
			deleted += n
			if err != nil {
				return deleted, archived, err
			}
		}
		if after := m.opts.Retention.ArchiveAfterFor(project); after > 0 {
			n, err := m.retainProject(ctx, project, m.opts.Now().Add(-after), m.retireToSummary)
			archived += n
			if err != nil {
				return deleted, archived, err
			}
		}
	}
	return deleted, archived, nil
}

// retainProject applies act to the project's expired runs and counts those it
// acted on.
func (m *Manager) retainProject(ctx context.Context, project string, cutoff time.Time, act func(context.Context, *evalsiv1alpha1.Run, time.Time) (bool, error)) (int, error) {
	done := 0
	var afterCreated int64
	afterID := ""
	for {
		page, err := m.store.RunsCreatedBefore(ctx, project, cutoff, afterCreated, afterID, retentionPage)
		if err != nil {
			return done, err
		}
		for _, run := range page {
			afterCreated, afterID = run.GetCreatedAt().AsTime().UnixNano(), run.GetId()
			if !expired(run, cutoff) {
				continue
			}
			ok, err := act(ctx, run, cutoff)
			if err != nil {
				return done, err
			}
			if ok {
				done++
			}
		}
		if len(page) < retentionPage {
			return done, nil
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
func (m *Manager) retire(ctx context.Context, run *evalsiv1alpha1.Run, cutoff time.Time) (bool, error) {
	id := run.GetId()
	m.admit.Lock()
	defer m.admit.Unlock()
	m.mu.Lock()
	_, active := m.active[id]
	m.mu.Unlock()
	if active {
		return false, nil
	}
	if err := m.auditRetention(ctx, run, "runs.delete", cutoff); err != nil {
		return false, err
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

// retireToSummary archives one run unless it already is, is active, or is gone.
func (m *Manager) retireToSummary(ctx context.Context, run *evalsiv1alpha1.Run, cutoff time.Time) (bool, error) {
	if run.GetArchivedAt() != nil {
		return false, nil
	}
	if err := m.auditRetention(ctx, run, "runs.archive", cutoff); err != nil {
		return false, err
	}
	_, did, err := m.archive(ctx, run.GetId())
	switch {
	case err == nil:
		return did, nil
	case connect.CodeOf(err) == connect.CodeFailedPrecondition, connect.CodeOf(err) == connect.CodeNotFound:
		return false, nil
	default:
		return false, err
	}
}

// auditRetention records an automatic delete or archive before it is made:
// the same event a caller's DeleteRun or ArchiveRun leaves, with the system as
// the principal. An error (strict audit mode) means the run must be left.
func (m *Manager) auditRetention(ctx context.Context, run *evalsiv1alpha1.Run, action string, cutoff time.Time) error {
	if m.opts.Audit == nil {
		return nil
	}
	return m.opts.Audit(ctx, &evalsiv1alpha1.AuditEvent{
		Principal: "system:retention", Action: action, Project: run.GetProject(),
		Resource: "run/" + run.GetId(), Allowed: true,
		Reason:    "run retention",
		Procedure: "runs.Retain",
		Detail:    "finished before " + cutoff.UTC().Format(time.RFC3339),
	})
}

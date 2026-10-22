package runs

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Retention deletes finished runs past their project's retention, and nothing
// else: not recent runs, not runs still going, not runs labelled to be kept.
func TestRetentionDeletesOnlyExpiredFinishedRuns(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	h := newRetentionHarness(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Retention = config.Runs{Retention: "720h", ProjectRetention: map[string]string{"research": "8760h", "audit": "0"}}
	})
	ctx := context.Background()
	day := 24 * time.Hour
	add := func(id, project string, status evalsiv1alpha1.RunStatus, age time.Duration, labels map[string]string) {
		t.Helper()
		created := now.Add(-age)
		run := &evalsiv1alpha1.Run{
			Id: id, Project: project, Status: status, Labels: labels,
			CreatedAt: timestamppb.New(created), FinishedAt: timestamppb.New(created.Add(time.Hour)),
		}
		if err := h.st.CreateRun(ctx, run, []*evalsiv1alpha1.Record{{Id: "r"}}); err != nil {
			t.Fatal(err)
		}
	}
	done, running := evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING
	add("old", "support", done, 60*day, nil)
	add("old-failed", "support", evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED, 60*day, nil)
	add("recent", "support", done, 5*day, nil)
	add("old-running", "support", running, 60*day, nil)
	add("old-kept", "support", done, 60*day, map[string]string{config.KeepLabel: "true"})
	add("old-not-kept", "support", done, 60*day, map[string]string{config.KeepLabel: "false"})
	add("research-60d", "research", done, 60*day, nil) // within its longer retention
	add("research-2y", "research", done, 730*day, nil)
	add("audit-2y", "audit", done, 730*day, nil) // "0": forever
	// The window straddles the cutoff: it was created before it but finished after it.
	add("straddles", "support", done, 30*day+time.Minute, nil)

	n, _, err := h.m.retainOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{"old": true, "old-failed": true, "old-not-kept": true, "research-2y": true}
	if n != len(gone) {
		t.Errorf("deleted %d runs, want %d", n, len(gone))
	}
	for _, id := range []string{"old", "old-failed", "recent", "old-running", "old-kept", "old-not-kept", "research-60d", "research-2y", "audit-2y", "straddles"} {
		_, err := h.st.GetRun(ctx, id)
		if exists := err == nil; exists == gone[id] {
			t.Errorf("%s: exists=%v, want deleted=%v (%v)", id, exists, gone[id], err)
		}
	}
	// Their records went with them, and a second pass has nothing to do.
	if recs, err := h.st.Records(ctx, "old"); err == nil && len(recs) != 0 {
		t.Errorf("records of a deleted run remain: %d", len(recs))
	}
	if n, _, err := h.m.retainOnce(ctx); err != nil || n != 0 {
		t.Errorf("second pass: %d %v", n, err)
	}
}

// Kept and unfinished runs older than a whole page must not hide the expired
// ones behind them.
func TestRetentionWalksPastRunsItKeeps(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	h := newRetentionHarness(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Retention = config.Runs{Retention: "24h"}
	})
	ctx := context.Background()
	for i := 0; i < retentionPage+5; i++ {
		created := now.Add(-time.Duration(1000-i) * time.Hour)
		run := &evalsiv1alpha1.Run{
			Id: fmt.Sprintf("kept-%04d", i), Project: "p", Labels: map[string]string{config.KeepLabel: "true"},
			Status: evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, CreatedAt: timestamppb.New(created),
		}
		if err := h.st.CreateRun(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
	}
	run := &evalsiv1alpha1.Run{Id: "newer-expired", Project: "p", Status: evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, CreatedAt: timestamppb.New(now.Add(-48 * time.Hour))}
	if err := h.st.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	if n, _, err := h.m.retainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
}

func newRetentionHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return newHarnessWith(t, st, dir, mutate)
}

// A run interrupted by a restart is marked ERROR; it gets a finish time so
// retention does not age a long-lived, just-interrupted run from CreatedAt.
func TestRetentionKeepsRunInterruptedByRestart(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	run := &evalsiv1alpha1.Run{Id: "resumed", Project: "p", Status: evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING,
		CreatedAt: timestamppb.New(now.Add(-60 * 24 * time.Hour)), StartedAt: timestamppb.New(now.Add(-time.Minute))}
	if err := st.CreateRun(ctx, run, []*evalsiv1alpha1.Record{{Id: "r"}}); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, st, dir, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Retention = config.Runs{Retention: "720h"}
	})
	if _, _, err := h.m.retainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRun(ctx, "resumed"); err != nil {
		t.Fatalf("resumable run interrupted a minute ago was deleted by retention: %v", err)
	}
}

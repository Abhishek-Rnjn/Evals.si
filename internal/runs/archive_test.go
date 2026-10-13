package runs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func archive(h *harness, id string) (*evalsiv1alpha1.Run, error) {
	resp, err := h.m.ArchiveRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.ArchiveRunRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRun(), nil
}

// Archiving drops a run's records, outputs and results and keeps the run, and
// what needs them says why it cannot proceed.
func TestArchiveKeepsTheSummary(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	ctx := context.Background()
	seedSlice(t, h, "run-a", "first", "support", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), map[string]string{"app": "x"})
	run, err := h.st.GetRun(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	run.Summaries = []*evalsiv1alpha1.MetricSummary{{Metric: "exact-match", N: 8}}
	run.Records = 6
	if err := h.st.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := archive(h, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetArchivedAt() == nil || got.GetSummaries()[0].GetN() != 8 || got.GetRecords() != 6 || got.GetLabels()["app"] != "x" || got.GetProject() != "support" {
		t.Errorf("archived run = %v", got)
	}
	again, err := h.st.GetRun(ctx, "run-a")
	if err != nil || again.GetArchivedAt() == nil || len(again.GetSummaries()) != 1 {
		t.Errorf("stored run = %v, %v", again, err)
	}
	if recs, _ := h.st.Records(ctx, "run-a"); len(recs) != 0 {
		t.Errorf("records remain: %d", len(recs))
	}
	if outs, _ := h.st.Outputs(ctx, "run-a"); len(outs) != 0 {
		t.Errorf("outputs remain: %d", len(outs))
	}
	if res, _ := h.st.Results(ctx, "run-a", "", 0, 0); len(res) != 0 {
		t.Errorf("results remain: %d", len(res))
	}
	// Twice is harmless and keeps the first time.
	second, err := archive(h, "run-a")
	if err != nil || !second.GetArchivedAt().AsTime().Equal(got.GetArchivedAt().AsTime()) {
		t.Errorf("second archive = %v, %v", second.GetArchivedAt(), err)
	}

	// What needs the removed data is refused, with the reason.
	refused := map[string]error{}
	_, refused["results"] = h.m.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: "run-a"}))
	_, refused["resume"] = h.m.ResumeRun(ctx, connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: "run-a"}))
	_, refused["promote"] = h.m.PromoteResults(ctx, connect.NewRequest(&evalsiv1alpha1.PromoteResultsRequest{RunId: "run-a", Dataset: "d", When: "true"}))
	seedSlice(t, h, "run-b", "second", "support", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), nil)
	_, refused["compare"] = h.m.CompareRuns(ctx, connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{BaselineRunId: "run-b", CandidateRunId: "run-a"}))
	_, refused["dataset"] = h.m.runRecords(ctx, &evalsiv1alpha1.RunOutputs{RunId: "run-a"})
	for what, err := range refused {
		if err == nil || !strings.Contains(err.Error(), "archived") {
			t.Errorf("%s: err = %v, want an archived refusal", what, err)
		}
	}
	for _, what := range []string{"results", "resume", "promote", "compare"} {
		if connect.CodeOf(refused[what]) != connect.CodeFailedPrecondition {
			t.Errorf("%s: code %v", what, connect.CodeOf(refused[what]))
		}
	}
	// Reading the run still works, a slice skips it, and it can be deleted.
	if _, err := h.m.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: "run-a"})); err != nil {
		t.Error(err)
	}
	s, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"run-a", "run-b"}, Metric: "exact-match", By: "topic"})
	if err != nil || s.GetRuns() != 1 {
		t.Errorf("slice covered %d runs, %v; want 1", s.GetRuns(), err)
	}
	if _, err := h.m.DeleteRun(ctx, connect.NewRequest(&evalsiv1alpha1.DeleteRunRequest{Id: "run-a"})); err != nil {
		t.Errorf("deleting an archived run: %v", err)
	}
}

// An unfinished run, or a missing one, cannot be archived.
func TestArchiveNeedsAFinishedRun(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	ctx := context.Background()
	if err := h.st.CreateRun(ctx, &evalsiv1alpha1.Run{Id: "going", Project: "p", Status: evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING, CreatedAt: timestamppb.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := archive(h, "going"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("running run: %v", err)
	}
	if _, err := archive(h, "nope"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing run: %v", err)
	}
	if run, _ := h.st.GetRun(ctx, "going"); run.GetArchivedAt() != nil {
		t.Error("a running run was marked archived")
	}
}

// Retention archives runs past archive_after, deletes those past retention,
// leaves kept and unfinished runs alone, and audits both.
func TestRetentionArchivesThenDeletes(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var events []*evalsiv1alpha1.AuditEvent
	h := newRetentionHarness(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Retention = config.Runs{
			ArchiveAfter: "168h", Retention: "720h",
			ProjectArchiveAfter: map[string]string{"audit": "0"},
			ProjectRetention:    map[string]string{"research": "0"},
		}
		o.Audit = func(_ context.Context, ev *evalsiv1alpha1.AuditEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}
	})
	ctx := context.Background()
	day := 24 * time.Hour
	done, running := evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING
	add := func(id, project string, status evalsiv1alpha1.RunStatus, age time.Duration, labels map[string]string) {
		t.Helper()
		created := now.Add(-age)
		run := &evalsiv1alpha1.Run{Id: id, Project: project, Status: status, Labels: labels,
			CreatedAt: timestamppb.New(created), FinishedAt: timestamppb.New(created.Add(time.Hour))}
		if err := h.st.CreateRun(ctx, run, []*evalsiv1alpha1.Record{{Id: "r"}}); err != nil {
			t.Fatal(err)
		}
	}
	add("fresh", "support", done, 2*day, nil)
	add("week-old", "support", done, 10*day, nil)
	add("month-old", "support", done, 40*day, nil)
	add("old-running", "support", running, 40*day, nil)
	add("old-kept", "support", done, 40*day, map[string]string{config.KeepLabel: "true"})
	add("audit-old", "audit", done, 10*day, nil)        // "0": never archived
	add("research-old", "research", done, 400*day, nil) // archived, never deleted

	deleted, archived, err := h.m.retainOnce(ctx)
	if err != nil || deleted != 1 || archived != 2 {
		t.Fatalf("deleted %d, archived %d, %v; want 1 and 2", deleted, archived, err)
	}
	state := func(id string) string {
		run, err := h.st.GetRun(ctx, id)
		switch {
		case err != nil:
			return "gone"
		case run.GetArchivedAt() != nil:
			return "archived"
		}
		return "whole"
	}
	for id, want := range map[string]string{
		"fresh": "whole", "week-old": "archived", "month-old": "gone", "old-running": "whole",
		"old-kept": "whole", "audit-old": "whole", "research-old": "archived",
	} {
		if got := state(id); got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]string{}
	for _, ev := range events {
		seen[ev.GetResource()] = ev.GetAction()
		if ev.GetPrincipal() != "system:retention" || !ev.GetAllowed() || ev.GetProject() == "" || ev.GetDetail() == "" {
			t.Errorf("audit event = %v", ev)
		}
	}
	// month-old is past both, so it is deleted and not archived first.
	want := map[string]string{"run/month-old": "runs.delete", "run/week-old": "runs.archive", "run/research-old": "runs.archive"}
	if len(seen) != len(want) {
		t.Errorf("audited %v, want %v", seen, want)
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s audited as %q, want %q", k, seen[k], v)
		}
	}
	// A second pass has nothing to do.
	if d, a, err := h.m.retainOnce(ctx); err != nil || d != 0 || a != 0 {
		t.Errorf("second pass: %d %d %v", d, a, err)
	}
}

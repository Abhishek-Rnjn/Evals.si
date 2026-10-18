package watch

import (
	"context"
	"testing"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

func TestRestartRecoversStoredTraceAndKeepsPolicyRestriction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, name := range []string{"chosen", "other"} {
		if err := h.e.Apply(ctx, &evalsiv1alpha1.OnlineEvalPolicy{Name: name, Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/deep")}}}); err != nil {
			t.Fatal(err)
		}
	}
	tr := trace(1, "studio", "good", false)
	if err := h.e.IngestBatchContext(ctx, []ingest.Trace{tr}, []string{"chosen"}); err != nil {
		t.Fatal(err)
	}
	queued := <-h.e.queue // Simulate the process stopping before evaluation.
	if results, err := h.st.TraceResults(ctx, DefaultProject, tr.TraceID); err != nil || len(results) != 0 {
		t.Fatalf("trace already scored: %v, %v", results, err)
	}
	restarted, err := New(ctx, h.st, h.e.eval, h.e.opts)
	if err != nil {
		t.Fatal(err)
	}
	restarted.recoverPending(ctx)
	results, err := h.st.TraceResults(ctx, DefaultProject, tr.TraceID)
	if err != nil || len(results) != 1 || results[0].GetPolicy() != "chosen" {
		t.Fatalf("recovered results: %v, %v", results, err)
	}
	if pending, err := h.st.ListWatchPending(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("processed work remains pending: %v, %v", pending, err)
	}
	// Recovery can overlap a live enqueue; the old queue entry must not score twice.
	restarted.process(ctx, []item{queued})
	restarted.recoverPending(ctx)
	h.worker.mu.Lock()
	defer h.worker.mu.Unlock()
	if h.worker.deepCalls != 1 {
		t.Fatalf("duplicate evaluation: %d calls", h.worker.deepCalls)
	}
}

func TestRecoveryRunsOnlyOnLeader(t *testing.T) {
	h := newHarness(t)
	h.e.Ingest(trace(2, "studio", "good", false))
	h.e.SetLeader(false)
	h.e.recoverPending(t.Context())
	if pending, err := h.st.ListWatchPending(t.Context(), 10); err != nil || len(pending) != 1 {
		t.Fatalf("follower acknowledged pending work: %v, %v", pending, err)
	}
	h.e.SetLeader(true)
	h.e.recoverPending(t.Context())
	if pending, err := h.st.ListWatchPending(t.Context(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("leader did not recover: %v, %v", pending, err)
	}
}

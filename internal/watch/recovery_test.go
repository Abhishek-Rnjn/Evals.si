package watch

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

func TestRecoveryPreservesSelectorAttributeTypes(t *testing.T) {
	info := ingest.TraceInfo{Project: "p", Attributes: ingest.Attrs{
		"large": int64(9007199254740993), "double": float64(1),
		"nested": map[string]any{"values": []any{int64(3), true, "text"}},
	}}
	blob, err := encodePending(pendingPayload{Info: info, Only: []string{"selected"}})
	if err != nil {
		t.Fatal(err)
	}
	var restored pendingPayload
	if err := decodePending(blob, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(info, restored.Info) {
		t.Fatalf("selector attributes changed: %#v", restored.Info)
	}
}

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

func TestRecoveryKeepsIndependentSourcePolicyStreams(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	for _, name := range []string{"first", "second"} {
		if err := h.e.Apply(ctx, &evalsiv1alpha1.OnlineEvalPolicy{Name: name, Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/deep")}}}); err != nil {
			t.Fatal(err)
		}
		tr := trace(3, "studio", "good", false)
		tr.Adjust = func(_ *evalsiv1alpha1.Record, info *ingest.TraceInfo) {
			info.Labels = map[string]string{"source": name}
		}
		if err := h.e.IngestBatchContext(ctx, []ingest.Trace{tr}, []string{name}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := h.st.ListWatchPending(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("independent jobs lost: %d, %v", len(pending), err)
	}
	h.e.recoverPending(ctx)
	results, err := h.st.TraceResults(ctx, DefaultProject, trace(3, "studio", "good", false).TraceID)
	if err != nil || len(results) != 2 {
		t.Fatalf("source policies not both scored: %v, %v", results, err)
	}
}

func TestStaleQueuedProducerCannotOverwriteScoredRevision(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if err := h.e.Apply(ctx, &evalsiv1alpha1.OnlineEvalPolicy{Name: "quality", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/quality")}}}); err != nil {
		t.Fatal(err)
	}
	h.e.Ingest(trace(4, "studio", "old bad", false))
	old := <-h.e.queue
	h.e.Ingest(trace(4, "studio", "new good", false))
	newest := <-h.e.queue
	h.e.process(ctx, []item{newest})
	h.e.process(ctx, []item{old})
	_, record, err := h.st.GetTrace(ctx, DefaultProject, newest.record.GetId())
	if err != nil || record.GetOutput().GetText() != "new good" {
		t.Fatalf("stale producer overwrote scored trace: %v, %v", record, err)
	}
	results, err := h.st.TraceResults(ctx, DefaultProject, newest.record.GetId())
	if err != nil || len(results) != 1 || results[0].GetResults()[0].GetScores()[0].GetNumber() != 1 {
		t.Fatalf("scores do not match current trace: %v, %v", results, err)
	}
}

func TestRecoveryWaitsForResultHandoff(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if err := h.e.Apply(ctx, &evalsiv1alpha1.OnlineEvalPolicy{Name: "deep", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/deep")}}}); err != nil {
		t.Fatal(err)
	}
	ready := false
	h.e.opts.ResultsReady = func(ingest.TraceInfo) bool { return ready }
	h.e.Ingest(trace(5, "studio", "good", false))
	h.e.recoverPending(ctx)
	if h.worker.deepCalls != 0 {
		t.Fatal("evaluated before source runner was ready")
	}
	ready = true
	h.e.opts.OnResults = func(string, *evalsiv1alpha1.Record, ingest.TraceInfo, []*evalsiv1alpha1.EvaluationResult) error {
		return fmt.Errorf("runner stopped")
	}
	h.e.recoverPending(ctx)
	if pending, err := h.st.ListWatchPending(ctx, 10); err != nil || len(pending) != 1 {
		t.Fatalf("failed handoff acknowledged: %v %v", pending, err)
	}
	h.e.opts.OnResults = nil
	h.e.recoverPending(ctx)
	if pending, err := h.st.ListWatchPending(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("ready work not acknowledged: %v %v", pending, err)
	}
}

func TestUnavailablePolicyKeepsAcceptedWorkUntilRestoredOrDeleted(t *testing.T) {
	for _, restore := range []bool{true, false} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			h := newHarness(t)
			ctx := t.Context()
			policy := &evalsiv1alpha1.OnlineEvalPolicy{Name: "temporarily-unavailable", Project: DefaultProject, Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("missing/evaluator")}}}
			if err := h.st.PutPolicy(ctx, policy); err != nil {
				t.Fatal(err)
			}
			h.e.Ingest(trace(6, "studio", "good", false))
			restarted, err := New(ctx, h.st, h.e.eval, h.e.opts)
			if err != nil {
				t.Fatal(err)
			}
			restarted.recoverPending(ctx)
			if pending, err := h.st.ListWatchPending(ctx, 10); err != nil || len(pending) != 1 {
				t.Fatalf("unavailable policy lost work: %v %v", pending, err)
			}
			if restore {
				policy.Stages[0].Evaluators = refs("test/deep")
				if err := restarted.Apply(ctx, policy); err != nil {
					t.Fatal(err)
				}
			} else if err := restarted.Delete(ctx, policy.GetName()); err != nil {
				t.Fatal(err)
			}
			restarted.recoverPending(ctx)
			if pending, err := h.st.ListWatchPending(ctx, 10); err != nil || len(pending) != 0 {
				t.Fatalf("restored/deleted policy blocked work: %v %v", pending, err)
			}
			if restore && h.worker.deepCalls != 1 {
				t.Fatal("restored policy did not run")
			}
		})
	}
}

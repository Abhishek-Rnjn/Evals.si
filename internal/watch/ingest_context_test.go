package watch

import (
	"context"
	"errors"
	"testing"
	"time"

	connect "connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

// A trace source advances its watermark only when IngestBatchContext returns
// nil, so it must report what IngestBatch swallows and wait where IngestBatch
// drops.

func TestIngestBatchContextReturnsStoreErrors(t *testing.T) {
	h := newHarness(t)
	_ = h.st.Close() // the next write fails
	err := h.e.IngestBatchContext(context.Background(), []ingest.Trace{trace(1, "svc", "a good answer", false)}, nil)
	if err == nil {
		t.Fatal("a failed store write was reported as success")
	}
	if len(h.e.queue) != 0 {
		t.Fatal("a trace that was not stored was queued for scoring")
	}
	// IngestBatch keeps its live-stream behaviour: log, count, carry on.
	h.e.IngestBatch([]ingest.Trace{trace(2, "svc", "x", false)})
	if h.e.StoreErrors.Load() < 2 {
		t.Fatalf("store errors %d, want both counted", h.e.StoreErrors.Load())
	}
}

func TestIngestBatchContextWaitsForQueueRoom(t *testing.T) {
	h := newHarness(t)
	h.e.queue = make(chan item, 1)
	traces := []ingest.Trace{trace(1, "svc", "a", false), trace(2, "svc", "b", false)}

	// IngestBatch drops what does not fit.
	h.e.IngestBatch(traces)
	if h.e.TracesDropped.Load() != 1 || len(h.e.queue) != 1 {
		t.Fatalf("dropped %d, queued %d", h.e.TracesDropped.Load(), len(h.e.queue))
	}
	<-h.e.queue

	// IngestBatchContext blocks until there is room...
	done := make(chan error, 1)
	go func() { done <- h.e.IngestBatchContext(context.Background(), traces, nil) }()
	select {
	case err := <-done:
		t.Fatalf("returned %v with the queue full", err)
	case <-time.After(100 * time.Millisecond):
	}
	<-h.e.queue // room for the second trace
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not resume when the queue drained")
	}
	if h.e.TracesDropped.Load() != 1 {
		t.Fatal("a trace was dropped")
	}

	// ...and gives up, without success, when its context ends (a leader that
	// stepped down must not commit a watermark).
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := h.e.IngestBatchContext(ctx, []ingest.Trace{trace(3, "svc", "c", false), trace(4, "svc", "d", false)}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestIngestBatchContextRestrictsPolicies(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"first", "second"} {
		p := &evalsiv1alpha1.OnlineEvalPolicy{
			Name: name, Project: DefaultProject,
			Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/quality")}},
		}
		if _, err := h.e.ApplyPolicy(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: p})); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.e.IngestBatchContext(context.Background(), []ingest.Trace{trace(1, "svc", "a good answer", false)}, []string{"second"}); err != nil {
		t.Fatal(err)
	}
	h.e.process(context.Background(), []item{<-h.e.queue})
	first, _ := h.e.Stats("first")
	second, _ := h.e.Stats("second")
	if first.GetTracesSeen() != 0 || second.GetTracesEvaluated() != 1 {
		t.Fatalf("first=%v second=%v", first, second)
	}
}

package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func unwatchedHead(t *testing.T, st *Store) watchHead {
	t.Helper()
	h, err := getWatchHead(t.Context(), st.db, "p", "revision")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Storing without the queue binds scores exactly as the queue does: an
// unchanged trace keeps its score, a summary-only change keeps it too, and
// changed input leaves the old score behind.
func TestUnwatchedTracesKeepScoreBindings(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			if backend == "clickhouse" {
				st.UseTraces(traces)
			}
			start := time.Now()
			old := revisionTrace("s", "same input", start)
			acceptRevision(t, st, old)
			publishRevision(t, st, old)
			if err := st.PutWatchResults(t.Context(), PolicyLease{}, old.Pending, "quality", revisionScores(1)); err != nil {
				t.Fatal(err)
			}
			before := unwatchedHead(t, st)

			if err := st.PutUnwatchedTraces(t.Context(), PolicyLease{}, []TraceWrite{old.Trace}); err != nil {
				t.Fatal(err)
			}
			if h := unwatchedHead(t, st); h.Revision != before.Revision || h.ScoreRevision != before.ScoreRevision || !h.Published {
				t.Fatalf("an identical trace moved its head: %+v -> %+v", before, h)
			}
			assertCurrentRevisionScore(t, st, 1)

			meta := revisionTrace("s", "same input", start)
			meta.Trace.Summary.Service = "new service"
			if err := st.PutUnwatchedTraces(t.Context(), PolicyLease{}, []TraceWrite{meta.Trace}); err != nil {
				t.Fatal(err)
			}
			h := unwatchedHead(t, st)
			if h.Revision == before.Revision || h.ScoreRevision != before.ScoreRevision || !h.Published {
				t.Fatalf("summary-only change: %+v -> %+v", before, h)
			}
			if summary, _, err := st.GetTrace(t.Context(), "p", "revision"); err != nil || summary.GetService() != "new service" {
				t.Fatalf("summary-only change was not stored: %v %v", summary, err)
			}
			assertCurrentRevisionScore(t, st, 1)

			changed := revisionTrace("s", "new input", start)
			if err := st.PutUnwatchedTraces(t.Context(), PolicyLease{}, []TraceWrite{changed.Trace}); err != nil {
				t.Fatal(err)
			}
			if h := unwatchedHead(t, st); h.ScoreRevision == before.ScoreRevision || !h.Published {
				t.Fatalf("changed input kept the old binding: %+v", h)
			}
			if got, err := st.TraceResults(t.Context(), "p", "revision"); err != nil || len(got) != 0 {
				t.Fatalf("changed input shows the old score: %v %v", got, err)
			}
		})
	}
}

func TestUnwatchedTracesLastWriteWinsAndFenceTheLease(t *testing.T) {
	st := open(t)
	start := time.Now()
	first, second := revisionTrace("s", "first", start), revisionTrace("s", "second", start)
	if err := st.PutUnwatchedTraces(t.Context(), PolicyLease{}, []TraceWrite{first.Trace, second.Trace}); err != nil {
		t.Fatal(err)
	}
	_, record, err := st.GetTrace(t.Context(), "p", "revision")
	if err != nil || record.GetOutput().GetText() != "second" {
		t.Fatalf("last write did not win: %v %v", record, err)
	}
	third := revisionTrace("s", "third", start)
	err = st.PutUnwatchedTraces(t.Context(), PolicyLease{Owner: "gone", Epoch: 1}, []TraceWrite{third.Trace})
	if !errors.Is(err, ErrWatchStale) {
		t.Fatalf("a stale lease wrote: %v", err)
	}
	if _, record, _ := st.GetTrace(t.Context(), "p", "revision"); record.GetOutput().GetText() != "second" {
		t.Fatalf("a stale lease changed the trace: %v", record)
	}
}

// A trace stored without the queue and then queued (a policy applied in
// between) publishes once and keeps what was stored.
func TestUnwatchedThenQueuedTraceIsNotRepublished(t *testing.T) {
	st := open(t)
	w := revisionTrace("s", "input", time.Now())
	if err := st.PutUnwatchedTraces(t.Context(), PolicyLease{}, []TraceWrite{w.Trace}); err != nil {
		t.Fatal(err)
	}
	before := unwatchedHead(t, st)
	acceptRevision(t, st, w)
	ok, err := st.PutWatchTraces(t.Context(), PolicyLease{}, []WatchTraceWrite{w})
	if err != nil || len(ok) != 1 || !ok[0] {
		t.Fatalf("queued copy: %v %v", ok, err)
	}
	if h := unwatchedHead(t, st); h.Revision != before.Revision || h.ScoreRevision != before.ScoreRevision {
		t.Fatalf("queueing an identical trace moved its head: %+v -> %+v", before, h)
	}
}

func TestAckWatchPendingBatchRemovesOnlyTheNamedRevisions(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	a := WatchPending{Project: "p", TraceID: "a", Payload: []byte("a")}
	b := WatchPending{Project: "p", TraceID: "b", Payload: []byte("b")}
	c := WatchPending{Project: "p", TraceID: "c", Payload: []byte("c")}
	if err := st.PutWatchPending(ctx, []WatchPending{a, b, c}); err != nil {
		t.Fatal(err)
	}
	newerB := WatchPending{Project: "p", TraceID: "b", Payload: []byte("b2")}
	if err := st.PutWatchPending(ctx, []WatchPending{newerB}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckWatchPendingBatch(ctx, PolicyLease{}, []WatchPending{a, b}); err != nil {
		t.Fatal(err)
	}
	for item, want := range map[*WatchPending]bool{&a: false, &newerB: true, &c: true} {
		if ok, err := st.WatchPendingMatches(ctx, *item); err != nil || ok != want {
			t.Fatalf("%s: pending %v, want %v (%v)", item.Payload, ok, want, err)
		}
	}
	if err := st.AckWatchPendingBatch(ctx, PolicyLease{Owner: "gone", Epoch: 1}, []WatchPending{c}); !errors.Is(err, ErrWatchStale) {
		t.Fatalf("a stale lease acknowledged: %v", err)
	}
}

func TestSQLiteSynchronousIsOptIn(t *testing.T) {
	for opts, want := range map[Options]int{{}: 2, {SynchronousNormal: true}: 1} {
		st, err := OpenWithOptions(filepath.Join(t.TempDir(), "evalsi.db"), opts)
		if err != nil {
			t.Fatal(err)
		}
		var got int
		if err := st.db.db.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&got); err != nil || got != want {
			t.Errorf("%+v: synchronous %d, want %d (%v)", opts, got, want, err)
		}
		_ = st.Close()
	}
}

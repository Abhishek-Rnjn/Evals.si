package store

import (
	"testing"
	"time"
)

func TestPendingAcknowledgmentCannotDeleteNewerTrace(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	old := WatchPending{Project: "p", TraceID: "same", Payload: []byte("old")}
	newer := WatchPending{Project: "p", TraceID: "same", Payload: []byte("new")}
	other := WatchPending{Project: "q", TraceID: "same", Payload: []byte("other project")}
	if err := st.PutWatchPending(ctx, []WatchPending{old, other}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWatchPending(ctx, []WatchPending{newer}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckWatchPending(ctx, old); err != nil {
		t.Fatal(err)
	}
	for _, want := range []WatchPending{newer, other} {
		if ok, err := st.WatchPendingMatches(ctx, want); err != nil || !ok {
			t.Fatalf("lost pending work: %v, %v", ok, err)
		}
	}
}

func TestTraceRetentionRemovesPendingCopies(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	now := time.Now()
	old := WatchPending{Project: "p", TraceID: "old", Payload: []byte("old"), StartNS: now.Add(-time.Hour).UnixNano()}
	newer := WatchPending{Project: "p", TraceID: "new", Payload: []byte("new"), StartNS: now.UnixNano()}
	if err := st.PutWatchPending(ctx, []WatchPending{old, newer}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTracesBefore(ctx, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if exists, err := st.WatchPendingMatches(ctx, old); err != nil || exists {
		t.Fatalf("old copy retained: %v, %v", exists, err)
	}
	if exists, err := st.WatchPendingMatches(ctx, newer); err != nil || !exists {
		t.Fatalf("new copy removed: %v, %v", exists, err)
	}
}

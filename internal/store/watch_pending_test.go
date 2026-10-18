package store

import "testing"

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

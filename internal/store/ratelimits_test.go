package store

import (
	"context"
	"testing"
)

func TestRateCountsAddUpPerWindowAndPrune(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	got, err := st.AddRateCounts(ctx, 1000, map[string]int64{"api|key:a": 3, "api|key:b": 1, "ingest|key:a": 0})
	if err != nil {
		t.Fatal(err)
	}
	if got["api|key:a"] != 3 || got["api|key:b"] != 1 || got["ingest|key:a"] != 0 {
		t.Fatalf("first add: %v", got)
	}
	// Another replica adds to the same window and sees the total.
	got, err = st.AddRateCounts(ctx, 1000, map[string]int64{"api|key:a": 2, "api|key:b": 0})
	if err != nil {
		t.Fatal(err)
	}
	if got["api|key:a"] != 5 || got["api|key:b"] != 1 {
		t.Fatalf("second add: %v", got)
	}
	// A new window starts from zero.
	if got, _ = st.AddRateCounts(ctx, 2000, map[string]int64{"api|key:a": 1}); got["api|key:a"] != 1 {
		t.Fatalf("new window: %v", got)
	}
	if err := st.PruneRateCounts(ctx, 2000); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.AddRateCounts(ctx, 1000, map[string]int64{"api|key:a": 0}); got["api|key:a"] != 0 {
		t.Fatalf("pruned window still counted: %v", got)
	}
	if got, _ = st.AddRateCounts(ctx, 2000, map[string]int64{"api|key:a": 0}); got["api|key:a"] != 1 {
		t.Fatalf("newer window pruned: %v", got)
	}
}

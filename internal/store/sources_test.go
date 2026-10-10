package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestSources(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	src := &evalsiv1alpha1.TraceSource{
		Name: "studio", Project: "p", Connector: "mlflow", Endpoint: "http://mlflow:5000", Locations: []string{"1"},
		UpdatedAt: timestamppb.New(time.Unix(100, 0)),
		Status:    &evalsiv1alpha1.SourceStatus{LastError: "never stored"},
	}
	if err := s.PutSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSource(ctx, "p", "studio")
	if err != nil || got.GetEndpoint() != "http://mlflow:5000" || got.GetStatus() != nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if _, err := s.GetSource(ctx, "p", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	other := &evalsiv1alpha1.TraceSource{Name: "other", Project: "q", UpdatedAt: timestamppb.Now()}
	if err := s.PutSource(ctx, other); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListSources(ctx, ""); len(all) != 2 {
		t.Fatalf("every project: %d", len(all))
	}
	if mine, _ := s.ListSources(ctx, "p"); len(mine) != 1 || mine[0].GetName() != "studio" {
		t.Fatalf("project p: %v", mine)
	}

	// State round-trips; a source never pulled reads as zero.
	if st, err := s.SourceState(ctx, "p", "studio"); err != nil || !st.Watermark.IsZero() {
		t.Fatalf("fresh state: %+v %v", st, err)
	}
	want := SourceState{Watermark: time.Unix(5000, 7).UTC(), BackfillFrom: time.Unix(1000, 0).UTC(), Pulled: 12, Scored: 9, Deferred: 2,
		LastError: "boom", LastErrorAt: time.Unix(6000, 0).UTC(), LastPullAt: time.Unix(6001, 0).UTC(), LastWriteAt: time.Unix(6002, 0).UTC()}
	if err := s.PutSourceState(ctx, "p", "studio", want); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.SourceState(ctx, "p", "studio"); st != want {
		t.Fatalf("state %+v, want %+v", st, want)
	}

	// Seen traces: dedup keys, digest changes, pruning.
	base := time.Unix(10_000, 0).UTC()
	if err := s.MarkSeen(ctx, "p", "studio", []SeenTrace{
		{TraceID: "a", Digest: "d1", Started: base},
		{TraceID: "b", Digest: "d1", Started: base.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	seen, err := s.SeenTraces(ctx, "p", "studio", []string{"a", "b", "c"})
	if err != nil || len(seen) != 2 || seen["a"].Digest != "d1" || !seen["b"].Started.Equal(base.Add(time.Hour)) {
		t.Fatalf("seen: %+v %v", seen, err)
	}
	if err := s.MarkSeen(ctx, "p", "studio", []SeenTrace{{TraceID: "a", Digest: "d2", Started: base}}); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.SeenTraces(ctx, "p", "studio", []string{"a"}); seen["a"].Digest != "d2" {
		t.Fatalf("digest not updated: %+v", seen)
	}
	if n, _ := s.PruneSeen(ctx, "p", "studio", base.Add(time.Minute)); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	if seen, _ := s.SeenTraces(ctx, "p", "studio", []string{"a", "b"}); len(seen) != 1 || seen["b"].TraceID != "b" {
		t.Fatalf("after prune: %+v", seen)
	}
	// A page larger than one parameter chunk.
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = "x" + string(rune('a'+i%26)) + time.Duration(i).String()
	}
	if _, err := s.SeenTraces(ctx, "p", "studio", ids); err != nil {
		t.Fatalf("large lookup: %v", err)
	}

	// Write-back records.
	if err := s.PutSourceWrite(ctx, "p", "studio", SourceWrite{TraceID: "a", Metric: "task-success", RemoteID: "a-1", Digest: "v1", Written: base}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSourceWrite(ctx, "p", "studio", SourceWrite{TraceID: "a", Metric: "task-success", RemoteID: "a-1", Digest: "v2", Written: base}); err != nil {
		t.Fatal(err)
	}
	writes, err := s.SourceWrites(ctx, "p", "studio", []string{"a", "z"})
	if err != nil || len(writes) != 1 || writes[[2]string{"a", "task-success"}].Digest != "v2" {
		t.Fatalf("writes: %+v %v", writes, err)
	}

	// Deleting a source takes its state with it, and only its own.
	if err := s.DeleteSource(ctx, "p", "studio"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSource(ctx, "p", "studio"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if st, _ := s.SourceState(ctx, "p", "studio"); !st.Watermark.IsZero() {
		t.Fatalf("state survived delete: %+v", st)
	}
	if seen, _ := s.SeenTraces(ctx, "p", "studio", []string{"b"}); len(seen) != 0 {
		t.Fatal("seen traces survived delete")
	}
	if w, _ := s.SourceWrites(ctx, "p", "studio", []string{"a"}); len(w) != 0 {
		t.Fatal("writes survived delete")
	}
}

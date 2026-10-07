package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestAnnotationClaims(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	q := &evalsiv1alpha1.AnnotationQueue{Name: "q", Project: "p", AnnotationsPerItem: 2}
	if err := s.CreateQueue(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateQueue(ctx, q); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	var items []*evalsiv1alpha1.AnnotationItem
	for _, id := range []string{"r1", "r2"} {
		items = append(items, &evalsiv1alpha1.AnnotationItem{Record: &evalsiv1alpha1.Record{Id: id}})
	}
	if err := s.AddItems(ctx, "p", "q", items); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	claim := func(who string, at time.Time) string {
		t.Helper()
		it, _, err := s.ClaimItem(ctx, "p", "q", who, 2, time.Minute, at)
		if err != nil {
			t.Fatal(err)
		}
		return it.GetRecord().GetId()
	}
	save := func(who, item string, skipped bool) *evalsiv1alpha1.AnnotationItem {
		t.Helper()
		all, err := s.QueueItems(ctx, "p", "q", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range all {
			if it.GetRecord().GetId() == item {
				got, err := s.SaveAnnotation(ctx, "p", &evalsiv1alpha1.Annotation{
					ItemId: it.GetId(), Queue: "q", Annotator: who, Skipped: skipped, CreatedAt: timestamppb.New(now),
				}, 2)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
		}
		t.Fatalf("no item %s", item)
		return nil
	}

	// Two claims fit an item needing two answers; a third goes to the next.
	if a, b, c := claim("a", now), claim("b", now), claim("c", now); a != "r1" || b != "r1" || c != "r2" {
		t.Fatalf("claims: %s %s %s", a, b, c)
	}
	if got := claim("a", now); got != "r1" {
		t.Errorf("held item first: %s", got)
	}
	// "d" finds both items full until a lease runs out.
	if got := claim("d", now); got != "r2" {
		t.Errorf("r2 has room for one more: %s", got)
	}
	if got := claim("e", now); got != "" {
		t.Errorf("everything is claimed: %s", got)
	}
	if got := claim("e", now.Add(2*time.Minute)); got != "r1" {
		t.Errorf("after the leases end: %s", got)
	}

	// A skip releases the claim but is not an answer.
	if it := save("a", "r1", true); it.GetAnnotations() != 0 || it.GetDone() {
		t.Errorf("after a skip: %v", it)
	}
	save("b", "r1", false)
	if it := save("e", "r1", false); it.GetAnnotations() != 2 || !it.GetDone() {
		t.Errorf("after two answers: %v", it)
	}
	// Done items and items already answered are not offered again.
	if got := claim("b", now.Add(3*time.Minute)); got != "r2" {
		t.Errorf("b's next: %s", got)
	}
	anns, err := s.QueueAnnotations(ctx, "p", "q", 0, 0)
	if err != nil || len(anns) != 3 {
		t.Fatalf("annotations: %d %v", len(anns), err)
	}
	if err := s.DeleteQueue(ctx, "p", "q"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.QueueItems(ctx, "p", "q", 0, 0); len(left) != 0 {
		t.Errorf("items left: %d", len(left))
	}
}

package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func failureFixture(project, trace, hash string) *pb.FailureCase {
	return &pb.FailureCase{Project: project, Source: &pb.TraceSummary{Project: project, TraceId: trace}, SourceSha256: hash, Revision: 1, Status: pb.FailureStatus_FAILURE_STATUS_OPEN, Record: &pb.Record{Id: trace}, Title: "incident"}
}

func TestFailuresDeduplicateConcurrentlyAndPreserveEdits(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			f, _, err := s.CaptureFailure(ctx, failureFixture("support", "trace", "hash"), &pb.FailureEvent{Revision: 1})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- f.GetId()
		})
	}
	wg.Wait()
	close(ids)
	id := ""
	for found := range ids {
		if id != "" && found != id {
			t.Fatalf("duplicate IDs: %s and %s", id, found)
		}
		id = found
	}
	f, err := s.GetFailure(ctx, "support", id)
	if err != nil {
		t.Fatal(err)
	}
	f.Revision, f.Title, f.Status = 2, "reviewed edit", pb.FailureStatus_FAILURE_STATUS_APPROVED
	if err := s.SaveFailure(ctx, f, 1, &pb.FailureEvent{Revision: 2}); err != nil {
		t.Fatal(err)
	}
	again, duplicate, err := s.CaptureFailure(ctx, failureFixture("support", "trace", "hash"), &pb.FailureEvent{Revision: 1})
	if err != nil || !duplicate || !proto.Equal(again, f) {
		t.Fatalf("recapture overwrote edits: %v %v", again, err)
	}
	for _, other := range []*pb.FailureCase{failureFixture("checkout", "trace", "hash"), failureFixture("support", "trace", "new-hash")} {
		got, duplicate, err := s.CaptureFailure(ctx, other, &pb.FailureEvent{Revision: 1})
		if err != nil || duplicate || got.GetId() == id {
			t.Fatalf("wrong deduplication scope: %v %v", got, err)
		}
	}
	events, _, err := s.ListFailureEvents(ctx, "support", id, 100, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %v %v", events, err)
	}
}

func TestFailureCASRollbackPaginationAndDelete(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	f, _, err := s.CaptureFailure(ctx, failureFixture("support", "trace", "hash"), &pb.FailureEvent{Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A duplicate history revision makes the event insert fail. The payload
	// update must roll back too, on both SQLite and PostgreSQL.
	changed := proto.Clone(f).(*pb.FailureCase)
	changed.Revision, changed.Title = 1, "must roll back"
	if err := s.SaveFailure(ctx, changed, 1, &pb.FailureEvent{Revision: 1}); err == nil {
		t.Fatal("expected event write failure")
	}
	got, err := s.GetFailure(ctx, "support", f.GetId())
	if err != nil || !proto.Equal(got, f) {
		t.Fatalf("payload committed without history: %v %v", got, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, title := range []string{"one", "two"} {
		wg.Go(func() {
			copy := proto.Clone(f).(*pb.FailureCase)
			copy.Title, copy.Revision = title, 2
			results <- s.SaveFailure(ctx, copy, 1, &pb.FailureEvent{Revision: 2})
		})
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrFailureChanged) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent results: success=%d conflict=%d", success, conflict)
	}
	events, next, err := s.ListFailureEvents(ctx, "support", f.GetId(), 1, 0)
	if err != nil || len(events) != 1 || events[0].GetRevision() != 2 || next != "2" {
		t.Fatalf("event page: %v %q %v", events, next, err)
	}
	events, next, err = s.ListFailureEvents(ctx, "support", f.GetId(), 1, 2)
	if err != nil || len(events) != 1 || events[0].GetRevision() != 1 || next != "" {
		t.Fatalf("event tail: %v %q %v", events, next, err)
	}
	if err := s.DeleteFailure(ctx, "support", f.GetId(), 1); !errors.Is(err, ErrFailureChanged) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.DeleteFailure(ctx, "support", f.GetId(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFailure(ctx, "support", f.GetId()); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	events, _, err = s.ListFailureEvents(ctx, "support", f.GetId(), 50, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("history survived deletion: %v %v", events, err)
	}
}

func TestFailureListFiltersAndProjectIsolation(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	for _, p := range []string{"support", "checkout"} {
		for _, trace := range []string{"a", "b", "c"} {
			f := failureFixture(p, trace, "hash")
			if trace == "b" {
				f.Assignee, f.Status = "owner", pb.FailureStatus_FAILURE_STATUS_IN_REVIEW
			}
			if _, _, err := s.CaptureFailure(ctx, f, &pb.FailureEvent{Revision: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen, cursor := map[string]bool{}, ""
	for {
		rows, next, err := s.ListFailures(ctx, "support", 0, "", 1, cursor)
		if err != nil || len(rows) != 1 {
			t.Fatalf("page: %v %v", rows, err)
		}
		f := rows[0]
		if f.GetProject() != "support" || f.GetRecord() != nil || seen[f.GetId()] {
			t.Fatalf("bad list row: %v", f)
		}
		seen[f.GetId()] = true
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("lost rows: %v", seen)
	}
	rows, _, err := s.ListFailures(ctx, "support", pb.FailureStatus_FAILURE_STATUS_IN_REVIEW, "owner", 50, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("filter: %v %v", rows, err)
	}
	if _, err := s.GetFailure(ctx, "checkout", rows[0].GetId()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project get: %v", err)
	}
	selected, err := s.SelectedFailures(ctx, "checkout", []string{rows[0].GetId()})
	if err != nil || len(selected) != 0 {
		t.Fatalf("cross-project export: %v %v", selected, err)
	}
}

func TestFailurePayloadAndHistorySurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := s.CaptureFailure(t.Context(), failureFixture("support", "trace", "hash"), &pb.FailureEvent{Revision: 1, Action: "captured"})
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.GetFailure(t.Context(), "support", f.GetId())
	if err != nil || !proto.Equal(got, f) {
		t.Fatalf("case did not survive restart: %v %v", got, err)
	}
	events, _, err := s.ListFailureEvents(t.Context(), "support", f.GetId(), 50, 0)
	if err != nil || len(events) != 1 || events[0].GetAction() != "captured" {
		t.Fatalf("history did not survive restart: %v %v", events, err)
	}
}

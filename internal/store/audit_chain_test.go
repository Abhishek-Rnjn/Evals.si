package store

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
)

func appendEvents(t *testing.T, s *Store, n int, start time.Time) []int64 {
	t.Helper()
	var ids []int64
	for i := range n {
		ev := &evalsiv1alpha1.AuditEvent{Time: timestamppb.New(start.Add(time.Duration(i) * time.Minute)),
			Principal: "key:ci", Action: "runs.create", Project: "support", Allowed: true}
		id, err := s.AppendAudit(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		if ev.GetSequence() == 0 || ev.GetHash() == "" {
			t.Fatalf("event not chained: %v", ev)
		}
		ids = append(ids, id)
	}
	return ids
}

func verify(t *testing.T, s *Store) *evalsiv1alpha1.VerifyAuditLogResponse {
	t.Helper()
	res, err := s.VerifyAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTheAuditChainDetectsEditsAndGaps(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ids := appendEvents(t, s, 6, start)

	res := verify(t, s)
	if !res.GetOk() || res.GetChecked() != 6 || res.GetFirstSequence() != 1 || res.GetLastSequence() != 6 || res.GetLastHash() == "" {
		t.Fatalf("intact chain: %v", res)
	}
	listed, _, err := s.ListAudit(ctx, authz.AuditQuery{}, 10, "")
	if err != nil || listed[0].GetHash() != res.GetLastHash() || listed[0].GetPrevHash() == "" {
		t.Fatalf("listed events lack the chain: %v %v", listed, err)
	}

	// Retention removes the oldest entries: the rest still verifies.
	if n, err := s.DeleteAuditBefore(ctx, start.Add(90*time.Second)); err != nil || n != 2 {
		t.Fatalf("retention deleted %d, %v", n, err)
	}
	if res := verify(t, s); !res.GetOk() || res.GetFirstSequence() != 3 || res.GetChecked() != 4 {
		t.Fatalf("after retention: %v", res)
	}

	// Deleting entries from the middle breaks the chain.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE id = ?`, ids[3]); err != nil {
		t.Fatal(err)
	}
	if res := verify(t, s); res.GetOk() || res.GetBreakSequence() != 5 {
		t.Fatalf("a deleted entry went unnoticed: %v", res)
	}

	// Editing an entry, here turning a denial into an allowed call, is caught.
	s2 := open(t)
	ids = appendEvents(t, s2, 3, start)
	if _, err := s2.db.ExecContext(ctx, `UPDATE audit SET event = (SELECT event FROM audit WHERE id = ?) WHERE id = ?`, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	if res := verify(t, s2); res.GetOk() || res.GetBreakSequence() != 2 {
		t.Fatalf("an edited entry went unnoticed: %v", res)
	}
}

// Entries from before the chain (or from a release without it) are counted,
// not treated as breaks.
func TestEntriesWithoutAChainAreCounted(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (time_ns, project, action, allowed, event) VALUES (1, 'support', 'runs.create', 1, ?)`,
		marshal(&evalsiv1alpha1.AuditEvent{Action: "runs.create"})); err != nil {
		t.Fatal(err)
	}
	appendEvents(t, s, 2, time.Now())
	if res := verify(t, s); !res.GetOk() || res.GetUnchained() != 1 || res.GetChecked() != 2 {
		t.Fatalf("%v", res)
	}
}

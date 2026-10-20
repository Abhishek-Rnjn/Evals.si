package store

import (
	"errors"
	"sync"
	"testing"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func releaseStoreFixture(id string) *pb.StoredReleaseDecision {
	d := &pb.ReleaseDecision{Id: id, Project: "support", Outcome: pb.ReleaseOutcome_RELEASE_OUTCOME_PASS, Policy: &pb.ReleasePolicy{Name: "standard", Version: "v1"}, Baseline: &pb.ReleaseRunEvidence{Reference: &pb.ReleaseRunEvidenceRef{RunId: "baseline"}}, Candidate: &pb.ReleaseRunEvidence{Reference: &pb.ReleaseRunEvidenceRef{RunId: "candidate"}}}
	d.Sha256 = ReleaseDecisionHash(d)
	return &pb.StoredReleaseDecision{Decision: d, BaselineAccess: &pb.Run{Id: "baseline", Project: "support", Labels: map[string]string{"team": "support", "classification": "private"}}, CandidateAccess: &pb.Run{Id: "candidate", Project: "support"}, DatasetAccess: &pb.DatasetVersionContent{Project: "support", Name: "suite", Version: "v1", Labels: map[string]string{"team": "support", "class": "private"}}}
}
func TestReleaseDecisionStoreImmutabilityAndMapHash(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	d := releaseStoreFixture("v1")
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { results <- st.CreateReleaseDecision(ctx, proto.Clone(d).(*pb.StoredReleaseDecision)) })
	}
	wg.Wait()
	close(results)
	wins, duplicates := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrExists) {
			duplicates++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || duplicates != 7 {
		t.Fatalf("publication wins=%d duplicates=%d", wins, duplicates)
	}
	// Repeated deterministic verification must ignore protobuf map iteration order.
	for range 30 {
		got, err := st.GetReleaseDecision(ctx, "support", "v1")
		if err != nil || !proto.Equal(got, d) {
			t.Fatalf("stored report mismatch: %v %v", got, err)
		}
	}
	if _, err := st.GetReleaseDecision(ctx, "other", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("project isolation", err)
	}
}
func TestReleaseDecisionStoreDetectsReportAndAccessTampering(t *testing.T) {
	for _, access := range []bool{false, true} {
		t.Run(map[bool]string{false: "report", true: "access"}[access], func(t *testing.T) {
			st := open(t)
			d := releaseStoreFixture("v1")
			if err := st.CreateReleaseDecision(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			if access {
				d.CandidateAccess.Project = "other"
			} else {
				d.Decision.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK
			}
			if _, err := st.db.ExecContext(t.Context(), `UPDATE release_decisions SET body=? WHERE project=? AND id=?`, marshal(d), "support", "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetReleaseDecision(t.Context(), "support", "v1"); !errors.Is(err, ErrReleaseIntegrity) {
				t.Fatal("tampering undetected", err)
			}
			if _, err := st.ListReleaseDecisions(t.Context(), "support", "", 20); !errors.Is(err, ErrReleaseIntegrity) {
				t.Fatal("list tampering undetected", err)
			}
		})
	}
}

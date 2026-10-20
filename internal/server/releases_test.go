package server

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"google.golang.org/protobuf/proto"
)

func TestReleaseDecisionAuthorizationRESTAndRunRetention(t *testing.T) {
	s := startAuthServer(t, func(*config.Config) {})
	ctx := t.Context()
	catalog := func(key string) evalsiv1alpha1connect.DatasetCatalogServiceClient {
		return evalsiv1alpha1connect.NewDatasetCatalogServiceClient(s.http, s.url, as(s.keys[key]))
	}
	release := func(key string) evalsiv1alpha1connect.ReleaseServiceClient {
		return evalsiv1alpha1connect.NewReleaseServiceClient(s.http, s.url, as(s.keys[key]))
	}
	runClient := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"]))
	created, err := catalog("editor").PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: &pb.DatasetVersionContent{Project: "support", Name: "release-suite", Version: "v1", Owner: "support", Split: "held-out", Records: []*pb.Record{{Id: "one", Input: text("request"), Output: text("yes"), Reference: text("yes")}, {Id: "two", Input: text("other request"), Output: text("yes"), Reference: text("yes")}}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := created.Msg.GetDataset()
	if _, err := catalog("admin").ReviewDatasetVersion(ctx, connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: "support", Name: "release-suite", Version: "v1", Sha256: d.GetSha256(), ExpectedRevision: 1, Decision: pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED, Note: "reviewed"})); err != nil {
		t.Fatal(err)
	}
	refs := []*pb.ReleaseRunEvidenceRef{}
	for _, commit := range []string{"a", "b"} {
		response, err := runClient.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{Project: "support", Labels: map[string]string{"agent.commit": strings.Repeat(commit, 40)}, Spec: &pb.RunSpec{Dataset: &pb.DatasetSource{Source: &pb.DatasetSource_Catalog{Catalog: created.Msg.GetReference()}}, Evaluators: []*pb.EvaluatorRef{{Ref: "exact-match"}}}}))
		if err != nil {
			t.Fatal(err)
		}
		id := response.Msg.GetRun().GetId()
		var run *pb.Run
		for range 100 {
			r, err := runClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{Id: id}))
			if err != nil {
				t.Fatal(err)
			}
			run = r.Msg.GetRun()
			if run.GetStatus() == pb.RunStatus_RUN_STATUS_SUCCEEDED {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if run.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
			t.Fatal(run)
		}
		evidence, err := release("runner").GetReleaseRunEvidence(ctx, connect.NewRequest(&pb.GetReleaseRunEvidenceRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, evidence.Msg.GetEvidence().GetReference())
	}
	policy := &pb.ReleasePolicy{Name: "production", Version: "v1", Dataset: created.Msg.GetReference(), MinRecords: 2, Rules: []*pb.ReleaseMetricRule{{Metric: "exact-match", Direction: pb.MetricDirection_METRIC_DIRECTION_HIGHER, MaxRegression: proto.Float64(0), MinCase: proto.Float64(1)}}}
	inspected, err := release("viewer").InspectReleasePolicy(ctx, connect.NewRequest(&pb.InspectReleasePolicyRequest{Policy: policy}))
	if err != nil {
		t.Fatal(err)
	}
	request := func(id string) *connect.Request[pb.CreateReleaseDecisionRequest] {
		return connect.NewRequest(&pb.CreateReleaseDecisionRequest{Project: "support", Id: id, Policy: policy, PolicySha256: inspected.Msg.GetSha256(), Baseline: refs[0], Candidate: refs[1]})
	}
	if _, err := release("viewer").CreateReleaseDecision(ctx, request("forbidden")); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer wrote decision: %v", err)
	}
	if _, err := release("checkout-runner").CreateReleaseDecision(ctx, request("forbidden")); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross project decision: %v", err)
	}
	decision, err := release("runner").CreateReleaseDecision(ctx, request("release-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Msg.GetDecision().GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_PASS {
		t.Fatal(decision.Msg)
	}
	if _, err := release("blind-runner").GetReleaseDecision(ctx, connect.NewRequest(&pb.GetReleaseDecisionRequest{Project: "support", Id: "release-v1"})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("run permission bypass: %v", err)
	}
	if _, err := release("checkout-viewer").GetReleaseDecision(ctx, connect.NewRequest(&pb.GetReleaseDecisionRequest{Project: "support", Id: "release-v1"})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-project read: %v", err)
	}
	adminRuns := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["admin"]))
	for _, ref := range refs {
		if _, err := adminRuns.DeleteRun(ctx, connect.NewRequest(&pb.DeleteRunRequest{Id: ref.GetRunId()})); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := release("viewer").GetReleaseDecision(ctx, connect.NewRequest(&pb.GetReleaseDecisionRequest{Project: "support", Id: "release-v1"}))
	if err != nil || saved.Msg.GetDecision().GetSha256() != decision.Msg.GetDecision().GetSha256() {
		t.Fatalf("retained decision: %v %v", saved, err)
	}
	listed, err := release("checkout-viewer").ListReleaseDecisions(ctx, connect.NewRequest(&pb.ListReleaseDecisionsRequest{Project: "support", PageSize: 1}))
	if err != nil || len(listed.Msg.GetDecisions()) != 0 || listed.Msg.GetNextPageToken() != "" {
		t.Fatalf("list leaked private decision: %v %v", listed, err)
	}
}

func TestReleaseTargetsUseCapturedEffectiveAuthorization(t *testing.T) {
	baseline := &pb.Run{Id: "baseline", Project: "support", Spec: &pb.RunSpec{}}
	candidate := &pb.Run{Id: "candidate", Project: "support", Spec: &pb.RunSpec{}}
	d := &pb.StoredReleaseDecision{Decision: &pb.ReleaseDecision{Id: "saved", Project: "support"}, BaselineAccess: baseline, CandidateAccess: candidate,
		BaselineAttributes:  &pb.ReleaseRunAccessAttributes{Judge: "restricted-judge", RunsCode: true},
		CandidateAttributes: &pb.ReleaseRunAccessAttributes{Judge: "other-judge"}, DatasetAccess: &pb.DatasetVersionContent{Project: "support", Name: "suite", Version: "v1"}}
	targets := releaseTargets(d)
	if len(targets) != 4 {
		t.Fatal(targets)
	}
	for i, want := range []*pb.ReleaseRunAccessAttributes{d.CandidateAttributes, d.BaselineAttributes, d.CandidateAttributes} {
		if targets[i].resource["judge"] != want.GetJudge() || targets[i].resource["runs_code"] != want.GetRunsCode() {
			t.Fatalf("gate lost captured authorization for target %d: %v", i, targets[i])
		}
	}
	if targets[1].action != "runs.read" || targets[2].action != "runs.read" || targets[3].action != "datasets.read" {
		t.Fatal(targets)
	}
}

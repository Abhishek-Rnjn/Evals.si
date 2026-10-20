package runs

import (
	"context"
	"math"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"google.golang.org/protobuf/proto"
)

type releaseFixture struct {
	h                   *harness
	dataset             *pb.DatasetVersion
	baseline, candidate *pb.Run
	policy              *pb.ReleasePolicy
}

func releaseFixtureFor(t *testing.T) *releaseFixture {
	t.Helper()
	h := newHarness(t)
	ctx := t.Context()
	catalog := datasetcatalog.New(h.st)
	published, err := catalog.PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: &pb.DatasetVersionContent{Project: "support", Name: "regressions", Version: "v1", Owner: "support", Split: "held-out", Records: []*pb.Record{{Id: "refund", Input: text("easy"), Output: text("right"), Reference: text("right")}, {Id: "handoff", Input: text("easy"), Output: text("right"), Reference: text("right")}}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := published.Msg.GetDataset()
	approved, err := catalog.ReviewDatasetVersion(ctx, connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: "support", Name: "regressions", Version: "v1", Sha256: d.GetSha256(), ExpectedRevision: 1, Decision: pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED, Note: "reviewed"}))
	if err != nil {
		t.Fatal(err)
	}
	d = approved.Msg.GetDataset()
	create := func(commit string) *pb.Run {
		r, err := h.m.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{Project: "support", Labels: map[string]string{"agent.commit": strings.Repeat(commit, 40), "team": "support"}, Spec: &pb.RunSpec{Dataset: &pb.DatasetSource{Source: &pb.DatasetSource_Catalog{Catalog: datasetcatalog.Reference(d)}}, Evaluators: refs("exact-match")}}))
		if err != nil {
			t.Fatal(err)
		}
		return h.wait(t, r.Msg.GetRun().GetId())
	}
	return &releaseFixture{h: h, dataset: d, baseline: create("a"), candidate: create("b"), policy: &pb.ReleasePolicy{Name: "support", Version: "v1", Dataset: datasetcatalog.Reference(d), MinRecords: 2, Rules: []*pb.ReleaseMetricRule{{Metric: "exact-match", Direction: pb.MetricDirection_METRIC_DIRECTION_HIGHER, MaxRegression: proto.Float64(0), MinCase: proto.Float64(1)}}}}
}
func (f *releaseFixture) request(t *testing.T, id string) *connect.Request[pb.CreateReleaseDecisionRequest] {
	t.Helper()
	ref := func(run *pb.Run) *pb.ReleaseRunEvidenceRef {
		r, err := f.h.m.GetReleaseRunEvidence(t.Context(), connect.NewRequest(&pb.GetReleaseRunEvidenceRequest{RunId: run.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetEvidence().GetReference()
	}
	return connect.NewRequest(&pb.CreateReleaseDecisionRequest{Id: id, Project: "support", Policy: proto.Clone(f.policy).(*pb.ReleasePolicy), PolicySha256: releaseHash("evalsi.release-policy/v1", f.policy), Baseline: ref(f.baseline), Candidate: ref(f.candidate)})
}
func TestReleaseDecisionPassPersistsAndSurvivesSourceRetention(t *testing.T) {
	f := releaseFixtureFor(t)
	r := f.request(t, "release-v1")
	result, err := f.h.m.CreateReleaseDecision(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	d := result.Msg.GetDecision()
	if d.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_PASS || len(d.GetMetrics()) != 1 || d.GetMetrics()[0].GetPairedRecords() != 2 || d.GetSha256() != store.ReleaseDecisionHash(d) {
		t.Fatal(d)
	}
	if _, err := f.h.m.CreateReleaseDecision(t.Context(), r); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("overwrote decision: %v", err)
	}
	if err := f.h.st.DeleteRun(t.Context(), f.baseline.GetId()); err != nil {
		t.Fatal(err)
	}
	if err := f.h.st.DeleteRun(t.Context(), f.candidate.GetId()); err != nil {
		t.Fatal(err)
	}
	got, err := f.h.m.GetReleaseDecision(t.Context(), connect.NewRequest(&pb.GetReleaseDecisionRequest{Project: "support", Id: d.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetDecision(), d) {
		t.Fatalf("retention lost decision: %v %v", got, err)
	}
	engine, err := authz.NewEngine(t.Context(), authz.Options{Enabled: true, RBAC: authz.RBACConfig{Roles: []authz.Role{{Name: "release-only", Permissions: []string{"releases.read", "datasets.read"}}}, Projects: map[string]map[string][]string{"support": {"release-only": {"key:limited"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{Kind: auth.KindAPIKey, KeyName: "limited"}
	ctx := authz.WithChecker(auth.WithResult(t.Context(), auth.Result{Principal: p}), &authz.Checker{Engine: engine, Principal: p})
	if _, err := f.h.m.GetReleaseDecision(ctx, connect.NewRequest(&pb.GetReleaseDecisionRequest{Project: "support", Id: d.GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("source run access bypass after deletion: %v", err)
	}
	list, err := f.h.m.ListReleaseDecisions(ctx, connect.NewRequest(&pb.ListReleaseDecisionsRequest{Project: "support", PageSize: 1}))
	if err != nil || len(list.Msg.GetDecisions()) != 0 || list.Msg.GetNextPageToken() != "" {
		t.Fatalf("hidden release leaked: %v %v", list, err)
	}
}
func TestReleaseDecisionCriticalFailureAndEvidencePins(t *testing.T) {
	f := releaseFixtureFor(t)
	rows, err := f.h.st.Results(t.Context(), f.candidate.GetId(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := f.request(t, "old-pin")
	rows[0].Result.Scores[0].Value = &pb.Score_Passed{Passed: false}
	if err := f.h.st.PutResults(t.Context(), f.candidate.GetId(), rows); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.m.CreateReleaseDecision(t.Context(), old); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale evidence passed: %v", err)
	}
	r := f.request(t, "blocked")
	out, err := f.h.m.CreateReleaseDecision(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	d := out.Msg.GetDecision()
	if d.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK || len(d.GetMetrics()[0].GetChanges()) != 1 || !d.GetMetrics()[0].GetChanges()[0].GetCriticalFailure() {
		t.Fatal(d)
	}
	if d.GetMetrics()[0].GetChanges()[0].GetRecordId() != "refund" {
		t.Fatal(d)
	}
	bad := f.request(t, "bad-policy")
	bad.Msg.Policy.Rules[0].MinCase = proto.Float64(0)
	if _, err := f.h.m.CreateReleaseDecision(t.Context(), bad); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("changed policy pin accepted: %v", err)
	}
	bad = f.request(t, "bad-commit")
	bad.Msg.Candidate.Commit = strings.Repeat("c", 40)
	if _, err := f.h.m.CreateReleaseDecision(t.Context(), bad); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("different declared commit accepted: %v", err)
	}
}
func TestReleaseDecisionIncompleteOrIncompatibleEvidenceCannotPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*releaseFixture)
	}{
		{"sample-size", func(f *releaseFixture) { f.policy.MinRecords = 3 }},
		{"unmeasured-cost", func(f *releaseFixture) { f.policy.Rules[0].Metric = "usage.cost_usd" }},
		{"critical-case-missing", func(f *releaseFixture) { f.policy.Rules[0].CaseIds = []string{"absent"} }},
		{"old-contract", func(f *releaseFixture) {
			f.candidate.EvaluationContract = nil
			_ = f.h.st.UpdateRun(context.Background(), f.candidate)
		}},
		{"contract-changed", func(f *releaseFixture) {
			f.candidate.EvaluationContract.ServerVersion = "changed"
			_ = f.h.st.UpdateRun(context.Background(), f.candidate)
		}},
		{"not-terminal", func(f *releaseFixture) {
			f.candidate.Status = pb.RunStatus_RUN_STATUS_ERROR
			_ = f.h.st.UpdateRun(context.Background(), f.candidate)
		}},
		{"imported", func(f *releaseFixture) {
			f.candidate.Imported = true
			_ = f.h.st.UpdateRun(context.Background(), f.candidate)
		}},
		{"errored-score", func(f *releaseFixture) {
			rows, _ := f.h.st.Results(context.Background(), f.candidate.GetId(), "", 0, 0)
			rows[0].Result.Outcome = pb.Outcome_OUTCOME_ERROR
			_ = f.h.st.PutResults(context.Background(), f.candidate.GetId(), rows)
		}},
		{"retired-dataset", func(f *releaseFixture) {
			d := f.dataset
			_, _ = datasetcatalog.New(f.h.st).ReviewDatasetVersion(context.Background(), connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: "support", Name: d.GetContent().GetName(), Version: "v1", Sha256: d.GetSha256(), ExpectedRevision: 2, Decision: pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED, Note: "retired"}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := releaseFixtureFor(t)
			tc.mutate(f)
			out, err := f.h.m.CreateReleaseDecision(t.Context(), f.request(t, "decision"))
			if err != nil {
				t.Fatal(err)
			}
			if out.Msg.GetDecision().GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT {
				t.Fatal(out.Msg)
			}
		})
	}
}
func TestReleaseMetricTrialsClusterAndLowerDirection(t *testing.T) {
	records := []*pb.Record{{Id: "one"}, {Id: "two"}}
	values := func(a, b float64) releaseValues {
		return releaseValues{metrics: map[string]map[[2]int]float64{"usage.latency_ms": {{0, 0}: a, {0, 1}: a, {1, 0}: b, {1, 1}: b}}}
	}
	rule := &pb.ReleaseMetricRule{Metric: "usage.latency_ms", Direction: pb.MetricDirection_METRIC_DIRECTION_LOWER, MaxRegression: proto.Float64(0)}
	policy := &pb.ReleasePolicy{MinRecords: 2}
	good := metricDecision(rule, policy, values(10, 20), values(9, 19), records, 2)
	if good.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_PASS || good.GetPairedRecords() != 2 || good.GetMeanRegression() != -1 {
		t.Fatal(good)
	}
	bad := metricDecision(rule, policy, values(10, 20), values(11, 21), records, 2)
	if bad.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK {
		t.Fatal(bad)
	}
	policy.MinRecords = 3
	insufficient := metricDecision(rule, policy, values(10, 20), values(9, 19), records, 2)
	if insufficient.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT {
		t.Fatal("trials counted as independent records", insufficient)
	}
}

func TestReleaseRejectsTargetChangingApprovedExpectations(t *testing.T) {
	f := releaseFixtureFor(t)
	run := f.candidate
	run.Spec.Target = target()
	if err := f.h.st.UpdateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	records, err := f.h.st.Records(t.Context(), run.GetId())
	if err != nil {
		t.Fatal(err)
	}
	var outputs []store.Output
	for i, record := range records {
		record.Reference = text("forged expectation")
		outputs = append(outputs, store.Output{RecordIdx: i, Record: record})
	}
	if err := f.h.st.PutOutputs(t.Context(), run.GetId(), outputs); err != nil {
		t.Fatal(err)
	}
	out, err := f.h.m.CreateReleaseDecision(t.Context(), f.request(t, "forged-reference"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Msg.GetDecision().GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT {
		t.Fatal("target altered approved expectations", out.Msg)
	}
}
func TestChangedEvaluationContractStopsResumedWork(t *testing.T) {
	f := releaseFixtureFor(t)
	run := f.candidate
	run.Status = pb.RunStatus_RUN_STATUS_ERROR
	if err := f.h.st.UpdateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	before := f.h.worker.evaluateCalls.Load()
	f.h.m.opts.Evaluate.BatchSize++
	if _, err := f.h.m.ResumeRun(t.Context(), connect.NewRequest(&pb.ResumeRunRequest{Id: run.GetId()})); err != nil {
		t.Fatal(err)
	}
	after := f.h.wait(t, run.GetId())
	if after.GetStatus() != pb.RunStatus_RUN_STATUS_ERROR || !strings.Contains(after.GetError(), "contract changed") || f.h.worker.evaluateCalls.Load() != before {
		t.Fatalf("resumed mixed contracts: %v", after)
	}
}
func TestCriticalCaseCannotHideBehindPassingMean(t *testing.T) {
	records := []*pb.Record{{Id: "critical"}, {Id: "other"}}
	values := func(x, y float64) releaseValues {
		return releaseValues{metrics: map[string]map[[2]int]float64{"quality": {{0, 0}: x, {1, 0}: y}}}
	}
	rule := &pb.ReleaseMetricRule{Metric: "quality", Direction: pb.MetricDirection_METRIC_DIRECTION_HIGHER, MinMean: proto.Float64(.5), MinCase: proto.Float64(.2), CaseIds: []string{"critical"}}
	result := metricDecision(rule, &pb.ReleasePolicy{MinRecords: 2}, values(.2, .8), values(0, 1), records, 1)
	if result.GetCandidateMean() != .5 || result.GetOutcome() != pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK || !result.GetChanges()[0].GetCriticalFailure() {
		t.Fatal(result)
	}
}

func TestReleasePolicyNonfiniteThresholdsCannotProduceDecisions(t *testing.T) {
	f := releaseFixtureFor(t)
	for _, mutate := range []func(*pb.ReleasePolicy){
		func(p *pb.ReleasePolicy) { p.Rules[0].MaxRegression = proto.Float64(math.NaN()) },
		func(p *pb.ReleasePolicy) { p.Rules[0].MinCase = proto.Float64(math.Inf(1)) },
		func(p *pb.ReleasePolicy) { p.ConfidenceLevel = math.NaN() },
		func(p *pb.ReleasePolicy) { p.Rules[0].Direction = pb.MetricDirection_METRIC_DIRECTION_UNSPECIFIED },
	} {
		request := f.request(t, "invalid-policy")
		mutate(request.Msg.Policy)
		request.Msg.PolicySha256 = releaseHash("evalsi.release-policy/v1", request.Msg.Policy)
		if _, err := f.h.m.CreateReleaseDecision(t.Context(), request); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid policy accepted: %v", err)
		}
	}
	list, err := f.h.m.ListReleaseDecisions(t.Context(), connect.NewRequest(&pb.ListReleaseDecisionsRequest{Project: "support"}))
	if err != nil || len(list.Msg.GetDecisions()) != 0 {
		t.Fatalf("invalid policy persisted: %v %v", list, err)
	}
}

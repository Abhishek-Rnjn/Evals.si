package evaluation

import (
	"context"
	"math"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// A NaN score would make the metric's mean NaN, and NaN passes every gate
// comparison, so the engine records it as an error instead.
func TestNonFiniteScoresBecomeErrors(t *testing.T) {
	svc := newService(t, &fakeWorker{})
	insts, err := svc.Bind([]*evalsiv1alpha1.EvaluatorRef{{Ref: "builtin/llm-judge"}}, "claude")
	if err != nil {
		t.Fatal(err)
	}
	results, err := svc.RunRecords(context.Background(), insts, []*evalsiv1alpha1.Record{
		record("ok", "x", "", "t"), record("nan", "x", "", "t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
		t.Errorf("finite score: %v", results[0])
	}
	if r := results[1]; r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_ERROR || len(r.GetScores()) != 0 || r.GetReason() == "" {
		t.Errorf("NaN score: %v", r)
	}
	if _, bad := NonFinite([]*evalsiv1alpha1.Score{{Name: "x", Value: &evalsiv1alpha1.Score_Number{Number: math.Inf(-1)}}}); !bad {
		t.Error("-Inf is finite?")
	}
}

func TestSummarizeRefusesNaNConfidenceLevel(t *testing.T) {
	in := Instance{Name: "e", Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: "acme/e"}}
	res := []*evalsiv1alpha1.EvaluationResult{
		{Evaluator: "e", RecordId: "a", Scores: []*evalsiv1alpha1.Score{{Name: "e", Value: &evalsiv1alpha1.Score_Number{Number: 0.2}}}},
		{Evaluator: "e", RecordId: "b", Scores: []*evalsiv1alpha1.Score{{Name: "e", Value: &evalsiv1alpha1.Score_Number{Number: 0.8}}}},
	}
	for _, level := range []float64{math.NaN(), 1, -0.5} {
		_, err := Summarize([]Instance{in}, res, nil, &evalsiv1alpha1.SummaryOptions{ConfidenceLevel: level}, 1)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("level %v: %v", level, err)
		}
	}
}

// A trial whose evaluation errored (the agent crashed) did not pass: one
// pass and four crashes is pass@5 of 1 but pass^5 of 0.
func TestPassAtKCountsErroredTrials(t *testing.T) {
	in := Instance{Name: "task-success", Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: "builtin/task-success"}}
	res := []*evalsiv1alpha1.EvaluationResult{{
		Evaluator: "task-success", RecordId: "r", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
		Scores: []*evalsiv1alpha1.Score{{Name: "task-success", Value: &evalsiv1alpha1.Score_Passed{Passed: true}}},
	}}
	for trial := 1; trial < 5; trial++ {
		res = append(res, &evalsiv1alpha1.EvaluationResult{
			Evaluator: "task-success", RecordId: "r", Trial: int32(trial),
			Outcome: evalsiv1alpha1.Outcome_OUTCOME_ERROR, Reason: "target failed",
		})
	}
	// A record that never ran at all counts too, as failed in every trial.
	for trial := range 5 {
		res = append(res, &evalsiv1alpha1.EvaluationResult{
			Evaluator: "task-success", RecordId: "never", Trial: int32(trial),
			Outcome: evalsiv1alpha1.Outcome_OUTCOME_ERROR, Reason: "target failed",
		})
	}
	sums, err := Summarize([]Instance{in}, res, []*evalsiv1alpha1.Record{{Id: "r"}, {Id: "never"}}, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*evalsiv1alpha1.MetricSummary{}
	for _, s := range sums {
		got[s.GetMetric()] = s
	}
	if s := got["task-success.pass@5"]; s.GetN() != 2 || s.GetMean() != 0.5 || s.GetErrors() != 0 {
		t.Errorf("pass@5 = %v", s)
	}
	if s := got["task-success.pass^5"]; s.GetN() != 2 || s.GetMean() != 0 {
		t.Errorf("pass^5 = %v", s)
	}
	// The errors are the base metric's, counted once.
	if s := got["task-success"]; s.GetErrors() != 9 {
		t.Errorf("task-success = %v", s)
	}
}

// A passed trial the trial-tampering evaluator flagged is not credited by
// pass@k, but only when the run asks for it.
func TestPassAtKExcludesFlaggedTrialsOnRequest(t *testing.T) {
	check := Instance{Name: "task-success", Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: "builtin/task-success"}}
	hack := Instance{Name: "trial-tampering", Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: "builtin/trial-tampering"}}
	passed := func(name string, ok bool) []*evalsiv1alpha1.Score {
		return []*evalsiv1alpha1.Score{{Name: name, Value: &evalsiv1alpha1.Score_Passed{Passed: ok}}}
	}
	var res []*evalsiv1alpha1.EvaluationResult
	for trial := range 2 {
		res = append(res,
			&evalsiv1alpha1.EvaluationResult{Evaluator: "task-success", RecordId: "r", Trial: int32(trial), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: passed("task-success", trial == 0)},
			// The only passing trial (0) is flagged.
			&evalsiv1alpha1.EvaluationResult{Evaluator: "trial-tampering", RecordId: "r", Trial: int32(trial), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: passed("reward-hacking-clean", trial != 0)},
		)
	}
	atK := func(opts *evalsiv1alpha1.SummaryOptions) float64 {
		sums, err := Summarize([]Instance{check, hack}, res, []*evalsiv1alpha1.Record{{Id: "r"}}, opts, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sums {
			if s.GetMetric() == "task-success.pass@2" {
				return s.GetMean()
			}
		}
		t.Fatal("no pass@2")
		return -1
	}
	if got := atK(nil); got != 1 {
		t.Errorf("default pass@2 = %v, want 1", got)
	}
	if got := atK(&evalsiv1alpha1.SummaryOptions{ExcludeFlaggedTrials: true}); got != 0 {
		t.Errorf("excluding pass@2 = %v, want 0", got)
	}
}

// A staged evaluator (evidence-stage-audit) emits a later stage only when the one before
// passed. A declared output a record did not emit is neither an error nor a zero: the
// stage's N is how many records reached it, and its mean is the rate among them.
func TestSummarizeTreatsAnUnemittedDeclaredOutputAsNotReached(t *testing.T) {
	stage := func(name string) *evalsiv1alpha1.MetricSpec {
		return &evalsiv1alpha1.MetricSpec{Name: name, Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}
	}
	in := Instance{Name: "audit", Manifest: &evalsiv1alpha1.EvaluatorManifest{
		Name:    "builtin/audit",
		Outputs: []*evalsiv1alpha1.MetricSpec{stage("one"), stage("two"), stage("three")},
	}}
	passed := func(name string, ok bool) *evalsiv1alpha1.Score {
		return &evalsiv1alpha1.Score{Name: name, Value: &evalsiv1alpha1.Score_Passed{Passed: ok}}
	}
	scored := func(id string, scores ...*evalsiv1alpha1.Score) *evalsiv1alpha1.EvaluationResult {
		return &evalsiv1alpha1.EvaluationResult{
			Evaluator: "audit", RecordId: id, Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: scores,
		}
	}
	res := []*evalsiv1alpha1.EvaluationResult{
		scored("a", passed("one", false)),
		scored("b", passed("one", true), passed("two", false)),
		scored("c", passed("one", true), passed("two", true), passed("three", false)),
		scored("d", passed("one", true), passed("two", true), passed("three", true)),
	}
	sums, err := Summarize([]Instance{in}, res, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*evalsiv1alpha1.MetricSummary{}
	for _, s := range sums {
		got[s.GetMetric()] = s
	}
	for name, want := range map[string]struct {
		n    int64
		mean float64
	}{"audit.one": {4, 0.75}, "audit.two": {3, 2.0 / 3}, "audit.three": {2, 0.5}} {
		s := got[name]
		if s == nil || s.GetN() != want.n || math.Abs(s.GetMean()-want.mean) > 1e-12 || s.GetErrors() != 0 {
			t.Errorf("%s = %v, want n=%d mean=%v", name, s, want.n, want.mean)
		}
	}
}

// "1", 1 and true are different cluster values, as in the Python SDK.
func TestClusterOfKeepsTypesApart(t *testing.T) {
	vals := []*structpb.Value{
		structpb.NewStringValue("1"), structpb.NewNumberValue(1),
		structpb.NewStringValue("true"), structpb.NewBoolValue(true),
		structpb.NewStringValue("null"), structpb.NewNullValue(),
	}
	seen := map[string]bool{}
	for _, v := range vals {
		r := &evalsiv1alpha1.Record{Metadata: map[string]*structpb.Value{"k": v}}
		seen[clusterOf(r, "k", "id")] = true
	}
	if len(seen) != len(vals) {
		t.Errorf("got %d distinct clusters for %d typed values", len(seen), len(vals))
	}
}

// A CI that overflows is dropped rather than reported as +-Inf/NaN.
func TestSummarizeDropsNonFiniteCI(t *testing.T) {
	num := func(id string, x float64) scored {
		return scored{recordID: id, score: &evalsiv1alpha1.Score{Name: "m", Value: &evalsiv1alpha1.Score_Number{Number: x}}}
	}
	in := Instance{Name: "e", Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: "acme/e"}}
	sum, err := summarizeMetric("m", in, nil, []scored{num("a", 1.7e308), num("b", -1.7e308), num("c", 1.7e308)}, nil, 0.95, "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if sum.GetCi() != nil {
		t.Errorf("non-finite CI kept: %v", sum.GetCi())
	}
}

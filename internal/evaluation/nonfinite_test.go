package evaluation

import (
	"context"
	"math"
	"testing"

	"connectrpc.com/connect"

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
	if s := got["task-success.pass@5"]; s.GetN() != 2 || s.GetMean() != 0.5 || s.GetErrors() != 9 {
		t.Errorf("pass@5 = %v", s)
	}
	if s := got["task-success.pass^5"]; s.GetN() != 2 || s.GetMean() != 0 {
		t.Errorf("pass^5 = %v", s)
	}
}

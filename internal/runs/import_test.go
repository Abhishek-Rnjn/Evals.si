package runs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func importRequest(mean float64, gateMin float64) *evalsiv1alpha1.ImportRunRequest {
	passed := func(record string, ok bool) *evalsiv1alpha1.EvaluationResult {
		return &evalsiv1alpha1.EvaluationResult{
			RecordId: record, Evaluator: "exact-match", EvaluatorRef: "builtin/exact-match@1.0.0",
			Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
			Scores:  []*evalsiv1alpha1.Score{{Name: "exact-match", Value: &evalsiv1alpha1.Score_Passed{Passed: ok}}},
		}
	}
	return &evalsiv1alpha1.ImportRunRequest{
		Name: "epoch-3", Project: "p", Labels: map[string]string{"loop": "train"},
		Spec: &evalsiv1alpha1.RunSpec{
			Evaluators: refs("builtin/exact-match"),
			Gates:      []*evalsiv1alpha1.Gate{{Metric: "exact-match", Min: proto.Float64(gateMin)}},
		},
		Records: []*evalsiv1alpha1.Record{
			{Id: "a", Input: text("q1"), Output: text("right"), Reference: text("right")},
			{Id: "b", Input: text("q2"), Output: text("wrong"), Reference: text("right")},
		},
		Results:   []*evalsiv1alpha1.EvaluationResult{passed("a", true), passed("b", false)},
		Summaries: []*evalsiv1alpha1.MetricSummary{{Metric: "exact-match", Evaluator: "exact-match", N: 2, Mean: proto.Float64(mean)}},
	}
}

// An imported run is stored finished, with its records and results, and
// reported to webhooks as a run that ran here would be.
func TestImportRunStoresAFinishedRun(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var finished []*evalsiv1alpha1.Run
	h := newHarnessWith(t, st, dir, func(o *Options) {
		o.OnFinished = func(r *evalsiv1alpha1.Run) { finished = append(finished, r) }
	})
	ctx := context.Background()

	resp, err := h.m.ImportRun(ctx, connect.NewRequest(importRequest(0.5, 0.4)))
	if err != nil {
		t.Fatal(err)
	}
	run := resp.Msg.GetRun()
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || !run.GetGates()[0].GetPassed() {
		t.Fatalf("run = %v", run)
	}
	if run.GetRecords() != 2 || run.GetProgress().GetDone() != 2 || run.GetFinishedAt() == nil || run.GetLabels()["loop"] != "train" {
		t.Errorf("run = %v", run)
	}
	got, err := h.m.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: run.GetId()}))
	if err != nil || got.Msg.GetRun().GetSummaries()[0].GetMean() != 0.5 {
		t.Fatalf("stored = %v, %v", got, err)
	}
	results, err := h.m.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: run.GetId()}))
	if err != nil || len(results.Msg.GetResults()) != 2 {
		t.Fatalf("results = %v, %v", results, err)
	}
	if len(finished) != 1 || finished[0].GetId() != run.GetId() {
		t.Errorf("OnFinished saw %v", finished)
	}

	// A summary below the gate fails the run, as a gate does here.
	resp, err = h.m.ImportRun(ctx, connect.NewRequest(importRequest(0.5, 0.9)))
	if err != nil {
		t.Fatal(err)
	}
	if run := resp.Msg.GetRun(); run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED || run.GetGates()[0].GetPassed() {
		t.Errorf("gate failure: %v", run)
	}
}

func TestImportRunWhenEveryEvaluationErrored(t *testing.T) {
	h := newHarness(t)
	req := importRequest(0, 0)
	req.Summaries, req.Spec.Gates = nil, nil
	for _, r := range req.Results {
		r.Outcome, r.Scores, r.Reason = evalsiv1alpha1.Outcome_OUTCOME_ERROR, nil, "judge unreachable"
	}
	resp, err := h.m.ImportRun(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if run := resp.Msg.GetRun(); run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(run.GetError(), "judge unreachable") {
		t.Errorf("run = %v", run)
	}
}

func TestImportRunRejectsInconsistentRequests(t *testing.T) {
	h := newHarness(t)
	for name, tc := range map[string]struct {
		edit func(*evalsiv1alpha1.ImportRunRequest)
		want string
	}{
		"no evaluators":    {func(r *evalsiv1alpha1.ImportRunRequest) { r.Spec.Evaluators = nil }, "evaluators"},
		"no records":       {func(r *evalsiv1alpha1.ImportRunRequest) { r.Records, r.Results = nil, nil }, "records are required"},
		"duplicate record": {func(r *evalsiv1alpha1.ImportRunRequest) { r.Records[1].Id = "a" }, "used twice"},
		"unknown record":   {func(r *evalsiv1alpha1.ImportRunRequest) { r.Results[1].RecordId = "zz" }, "not among the records"},
		"trial too high":   {func(r *evalsiv1alpha1.ImportRunRequest) { r.Results[1].Trial = 1 }, "1 trial(s)"},
		"repeated result":  {func(r *evalsiv1alpha1.ImportRunRequest) { r.Results[1].RecordId = "a" }, "repeats record"},
		"no evaluator":     {func(r *evalsiv1alpha1.ImportRunRequest) { r.Results[0].Evaluator = "" }, "has no evaluator"},
		"gate without bound": {func(r *evalsiv1alpha1.ImportRunRequest) {
			r.Spec.Gates = []*evalsiv1alpha1.Gate{{Metric: "exact-match"}}
		}, "min or max"},
	} {
		t.Run(name, func(t *testing.T) {
			req := importRequest(0.5, 0.4)
			tc.edit(req)
			_, err := h.m.ImportRun(context.Background(), connect.NewRequest(req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want InvalidArgument with %q", err, tc.want)
			}
		})
	}
	runs, _, err := h.st.ListRuns(context.Background(), nil, 100, "")
	if err == nil && len(runs) != 0 {
		t.Errorf("a refused import stored %d run(s)", len(runs))
	}
}

// Imported runs are read back with the evaluator names their results carry,
// even when this server has no such evaluator, and cannot be resumed.
func TestImportedRunsCompareAndCannotResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	local := func(mean float64, wrong string) *evalsiv1alpha1.ImportRunRequest {
		req := importRequest(mean, 0.1)
		req.Spec.Evaluators = []*evalsiv1alpha1.EvaluatorRef{{Ref: "local/short-answer@1.0.0"}}
		for _, r := range req.Results {
			r.Evaluator, r.EvaluatorRef = "short-answer", "local/short-answer@1.0.0"
			r.Scores[0].Name = "short-answer"
			r.Scores[0].Value = &evalsiv1alpha1.Score_Passed{Passed: r.GetRecordId() != wrong}
		}
		req.Spec.Gates = nil
		return req
	}
	base, err := h.m.ImportRun(ctx, connect.NewRequest(local(0.5, "b")))
	if err != nil {
		t.Fatal(err)
	}
	cand, err := h.m.ImportRun(ctx, connect.NewRequest(local(1, "")))
	if err != nil {
		t.Fatal(err)
	}
	if !base.Msg.GetRun().GetImported() {
		t.Error("an imported run is not marked imported")
	}
	cmp, err := h.m.CompareRuns(ctx, connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{
		BaselineRunId: base.Msg.GetRun().GetId(), CandidateRunId: cand.Msg.GetRun().GetId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cmp.Msg.GetComparisons(); len(got) != 1 || got[0].GetMetric() != "short-answer" || got[0].GetPairedN() != 2 || got[0].GetDiff() != 0.5 {
		t.Errorf("comparisons = %v", got)
	}

	errored := importRequest(0, 0)
	errored.Summaries, errored.Spec.Gates = nil, nil
	for _, r := range errored.Results {
		r.Outcome, r.Scores = evalsiv1alpha1.Outcome_OUTCOME_ERROR, nil
	}
	run, err := h.m.ImportRun(ctx, connect.NewRequest(errored))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.m.ResumeRun(ctx, connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: run.Msg.GetRun().GetId()}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "imported") {
		t.Errorf("resuming an imported run: %v", err)
	}
}

package runs

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func meta(t *testing.T, kv map[string]any) map[string]*structpb.Value {
	t.Helper()
	s, err := structpb.NewStruct(kv)
	if err != nil {
		t.Fatal(err)
	}
	return s.GetFields()
}

func num(name string, v float64) *evalsiv1alpha1.Score {
	return &evalsiv1alpha1.Score{Name: name, Value: &evalsiv1alpha1.Score_Number{Number: v}}
}

func passed(name string, v bool) *evalsiv1alpha1.Score {
	return &evalsiv1alpha1.Score{Name: name, Value: &evalsiv1alpha1.Score_Passed{Passed: v}}
}

func scored(idx, trial int, evaluator, ref string, scores ...*evalsiv1alpha1.Score) store.Result {
	return store.Result{RecordIdx: idx, Trial: trial, EvalIdx: 0, Result: &evalsiv1alpha1.EvaluationResult{
		RecordId: fmt.Sprint("r", idx), Evaluator: evaluator, EvaluatorRef: ref, Trial: int32(trial),
		Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: scores,
	}}
}

func slice(t *testing.T, h *harness, req *evalsiv1alpha1.SliceRunsRequest) (*evalsiv1alpha1.SliceRunsResponse, error) {
	t.Helper()
	resp, err := h.m.SliceRuns(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// seedSlice stores a run whose records carry metadata of several shapes.
func seedSlice(t *testing.T, h *harness, id, name, project string, created time.Time, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	records := []*evalsiv1alpha1.Record{
		{Id: "r0", Metadata: meta(t, map[string]any{"topic": "billing", "tier": 1})},
		{Id: "r1", Metadata: meta(t, map[string]any{"topic": "billing", "tier": 1})},
		{Id: "r2", Metadata: meta(t, map[string]any{"topic": "billing", "tier": 2})},
		{Id: "r3", Metadata: meta(t, map[string]any{"topic": "refunds", "vip": true})},
		{Id: "r4", Metadata: meta(t, map[string]any{"nested": map[string]any{"team": "core"}})},
		{Id: "r5", Metadata: meta(t, map[string]any{"topic": nil})},
	}
	run := &evalsiv1alpha1.Run{Id: id, Name: name, Project: project, Labels: labels,
		Status: evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, CreatedAt: timestamppb.New(created)}
	if err := h.st.CreateRun(ctx, run, records); err != nil {
		t.Fatal(err)
	}
	// r0's output (trial 0) carries different metadata than its dataset record:
	// the score of that trial is grouped by the output's.
	if err := h.st.PutOutputs(ctx, id, []store.Output{
		{RecordIdx: 0, Trial: 0, Record: &evalsiv1alpha1.Record{Id: "r0", Metadata: meta(t, map[string]any{"topic": "shipping"})}},
	}); err != nil {
		t.Fatal(err)
	}
	em := "builtin/exact-match@1.0.0"
	results := []store.Result{
		scored(0, 0, "exact-match", em, passed("exact-match", true)),
		scored(0, 1, "exact-match", em, passed("exact-match", false)), // no output for trial 1: the dataset record
		scored(1, 0, "exact-match", em, passed("exact-match", false)),
		scored(2, 0, "exact-match", em, passed("exact-match", true)),
		scored(3, 0, "exact-match", em, passed("exact-match", true)),
		scored(4, 0, "exact-match", em, passed("exact-match", true)),
		scored(5, 0, "exact-match", em, passed("exact-match", false)),
		// A dataset-scope result has no record: (none).
		scored(-1, 0, "exact-match", em, passed("exact-match", true)),
		// Another score of the same evaluator and another evaluator.
		scored(1, 0, "tone", "", num("tone", 0.5), num("warmth", 0.75)),
		scored(2, 0, "tone", "", num("tone", 1.0)),
		// An errored result counts for nothing, whatever it holds.
		{RecordIdx: 3, Trial: 0, Result: &evalsiv1alpha1.EvaluationResult{RecordId: "r3", Evaluator: "exact-match", EvaluatorRef: em, Outcome: evalsiv1alpha1.Outcome_OUTCOME_ERROR, Scores: []*evalsiv1alpha1.Score{passed("exact-match", true)}}},
	}
	for i := range results {
		results[i].EvalIdx = i
	}
	if err := h.st.PutResults(ctx, id, results); err != nil {
		t.Fatal(err)
	}
}

func rowsByKey(resp *evalsiv1alpha1.SliceRunsResponse) map[string]*evalsiv1alpha1.SliceRow {
	out := map[string]*evalsiv1alpha1.SliceRow{}
	for _, r := range resp.GetRows() {
		out[r.GetRunId()+"|"+r.GetKey()] = r
	}
	return out
}

// The slice groups a metric by a record metadata field the way `evalsi analyze
// slice` does: metric names, the output-record-else-dataset-record lookup,
// the text of non-string values, (none), and the interval.
func TestSliceRunsMatchesClientSlice(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	seedSlice(t, h, "run-a", "first", "support", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), nil)

	resp, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"run-a"}, Metric: "exact-match", By: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	rows := rowsByKey(resp)
	// shipping: r0 trial 0 (its output). billing: r0 trial 1 (dataset record), r1, r2.
	// refunds: r3. (none): r4 (no field), r5 (null), and the dataset-scope result.
	want := map[string][2]float64{ // n, mean
		"run-a|shipping": {1, 1},
		"run-a|billing":  {3, 1.0 / 3},
		"run-a|refunds":  {1, 1},
		"run-a|(none)":   {3, 2.0 / 3},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v", resp.GetRows())
	}
	for k, w := range want {
		r := rows[k]
		if r == nil || float64(r.GetN()) != w[0] || !near(r.GetMean(), w[1]) {
			t.Errorf("%s = %v, want n=%v mean=%v", k, r, w[0], w[1])
		}
	}
	// billing: values 0,0,1 -> sample sd sqrt(1/3), interval mean +/- 1.96*sd/sqrt(3).
	b := rows["run-a|billing"]
	half := 1.96 * math.Sqrt(1.0/3) / math.Sqrt(3)
	if b.Low == nil || !near(b.GetLow(), 1.0/3-half) || !near(b.GetHigh(), 1.0/3+half) {
		t.Errorf("billing interval = %v %v, want %v", b.Low, b.High, half)
	}
	// One value has no interval.
	if r := rows["run-a|refunds"]; r.Low != nil || r.High != nil {
		t.Errorf("a single value has an interval: %v", r)
	}
	if resp.GetRuns() != 1 {
		t.Errorf("runs = %d", resp.GetRuns())
	}

	// Non-string values are grouped by their text; a nested path is followed.
	for by, want := range map[string]map[string]int64{
		"tier":        {"1": 2, "2": 1, "(none)": 5}, // r0 trial 0 is the output (no tier)
		"vip":         {"true": 1, "(none)": 7},
		"nested.team": {"core": 1, "(none)": 7},
	} {
		resp, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"run-a"}, Metric: "exact-match", By: by})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, r := range resp.GetRows() {
			got[r.GetKey()] = r.GetN()
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("by %s: %v, want %v", by, got, want)
		}
	}

	// A score named like its evaluator is the evaluator's metric; others are
	// "<evaluator>.<score>"; a metric nothing scored gives no rows.
	for metric, n := range map[string]int{"tone": 1, "tone.warmth": 1, "exact-match.other": 0, "nope": 0} {
		resp, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"run-a"}, Metric: metric, By: "topic"})
		if err != nil || len(resp.GetRows()) != n {
			t.Errorf("%s: %d rows, %v; want %d", metric, len(resp.GetRows()), err, n)
		}
	}
	resp, _ = slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"run-a"}, Metric: "tone", By: "topic"})
	if got := rowsByKey(resp); len(got) != 1 || got["run-a|billing"].GetN() != 2 || !near(got["run-a|billing"].GetMean(), 0.75) {
		t.Errorf("tone = %v", resp.GetRows())
	}
}

// Runs are selected by id, or the newest in a project with the labels, and
// rows come back ordered by key, then run creation time.
func TestSliceRunsSelectsRuns(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	day := 24 * time.Hour
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedSlice(t, h, "old", "old", "support", base, map[string]string{"app": "x"})
	seedSlice(t, h, "new", "new", "support", base.Add(day), map[string]string{"app": "x", "env": "ci"})
	seedSlice(t, h, "other", "other", "billing", base.Add(2*day), map[string]string{"app": "x"})

	order := func(req *evalsiv1alpha1.SliceRunsRequest) string {
		t.Helper()
		resp, err := slice(t, h, req)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range resp.GetRows() {
			if r.GetKey() == "refunds" || r.GetKey() == "shipping" {
				ids = append(ids, r.GetKey()+":"+r.GetRunId())
			}
		}
		return strings.Join(ids, " ")
	}
	metric := func(req *evalsiv1alpha1.SliceRunsRequest) *evalsiv1alpha1.SliceRunsRequest {
		req.Metric, req.By = "exact-match", "topic"
		return req
	}
	if got := order(metric(&evalsiv1alpha1.SliceRunsRequest{})); got != "refunds:old refunds:new refunds:other shipping:old shipping:new shipping:other" {
		t.Errorf("all runs: %s", got)
	}
	if got := order(metric(&evalsiv1alpha1.SliceRunsRequest{Project: "support"})); got != "refunds:old refunds:new shipping:old shipping:new" {
		t.Errorf("project: %s", got)
	}
	if got := order(metric(&evalsiv1alpha1.SliceRunsRequest{Labels: map[string]string{"env": "ci"}})); got != "refunds:new shipping:new" {
		t.Errorf("labels: %s", got)
	}
	// limit takes the newest.
	if got := order(metric(&evalsiv1alpha1.SliceRunsRequest{Limit: 1})); got != "refunds:other shipping:other" {
		t.Errorf("limit: %s", got)
	}
	if got := order(metric(&evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"new", "new", "old"}})); got != "refunds:old refunds:new shipping:old shipping:new" {
		t.Errorf("ids: %s", got)
	}
	if _, err := slice(t, h, metric(&evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"missing"}})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing run: %v", err)
	}
}

func TestSliceRunsRejectsBadRequests(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	seedSlice(t, h, "run-a", "a", "support", time.Now(), nil)
	for name, req := range map[string]*evalsiv1alpha1.SliceRunsRequest{
		"no metric":  {By: "topic"},
		"no by":      {Metric: "exact-match"},
		"empty step": {Metric: "exact-match", By: "a..b"},
		"level":      {Metric: "exact-match", By: "topic", ConfidenceLevel: 0.8},
		"too many":   {Metric: "exact-match", By: "topic", RunIds: make([]string, sliceMaxRuns+1)},
	} {
		if _, err := slice(t, h, req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 0.9 and 0.99 are accepted and widen or narrow the interval.
	get := func(level float64) float64 {
		resp, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{Metric: "exact-match", By: "topic", ConfidenceLevel: level})
		if err != nil {
			t.Fatal(err)
		}
		r := rowsByKey(resp)["run-a|billing"]
		return r.GetHigh() - r.GetLow()
	}
	if a, b, c := get(0.9), get(0.95), get(0.99); !(a < b && b < c) {
		t.Errorf("widths %v %v %v", a, b, c)
	}
}

// A field with more values than the cap is refused rather than answered with
// a row for each.
func TestSliceRunsCapsGroups(t *testing.T) {
	h := newRetentionHarness(t, func(*Options) {})
	ctx := context.Background()
	var records []*evalsiv1alpha1.Record
	var results []store.Result
	for i := range sliceMaxGroups + 1 {
		records = append(records, &evalsiv1alpha1.Record{Id: fmt.Sprint(i), Metadata: meta(t, map[string]any{"id": fmt.Sprint(i)})})
		results = append(results, scored(i, 0, "em", "", passed("em", true)))
		results[i].EvalIdx = i
	}
	if err := h.st.CreateRun(ctx, &evalsiv1alpha1.Run{Id: "wide", Project: "p", CreatedAt: timestamppb.Now()}, records); err != nil {
		t.Fatal(err)
	}
	if err := h.st.PutResults(ctx, "wide", results); err != nil {
		t.Fatal(err)
	}
	if _, err := slice(t, h, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"wide"}, Metric: "em", By: "id"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("err = %v", err)
	}
}

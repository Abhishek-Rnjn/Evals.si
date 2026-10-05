package evaluation

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func schema(t *testing.T, props map[string]any, required ...string) *structpb.Struct {
	t.Helper()
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, k := range required {
			r[i] = k
		}
		m["required"] = r
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func manifests(t *testing.T) []*evalsiv1alpha1.EvaluatorManifest {
	passed := evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED
	number := evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER
	return []*evalsiv1alpha1.EvaluatorManifest{
		{
			Name: "builtin/exact-match", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Requires:     &evalsiv1alpha1.Requirements{Output: true, Reference: true},
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "exact-match", Type: passed}},
			ParamsSchema: schema(t, map[string]any{"normalize": map[string]any{"default": true}}),
		},
		{
			Name: "builtin/llm-judge", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Requires:     &evalsiv1alpha1.Requirements{Output: true, Judge: true},
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "llm-judge", Type: number, Min: proto.Float64(0), Max: proto.Float64(1)}},
			ParamsSchema: schema(t, map[string]any{"rubric": map[string]any{"default": "correctness"}}),
		},
		{
			Name: "builtin/regex-match", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "regex-match", Type: passed}},
			ParamsSchema: schema(t, map[string]any{"pattern": map[string]any{}}, "pattern"),
		},
		{
			Name: "test/count", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_DATASET,
			Requires:     &evalsiv1alpha1.Requirements{Reference: true},
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "count", Type: number}},
			ParamsSchema: schema(t, map[string]any{}),
		},
	}
}

// fakeWorker grades like the real packs would, without Python.
type fakeWorker struct {
	calls atomic.Int64
	fail  error
}

func (f *fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	return nil, nil
}

func (f *fakeWorker) Evaluate(_ context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	f.calls.Add(1)
	if f.fail != nil {
		return nil, f.fail
	}
	resp := &pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId()}
	for _, r := range req.GetRecords() {
		res := &evalsiv1alpha1.EvaluationResult{
			RecordId: r.GetId(), Evaluator: catalog.ShortName(req.GetEvaluator()),
			EvaluatorRef: req.GetEvaluator() + "@1.0.0", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
		}
		switch req.GetEvaluator() {
		case "builtin/exact-match":
			if r.GetReference() == nil {
				res.Outcome, res.Reason = evalsiv1alpha1.Outcome_OUTCOME_SKIPPED, "record has no reference"
				break
			}
			res.Scores = []*evalsiv1alpha1.Score{{Name: "exact-match", Value: &evalsiv1alpha1.Score_Passed{
				Passed: r.GetOutput().GetText() == r.GetReference().GetText()}}}
		case "builtin/llm-judge":
			if req.GetJudge() == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("needs a judge"))
			}
			if r.GetId() == "boom" {
				res.Outcome, res.Reason = evalsiv1alpha1.Outcome_OUTCOME_ERROR, "JudgeError: down"
				break
			}
			res.Scores = []*evalsiv1alpha1.Score{{Name: "llm-judge", Value: &evalsiv1alpha1.Score_Number{Number: 0.75}}}
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

func (f *fakeWorker) Generate(context.Context, *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeWorker) RunTask(context.Context, *pluginv1alpha1.RunTaskRequest, func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	return nil, errors.New("no agent runs here")
}

func (f *fakeWorker) LoadDataset(context.Context, *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	return nil, errors.New("not used")
}

func (f *fakeWorker) Reduce(_ context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return &pluginv1alpha1.ReduceResponse{Scores: []*evalsiv1alpha1.Score{
		{Name: "count", Value: &evalsiv1alpha1.Score_Number{Number: float64(len(req.GetRecords()))}},
	}}, nil
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

func record(id, output, reference, topic string) *evalsiv1alpha1.Record {
	r := &evalsiv1alpha1.Record{Id: id, Output: text(output), Metadata: map[string]*structpb.Value{
		"topic": structpb.NewStringValue(topic),
	}}
	if reference != "" {
		r.Reference = text(reference)
	}
	return r
}

func newService(t *testing.T, w *fakeWorker) *Service {
	return New(w, catalog.New(manifests(t)), []string{"claude"}, "claude",
		config.Evaluate{BatchSize: 2, Parallelism: 3, MaxRecords: 100})
}

func sampleRecords() []*evalsiv1alpha1.Record {
	return []*evalsiv1alpha1.Record{
		record("a", "Paris", "Paris", "geo"),
		record("b", "Lyon", "Paris", "geo"),
		record("c", "4", "4", "math"),
		record("", "x", "", "math"),
		record("e", "Rome", "Rome", "art"),
	}
}

func TestEvaluateOrdersResultsAndSummarizes(t *testing.T) {
	w := &fakeWorker{}
	svc := newService(t, w)
	resp, err := svc.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Records: sampleRecords(),
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{
			{Ref: "exact-match"},
			{Ref: "builtin/llm-judge", Name: "judge"},
			{Ref: "test/count"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range resp.Msg.GetResults() {
		got = append(got, r.GetRecordId()+"/"+r.GetEvaluator())
	}
	want := "a/exact-match a/judge b/exact-match b/judge c/exact-match c/judge 3/exact-match 3/judge e/exact-match e/judge /count"
	if strings.Join(got, " ") != want {
		t.Fatalf("results order:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if calls := w.calls.Load(); calls != 6 { // 3 batches of 2 for each of 2 record-scope evaluators
		t.Errorf("worker calls = %d, want 6", calls)
	}
	byMetric := map[string]*evalsiv1alpha1.MetricSummary{}
	for _, s := range resp.Msg.GetSummaries() {
		byMetric[s.GetMetric()] = s
	}
	em := byMetric["exact-match"]
	if em.GetN() != 4 || em.GetSkipped() != 1 || em.GetMean() != 0.75 || em.GetCi().GetMethod() != "wilson" {
		t.Errorf("exact-match summary = %v", em)
	}
	if j := byMetric["judge"]; j.GetMean() != 0.75 || j.GetCi().GetHigh() > 1 {
		t.Errorf("judge summary = %v", j)
	}
	if c := byMetric["count"]; c.GetMean() != 4 || c.GetCi() != nil {
		t.Errorf("count summary = %v", c)
	}
	if !strings.Contains(resp.Msg.GetResults()[10].GetReason(), "1 records lacked") {
		t.Errorf("dataset result reason = %q", resp.Msg.GetResults()[10].GetReason())
	}
}

func TestEvaluateClustersAndErrors(t *testing.T) {
	svc := newService(t, &fakeWorker{})
	records := sampleRecords()
	records[1].Id = "boom"
	resp, err := svc.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Records:    records,
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "llm-judge"}},
		Summary:    &evalsiv1alpha1.SummaryOptions{ClusterBy: "metadata.topic"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	em, judge := resp.Msg.GetSummaries()[0], resp.Msg.GetSummaries()[1]
	if em.GetClusters() != 3 || em.GetCi().GetMethod() != "clustered-t" {
		t.Errorf("exact-match summary = %v", em)
	}
	if judge.GetErrors() != 1 || judge.GetN() != 4 {
		t.Errorf("judge summary = %v", judge)
	}
}

func TestEvaluateRejectsBadRequests(t *testing.T) {
	cases := map[string]*evalsiv1alpha1.EvaluateRequest{
		"at least one evaluator": {},
		"unknown evaluator":      {Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "nope"}}},
		"more than once":         {Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "builtin/exact-match"}}},
		"unknown param":          {Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match", Params: mustStruct(t, map[string]any{"bogus": 1})}}},
		"requires param":         {Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "regex-match"}}},
		"unknown judge":          {Judge: "gpt", Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "llm-judge"}}},
		"duplicate record id": {
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
			Records:    []*evalsiv1alpha1.Record{{Id: "x"}, {Id: "x"}},
		},
		"confidence_level": {
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
			Summary:    &evalsiv1alpha1.SummaryOptions{ConfidenceLevel: 2},
		},
	}
	svc := newService(t, &fakeWorker{})
	for want, req := range cases {
		_, err := svc.Evaluate(context.Background(), connect.NewRequest(req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestNoDefaultJudge(t *testing.T) {
	svc := New(&fakeWorker{}, catalog.New(manifests(t)), nil, "", config.Evaluate{BatchSize: 1, Parallelism: 1, MaxRecords: 10})
	_, err := svc.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "llm-judge"}},
	}))
	if err == nil || !strings.Contains(err.Error(), "needs a judge") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkerFailuresMapToStatusCodes(t *testing.T) {
	for in, want := range map[error]connect.Code{
		connect.NewError(connect.CodeFailedPrecondition, errors.New("bad key")): connect.CodeFailedPrecondition,
		connect.NewError(connect.CodeUnavailable, errors.New("down")):           connect.CodeUnavailable,
		errors.New("weird"): connect.CodeUnavailable,
	} {
		svc := newService(t, &fakeWorker{fail: in})
		_, err := svc.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Records:    sampleRecords(),
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
		}))
		if connect.CodeOf(err) != want {
			t.Errorf("worker error %v -> %v, want %v", in, err, want)
		}
	}
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEvaluateStreamOverHTTP2 runs the streaming RPC through a real Connect
// handler and client, the way gRPC clients reach the server.
func TestEvaluateStreamOverHTTP2(t *testing.T) {
	svc := newService(t, &fakeWorker{})
	mux := http.NewServeMux()
	mux.Handle(evalsiv1alpha1connect.NewEvaluationServiceHandler(svc))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := evalsiv1alpha1connect.NewEvaluationServiceClient(srv.Client(), srv.URL, connect.WithGRPC())
	stream := client.EvaluateStream(context.Background())
	send := func(m *evalsiv1alpha1.EvaluateStreamRequest) {
		if err := stream.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Config{
		Config: &evalsiv1alpha1.EvaluateStreamConfig{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "test/count"}}},
	}})
	for _, r := range sampleRecords() {
		send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Record{Record: r}})
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatal(err)
	}
	var results int
	var summaries []*evalsiv1alpha1.MetricSummary
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if msg.GetResult() != nil {
			results++
		}
		if s := msg.GetSummaries(); s != nil {
			summaries = s.GetSummaries()
		}
	}
	if results != 6 || len(summaries) != 2 || summaries[0].GetMean() != 0.75 || summaries[1].GetMean() != 4 {
		t.Fatalf("results=%d summaries=%v", results, summaries)
	}
}

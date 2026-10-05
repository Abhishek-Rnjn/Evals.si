package sinks

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func f64(v float64) *float64 { return &v }

func finishedRun() *evalsiv1alpha1.Run {
	return &evalsiv1alpha1.Run{
		Id: "run-123", Name: "nightly", Project: "support",
		Status:    evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		StartedAt: timestamppb.New(time.Unix(1000, 0)), FinishedAt: timestamppb.New(time.Unix(1060, 0)),
		Spec: &evalsiv1alpha1.RunSpec{
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "rouge"}},
			Target:     &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "qwen3"},
			Trials:     3,
		},
		Summaries: []*evalsiv1alpha1.MetricSummary{
			{Metric: "exact-match", N: 10, Mean: f64(0.7), Ci: &evalsiv1alpha1.ConfidenceInterval{Low: 0.4, High: 0.9, Level: 0.95}},
			{Metric: "rouge.rougeL", N: 10, Mean: f64(0.5)},
		},
		Gates: []*evalsiv1alpha1.GateResult{{
			Gate: &evalsiv1alpha1.Gate{Metric: "exact-match", Min: f64(0.8)}, Passed: false,
		}},
		DatasetSha256: "abc", Records: 10,
	}
}

func traceResults() *Trace {
	return &Trace{
		TraceID: "0af7651916cd43dd8448eb211c80319c", RootSpanID: "b7ad6b7169203331",
		Service: "support-bot", Policy: "prod", Time: time.Unix(2000, 0),
		Results: []*evalsiv1alpha1.EvaluationResult{
			{
				Evaluator: "faithfulness", EvaluatorRef: "builtin/faithfulness@1.0.0",
				Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
				Scores: []*evalsiv1alpha1.Score{{
					Name: "faithfulness", Value: &evalsiv1alpha1.Score_Number{Number: 0.75}, Explanation: "3 of 4",
					Metadata: map[string]*structpb.Value{"judge_model": structpb.NewStringValue("j")},
				}},
			},
			{
				Evaluator: "pii", EvaluatorRef: "builtin/pii-leak@1.0.0", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
				Scores: []*evalsiv1alpha1.Score{{Name: "pii-leak", Value: &evalsiv1alpha1.Score_Passed{Passed: true}}},
			},
			{Evaluator: "ctx", EvaluatorRef: "builtin/context-recall@1.0.0", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SKIPPED},
		},
	}
}

func TestOTelEvents(t *testing.T) {
	var got []*collogs.ExportLogsServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("X-Key") != "secret" {
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		req := &collogs.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			t.Error(err)
		}
		got = append(got, req)
	}))
	defer srv.Close()
	t.Setenv("OTEL_KEY", "secret")
	sink := newOTel(OTelConfig{Endpoint: srv.URL, HeadersEnv: map[string]string{"X-Key": "OTEL_KEY"}}, srv.Client())

	if err := sink.ExportTrace(context.Background(), traceResults()); err != nil {
		t.Fatal(err)
	}
	recs := got[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()
	if len(recs) != 2 { // the skipped result has no event
		t.Fatalf("%d events", len(recs))
	}
	r := recs[0]
	attrs := map[string]string{}
	for _, kv := range r.GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().String()
	}
	if r.GetEventName() != "gen_ai.evaluation.result" || hex.EncodeToString(r.GetTraceId()) != "0af7651916cd43dd8448eb211c80319c" ||
		hex.EncodeToString(r.GetSpanId()) != "b7ad6b7169203331" {
		t.Errorf("event %v", r)
	}
	if !strings.Contains(attrs["gen_ai.evaluation.name"], "faithfulness") || !strings.Contains(attrs["gen_ai.evaluation.score.value"], "0.75") {
		t.Errorf("attributes %v", attrs)
	}
	if !strings.Contains(recs[1].String(), "pass") {
		t.Errorf("passed score has no pass label: %v", recs[1])
	}

	if err := sink.ExportRun(context.Background(), finishedRun()); err != nil {
		t.Fatal(err)
	}
	runRecs := got[1].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()
	if len(runRecs) != 2 || runRecs[0].GetEventName() != "evalsi.run.metric" {
		t.Fatalf("run events %v", runRecs)
	}
}

func TestDispatcherRetriesTransientErrorsOnly(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls[r.Header.Get("X-Mode")]++
		switch {
		case r.Header.Get("X-Mode") == "flaky" && calls["flaky"] < 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Header.Get("X-Mode") == "bad":
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	for _, mode := range []string{"flaky", "bad"} {
		t.Setenv("MODE_"+mode, mode)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New([]Config{
		{OTel: &OTelConfig{Endpoint: srv.URL, HeadersEnv: map[string]string{"X-Mode": "MODE_flaky"}}},
		{OTel: &OTelConfig{Endpoint: srv.URL, HeadersEnv: map[string]string{"X-Mode": "MODE_bad"}}},
	}, srv.Client(), log)
	if err != nil {
		t.Fatal(err)
	}
	d.backoff = time.Millisecond
	d.Start(2)
	d.Trace(traceResults())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)
	d.Trace(traceResults()) // after Close: dropped, not a panic
	if calls["flaky"] != 3 || calls["bad"] != 1 {
		t.Errorf("calls %v", calls)
	}
	if d.Exported.Load() != 1 || d.Failed.Load() != 1 || d.Dropped.Load() != 2 {
		t.Errorf("exported %d failed %d dropped %d", d.Exported.Load(), d.Failed.Load(), d.Dropped.Load())
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []Config{
		{},
		{MLflow: &MLflowConfig{TrackingURI: "mlflow:5000"}},
		{OTel: &OTelConfig{Endpoint: "collector"}},
		{MLflow: &MLflowConfig{TrackingURI: "http://m"}, OTel: &OTelConfig{Endpoint: "http://c"}},
	} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// fakeMLflow implements the slice of the MLflow REST API the sink uses.
type fakeMLflow struct {
	mu          sync.Mutex
	experiments map[string]string
	runs        map[string]map[string]any // run id -> state
	assessments []map[string]any
}

func (f *fakeMLflow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/api/2.0/mlflow/experiments/get-by-name":
		id, ok := f.experiments[r.URL.Query().Get("experiment_name")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			reply(map[string]string{"error_code": "RESOURCE_DOES_NOT_EXIST"})
			return
		}
		reply(map[string]any{"experiment": map[string]string{"experiment_id": id}})
	case r.URL.Path == "/api/2.0/mlflow/experiments/create":
		id := "exp-" + body["name"].(string)
		f.experiments[body["name"].(string)] = id
		reply(map[string]string{"experiment_id": id})
	case r.URL.Path == "/api/2.0/mlflow/runs/search":
		var runs []any
		for id, st := range f.runs {
			if strings.Contains(body["filter"].(string), st["evalsi_id"].(string)) {
				runs = append(runs, map[string]any{"info": map[string]string{"run_id": id}})
			}
		}
		reply(map[string]any{"runs": runs})
	case r.URL.Path == "/api/2.0/mlflow/runs/create":
		id := "mlrun-" + string(rune('a'+len(f.runs)))
		tags := body["tags"].([]any)
		f.runs[id] = map[string]any{"evalsi_id": tags[0].(map[string]any)["value"], "exp": body["experiment_id"], "metrics": map[string]float64{}}
		reply(map[string]any{"run": map[string]any{"info": map[string]string{"run_id": id}}})
	case r.URL.Path == "/api/2.0/mlflow/runs/log-batch":
		st := f.runs[body["run_id"].(string)]
		for _, m := range body["metrics"].([]any) {
			m := m.(map[string]any)
			st["metrics"].(map[string]float64)[m["key"].(string)] = m["value"].(float64)
		}
		if p, ok := body["params"]; ok {
			st["params"], st["tags"] = p, body["tags"]
		}
		reply(map[string]any{})
	case r.URL.Path == "/api/2.0/mlflow/runs/update":
		f.runs[body["run_id"].(string)]["status"] = body["status"]
		reply(map[string]any{})
	case strings.HasPrefix(r.URL.Path, "/api/3.0/mlflow/traces/tr-") && strings.HasSuffix(r.URL.Path, "/assessments"):
		f.assessments = append(f.assessments, body["assessment"].(map[string]any))
		reply(body)
	default:
		w.WriteHeader(http.StatusNotFound)
		reply(map[string]string{"error_code": "ENDPOINT_NOT_FOUND"})
	}
}

func TestMLflowRunsAndFeedback(t *testing.T) {
	fake := &fakeMLflow{experiments: map[string]string{}, runs: map[string]map[string]any{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	sink := newMLflow(MLflowConfig{TrackingURI: srv.URL, TraceFeedback: true}, srv.Client())
	ctx := context.Background()
	for range 2 { // a resumed run re-exports into the same MLflow run
		if err := sink.ExportRun(ctx, finishedRun()); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.runs) != 1 {
		t.Fatalf("%d MLflow runs", len(fake.runs))
	}
	for _, st := range fake.runs {
		m := st["metrics"].(map[string]float64)
		if st["exp"] != "exp-support" || st["status"] != "FAILED" || m["exact-match"] != 0.7 || m["exact-match.ci_low"] != 0.4 || m["rouge.rougeL"] != 0.5 {
			t.Errorf("run state %v", st)
		}
		if !strings.Contains(toJSON(st["tags"]), `"evalsi.gate.exact-match","value":"false"`) {
			t.Errorf("tags %v", toJSON(st["tags"]))
		}
	}
	if err := sink.ExportTrace(ctx, traceResults()); err != nil {
		t.Fatal(err)
	}
	if len(fake.assessments) != 2 {
		t.Fatalf("assessments %v", fake.assessments)
	}
	a := fake.assessments[0]
	if a["trace_id"] != "tr-0af7651916cd43dd8448eb211c80319c" || a["assessment_name"] != "faithfulness" ||
		a["source"].(map[string]any)["source_type"] != "LLM_JUDGE" || a["rationale"] != "3 of 4" {
		t.Errorf("assessment %v", a)
	}
	if fake.assessments[1]["feedback"].(map[string]any)["value"] != true {
		t.Errorf("passed feedback %v", fake.assessments[1])
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestRealMLflow runs against a live tracking server: EVALSI_TEST_MLFLOW=http://127.0.0.1:5000.
func TestRealMLflow(t *testing.T) {
	uri := os.Getenv("EVALSI_TEST_MLFLOW")
	if uri == "" {
		t.Skip("set EVALSI_TEST_MLFLOW to a tracking server")
	}
	sink := newMLflow(MLflowConfig{TrackingURI: uri, Experiment: "evalsi-sink-test"}, http.DefaultClient)
	run := finishedRun()
	run.Id = "run-" + time.Now().Format("150405.000000")
	for range 2 {
		if err := sink.ExportRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
}

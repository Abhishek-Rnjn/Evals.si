package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// The fixtures are responses recorded from a live MLflow 3.17.0 server
// (testdata/README.md).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const traceID = "tr-f864e522377586ed0a722b8a01c9a611"

type fakeMLflow struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	// Assessments the server already holds, as a trace's info shows them.
	existing string
	status   int
	headers  map[string]string
}

func (f *fakeMLflow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, body)
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error_code":"RESOURCE_EXHAUSTED","message":"slow down"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/3.0/mlflow/traces/search":
		_, _ = w.Write(fixture(f.t, "search"))
	case r.Method == http.MethodGet && r.URL.Path == "/api/2.0/mlflow/experiments/get-by-name":
		if r.URL.Query().Get("experiment_name") != "studio-agents" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_code":"RESOURCE_DOES_NOT_EXIST","message":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"experiment":{"experiment_id":"7","name":"studio-agents"}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3.0/mlflow/traces/batchGet":
		_, _ = w.Write(fixture(f.t, "batchget"))
	case r.Method == http.MethodGet && r.URL.Path == "/api/3.0/mlflow/traces/"+traceID:
		resp := map[string]any{"trace": map[string]any{"trace_info": map[string]any{"trace_id": traceID}}}
		if f.existing != "" {
			var a any
			_ = json.Unmarshal([]byte(f.existing), &a)
			resp["trace"].(map[string]any)["trace_info"].(map[string]any)["assessments"] = a
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assessments"):
		_, _ = w.Write(fixture(f.t, "assess_create"))
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/assessments/"):
		_, _ = w.Write(fixture(f.t, "assess_patch"))
	default:
		http.NotFound(w, r)
	}
}

func newConnector(t *testing.T, f *fakeMLflow, mutate func(*evalsiv1alpha1.TraceSource)) source.Connector {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	src := &evalsiv1alpha1.TraceSource{Name: "studio", Project: "p", Connector: "mlflow", Endpoint: srv.URL, Locations: []string{"1"}}
	if mutate != nil {
		mutate(src)
	}
	c, err := Factory(src, "secret-token", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListSearchesByStartTimeOldestFirst(t *testing.T) {
	f := &fakeMLflow{}
	c := newConnector(t, f, nil)
	since := time.Date(2026, 10, 9, 17, 0, 0, 0, time.UTC)
	res, err := c.List(context.Background(), source.ListRequest{Since: since, Cursor: "page2", Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Infos) != 1 || res.Infos[0].ID != traceID || res.Infos[0].InProgress {
		t.Fatalf("infos %+v", res.Infos)
	}
	if want := time.Date(2026, 10, 9, 17, 40, 31, 612_000_000, time.UTC); !res.Infos[0].Started.Equal(want) {
		t.Fatalf("started %v, want %v", res.Infos[0].Started, want)
	}
	if res.Infos[0].Digest == "" {
		t.Fatal("no digest")
	}
	body := f.bodies[0]
	if !strings.Contains(f.requests[0], "auth=Bearer secret-token") {
		t.Fatalf("no bearer token: %v", f.requests)
	}
	if body["filter"] != "trace.timestamp_ms >= "+itoa(since.UnixMilli()) {
		t.Fatalf("filter %v", body["filter"])
	}
	if ob, _ := body["order_by"].([]any); len(ob) != 1 || ob[0] != "timestamp_ms ASC" {
		t.Fatalf("order_by %v", body["order_by"])
	}
	if body["max_results"] != float64(500) || body["page_token"] != "page2" {
		t.Fatalf("paging %v", body)
	}
	loc := body["locations"].([]any)[0].(map[string]any)
	if loc["type"] != "MLFLOW_EXPERIMENT" || loc["mlflow_experiment"].(map[string]any)["experiment_id"] != "1" {
		t.Fatalf("locations %v", body["locations"])
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// The recorded spans (base64 IDs, decoded attributes) become a trace the
// ingest conventions turn into a trajectory.
func TestFetchedTraceBecomesARecord(t *testing.T) {
	c := newConnector(t, &fakeMLflow{}, nil)
	traces, err := c.Fetch(context.Background(), []source.Info{{ID: traceID}})
	if err != nil || len(traces) != 1 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	tr := traces[0]
	if tr.TraceID != "f864e522377586ed0a722b8a01c9a611" || len(tr.Spans) != 3 {
		t.Fatalf("trace %s with %d spans", tr.TraceID, len(tr.Spans))
	}
	if got := tr.Spans[0].Resource[source.AttrSourceTraceID]; got != traceID {
		t.Fatalf("source trace id %v", got)
	}
	rec, info := ingest.ToRecord(tr)
	if rec.GetId() != tr.TraceID {
		t.Fatalf("record id %q", rec.GetId())
	}
	if q := rec.GetInput().GetJson().GetStructValue().GetFields()["q"].GetStringValue(); q != "What is 2+2?" || rec.GetOutput().GetText() != "4" {
		t.Fatalf("input %v output %v", rec.GetInput(), rec.GetOutput())
	}
	steps := rec.GetTrajectory().GetSteps()
	if len(steps) != 3 {
		t.Fatalf("trajectory has %d steps: %v", len(steps), steps)
	}
	// The model call's chat messages and the tool call came through too.
	llm, tool := steps[1], steps[2]
	if llm.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_LLM || llm.GetInput().GetMessages().GetMessages()[0].GetContent() != "What is 2+2?" {
		t.Fatalf("llm step %v", llm)
	}
	if tool.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_TOOL || tool.GetName() != "calc" {
		t.Fatalf("tool step %v", tool)
	}
	if info.Service != "agent" || info.Error {
		t.Fatalf("info %+v", info)
	}
}

func TestLabelsFromTags(t *testing.T) {
	var info traceInfo
	info.TraceID = traceID
	info.Tags = map[string]string{"evalsi.label.workflow": "support", "mlflow.traceName": "agent", "other": "x"}
	tr, err := toTrace(info, nil)
	if err != nil || len(tr.Spans) != 0 {
		t.Fatal(err)
	}
	var env struct {
		Traces []struct {
			Spans []json.RawMessage `json:"spans"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(fixture(t, "batchget"), &env); err != nil {
		t.Fatal(err)
	}
	tr, err = toTrace(info, env.Traces[0].Spans)
	if err != nil {
		t.Fatal(err)
	}
	if l := tr.Spans[0].Labels; l["workflow"] != "support" || len(l) != 1 {
		t.Fatalf("labels %v", l)
	}
	if tr.Spans[0].Resource["mlflow.tag.other"] != "x" {
		t.Fatalf("resource %v", tr.Spans[0].Resource)
	}
}

func score(metric string, v any) source.Score {
	return source.Score{TraceID: traceID, Policy: "studio-agents", Evaluator: "builtin/task-success@1", Metric: metric, Value: v, Rationale: "why"}
}

func TestWriteBackCreatesThenUpdatesInPlace(t *testing.T) {
	f := &fakeMLflow{}
	c := newConnector(t, f, nil)
	ctx := context.Background()
	written, err := c.WriteBack(ctx, []source.Score{score("task-success", 1.0)}, nil)
	if err != nil || len(written) != 1 || written[0].RemoteID != "a-4a57e1390bab4fd486f444b5e47385f2" {
		t.Fatalf("create: %+v %v", written, err)
	}
	// The trace has no assessments yet, so it was searched, then created on.
	if got := f.requests; len(got) != 2 || !strings.HasPrefix(got[0], "GET /api/3.0/mlflow/traces/"+traceID) || !strings.HasPrefix(got[1], "POST /api/3.0/mlflow/traces/"+traceID+"/assessments") {
		t.Fatalf("requests %v", got)
	}
	a := f.bodies[1]["assessment"].(map[string]any)
	src := a["source"].(map[string]any)
	if a["assessment_name"] != "task-success" || src["source_type"] != "CODE" || src["source_id"] != "evalsi/studio-agents" || a["feedback"].(map[string]any)["value"] != 1.0 {
		t.Fatalf("assessment %v", a)
	}

	// With the stored ID, a changed score is a PATCH, not another assessment.
	f.requests, f.bodies = nil, nil
	prior := map[store.WriteKey]store.SourceWrite{written[0].Key(): written[0]}
	written, err = c.WriteBack(ctx, []source.Score{score("task-success", 0.0)}, prior)
	if err != nil || len(written) != 1 {
		t.Fatal(written, err)
	}
	if len(f.requests) != 1 || !strings.HasPrefix(f.requests[0], "PATCH /api/3.0/mlflow/traces/"+traceID+"/assessments/a-4a57e1390bab4fd486f444b5e47385f2") {
		t.Fatalf("requests %v", f.requests)
	}
	if f.bodies[0]["update_mask"] != "feedback,rationale" {
		t.Fatalf("update_mask %v", f.bodies[0]["update_mask"])
	}

	// With the bookkeeping lost, the assessment already on the trace is found.
	f.requests, f.bodies = nil, nil
	f.existing = `[{"assessment_id":"a-old","assessment_name":"task-success","source":{"source_id":"evalsi/studio-agents"}},
	               {"assessment_id":"a-other","assessment_name":"task-success","source":{"source_id":"someone-else"}}]`
	written, err = c.WriteBack(ctx, []source.Score{score("task-success", 1.0)}, nil)
	if err != nil || len(written) != 1 || written[0].RemoteID != "a-old" {
		t.Fatalf("recovery: %+v %v", written, err)
	}
	if !strings.HasPrefix(f.requests[len(f.requests)-1], "PATCH ") {
		t.Fatalf("requests %v", f.requests)
	}

	// A judge model's score is an LLM_JUDGE assessment.
	f.requests, f.bodies, f.existing = nil, nil, ""
	s := score("quality", "good")
	s.Judged = true
	if _, err := c.WriteBack(ctx, []source.Score{s}, nil); err != nil {
		t.Fatal(err)
	}
	if k := f.bodies[1]["assessment"].(map[string]any)["source"].(map[string]any)["source_type"]; k != "LLM_JUDGE" {
		t.Fatalf("source type %v", k)
	}
}

func TestThrottlingBecomesRetryAfter(t *testing.T) {
	f := &fakeMLflow{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "7"}}
	c := newConnector(t, f, nil)
	_, err := c.List(context.Background(), source.ListRequest{Since: time.Now()})
	var ra *source.RetryAfterError
	if !errors.As(err, &ra) || ra.After != 7*time.Second {
		t.Fatalf("got %v", err)
	}
	f.status, f.headers = http.StatusInternalServerError, nil
	_, err = c.List(context.Background(), source.ListRequest{Since: time.Now()})
	if err == nil || errors.As(err, &ra) {
		t.Fatalf("a 500 is an error, not a request to wait: %v", err)
	}
}

func TestVariantsAndLocationsAreChecked(t *testing.T) {
	for _, v := range []string{"databricks", "sagemaker", "azureml", "other"} {
		_, err := Factory(&evalsiv1alpha1.TraceSource{Variant: v, Endpoint: "http://x", Locations: []string{"1"}}, "", nil)
		if err == nil || !strings.Contains(err.Error(), "not built") {
			t.Errorf("variant %q: %v", v, err)
		}
	}
	if _, err := Factory(&evalsiv1alpha1.TraceSource{Endpoint: "http://x"}, "", nil); err == nil {
		t.Error("no locations was accepted")
	}
	if _, err := Factory(&evalsiv1alpha1.TraceSource{Endpoint: "ftp://x", Locations: []string{"1"}}, "", nil); err == nil {
		t.Error("a non-http endpoint was accepted")
	}
}

// Run against a real MLflow when EVALSI_TEST_MLFLOW_URL names one (the
// fixtures were recorded from one). The experiment needs at least one trace.
func TestAgainstALiveServer(t *testing.T) {
	url := os.Getenv("EVALSI_TEST_MLFLOW_URL")
	if url == "" {
		t.Skip("set EVALSI_TEST_MLFLOW_URL (and EVALSI_TEST_MLFLOW_EXPERIMENT) to run against a live MLflow")
	}
	exp := os.Getenv("EVALSI_TEST_MLFLOW_EXPERIMENT")
	if exp == "" {
		exp = "1"
	}
	src := &evalsiv1alpha1.TraceSource{Name: "live", Project: "p", Connector: "mlflow", Endpoint: url, Locations: []string{exp}}
	c, err := Factory(src, "", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := c.List(ctx, source.ListRequest{Since: time.Unix(0, 0), Limit: 50})
	if err != nil || len(res.Infos) == 0 {
		t.Fatalf("list: %+v %v", res, err)
	}
	for i := 1; i < len(res.Infos); i++ {
		if res.Infos[i].Started.Before(res.Infos[i-1].Started) {
			t.Fatal("not oldest first")
		}
	}
	traces, err := c.Fetch(ctx, res.Infos[:1])
	if err != nil || len(traces) != 1 || len(traces[0].Spans) == 0 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	rec, _ := ingest.ToRecord(traces[0])
	if rec.GetInput().GetText() == "" && len(rec.GetTrajectory().GetSteps()) == 0 {
		t.Fatalf("an empty record from a real trace: %v", rec)
	}
	id := res.Infos[0].ID
	w, err := c.WriteBack(ctx, []source.Score{{TraceID: id, Policy: "live-test", Metric: "live-test", Value: 1.0}}, nil)
	if err != nil || len(w) != 1 {
		t.Fatalf("write back: %v %v", w, err)
	}
	// Again with nothing stored: finds the one just created and updates it.
	w2, err := c.WriteBack(ctx, []source.Score{{TraceID: id, Policy: "live-test", Metric: "live-test", Value: 0.0}}, nil)
	if err != nil || len(w2) != 1 || w2[0].RemoteID != w[0].RemoteID {
		t.Fatalf("second write: %v %v (first %v)", w2, err, w)
	}
}

// A location that is not an ID is an experiment name, resolved by MLflow.
func TestLocationsByName(t *testing.T) {
	f := &fakeMLflow{}
	c := newConnector(t, f, func(s *evalsiv1alpha1.TraceSource) { s.Locations = []string{"studio-agents", "3"} })
	if _, err := c.List(context.Background(), source.ListRequest{Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	locs := f.bodies[len(f.bodies)-1]["locations"].([]any)
	got := []any{}
	for _, l := range locs {
		got = append(got, l.(map[string]any)["mlflow_experiment"].(map[string]any)["experiment_id"])
	}
	if len(got) != 2 || got[0] != "7" || got[1] != "3" {
		t.Fatalf("experiment ids %v", got)
	}
	c = newConnector(t, &fakeMLflow{}, func(s *evalsiv1alpha1.TraceSource) { s.Locations = []string{"nope"} })
	if _, err := c.List(context.Background(), source.ListRequest{Since: time.Now()}); err == nil || !strings.Contains(err.Error(), `no experiment named "nope"`) {
		t.Fatalf("got %v", err)
	}
}

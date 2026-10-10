package phoenix

import (
	"context"
	"encoding/json"
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
)

// The fixtures are responses recorded from a live Phoenix 20.20.0 server
// (testdata/README.md).
const (
	okTrace     = "be3f90a78c62dbfa9f184f136ce8d1a5"
	failedTrace = "c2be2de4942854960790118094948902"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakePhoenix struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
}

func (f *fakePhoenix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/projects/studio/traces" && r.URL.Query().Get("cursor") == "":
		_, _ = w.Write(fixture(f.t, "traces_p1"))
	case r.URL.Path == "/v1/projects/studio/traces":
		_, _ = w.Write(fixture(f.t, "traces_p2"))
	case r.URL.Path == "/v1/projects/studio/spans":
		_, _ = w.Write(fixture(f.t, "spans_by_trace"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/trace_annotations":
		_, _ = w.Write(fixture(f.t, "annot_post"))
	default:
		http.NotFound(w, r)
	}
}

func newConnector(t *testing.T, f *fakePhoenix) source.Connector {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := Factory(&evalsiv1alpha1.TraceSource{Connector: "phoenix", Endpoint: srv.URL, Locations: []string{"studio"}}, "px-key", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListPagesTracesOldestFirst(t *testing.T) {
	f := &fakePhoenix{}
	c := newConnector(t, f)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	res, err := c.List(context.Background(), source.ListRequest{Since: since, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Infos) != 1 || res.Infos[0].ID != okTrace || res.Infos[0].InProgress || res.Next == "" {
		t.Fatalf("page 1: %+v", res)
	}
	q := f.requests[0]
	for _, want := range []string{"start_time=2026-10-01T00%3A00%3A00Z", "sort=start_time", "order=asc", "limit=1000", "include_spans=true", "auth=Bearer px-key"} {
		if !strings.Contains(q, want) {
			t.Errorf("request %s lacks %s", q, want)
		}
	}
	res2, err := c.List(context.Background(), source.ListRequest{Since: since, Cursor: res.Next, Limit: 1})
	if err != nil || len(res2.Infos) != 1 || res2.Infos[0].ID != failedTrace || res2.Next != "" {
		t.Fatalf("page 2: %+v %v", res2, err)
	}
	if !strings.Contains(f.requests[1], "cursor=") {
		t.Fatalf("cursor not sent: %s", f.requests[1])
	}
	if res.Infos[0].Digest == res2.Infos[0].Digest {
		t.Fatal("different traces, same digest")
	}
}

// Phoenix moves the OpenInference span kind out of the attributes and drops
// resource attributes; the connector puts back what the conventions read.
func TestFetchedTracesBecomeRecords(t *testing.T) {
	c := newConnector(t, &fakePhoenix{})
	traces, err := c.Fetch(context.Background(), []source.Info{{ID: okTrace}, {ID: failedTrace}, {ID: "gone"}})
	if err != nil || len(traces) != 2 {
		t.Fatalf("fetch: %d traces, %v", len(traces), err)
	}
	rec, info := ingest.ToRecord(traces[1])
	if rec.GetId() != failedTrace || rec.GetInput().GetText() != "What is 1/0?" || rec.GetOutput().GetText() != "I could not compute that." {
		t.Fatalf("record %v", rec)
	}
	if !info.Error || len(info.Tools) != 1 || info.Tools[0] != "calculator" || len(info.Models) != 1 || info.Models[0] != "mock" {
		t.Fatalf("info %+v", info)
	}
	if info.Labels["workflow"] != "support" || info.Service != "studio" || info.Resource[source.AttrSourceTraceID] != failedTrace {
		t.Fatalf("labels %v service %q resource %v", info.Labels, info.Service, info.Resource)
	}
	var llm *evalsiv1alpha1.Step
	for _, s := range rec.GetTrajectory().GetSteps() {
		if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			llm = s
		}
	}
	if llm == nil || llm.GetUsage().GetInputTokens() != 12 || llm.GetOutput().GetMessages().GetMessages()[0].GetContent() != "I could not compute that." {
		t.Fatalf("llm step %v", llm)
	}
}

func TestWriteBackAnnotatesWithAnIdentifier(t *testing.T) {
	f := &fakePhoenix{}
	c := newConnector(t, f)
	scores := []source.Score{
		{TraceID: failedTrace, Policy: "studio-agents", Evaluator: "builtin/tool-errors@1", Metric: "tool-errors", Value: 0.5, Rationale: "one of two calls failed"},
		{TraceID: failedTrace, Policy: "studio-agents", Metric: "loop-detection", Value: true},
		{TraceID: failedTrace, Policy: "studio-agents", Metric: "tone", Value: "curt", Judged: true},
	}
	written, err := c.WriteBack(context.Background(), scores, nil)
	if err != nil || len(written) != 3 || written[0].RemoteID != "VHJhY2VBbm5vdGF0aW9uOjE=" {
		t.Fatalf("written %+v %v", written, err)
	}
	if !strings.Contains(f.requests[0], "POST /v1/trace_annotations?sync=true") {
		t.Fatalf("request %s", f.requests[0])
	}
	ann := func(i int) map[string]any { return f.bodies[i]["data"].([]any)[0].(map[string]any) }
	if a := ann(0); a["identifier"] != "evalsi/studio-agents" || a["annotator_kind"] != "CODE" || a["trace_id"] != failedTrace ||
		a["result"].(map[string]any)["score"] != 0.5 || a["result"].(map[string]any)["explanation"] != "one of two calls failed" {
		t.Fatalf("annotation %v", a)
	}
	if r := ann(1)["result"].(map[string]any); r["label"] != "pass" || r["score"] != 1.0 {
		t.Fatalf("bool result %v", r)
	}
	if a := ann(2); a["annotator_kind"] != "LLM" || a["result"].(map[string]any)["label"] != "curt" {
		t.Fatalf("judged %v", a)
	}
}

func TestFactoryChecks(t *testing.T) {
	for name, src := range map[string]*evalsiv1alpha1.TraceSource{
		"no project":   {Endpoint: "https://px"},
		"two projects": {Endpoint: "https://px", Locations: []string{"a", "b"}},
		"slash":        {Endpoint: "https://px", Locations: []string{"a/b"}},
		"variant":      {Endpoint: "https://px", Locations: []string{"a"}, Variant: "cloud"},
		"bad endpoint": {Endpoint: "ftp://px", Locations: []string{"a"}},
	} {
		if _, err := Factory(src, "", nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Run against a real Phoenix when EVALSI_TEST_PHOENIX_URL names one; the
// project (EVALSI_TEST_PHOENIX_PROJECT, default "studio") needs a trace.
func TestAgainstALiveServer(t *testing.T) {
	base := os.Getenv("EVALSI_TEST_PHOENIX_URL")
	if base == "" {
		t.Skip("set EVALSI_TEST_PHOENIX_URL to run against a live Phoenix")
	}
	project := os.Getenv("EVALSI_TEST_PHOENIX_PROJECT")
	if project == "" {
		project = "studio"
	}
	c, err := Factory(&evalsiv1alpha1.TraceSource{Endpoint: base, Locations: []string{project}}, os.Getenv("EVALSI_TEST_PHOENIX_KEY"), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := c.List(ctx, source.ListRequest{Since: time.Unix(0, 0), Limit: 50})
	if err != nil || len(res.Infos) == 0 {
		t.Fatalf("list: %+v %v", res, err)
	}
	traces, err := c.Fetch(ctx, res.Infos[:1])
	if err != nil || len(traces) != 1 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	if rec, _ := ingest.ToRecord(traces[0]); len(rec.GetTrajectory().GetSteps()) == 0 {
		t.Fatalf("an empty record from a real trace: %v", rec)
	}
	id := res.Infos[0].ID
	for _, v := range []float64{1, 0} {
		w, err := c.WriteBack(ctx, []source.Score{{TraceID: id, Policy: "live-test", Metric: "live-test", Value: v}}, nil)
		if err != nil || len(w) != 1 {
			t.Fatalf("write back: %v %v", w, err)
		}
	}
}

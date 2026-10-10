package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
)

// These responses follow the pinned OpenAPI spec's ObservationV2 and
// CreateScoreResponse schemas. They were not recorded from a running Langfuse.

const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

var epoch = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return epoch.Add(d).Format(time.RFC3339Nano) }

// observations of one agent run: a root span, a generation, a failed tool call.
func runObservations() []map[string]any {
	return []map[string]any{
		{"id": "a1b2c3d4e5f60718", "traceId": traceID, "type": "AGENT", "isRootObservation": true, "name": "support-agent",
			"startTime": ts(0), "endTime": ts(3 * time.Second), "updatedAt": ts(4 * time.Second), "level": "DEFAULT",
			"input": "Where is my order?", "output": "It ships tomorrow.", "traceName": "support-agent", "environment": "production",
			"metadata": map[string]any{"evalsi.label.workflow": "support", "tenant": "acme"}},
		{"id": "b1b2c3d4e5f60718", "traceId": traceID, "parentObservationId": "a1b2c3d4e5f60718", "type": "GENERATION", "name": "chat",
			"startTime": ts(time.Second), "endTime": ts(2 * time.Second), "level": "DEFAULT", "model": "gpt-x",
			"input": `[{"role":"user","content":"Where is my order?"}]`, "output": `{"role":"assistant","content":"Let me check."}`,
			"usageDetails": map[string]any{"input": 21, "output": 4, "total": 25}},
		{"id": "c1b2c3d4e5f60718", "traceId": traceID, "parentObservationId": "a1b2c3d4e5f60718", "type": "TOOL", "name": "lookup_order",
			"startTime": ts(2 * time.Second), "endTime": ts(2500 * time.Millisecond), "level": "ERROR", "statusMessage": "timeout",
			"input": `{"order":"42"}`, "output": "error: timeout"},
	}
}

type fakeLangfuse struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	// Root observations by start time.
	roots []map[string]any
}

func (f *fakeLangfuse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/public/v2/observations" && q.Get("traceId") != "":
		// Two pages, to exercise the cursor.
		obs := runObservations()
		if q.Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": obs[:2], "meta": map[string]any{"cursor": "page2"}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": obs[2:], "meta": map[string]any{}})
		}
	case r.URL.Path == "/api/public/v2/observations":
		from, _ := time.Parse(time.RFC3339Nano, q.Get("fromStartTime"))
		to, _ := time.Parse(time.RFC3339Nano, q.Get("toStartTime"))
		var out []map[string]any
		for _, o := range f.roots {
			st, _ := time.Parse(time.RFC3339Nano, o["startTime"].(string))
			if !st.Before(from) && st.Before(to) {
				out = append(out, o)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "meta": map[string]any{}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": body["id"]})
	default:
		http.NotFound(w, r)
	}
}

func newConnector(t *testing.T, f *fakeLangfuse, now time.Time) *connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := Factory(&evalsiv1alpha1.TraceSource{Connector: "langfuse", Endpoint: srv.URL, Locations: []string{"production"}}, "pk-lf-1:sk-lf-2", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	lc := c.(*connector)
	lc.now = func() time.Time { return now }
	return lc
}

func root(id string, start time.Duration, ended bool) map[string]any {
	o := map[string]any{"id": "r-" + id, "traceId": id, "isRootObservation": true, "startTime": ts(start), "updatedAt": ts(start)}
	if ended {
		o["endTime"] = ts(start + time.Second)
	}
	return o
}

func TestListReadsWholeWindowsOldestFirst(t *testing.T) {
	f := &fakeLangfuse{roots: []map[string]any{
		// Out of order, as the spec promises no order; a quiet stretch; one still running.
		root("t2", 30*time.Minute, true), root("t1", 10*time.Minute, true),
		root("t3", 9*time.Hour, false),
	}}
	c := newConnector(t, f, epoch.Add(24*time.Hour))
	res, err := c.List(context.Background(), source.ListRequest{Since: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Infos) != 2 || res.Infos[0].ID != "t1" || res.Infos[1].ID != "t2" || res.Next == "" {
		t.Fatalf("first window: %+v", res)
	}
	r := f.requests[0].URL.Query()
	if r.Get("isRootObservation") != "true" || r.Get("environment") != "production" || r.Get("fromStartTime") != ts(0) || r.Get("toStartTime") != ts(time.Hour) {
		t.Fatalf("query %v", r)
	}
	if got := f.requests[0].Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("pk-lf-1:sk-lf-2")) {
		t.Fatalf("auth %q", got)
	}
	// The next call skips the empty hours with widening windows and finds t3.
	f.requests = nil
	res, err = c.List(context.Background(), source.ListRequest{Since: epoch, Cursor: res.Next})
	if err != nil || len(res.Infos) != 1 || res.Infos[0].ID != "t3" || !res.Infos[0].InProgress {
		t.Fatalf("second: %+v %v", res, err)
	}
	if n := len(f.requests); n > 5 {
		t.Fatalf("%d requests to cross eight empty hours; windows should widen", n)
	}
	// At the present, the last window ends the listing.
	for res.Next != "" {
		if res, err = c.List(context.Background(), source.ListRequest{Since: epoch, Cursor: res.Next}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.List(context.Background(), source.ListRequest{Cursor: "garbage"}); err == nil {
		t.Fatal("a bad cursor was accepted")
	}
}

func TestFetchedObservationsBecomeARecord(t *testing.T) {
	c := newConnector(t, &fakeLangfuse{}, epoch)
	traces, err := c.Fetch(context.Background(), []source.Info{{ID: traceID}})
	if err != nil || len(traces) != 1 || len(traces[0].Spans) != 3 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	rec, info := ingest.ToRecord(traces[0])
	if rec.GetId() != traceID || rec.GetInput().GetText() != "Where is my order?" || rec.GetOutput().GetText() != "It ships tomorrow." {
		t.Fatalf("record %v", rec)
	}
	if !info.Error || len(info.Tools) != 1 || info.Tools[0] != "lookup_order" || info.Service != "support-agent" {
		t.Fatalf("info %+v", info)
	}
	if info.Labels["workflow"] != "support" || info.Resource[source.AttrSourceTraceID] != traceID || info.Resource["langfuse.environment"] != "production" {
		t.Fatalf("labels %v resource %v", info.Labels, info.Resource)
	}
	var llm *evalsiv1alpha1.Step
	for _, s := range rec.GetTrajectory().GetSteps() {
		if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			llm = s
		}
	}
	if llm == nil || llm.GetUsage().GetInputTokens() != 21 || llm.GetUsage().GetOutputTokens() != 4 {
		t.Fatalf("llm step %v", llm)
	}
}

func TestIDs(t *testing.T) {
	if got := idBytes("4bf92f35-77b3-4da6-a3ce-929d0e0e4736", 16); len(got) != 16 || got[0] != 0x4b {
		t.Fatalf("uuid: %x", got)
	}
	a, b := idBytes("not-hex-at-all", 8), idBytes("not-hex-at-all", 8)
	if len(a) != 8 || string(a) != string(b) {
		t.Fatal("a hashed ID must be stable")
	}
}

func TestWriteBackScores(t *testing.T) {
	f := &fakeLangfuse{}
	c := newConnector(t, f, epoch)
	written, err := c.WriteBack(context.Background(), []source.Score{
		{TraceID: traceID, Policy: "support", Evaluator: "builtin/tool-errors@1", Metric: "tool-errors", Value: 0.5, Rationale: "one failed"},
		{TraceID: traceID, Policy: "support", Metric: "loop-detection", Value: true},
		{TraceID: traceID, Policy: "support", Metric: "tone", Value: "curt", Judged: true},
	}, nil)
	if err != nil || len(written) != 3 {
		t.Fatalf("written %v %v", written, err)
	}
	b := f.bodies
	if b[0]["traceId"] != traceID || b[0]["name"] != "tool-errors" || b[0]["value"] != 0.5 || b[0]["dataType"] != "NUMERIC" || b[0]["comment"] != "one failed" || b[0]["source"] != "API" {
		t.Fatalf("numeric %v", b[0])
	}
	if b[1]["value"] != 1.0 || b[1]["dataType"] != "BOOLEAN" || b[2]["value"] != "curt" || b[2]["dataType"] != "CATEGORICAL" {
		t.Fatalf("bool/categorical %v %v", b[1], b[2])
	}
	// The ID is the same for the same trace, policy and metric, so a changed
	// score is sent under the ID of the one it replaces.
	again, _ := c.WriteBack(context.Background(), []source.Score{{TraceID: traceID, Policy: "support", Metric: "tool-errors", Value: 1.0}}, nil)
	if again[0].RemoteID != written[0].RemoteID || !strings.HasPrefix(written[0].RemoteID, "evalsi-") {
		t.Fatalf("ids %q %q", again[0].RemoteID, written[0].RemoteID)
	}
}

func TestFactoryChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		src   *evalsiv1alpha1.TraceSource
		token string
	}{
		"two environments": {&evalsiv1alpha1.TraceSource{Endpoint: "https://lf", Locations: []string{"a", "b"}}, ""},
		"variant":          {&evalsiv1alpha1.TraceSource{Endpoint: "https://lf", Variant: "cloud"}, ""},
		"bad endpoint":     {&evalsiv1alpha1.TraceSource{Endpoint: "lf"}, ""},
		"bad credential":   {&evalsiv1alpha1.TraceSource{Endpoint: "https://lf"}, "only-a-secret"},
	} {
		if _, err := Factory(tc.src, tc.token, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

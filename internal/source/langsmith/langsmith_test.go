package langsmith

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// These responses follow the run and feedback shapes of the pinned langsmith
// Python client 0.4.37 (schemas.Run, FeedbackCreate). They were not recorded
// from a running LangSmith.

const (
	traceID   = "4bf92f35-77b3-4da6-a3ce-929d0e0e4736"
	projectID = "0d9e2f3a-1111-4222-8333-1234567890ab"
)

var epoch = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// ts is a time as LangSmith writes it: UTC, without a zone.
func ts(d time.Duration) string { return epoch.Add(d).Format("2006-01-02T15:04:05.000000") }

// traceRuns are one LangGraph agent run: the graph, a model call, a failed tool call.
func traceRuns() []map[string]any {
	return []map[string]any{
		{"id": traceID, "trace_id": traceID, "parent_run_id": nil, "name": "support-agent", "run_type": "chain",
			"start_time": ts(0), "end_time": ts(3 * time.Second), "status": "success", "session_id": projectID,
			"inputs": map[string]any{"input": "Where is my order?"}, "outputs": map[string]any{"output": "It ships tomorrow."},
			"extra":        map[string]any{"metadata": map[string]any{"evalsi.label.workflow": "support", "tenant": "acme", "thread_id": "th-1"}},
			"total_tokens": 25, "prompt_tokens": 21, "completion_tokens": 4, "tags": []string{"prod"}},
		{"id": "b1b2c3d4-0000-4000-8000-000000000002", "trace_id": traceID, "parent_run_id": traceID, "name": "ChatOpenAI", "run_type": "llm",
			"start_time": ts(time.Second), "end_time": ts(2 * time.Second), "status": "success",
			"inputs":        map[string]any{"messages": [][]map[string]any{{{"lc": 1, "type": "constructor", "kwargs": map[string]any{"type": "human", "content": "Where is my order?"}}}}},
			"outputs":       map[string]any{"generations": [][]map[string]any{{{"text": "Let me check.", "message": map[string]any{"type": "ai", "content": "Let me check."}}}}},
			"extra":         map[string]any{"metadata": map[string]any{"ls_model_name": "gpt-x"}, "invocation_params": map[string]any{"model": "ignored"}},
			"prompt_tokens": 21, "completion_tokens": 4, "total_tokens": 25},
		{"id": "c1b2c3d4-0000-4000-8000-000000000003", "trace_id": traceID, "parent_run_id": traceID, "name": "lookup_order", "run_type": "tool",
			"start_time": ts(2 * time.Second), "end_time": ts(2500 * time.Millisecond), "status": "error", "error": "timeout",
			"inputs": map[string]any{"input": `{"order":"42"}`}},
	}
}

type fakeLangSmith struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	// Root runs by start time.
	roots    []map[string]any
	feedback map[string]map[string]any
}

var windowRE = regexp.MustCompile(`gte\(start_time, "([^"]+)"\), lt\(start_time, "([^"]+)"\)`)

func (f *fakeLangSmith) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
	if f.feedback == nil {
		f.feedback = map[string]map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/sessions":
		if r.URL.Query().Get("name") == "support" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": projectID, "name": "support"}})
		} else {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	case r.Method == http.MethodPost && r.URL.Path == "/runs/query" && body["trace"] != nil:
		// Two pages, to exercise the cursor.
		runs := traceRuns()
		if body["cursor"] == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs[:2], "cursors": map[string]any{"next": "page2"}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs[2:], "cursors": map[string]any{"next": nil}})
		}
	case r.Method == http.MethodPost && r.URL.Path == "/runs/query":
		m := windowRE.FindStringSubmatch(body["filter"].(string))
		from, _ := time.Parse(time.RFC3339Nano, m[1])
		to, _ := time.Parse(time.RFC3339Nano, m[2])
		out := []map[string]any{}
		for _, run := range f.roots {
			st, _ := parseTime(run["start_time"].(string))
			if !st.Before(from) && st.Before(to) {
				out = append(out, run)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": out, "cursors": map[string]any{}})
	case r.Method == http.MethodPost && r.URL.Path == "/feedback":
		id := body["id"].(string)
		if f.feedback[id] != nil {
			http.Error(w, `{"detail":"already exists"}`, http.StatusConflict)
			return
		}
		f.feedback[id] = body
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/feedback/"):
		fb := f.feedback[strings.TrimPrefix(r.URL.Path, "/feedback/")]
		if fb == nil {
			http.NotFound(w, r)
			return
		}
		for k, v := range body {
			fb[k] = v
		}
		_ = json.NewEncoder(w).Encode(fb)
	default:
		http.NotFound(w, r)
	}
}

func newConnector(t *testing.T, f *fakeLangSmith, now time.Time, token string) *connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := Factory(&evalsiv1alpha1.TraceSource{Connector: "langsmith", Endpoint: srv.URL, Locations: []string{"support"}}, token, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	lc := c.(*connector)
	lc.now = func() time.Time { return now }
	return lc
}

func rootRun(id string, start time.Duration, ended bool) map[string]any {
	r := map[string]any{"id": id, "trace_id": id, "name": "agent", "start_time": ts(start), "status": "pending"}
	if ended {
		r["end_time"], r["status"] = ts(start+time.Second), "success"
	}
	return r
}

func TestListReadsWholeWindowsOldestFirst(t *testing.T) {
	f := &fakeLangSmith{roots: []map[string]any{
		// Out of order; a quiet stretch; one still running.
		rootRun("t2", 30*time.Minute, true), rootRun("t1", 10*time.Minute, true),
		rootRun("t3", 9*time.Hour, false),
	}}
	c := newConnector(t, f, epoch.Add(24*time.Hour), "test-api-key")
	res, err := c.List(context.Background(), source.ListRequest{Since: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Infos) != 2 || res.Infos[0].ID != "t1" || res.Infos[1].ID != "t2" || res.Next == "" {
		t.Fatalf("first window: %+v", res)
	}
	if !res.Infos[0].Started.Equal(epoch.Add(10*time.Minute)) || res.Infos[0].InProgress {
		t.Fatalf("a zone-less time must read as UTC: %+v", res.Infos[0])
	}
	// The project name was resolved once, then queried by ID.
	if f.requests[0].URL.Path != "/sessions" || f.requests[0].Header.Get("x-api-key") != "test-api-key" || f.requests[0].Header.Get("X-Tenant-Id") != "" {
		t.Fatalf("lookup %v %v", f.requests[0].URL, f.requests[0].Header)
	}
	q := f.bodies[1]
	if q["is_root"] != true || q["session"].([]any)[0] != projectID || !strings.Contains(q["filter"].(string), epoch.Add(time.Hour).Format(time.RFC3339Nano)) {
		t.Fatalf("query %v", q)
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
	for _, r := range f.requests {
		if r.URL.Path == "/sessions" {
			t.Fatal("the project was looked up again")
		}
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

func TestListDigestChangesWhenTheTraceEnds(t *testing.T) {
	running := rootRun("t1", 0, false)
	f := &fakeLangSmith{roots: []map[string]any{running}}
	c := newConnector(t, f, epoch.Add(time.Minute), "")
	before, err := c.List(context.Background(), source.ListRequest{Since: epoch})
	if err != nil || len(before.Infos) != 1 || !before.Infos[0].InProgress {
		t.Fatalf("%+v %v", before, err)
	}
	f.roots = []map[string]any{rootRun("t1", 0, true)}
	after, _ := c.List(context.Background(), source.ListRequest{Since: epoch})
	if after.Infos[0].InProgress || after.Infos[0].Digest == before.Infos[0].Digest {
		t.Fatalf("before %+v after %+v", before.Infos[0], after.Infos[0])
	}
}

func TestUnknownProjectIsAnError(t *testing.T) {
	srv := httptest.NewServer(&fakeLangSmith{})
	t.Cleanup(srv.Close)
	c, err := Factory(&evalsiv1alpha1.TraceSource{Endpoint: srv.URL, Locations: []string{"nope"}}, "k", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(context.Background(), source.ListRequest{Since: epoch}); err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Fatalf("got %v", err)
	}
}

func TestProjectIDsAreNotLookedUp(t *testing.T) {
	f := &fakeLangSmith{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, _ := Factory(&evalsiv1alpha1.TraceSource{Endpoint: srv.URL, Locations: []string{strings.ToUpper(projectID)}}, "ws-less", srv.Client())
	c.(*connector).now = func() time.Time { return epoch.Add(time.Minute) }
	if _, err := c.List(context.Background(), source.ListRequest{Since: epoch}); err != nil {
		t.Fatal(err)
	}
	if f.requests[0].URL.Path != "/runs/query" || f.bodies[0]["session"].([]any)[0] != projectID {
		t.Fatalf("%v %v", f.requests[0].URL, f.bodies[0])
	}
}

func TestFetchedRunsBecomeARecord(t *testing.T) {
	c := newConnector(t, &fakeLangSmith{}, epoch, "")
	traces, err := c.Fetch(context.Background(), []source.Info{{ID: traceID}})
	if err != nil || len(traces) != 1 || len(traces[0].Spans) != 3 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	if traces[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID %s", traces[0].TraceID)
	}
	rec, info := ingest.ToRecord(traces[0])
	if rec.GetInput().GetText() != "Where is my order?" || rec.GetOutput().GetText() != "It ships tomorrow." {
		t.Fatalf("record %v", rec)
	}
	if !info.Error || len(info.Tools) != 1 || info.Tools[0] != "lookup_order" || info.Service != "support-agent" {
		t.Fatalf("info %+v", info)
	}
	if info.Labels["workflow"] != "support" || info.Resource[source.AttrSourceTraceID] != traceID || info.Resource["langsmith.project_id"] != projectID {
		t.Fatalf("labels %v resource %v", info.Labels, info.Resource)
	}
	if rec.GetTrajectory().GetSessionId() != "th-1" || info.Attributes["langsmith.metadata.tenant"] != "acme" {
		t.Fatalf("session %q attrs %v", rec.GetTrajectory().GetSessionId(), info.Attributes)
	}
	var llm *evalsiv1alpha1.Step
	for _, s := range rec.GetTrajectory().GetSteps() {
		if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			llm = s
		}
	}
	if llm == nil || llm.GetUsage().GetInputTokens() != 21 || llm.GetUsage().GetOutputTokens() != 4 || len(info.Models) != 1 || info.Models[0] != "gpt-x" {
		t.Fatalf("llm step %v", llm)
	}
	// The chain's summed tokens are not counted a second time.
	if got := rec.GetUsage().GetInputTokens(); got != 21 {
		t.Fatalf("record input tokens %d, want 21", got)
	}
}

func TestIDs(t *testing.T) {
	if got := idBytes(traceID, 16); len(got) != 16 || got[0] != 0x4b {
		t.Fatalf("uuid: %x", got)
	}
	a, b := idBytes(traceID, 8), idBytes(traceID, 8)
	if len(a) != 8 || string(a) != string(b) {
		t.Fatal("a hashed ID must be stable")
	}
	id := feedbackID(source.Score{TraceID: traceID, Policy: "p", Metric: "m"})
	if !uuidRE.MatchString(id) || id[14] != '4' {
		t.Fatalf("feedback ID %q is not a v4-shaped UUID", id)
	}
}

func TestWriteBackFeedback(t *testing.T) {
	f := &fakeLangSmith{}
	c := newConnector(t, f, epoch, "")
	written, err := c.WriteBack(context.Background(), []source.Score{
		{TraceID: traceID, Policy: "support", Evaluator: "builtin/tool-errors@1", Metric: "tool-errors", Value: 0.5, Rationale: "one failed"},
		{TraceID: traceID, Policy: "support", Metric: "loop-detection", Value: true},
		{TraceID: traceID, Policy: "support", Metric: "tone", Value: "curt", Judged: true},
	}, nil)
	if err != nil || len(written) != 3 {
		t.Fatalf("written %v %v", written, err)
	}
	b := f.bodies
	src := b[0]["feedback_source"].(map[string]any)
	if b[0]["run_id"] != traceID || b[0]["trace_id"] != traceID || b[0]["key"] != "tool-errors" || b[0]["score"] != 0.5 ||
		b[0]["comment"] != "one failed" || src["type"] != "api" || src["metadata"].(map[string]any)["evalsi.evaluator"] != "builtin/tool-errors@1" {
		t.Fatalf("numeric %v", b[0])
	}
	if b[1]["score"] != 1.0 || b[2]["value"] != "curt" || b[2]["score"] != nil || b[2]["feedback_source"].(map[string]any)["type"] != "model" {
		t.Fatalf("bool/categorical %v %v", b[1], b[2])
	}
	// A changed score updates the feedback in place.
	prior := map[store.WriteKey]store.SourceWrite{written[0].Key(): written[0]}
	f.requests, f.bodies = nil, nil
	again, err := c.WriteBack(context.Background(), []source.Score{{TraceID: traceID, Policy: "support", Metric: "tool-errors", Value: 1.0}}, prior)
	if err != nil || len(again) != 1 || again[0].RemoteID != written[0].RemoteID || f.requests[0].Method != http.MethodPatch {
		t.Fatalf("update %v %v %v", again, err, f.requests)
	}
	if fb := f.feedback[written[0].RemoteID]; fb["score"] != 1.0 || fb["comment"] != "" {
		t.Fatalf("feedback %v", fb)
	}
	// Without the record of the earlier write, the create conflicts and the
	// feedback is updated rather than duplicated.
	f.requests = nil
	if _, err := c.WriteBack(context.Background(), []source.Score{{TraceID: traceID, Policy: "support", Metric: "tool-errors", Value: 0.0}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 2 || f.requests[1].Method != http.MethodPatch || f.feedback[written[0].RemoteID]["score"] != 0.0 || len(f.feedback) != 3 {
		t.Fatalf("requests %v feedback %v", f.requests, f.feedback)
	}
	// Feedback deleted in LangSmith is created again by its next changed score.
	delete(f.feedback, written[0].RemoteID)
	if _, err := c.WriteBack(context.Background(), []source.Score{{TraceID: traceID, Policy: "support", Metric: "tool-errors", Value: 0.25}}, prior); err != nil {
		t.Fatal(err)
	}
	if f.feedback[written[0].RemoteID]["score"] != 0.25 {
		t.Fatalf("recreated %v", f.feedback[written[0].RemoteID])
	}
}

func TestWorkspaceCredential(t *testing.T) {
	f := &fakeLangSmith{}
	c := newConnector(t, f, epoch.Add(time.Minute), projectID+":test-org-key")
	if _, err := c.List(context.Background(), source.ListRequest{Since: epoch}); err != nil {
		t.Fatal(err)
	}
	if h := f.requests[0].Header; h.Get("x-api-key") != "test-org-key" || h.Get("X-Tenant-Id") != projectID {
		t.Fatalf("headers %v", h)
	}
}

func TestRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c, _ := Factory(&evalsiv1alpha1.TraceSource{Endpoint: srv.URL, Locations: []string{projectID}}, "k", srv.Client())
	_, err := c.List(context.Background(), source.ListRequest{Since: epoch})
	var ra *source.RetryAfterError
	if !asRetry(err, &ra) || ra.After != 7*time.Second {
		t.Fatalf("got %v", err)
	}
}

func asRetry(err error, target **source.RetryAfterError) bool {
	for err != nil {
		if r, ok := err.(*source.RetryAfterError); ok {
			*target = r
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		`{"input":"hi"}`:                 "hi",
		`{"question":"q","context":"c"}`: `{"context":"c","question":"q"}`,
		`{"messages":[]}`:                `{"messages":[]}`,
		`{}`:                             "",
		`"plain"`:                        "plain",
	} {
		var v any
		_ = json.Unmarshal([]byte(in), &v)
		if got := text(v); got != want {
			t.Errorf("text(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestFactoryChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		src   *evalsiv1alpha1.TraceSource
		token string
	}{
		"no project":        {&evalsiv1alpha1.TraceSource{Endpoint: "https://ls"}, ""},
		"empty project":     {&evalsiv1alpha1.TraceSource{Endpoint: "https://ls", Locations: []string{" "}}, ""},
		"variant":           {&evalsiv1alpha1.TraceSource{Endpoint: "https://ls", Locations: []string{"p"}, Variant: "cloud"}, ""},
		"bad endpoint":      {&evalsiv1alpha1.TraceSource{Endpoint: "ls", Locations: []string{"p"}}, ""},
		"bad workspace":     {&evalsiv1alpha1.TraceSource{Endpoint: "https://ls", Locations: []string{"p"}}, "not-a-uuid:key"},
		"workspace, no key": {&evalsiv1alpha1.TraceSource{Endpoint: "https://ls", Locations: []string{"p"}}, projectID + ":"},
	} {
		if _, err := Factory(tc.src, tc.token, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Apply validates a source before its credential is read.
	src := &evalsiv1alpha1.TraceSource{Endpoint: "https://api.smith.langchain.com", Locations: []string{"support"}}
	if _, err := Factory(src, source.ValidationToken, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
}

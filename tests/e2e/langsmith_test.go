package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/source/langsmith"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// The same exercise runs in ordinary e2e CI against a derived fixture and,
// explicitly opted in, against LangSmith. The fixture is not live evidence.
func TestTraceSourceLangSmith(t *testing.T) {
	var mu sync.Mutex
	projects := map[string]map[string]any{}
	runs := map[string]map[string]any{}
	feedback := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("X-Tenant-Id") != "01234567-1234-4234-8234-0123456789ab" {
			http.Error(w, "missing credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case path == "/sessions" && r.Method == http.MethodPost:
			projects[body["id"].(string)] = body
			write(body)
		case path == "/sessions" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for _, p := range projects {
				if p["name"] == r.URL.Query().Get("name") {
					out = append(out, p)
				}
			}
			write(out)
		case strings.HasPrefix(path, "/sessions/") && r.Method == http.MethodDelete:
			delete(projects, strings.TrimPrefix(path, "/sessions/"))
			write(nil)
		case path == "/runs" && r.Method == http.MethodPost:
			runs[body["id"].(string)] = body
			write(nil)
		case strings.HasPrefix(path, "/runs/") && r.Method == http.MethodPatch:
			for k, v := range body {
				runs[strings.TrimPrefix(path, "/runs/")][k] = v
			}
			write(nil)
		case path == "/runs/query" && r.Method == http.MethodPost:
			out := []map[string]any{}
			for _, run := range runs {
				if run["session_id"] != body["session"].([]any)[0] {
					continue
				}
				if trace, ok := body["trace"]; ok {
					if run["trace_id"] == trace {
						out = append(out, run)
					}
					continue
				}
				if body["is_root"] != true || run["parent_run_id"] != nil {
					continue
				}
				// Apply both bounds independently of the connector's parser.
				bounds := strings.Split(body["filter"].(string), `"`)
				from, _ := time.Parse(time.RFC3339Nano, bounds[1])
				to, _ := time.Parse(time.RFC3339Nano, bounds[3])
				started, _ := time.Parse(time.RFC3339Nano, run["start_time"].(string))
				if !started.Before(from) && started.Before(to) {
					out = append(out, run)
				}
			}
			// Force pagination and return newest first, like the real API.
			sort.Slice(out, func(i, j int) bool { return out[i]["start_time"].(string) > out[j]["start_time"].(string) })
			next := ""
			if body["cursor"] == "second" {
				out = out[1:]
			} else if len(out) > 1 {
				out, next = out[:1], "second"
			}
			write(map[string]any{"runs": out, "cursors": map[string]any{"next": next}})
		case path == "/feedback" && r.Method == http.MethodPost:
			id := body["id"].(string)
			if feedback[id] != nil {
				http.Error(w, "duplicate", http.StatusConflict)
				return
			}
			feedback[id] = body
			write(body)
		case path == "/feedback" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for _, f := range feedback {
				if f["run_id"] == r.URL.Query().Get("run") {
					out = append(out, f)
				}
			}
			write(out)
		case strings.HasPrefix(path, "/feedback/"):
			id := strings.TrimPrefix(path, "/feedback/")
			if feedback[id] == nil {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodDelete {
				delete(feedback, id)
				write(nil)
				return
			}
			if r.Method == http.MethodPatch {
				for k, v := range body {
					feedback[id][k] = v
				}
			}
			write(feedback[id])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if len(projects) != 0 {
			t.Error("disposable project was not deleted")
		}
	})
	t.Setenv("EVALSI_TEST_LANGSMITH_CREDENTIAL", "01234567-1234-4234-8234-0123456789ab:fixture-key")
	exerciseLangSmith(t, srv.URL+"/api/v1")
}

func TestLiveTraceSourceLangSmith(t *testing.T) {
	endpoint := os.Getenv("EVALSI_TEST_LANGSMITH_URL")
	if endpoint == "" {
		t.Skip("set EVALSI_TEST_LANGSMITH_URL and EVALSI_TEST_LANGSMITH_CREDENTIAL for the opt-in live check")
	}
	if os.Getenv("EVALSI_TEST_LANGSMITH_CREDENTIAL") == "" || os.Getenv("EVALSI_E2E_WORKER") == "" {
		t.Fatal("the live check requires EVALSI_TEST_LANGSMITH_CREDENTIAL and EVALSI_E2E_WORKER")
	}
	exerciseLangSmith(t, strings.TrimRight(endpoint, "/"))
}

func exerciseLangSmith(t *testing.T, endpoint string) {
	t.Helper()
	if os.Getenv("EVALSI_E2E_WORKER") == "" {
		t.Skip("set EVALSI_E2E_WORKER to run end-to-end tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second}
	credential := os.Getenv("EVALSI_TEST_LANGSMITH_CREDENTIAL")
	// Validate the endpoint and credential before creating remote resources.
	if _, err := langsmith.Factory(&evalsiv1alpha1.TraceSource{Endpoint: endpoint, Locations: []string{"check"}}, credential, client); err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context, method, path string, body, out any) error {
		var rd io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, rd)
		if err != nil {
			return err
		}
		key := credential
		if workspace, apiKey, ok := strings.Cut(credential, ":"); ok {
			req.Header.Set("X-Tenant-Id", workspace)
			key = apiKey
		}
		req.Header.Set("x-api-key", key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			// Do not print remote response bodies or credentials in CI logs.
			return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		if out == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			return err
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	must := func(method, path string, body, out any) {
		t.Helper()
		if err := request(ctx, method, path, body, out); err != nil {
			t.Fatal(err)
		}
	}
	eventually := func(label string, check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", label, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	uuid := func() string {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	projectID := uuid()
	projectName := "evalsi-verification-" + projectID
	// Register cleanup before creation, including an ambiguous POST timeout.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := request(cleanupCtx, http.MethodDelete, "/sessions/"+projectID, nil, nil); err != nil {
			t.Errorf("remove disposable project %s: %v", projectName, err)
		}
	})
	must(http.MethodPost, "/sessions", map[string]any{"id": projectID, "name": projectName, "description": "Disposable Evals.si connector verification; no model calls."}, nil)
	seed := func(start time.Time, pending bool) string {
		root, tool := uuid(), uuid()
		for i, id := range []string{root, tool} {
			run := map[string]any{
				"id": id, "trace_id": root, "session_id": projectID, "name": "evalsi-verification-agent", "run_type": "chain",
				"start_time": start.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
				"inputs":     map[string]any{"input": "verification question"}, "outputs": map[string]any{"output": "verification answer"},
			}
			if !pending || i == 1 {
				run["end_time"] = start.Add(time.Duration(3-i) * time.Second).Format(time.RFC3339Nano)
			}
			if i == 1 {
				run["parent_run_id"], run["name"], run["run_type"], run["error"] = root, "verification-tool", "tool", "synthetic failure"
			}
			must(http.MethodPost, "/runs", run, nil)
		}
		return root
	}
	history := seed(time.Now().UTC().Add(-90*time.Minute), false)
	pending := seed(time.Now().UTC().Add(-time.Minute), true)
	u, _ := url.Parse(endpoint)
	e := start(t, func(c *config.Config) {
		c.Sources.AllowHosts = []string{u.Hostname()}
		c.Credentials.Grants = []credentials.Grant{{Env: "EVALSI_TEST_LANGSMITH_CREDENTIAL", Projects: []string{"default"}, Hosts: []string{u.Hostname()}, AllowHTTP: u.Scheme == "http"}}
	})
	monitor := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), e.base, connect.WithGRPC())
	_, err := monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: &evalsiv1alpha1.OnlineEvalPolicy{
		Name: "langsmith-check", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "tool-errors"}}}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	src := &evalsiv1alpha1.TraceSource{
		Name: "langsmith-check", Connector: "langsmith", Endpoint: endpoint, Locations: []string{projectName},
		Credentials: &evalsiv1alpha1.SourceCredentials{Env: "EVALSI_TEST_LANGSMITH_CREDENTIAL"},
		Policies:    []string{"langsmith-check"}, Backfill: &evalsiv1alpha1.Backfill{Since: durationpb.New(2 * time.Hour)},
		Poll: &evalsiv1alpha1.Poll{Interval: durationpb.New(time.Second)}, WriteBack: &evalsiv1alpha1.WriteBack{Enabled: true},
	}
	sources := evalsiv1alpha1connect.NewSourceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	if _, err := sources.ApplySource(ctx, connect.NewRequest(&evalsiv1alpha1.ApplySourceRequest{Source: src})); err != nil {
		t.Fatal(err)
	}
	feedbackFor := func(id string) []map[string]any {
		var out []map[string]any
		if err := request(ctx, http.MethodGet, "/feedback?run="+url.QueryEscape(id), nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	status := func() *evalsiv1alpha1.SourceStatus {
		got, err := sources.GetSource(ctx, connect.NewRequest(&evalsiv1alpha1.GetSourceRequest{Name: src.Name}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetSource().GetStatus()
	}
	eventually("historical trace scored; pending trace deferred", func() bool {
		st := status()
		return st.GetPulled() == 1 && st.GetScored() == 1 && st.GetDeferred() == 1 && len(feedbackFor(history)) == 1
	})
	if len(feedbackFor(pending)) != 0 {
		t.Fatal("unfinished trace was scored")
	}
	must(http.MethodPatch, "/runs/"+pending, map[string]any{"end_time": time.Now().UTC().Format(time.RFC3339Nano)}, nil)
	eventually("completed trace scored and source tailing", func() bool {
		st := status()
		return st.GetPhase() == evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING && st.GetPulled() == 2 && st.GetScored() == 2 && st.GetDeferred() == 0 && len(feedbackFor(pending)) == 1
	})
	traces := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	for _, id := range []string{history, pending} {
		f := feedbackFor(id)
		if len(f) != 1 || f[0]["key"] != "tool-errors" || f[0]["score"] != float64(1) {
			t.Fatalf("expected one tool-errors=1 feedback on %s", id)
		}
		got, err := traces.GetTrace(ctx, connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: strings.ReplaceAll(id, "-", "")}))
		if err != nil {
			t.Fatal(err)
		}
		rec := got.Msg.GetRecord()
		if rec.GetInput().GetText() != "verification question" || rec.GetOutput().GetText() != "verification answer" || len(rec.GetTrajectory().GetSteps()) != 2 {
			t.Fatal("pulled record lost input, output or trajectory")
		}
	}
	// Let repeated polls pass; the same traces and scores must remain deduplicated.
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if st := status(); st.GetPulled() != 2 || st.GetScored() != 2 || st.GetLastError() != "" {
		t.Fatalf("duplicate or failed polling: %v", st)
	}
	// Exercise replacement and crash recovery independently of manager dedup.
	conn, err := langsmith.Factory(src, credential, client)
	if err != nil {
		t.Fatal(err)
	}
	score := source.Score{TraceID: history, Policy: src.Name, Evaluator: "verification", Metric: "evalsi-verification", Value: 0.25, Rationale: "initial comment"}
	var prior map[store.WriteKey]store.SourceWrite
	remoteID := ""
	for i, value := range []float64{0.25, 0.75, 0.5, 0.9} {
		score.Value = value
		if i > 0 {
			score.Rationale = ""
		}
		if i == 2 {
			prior = nil // replay after a remote write whose local receipt was lost
		}
		if i == 3 {
			must(http.MethodDelete, "/feedback/"+remoteID, nil, nil)
		}
		written, err := conn.WriteBack(ctx, []source.Score{score}, prior)
		if err != nil || len(written) != 1 {
			t.Fatalf("feedback step %d: %v", i, err)
		}
		if remoteID != "" && written[0].RemoteID != remoteID {
			t.Fatal("replacement created another feedback ID")
		}
		remoteID = written[0].RemoteID
		prior = map[store.WriteKey]store.SourceWrite{score.Key(): written[0]}
		eventually("feedback read-back", func() bool {
			var f map[string]any
			if err := request(ctx, http.MethodGet, "/feedback/"+remoteID, nil, &f); err != nil {
				return false // the provider may index writes asynchronously
			}
			return f["id"] == remoteID && f["run_id"] == history && f["score"] == value && f["comment"] == score.Rationale
		})
	}
	eventually("one feedback per metric after replacement", func() bool {
		return len(feedbackFor(history)) == 2 && len(feedbackFor(pending)) == 1
	})
	t.Log("backfill, completion deferral, Python scoring, tail deduplication and feedback create/update/replay/recreate passed")
}

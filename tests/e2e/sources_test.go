package e2e

import (
	"bytes"
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

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// A TraceSource pulls a trace from MLflow, a policy scores it with the real
// Python worker, and the score is written back to MLflow as an assessment
// (decision 0016). MLflow here replays responses recorded from a real 3.17.0
// server.
func TestTraceSourceMLflow(t *testing.T) {
	const traceID = "tr-f864e522377586ed0a722b8a01c9a611"
	fixture := func(name string) []byte {
		b, err := os.ReadFile("../../internal/source/mlflow/testdata/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var mu sync.Mutex
	var searches int
	var assessments []map[string]any
	mlflow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/3.0/mlflow/traces/search":
			searches++
			// The recorded trace, started a minute ago.
			var doc map[string]any
			_ = json.Unmarshal(fixture("search"), &doc)
			doc["traces"].([]any)[0].(map[string]any)["request_time"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			_ = json.NewEncoder(w).Encode(doc)
		case r.Method == http.MethodGet && r.URL.Path == "/api/3.0/mlflow/traces/batchGet":
			_, _ = w.Write(fixture("batchget"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/3.0/mlflow/traces/"+traceID:
			_, _ = io.WriteString(w, `{"trace":{"trace_info":{"trace_id":"`+traceID+`"}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assessments"):
			var body struct{ Assessment map[string]any }
			_ = json.Unmarshal(raw, &body)
			assessments = append(assessments, body.Assessment)
			_, _ = w.Write(fixture("assess_create"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mlflow.Close)

	e := start(t, func(c *config.Config) { c.Sources.AllowHosts = []string{"127.0.0.1"} })
	ctx := context.Background()
	monitor := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), e.base, connect.WithGRPC())
	if _, err := monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: &evalsiv1alpha1.OnlineEvalPolicy{
		Name:   "studio-agents",
		Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "tool-errors"}}}},
	}})); err != nil {
		t.Fatal(err)
	}

	post := func(path string, body any) (int, string) {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(e.base+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	source := map[string]any{
		"name": "studio", "connector": "mlflow", "endpoint": mlflow.URL, "locations": []string{"1"},
		"policies": []string{"studio-agents"}, "writeBack": map[string]any{"enabled": true},
		"poll": map[string]any{"interval": "1s"}, "backfill": map[string]any{"since": "3600s"},
	}
	// A loopback endpoint is refused unless the operator allowed the host.
	if code, body := post("/v1alpha1/sources", map[string]any{"name": "bad", "connector": "mlflow", "endpoint": "http://169.254.169.254", "locations": []string{"1"}}); code != http.StatusBadRequest {
		t.Fatalf("a metadata address was accepted: %d %s", code, body)
	}
	if code, body := post("/v1alpha1/sources", source); code != http.StatusOK {
		t.Fatalf("apply: %d %s", code, body)
	}

	// The trace is pulled, scored by the Python worker, and the score lands in MLflow.
	deadline := time.Now().Add(90 * time.Second)
	for {
		mu.Lock()
		n := len(assessments)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no assessment reached MLflow after 90s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	a := assessments[0]
	mu.Unlock()
	src, _ := a["source"].(map[string]any)
	if a["assessment_name"] != "tool-errors" || a["trace_id"] != traceID || src["source_id"] != "evalsi/studio-agents" {
		t.Fatalf("assessment %v", a)
	}

	// Status is reported, the trace was handled once however often it was listed,
	// and it is an ordinary trace of the project.
	time.Sleep(3 * time.Second)
	resp, err := http.Get(e.base + "/v1alpha1/sources/studio")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Source struct {
			Status struct {
				Phase  string
				Pulled string
				Scored string
			}
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Source.Status.Pulled != "1" || got.Source.Status.Scored != "1" || got.Source.Status.Phase != "SOURCE_PHASE_TAILING" {
		t.Fatalf("status %+v", got.Source.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if searches < 2 || len(assessments) != 1 {
		t.Fatalf("%d searches and %d assessments: the same trace must be listed again but scored once", searches, len(assessments))
	}
	traces, err := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC()).
		ListTraces(ctx, connect.NewRequest(&evalsiv1alpha1.ListTracesRequest{}))
	if err != nil || len(traces.Msg.GetTraces()) != 1 || traces.Msg.GetTraces()[0].GetLabels()["source"] != "studio" {
		t.Fatalf("traces %v %v", traces, err)
	}
	metrics, _ := http.Get(e.base + "/metrics")
	body, _ := io.ReadAll(metrics.Body)
	metrics.Body.Close()
	if !strings.Contains(string(body), `evalsi_source_traces_pulled_total{project="default",source="studio"} 1`) {
		t.Fatalf("no source metrics in %s", body)
	}
}

// Package e2e runs evalsid against a real Python evaluator worker.
//
// It is skipped unless EVALSI_E2E_WORKER holds the command that runs the
// evalsi CLI, for example:
//
//	EVALSI_E2E_WORKER="$PWD/python/.venv/bin/python -m evalsi" go test ./tests/e2e/
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox/sandboxcli"
	"github.com/abhishek-rnjn/evals.si/internal/server"
)

// The server runs in this test binary, so the worker's EVALSID points here:
// answer `sandbox run` and `sandbox-exec` the way evalsid does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sandbox-exec":
			os.Exit(sandbox.Exec(os.Stderr))
		case "sandbox":
			os.Exit(sandboxcli.Main(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		}
	}
	os.Exit(m.Run())
}

// fakeModel speaks the OpenAI chat-completions protocol. As a judge (the
// prompt has a <rubric>) it always gives 4/5; as a target it answers capital
// questions, getting Japan wrong.
func fakeModel(t *testing.T) *httptest.Server {
	answers := map[string]string{"France": "Paris", "Italy": "Rome", "Japan": "Kyoto"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Messages[len(req.Messages)-1].Content
		content := `{"reasoning": "fine", "score": 4}`
		if !strings.Contains(prompt, "<rubric>") {
			content = "I don't know"
			for country, capital := range answers {
				if strings.Contains(prompt, country) {
					content = capital
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake-model",
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type env struct {
	base      string
	model     string
	datasets  string
	workerCmd []string
}

func startServer(t *testing.T) string {
	return start(t).base
}

func start(t *testing.T) env {
	t.Helper()
	worker := os.Getenv("EVALSI_E2E_WORKER")
	if worker == "" {
		t.Skip("set EVALSI_E2E_WORKER to run end-to-end tests")
	}
	model := fakeModel(t).URL + "/v1"
	datasets := t.TempDir()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.DatasetsDir = datasets
	cfg.Worker.Command = strings.Fields(worker)
	cfg.Worker.NoCache = true
	cfg.Judges = map[string]config.Judge{
		"local": {Provider: "openai-compatible", Model: "fake-judge", BaseURL: model},
	}
	cfg.DefaultJudge = "local"
	cfg.Evaluate.BatchSize = 2
	cfg.OTLP.Grace = "100ms"

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), os.Stderr, ready)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server.Run: %v", err)
		}
	})
	select {
	case addr := <-ready:
		return env{base: "http://" + addr, model: model, datasets: datasets, workerCmd: strings.Fields(worker)}
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	case <-time.After(90 * time.Second):
		t.Fatal("server did not start")
	}
	return env{}
}

func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: p, DialContext: (&net.Dialer{}).DialContext}}
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

// The quickstart's capital-city questions, as in examples/quickstart/qa.jsonl.
func records() []*evalsiv1alpha1.Record {
	pairs := [][3]string{
		{"capital-fr", "Paris", "Paris"}, {"capital-jp", "Kyoto", "Tokyo"}, {"capital-au", "canberra", "Canberra"},
		{"apples", "3 x 12 = 36, minus 4 leaves 32 apples.", "32"}, {"train", "The average speed is 80 km/h.", "80"},
		{"budget", "That comes to $14,000 per year.", "14400"},
	}
	var out []*evalsiv1alpha1.Record
	for _, p := range pairs {
		out = append(out, &evalsiv1alpha1.Record{Id: p[0], Input: text("q"), Output: text(p[1]), Reference: text(p[2])})
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	base := startServer(t)
	ctx := context.Background()

	t.Run("catalog", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewCatalogServiceClient(h2cClient(), base, connect.WithGRPC())
		resp, err := client.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, m := range resp.Msg.GetEvaluators() {
			names[m.GetName()] = true
		}
		if !names["builtin/exact-match"] || !names["builtin/llm-judge"] || resp.Msg.GetDefaultJudge() != "local" {
			t.Fatalf("unexpected catalog: %v", resp.Msg)
		}
	})

	t.Run("evaluate over gRPC", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		params, _ := structpb.NewStruct(map[string]any{"unit": "chars"})
		resp, err := client.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Records: records(),
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{
				{Ref: "exact-match"}, {Ref: "numeric-match"}, {Ref: "llm-judge"},
				{Ref: "length", Name: "chars", Params: params},
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Msg.GetResults()); got != 24 {
			t.Fatalf("got %d results, want 24", got)
		}
		sums := map[string]*evalsiv1alpha1.MetricSummary{}
		for _, s := range resp.Msg.GetSummaries() {
			sums[s.GetMetric()] = s
		}
		// Same numbers the embedded Python runner reports for this data.
		em := sums["exact-match"]
		if math.Abs(em.GetMean()-1.0/3) > 1e-12 || math.Abs(em.GetCi().GetLow()-0.0968) > 1e-3 || math.Abs(em.GetCi().GetHigh()-0.7000) > 1e-3 {
			t.Errorf("exact-match = %v", em)
		}
		if nm := sums["numeric-match"]; nm.GetN() != 3 || nm.GetSkipped() != 3 {
			t.Errorf("numeric-match = %v", nm)
		}
		if j := sums["llm-judge"]; j.GetMean() != 0.75 || j.GetN() != 6 {
			t.Errorf("llm-judge = %v", j)
		}
		if c := sums["chars"]; c.GetN() != 6 {
			t.Errorf("chars = %v", c)
		}
	})

	t.Run("evaluate over plain HTTP JSON", func(t *testing.T) {
		body := `{"records": [{"id": "1", "output": {"text": "Paris"}, "reference": {"text": "paris"}}],
		          "evaluators": [{"ref": "exact-match"}]}`
		resp, err := http.Post(base+"/evalsi.v1alpha1.EvaluationService/Evaluate", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Results []struct {
				Outcome string
				Scores  []struct{ Passed bool }
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || out.Results[0].Outcome != "OUTCOME_SCORED" || !out.Results[0].Scores[0].Passed {
			t.Fatalf("status %d, body %+v", resp.StatusCode, out)
		}
	})

	t.Run("bad request is InvalidArgument", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		_, err := client.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "regex-match"}},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stream", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		stream := client.EvaluateStream(ctx)
		_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Config{
			Config: &evalsiv1alpha1.EvaluateStreamConfig{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "fuzzy-match"}}},
		}})
		for _, r := range records() {
			_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Record{Record: r}})
		}
		_ = stream.CloseRequest()
		results, summaries := 0, 0
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
			if msg.GetSummaries() != nil {
				summaries++
			}
		}
		if results != 6 || summaries != 1 {
			t.Fatalf("results=%d summaries=%d", results, summaries)
		}
	})

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

func TestRuns(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	client := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	data := `{"id": "fr", "question": "Capital of France?", "answer": "Paris"}
{"id": "it", "question": "Capital of Italy?", "answer": "Rome"}
{"id": "jp", "question": "Capital of Japan?", "answer": "Tokyo"}
`
	if err := os.WriteFile(filepath.Join(e.datasets, "capitals.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "fake-model", BaseUrl: e.model},
		Dataset: &evalsiv1alpha1.DatasetSource{
			Source:  &evalsiv1alpha1.DatasetSource_Path{Path: "capitals.jsonl"},
			Mapping: map[string]string{"input": "question", "reference": "answer"},
		},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "llm-judge"}},
		Trials:     2,
		Gates:      []*evalsiv1alpha1.Gate{{Metric: "exact-match", Min: proto.Float64(0.6)}},
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: "capitals", Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchRun(ctx, connect.NewRequest(&evalsiv1alpha1.WatchRunRequest{Id: created.Msg.GetRun().GetId(), IncludeResults: true}))
	if err != nil {
		t.Fatal(err)
	}
	var final *evalsiv1alpha1.Run
	results := 0
	for stream.Receive() {
		if r := stream.Msg().GetRun(); r != nil {
			final = r
		}
		if stream.Msg().GetResult() != nil {
			results++
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("status = %v (%s)", final.GetStatus(), final.GetError())
	}
	if results != 12 {
		t.Errorf("streamed %d results, want 12 (3 records x 2 trials x 2 evaluators)", results)
	}
	sums := map[string]*evalsiv1alpha1.MetricSummary{}
	for _, s := range final.GetSummaries() {
		sums[s.GetMetric()] = s
	}
	if em := sums["exact-match"]; math.Abs(em.GetMean()-2.0/3) > 1e-9 || em.GetClusters() != 3 {
		t.Errorf("exact-match = %v", em)
	}
	if p := sums["exact-match.pass^2"]; math.Abs(p.GetMean()-2.0/3) > 1e-9 {
		t.Errorf("pass^2 = %v", p)
	}
	if j := sums["llm-judge"]; j.GetMean() != 0.75 {
		t.Errorf("llm-judge = %v", j)
	}
	if final.GetTargetUsage().GetInputTokens() != 60 || !final.GetGates()[0].GetPassed() {
		t.Errorf("usage %v gates %v", final.GetTargetUsage(), final.GetGates())
	}

	t.Run("python CLI against the server", func(t *testing.T) {
		specFile := filepath.Join(t.TempDir(), "run.yaml")
		yaml := `apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: from-cli}
spec:
  dataset:
    inline:
      records:
        - {id: a, output: {text: Paris}, reference: {text: Paris}}
        - {id: b, output: {text: Lyon}, reference: {text: Paris}}
  evaluators: [{ref: exact-match}]
  gates: [{metric: exact-match, min: 0.9}]
`
		if err := os.WriteFile(specFile, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		args := append(append([]string{}, e.workerCmd[1:]...), "run", "-f", specFile, "--server", e.base, "--quiet")
		out, err := exec.Command(e.workerCmd[0], args...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("want exit code 3 (gate failed), got %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "run-") || !strings.Contains(string(out), "[FAIL] exact-match") {
			t.Errorf("output:\n%s", out)
		}
	})
}

func TestWatch(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	monitor := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), e.base, connect.WithGRPC())
	params, _ := structpb.NewStruct(map[string]any{"substring": "ships"})
	policy := &evalsiv1alpha1.OnlineEvalPolicy{
		Name:     "support",
		Selector: `service == "support-agent"`,
		Stages: []*evalsiv1alpha1.CascadeStage{
			{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "contains", Params: params}}},
			{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "llm-judge", Params: mustStruct(map[string]any{"rubric": "helpfulness"})}}, When: `scores["contains"] == 0.0`},
		},
	}
	if _, err := monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); err != nil {
		t.Fatal(err)
	}
	// Two agent turns over OTLP/HTTP JSON, as an OTel SDK would send them.
	for i, answer := range []string{"It ships tomorrow.", "I am not sure."} {
		msgs := func(role, text string) string {
			raw, _ := json.Marshal([]any{map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": text}}}})
			return string(raw)
		}
		attr := func(k, v string) map[string]any {
			return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
		}
		payload, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attr("service.name", "support-agent")}},
			"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
				"traceId": fmt.Sprintf("%032x", i+1), "spanId": fmt.Sprintf("%016x", i+1), "name": "invoke_agent",
				"startTimeUnixNano": "1000", "endTimeUnixNano": "2000",
				"attributes": []any{
					attr("gen_ai.operation.name", "invoke_agent"),
					attr("gen_ai.input.messages", msgs("user", "Where is my order?")),
					attr("gen_ai.output.messages", msgs("assistant", answer)),
				},
			}}}},
		}}})
		body := string(payload)
		resp, err := http.Post(e.base+"/v1/traces", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("OTLP status %d", resp.StatusCode)
		}
	}
	var stats *evalsiv1alpha1.PolicyStats
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := monitor.GetPolicyStats(ctx, connect.NewRequest(&evalsiv1alpha1.GetPolicyStatsRequest{Name: "support"}))
		if err != nil {
			t.Fatal(err)
		}
		if stats = resp.Msg.GetStats(); stats.GetTracesEvaluated() == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if stats.GetTracesEvaluated() != 2 {
		t.Fatalf("stats = %v", stats)
	}
	means := map[string]float64{}
	for _, m := range stats.GetMetrics() {
		means[m.GetMetric()] = m.GetMean()
	}
	if means["contains"] != 0.5 || means["llm-judge"] != 0.75 {
		t.Errorf("window means = %v (the judge should only see the unhelpful answer)", means)
	}
	traces := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	got, err := traces.GetTrace(ctx, connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: fmt.Sprintf("%032x", 2)}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Msg.GetPolicies()[0].GetResults()); n != 2 {
		t.Errorf("trace 2 has %d results, want contains + llm-judge", n)
	}
	// The documented example policy applies cleanly through the Python CLI.
	args := append(append([]string{}, e.workerCmd[1:]...), "policy", "apply", "-f", "../../examples/watch/support-policy.yaml", "--server", e.base)
	if out, err := exec.Command(e.workerCmd[0], args...).CombinedOutput(); err != nil {
		t.Fatalf("policy apply: %v\n%s", err, out)
	}
	listed, err := monitor.ListPolicies(ctx, connect.NewRequest(&evalsiv1alpha1.ListPoliciesRequest{Project: "support"}))
	if err != nil || len(listed.Msg.GetPolicies()) != 1 || listed.Msg.GetPolicies()[0].GetName() != "support-agent" {
		t.Fatalf("ListPolicies = %v, %v", listed, err)
	}

	resp, err := http.Get(e.base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(metrics), `evalsi_policy_traces_total{policy="support",stage="evaluated"} 2`) {
		t.Errorf("metrics:\n%s", metrics)
	}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

// TestCodeSandbox runs generated code against tests through the worker and
// the real sandbox: passing, failing and escaping programs.
func TestCodeSandbox(t *testing.T) {
	base := startServer(t)
	probe, err := sandbox.New(sandbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	usable := false
	for _, st := range probe.Probe(context.Background()) {
		usable = usable || st.Available
	}
	if !usable {
		t.Skip("no sandbox rung works on this host")
	}
	tests := "def check(f):\n    assert f(2, 3) == 5\n"
	rec := func(id, code string) *evalsiv1alpha1.Record {
		return &evalsiv1alpha1.Record{
			Id: id, Output: text(code), Reference: text(tests),
			Metadata: map[string]*structpb.Value{"entry_point": structpb.NewStringValue("add")},
		}
	}
	client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
	resp, err := client.Evaluate(context.Background(), connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Records: []*evalsiv1alpha1.Record{
			rec("good", "```python\ndef add(a, b):\n    return a + b\n```"),
			rec("wrong", "def add(a, b):\n    return a - b\n"),
			rec("escape", "open('/usr/evalsi-escape', 'w')\ndef add(a, b):\n    return a + b\n"),
		},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "unit-tests"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"good": true, "wrong": false, "escape": false}
	for _, r := range resp.Msg.GetResults() {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			t.Fatalf("%s: %v %s", r.GetRecordId(), r.GetOutcome(), r.GetReason())
		}
		s := r.GetScores()[0]
		if s.GetPassed() != want[r.GetRecordId()] {
			t.Errorf("%s: passed=%v (%s)", r.GetRecordId(), s.GetPassed(), s.GetExplanation())
		}
		if s.GetMetadata()["isolation"].GetStructValue().GetFields()["driver"].GetStringValue() == "" {
			t.Errorf("%s: no isolation report", r.GetRecordId())
		}
	}
	if _, err := os.Stat("/usr/evalsi-escape"); err == nil {
		os.Remove("/usr/evalsi-escape")
		t.Fatal("generated code wrote to the host")
	}
}

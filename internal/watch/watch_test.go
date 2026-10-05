package watch

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// fakeWorker: "test/quality" scores 1 for outputs containing "good" and 0.2
// otherwise; "test/deep" always scores 0.1 and counts its calls.
type fakeWorker struct {
	mu        sync.Mutex
	deepCalls int
}

func (f *fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	return nil, nil
}
func (f *fakeWorker) Reduce(context.Context, *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return nil, fmt.Errorf("unused")
}
func (f *fakeWorker) Generate(context.Context, *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	return nil, fmt.Errorf("unused")
}
func (f *fakeWorker) RunTask(context.Context, *pluginv1alpha1.RunTaskRequest, func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	return nil, errors.New("no agent runs here")
}

func (f *fakeWorker) LoadDataset(context.Context, *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	return nil, fmt.Errorf("unused")
}

func (f *fakeWorker) Evaluate(_ context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	resp := &pluginv1alpha1.EvaluateResponse{}
	for _, r := range req.GetRecords() {
		v := 0.1
		if req.GetEvaluator() == "test/quality" {
			v = 0.2
			if strings.Contains(r.GetOutput().GetText(), "good") {
				v = 1
			}
		} else {
			f.mu.Lock()
			f.deepCalls++
			f.mu.Unlock()
		}
		resp.Results = append(resp.Results, &evalsiv1alpha1.EvaluationResult{
			RecordId: r.GetId(), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
			Scores: []*evalsiv1alpha1.Score{{Name: catalog.ShortName(req.GetEvaluator()), Value: &evalsiv1alpha1.Score_Number{Number: v}}},
		})
	}
	return resp, nil
}

func manifest(name string, scope evalsiv1alpha1.Scope) *evalsiv1alpha1.EvaluatorManifest {
	schema, _ := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{}})
	return &evalsiv1alpha1.EvaluatorManifest{
		Name: name, Version: "1.0.0", Scope: scope, ParamsSchema: schema,
		Outputs: []*evalsiv1alpha1.MetricSpec{{Name: catalog.ShortName(name), Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER}},
	}
}

type harness struct {
	e      *Engine
	st     *store.Store
	worker *fakeWorker
	dir    string
	hooks  chan alertEvent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	w := &fakeWorker{}
	eng := evaluation.New(w, catalog.New([]*evalsiv1alpha1.EvaluatorManifest{
		manifest("test/quality", evalsiv1alpha1.Scope_SCOPE_RECORD),
		manifest("test/deep", evalsiv1alpha1.Scope_SCOPE_RECORD),
		manifest("test/corpus", evalsiv1alpha1.Scope_SCOPE_DATASET),
	}), nil, "", config.Evaluate{BatchSize: 8, Parallelism: 2, MaxRecords: 100})
	e, err := New(context.Background(), st, eng, Options{DatasetsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	hooks := make(chan alertEvent, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev alertEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		hooks <- ev
	}))
	t.Cleanup(srv.Close)
	return &harness{e: e, st: st, worker: w, dir: dir, hooks: hooks}
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// trace builds a one-span trace with the given service, output and error flag.
func trace(n int, service, output string, failed bool) ingest.Trace {
	id := make([]byte, 16)
	id[15] = byte(n)
	sp := &tracepb.Span{
		TraceId: id, SpanId: []byte{1, 2, 3, 4, 5, 6, 7, byte(n)}, Name: "turn",
		StartTimeUnixNano: uint64(time.Date(2026, 10, 5, 12, 0, n, 0, time.UTC).UnixNano()),
		EndTimeUnixNano:   uint64(time.Date(2026, 10, 5, 12, 0, n+1, 0, time.UTC).UnixNano()),
		Attributes:        []*commonpb.KeyValue{kv("input.value", "question"), kv("output.value", output), kv("tier", "gold")},
	}
	if failed {
		sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"}
	}
	return ingest.Trace{TraceID: hex.EncodeToString(id), Spans: []ingest.Span{{Span: sp, Resource: ingest.Attrs{"service.name": service}}}}
}

func refs(names ...string) []*evalsiv1alpha1.EvaluatorRef {
	var out []*evalsiv1alpha1.EvaluatorRef
	for _, n := range names {
		out = append(out, &evalsiv1alpha1.EvaluatorRef{Ref: n})
	}
	return out
}

func TestCompileRejectsBadPolicies(t *testing.T) {
	h := newHarness(t)
	base := func() *evalsiv1alpha1.OnlineEvalPolicy {
		return &evalsiv1alpha1.OnlineEvalPolicy{Name: "p", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/quality")}}}
	}
	cases := map[string]func(p *evalsiv1alpha1.OnlineEvalPolicy){
		"policy name":        func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Name = "Bad Name" },
		"selector":           func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Selector = "service ==" },
		"boolean":            func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Selector = "service" },
		"at least one stage": func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Stages = nil },
		"more than one stage": func(p *evalsiv1alpha1.OnlineEvalPolicy) {
			p.Stages = append(p.Stages, &evalsiv1alpha1.CascadeStage{Evaluators: refs("test/quality")})
		},
		"dataset-scope": func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Stages[0].Evaluators = refs("test/corpus") },
		"sampling.rate": func(p *evalsiv1alpha1.OnlineEvalPolicy) {
			p.Sampling = &evalsiv1alpha1.Sampling{Rate: proto.Float64(2)}
		},
		"below or above":       func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Alerts = []*evalsiv1alpha1.Alert{{Metric: "quality"}} },
		"promote.when":         func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Promote = &evalsiv1alpha1.Promotion{Dataset: "d"} },
		"unknown evaluator":    func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Stages[0].Evaluators = refs("nope") },
		"undeclared reference": func(p *evalsiv1alpha1.OnlineEvalPolicy) { p.Selector = "nonexistent == 1" },
	}
	for want, mutate := range cases {
		p := base()
		mutate(p)
		_, err := h.e.ApplyPolicy(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: p}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestPolicyPipeline(t *testing.T) {
	h := newHarness(t)
	webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var ev alertEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		h.hooks <- ev
	}))
	defer webhook.Close()
	policy := &evalsiv1alpha1.OnlineEvalPolicy{
		Name:     "support",
		Selector: `service == "support-agent" && attributes["tier"] == "gold"`,
		Sampling: &evalsiv1alpha1.Sampling{Rate: proto.Float64(0), Always: []string{"error", `name == "turn" && duration_ms > 10`}},
		Stages: []*evalsiv1alpha1.CascadeStage{
			{Evaluators: refs("test/quality")},
			{Evaluators: refs("test/deep"), When: `scores["quality"] < 0.5`},
		},
		Alerts:  []*evalsiv1alpha1.Alert{{Metric: "quality", Below: proto.Float64(0.7), MinSamples: 3, Webhook: webhook.URL}},
		Promote: &evalsiv1alpha1.Promotion{Dataset: "support-regressions", When: `scores["deep"] < 0.5`},
	}
	if _, err := h.e.ApplyPolicy(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); err != nil {
		t.Fatal(err)
	}
	traces := []ingest.Trace{
		trace(1, "support-agent", "a good answer", false),
		trace(2, "support-agent", "a bad answer", false),
		trace(3, "support-agent", "a bad answer", true),
		trace(4, "billing", "a good answer", false),
	}
	var items []item
	for _, tr := range traces {
		h.e.Ingest(tr)
		items = append(items, <-h.e.queue)
	}
	h.e.process(context.Background(), items)

	stats, _ := h.e.Stats("support")
	if stats.GetTracesSeen() != 4 || stats.GetTracesMatched() != 3 || stats.GetTracesSampled() != 3 || stats.GetTracesEvaluated() != 3 || stats.GetTracesPromoted() != 2 {
		t.Errorf("stats = %v", stats)
	}
	if h.worker.deepCalls != 2 {
		t.Errorf("deep stage ran %d times, want 2 (only low-quality traces)", h.worker.deepCalls)
	}
	means := map[string]float64{}
	for _, m := range stats.GetMetrics() {
		means[m.GetMetric()] = m.GetMean()
	}
	if math.Abs(means["quality"]-(1+0.2+0.2)/3) > 1e-12 || means["deep"] != 0.1 {
		t.Errorf("window means = %v", means)
	}
	select {
	case ev := <-h.hooks:
		if !ev.Firing || ev.Metric != "quality" || ev.N != 3 {
			t.Errorf("alert = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("alert webhook not called")
	}
	if !stats.GetAlerts()[0].GetFiring() {
		t.Error("alert state not firing")
	}

	// Results are stored per trace and policy.
	got, err := Traces{Store: h.st}.GetTrace(context.Background(), connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: traces[1].TraceID}))
	if err != nil {
		t.Fatal(err)
	}
	if pr := got.Msg.GetPolicies(); len(pr) != 1 || len(pr[0].GetResults()) != 2 || got.Msg.GetRecord().GetOutput().GetText() != "a bad answer" {
		t.Errorf("trace = %v", got.Msg)
	}
	list, _ := Traces{Store: h.st}.ListTraces(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListTracesRequest{Service: "support-agent"}))
	if len(list.Msg.GetTraces()) != 3 || list.Msg.GetTraces()[0].GetResults() != 2 || !list.Msg.GetTraces()[0].GetError() {
		t.Errorf("traces = %v", list.Msg.GetTraces())
	}

	// Promoted traces form a dataset the Python loader understands.
	raw, err := os.ReadFile(filepath.Join(h.dir, "promoted", "default", "support-regressions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var row map[string]any
	if err := json.Unmarshal(lines[0], &row); err != nil || len(lines) != 2 {
		t.Fatalf("promoted rows = %d, %v", len(lines), err)
	}
	if row["output"].(map[string]any)["text"] != "a bad answer" || row["metadata"].(map[string]any)["online_scores"] == nil {
		t.Errorf("promoted row = %v", row)
	}

	// Metrics.
	rec := httptest.NewRecorder()
	MetricsHandler(h.e, ingest.NewAssembler(ingest.AssemblerOptions{}, func(ingest.Trace) {})).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`evalsi_traces_ingested_total 4`,
		`evalsi_policy_traces_total{policy="support",stage="matched"} 3`,
		`evalsi_policy_alert_firing{policy="support",metric="quality"} 1`,
		`evalsi_policy_metric_samples{policy="support",metric="deep"} 2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestSamplingIsDeterministic(t *testing.T) {
	c := &compiled{rate: 0.3}
	hits := 0
	for i := range 2000 {
		id := fmt.Sprintf("%032x", i)
		first := c.sampled(id, nil)
		if first != c.sampled(id, nil) {
			t.Fatal("sampling decision changed for the same trace")
		}
		if first {
			hits++
		}
	}
	if hits < 500 || hits > 700 {
		t.Errorf("rate 0.3 sampled %d of 2000", hits)
	}
}

func TestPoliciesPersistAndDelete(t *testing.T) {
	h := newHarness(t)
	p := &evalsiv1alpha1.OnlineEvalPolicy{Name: "keep", Project: "x", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: refs("test/quality")}}}
	if err := h.e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(context.Background(), h.st, h.e.eval, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Policies("x"); len(got) != 1 || got[0].GetName() != "keep" {
		t.Fatalf("reloaded policies = %v", got)
	}
	if err := reloaded.Delete(context.Background(), "keep"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Delete(context.Background(), "keep"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("second delete: %v", err)
	}
	if _, err := h.e.GetPolicyStats(context.Background(), connect.NewRequest(&evalsiv1alpha1.GetPolicyStatsRequest{Name: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("stats for unknown policy: %v", err)
	}
}

func TestTraceRetention(t *testing.T) {
	h := newHarness(t)
	h.e.Ingest(trace(1, "s", "x", false))
	h.e.Ingest(trace(9, "s", "x", false))
	n, err := h.st.DeleteTracesBefore(context.Background(), time.Date(2026, 10, 5, 12, 0, 5, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	list, _, _ := h.st.ListTraces(context.Background(), store.TraceFilter{}, 10, "")
	if len(list) != 1 {
		t.Fatalf("left %d traces", len(list))
	}
}

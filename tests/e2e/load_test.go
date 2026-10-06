package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// TestLoad measures the §14 scale targets on one evalsid with a real Python
// worker (EVALSI_LOAD=1; EVALSI_LOAD_REPORT=<file> writes the numbers as
// JSON). Sizes scale with EVALSI_LOAD_SPANS and EVALSI_LOAD_RUN_RECORDS.
func TestLoad(t *testing.T) {
	if os.Getenv("EVALSI_LOAD") == "" {
		t.Skip("set EVALSI_LOAD=1 to run the load test")
	}
	e := start(t, func(c *config.Config) {
		// Production settings, not the other tests' short grace.
		c.OTLP.Grace = config.Default().OTLP.Grace
		c.OTLP.MaxTraces = 100000
		c.Sinks = nil
		c.Evaluate = config.Default().Evaluate
	})
	report := map[string]any{}
	defer func() {
		out, _ := json.MarshalIndent(report, "", "  ")
		t.Logf("load report:\n%s", out)
		if path := os.Getenv("EVALSI_LOAD_REPORT"); path != "" {
			_ = os.WriteFile(path, out, 0o644)
		}
	}()

	t.Run("EvaluateOverhead", func(t *testing.T) { evaluateOverhead(t, e, report) })
	t.Run("OTLPIngest", func(t *testing.T) { otlpIngest(t, e, report) })
	t.Run("OnlineFreshness", func(t *testing.T) { onlineFreshness(t, e, report) })
	t.Run("RunSize", func(t *testing.T) { runSize(t, e, report) })
	t.Run("SandboxLease", func(t *testing.T) { sandboxLease(t, report) })
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func percentile(ds []time.Duration, p float64) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Evaluate of one record with a deterministic evaluator, end to end through
// the API and the Python worker: target p50 < 20 ms.
func evaluateOverhead(t *testing.T, e env, report map[string]any) {
	ctx := context.Background()
	c := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), e.base, connect.WithGRPC())
	call := func(i int) time.Duration {
		begin := time.Now()
		_, err := c.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Records:    []*evalsiv1alpha1.Record{{Id: strconv.Itoa(i), Output: text("Paris"), Reference: text("Paris")}},
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(begin)
	}
	for i := range 50 {
		call(i)
	}
	var lat []time.Duration
	for i := range 1000 {
		lat = append(lat, call(i))
	}
	p50, p95 := percentile(lat, 0.5), percentile(lat, 0.95)
	report["evaluate_p50_ms"], report["evaluate_p95_ms"] = ms(p50), ms(p95)
	if p50 >= 20*time.Millisecond {
		t.Errorf("Evaluate p50 %v, target < 20ms", p50)
	}
}

// otlpBatch is one export request: traces of spansPerTrace spans each, the
// root span carrying the GenAI attributes ingest normalizes and redacts.
func otlpBatch(service string, traces, spansPerTrace int, seq *atomic.Uint64) ([]byte, [][]byte) {
	now := uint64(time.Now().UnixNano())
	var spans []*tracepb.Span
	var ids [][]byte
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	for range traces {
		n := seq.Add(1)
		traceID := make([]byte, 16)
		for i := range 8 {
			traceID[8+i] = byte(n >> (8 * (7 - i)))
		}
		traceID[0] = byte(rand.IntN(256))
		ids = append(ids, traceID)
		root := make([]byte, 8)
		copy(root, traceID[8:])
		root[0] = 1
		for s := range spansPerTrace {
			id := slices.Clone(root)
			id[1] = byte(s)
			sp := &tracepb.Span{TraceId: traceID, SpanId: id, Name: "chat", StartTimeUnixNano: now, EndTimeUnixNano: now + 1_000_000,
				Attributes: []*commonpb.KeyValue{str("gen_ai.operation.name", "chat"), str("gen_ai.request.model", "m"), str("user.email", "someone@example.com")}}
			if s == 0 {
				sp.Name = "invoke_agent"
				sp.EndTimeUnixNano = now + 2_000_000
				sp.Attributes = append(sp.Attributes, str("gen_ai.operation.name", "invoke_agent"),
					str("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"Where is my order?"}]}]`),
					str("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"It ships tomorrow."}]}]`))
			} else {
				sp.ParentSpanId = root
			}
			spans = append(spans, sp)
		}
	}
	body, _ := proto.Marshal(&collectortracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", service)}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}})
	return body, ids
}

func export(t *testing.T, client *http.Client, base string, body []byte) {
	resp, err := client.Post(base+"/v1/traces", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Error(err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("OTLP status %d", resp.StatusCode)
	}
}

func traceStored(ctx context.Context, c evalsiv1alpha1connect.TraceServiceClient, id []byte, wantPolicy bool) bool {
	got, err := c.GetTrace(ctx, connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: fmt.Sprintf("%x", id)}))
	if err != nil {
		return false
	}
	return !wantPolicy || len(got.Msg.GetPolicies()) > 0 && len(got.Msg.GetPolicies()[0].GetResults()) > 0
}

// OTLP/HTTP ingest on one replica: target ≥ 20k spans/s, with every trace
// stored (normalized, redacted, written) by the end of the measurement.
func otlpIngest(t *testing.T, e env, report map[string]any) {
	ctx := context.Background()
	total := envInt("EVALSI_LOAD_SPANS", 400_000)
	const spansPerTrace, tracesPerBatch, senders = 10, 50, 16
	batches := total / (spansPerTrace * tracesPerBatch)
	var seq atomic.Uint64
	bodies := make([][]byte, batches)
	var last []byte
	for i := range bodies {
		var ids [][]byte
		bodies[i], ids = otlpBatch("load", tracesPerBatch, spansPerTrace, &seq)
		last = ids[len(ids)-1]
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: senders}}
	var next atomic.Int64
	begin := time.Now()
	var wg sync.WaitGroup
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := next.Add(1) - 1; i < int64(batches); i = next.Add(1) - 1 {
				export(t, client, e.base, bodies[i])
			}
		}()
	}
	wg.Wait()
	accepted := time.Since(begin)
	traces := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	for !traceStored(ctx, traces, last, false) {
		if time.Since(begin) > 5*time.Minute {
			t.Fatal("the last trace was not stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stored := time.Since(begin)
	spans := float64(batches * tracesPerBatch * spansPerTrace)
	report["ingest_spans"] = int(spans)
	report["ingest_accepted_spans_per_s"] = int(spans / accepted.Seconds())
	// Includes the assembler's grace before the last trace is written.
	report["ingest_stored_spans_per_s"] = int(spans / stored.Seconds())
	if rate := spans / accepted.Seconds(); rate < 20_000 {
		t.Errorf("ingest %.0f spans/s, target ≥ 20k", rate)
	}
}

// From a trace's completion to its stored online score, non-judge
// evaluators, under a steady stream: target p95 < 2 min.
func onlineFreshness(t *testing.T, e env, report map[string]any) {
	ctx := context.Background()
	monitor := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), e.base, connect.WithGRPC())
	if _, err := monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: &evalsiv1alpha1.OnlineEvalPolicy{
		Name: "load-fresh", Selector: `service == "fresh"`,
		Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "latency"}, {Ref: "json-valid"}}}},
	}})); err != nil {
		t.Fatal(err)
	}
	traces := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	var seq atomic.Uint64
	seq.Store(1 << 40)
	type sent struct {
		id []byte
		at time.Time
	}
	var mu sync.Mutex
	var pending []sent
	var lat []time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			mu.Lock()
			batch := slices.Clone(pending)
			mu.Unlock()
			if len(batch) == 0 && len(lat) >= 200 {
				return
			}
			for _, s := range batch {
				if traceStored(ctx, traces, s.id, true) {
					mu.Lock()
					lat = append(lat, time.Since(s.at))
					pending = slices.DeleteFunc(pending, func(p sent) bool { return bytes.Equal(p.id, s.id) })
					mu.Unlock()
				}
			}
		}
	}()
	// 200 traces at 20/s.
	for range 200 {
		body, ids := otlpBatch("fresh", 1, 3, &seq)
		export(t, http.DefaultClient, e.base, body)
		mu.Lock()
		pending = append(pending, sent{ids[0], time.Now()})
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	<-done
	if len(lat) < 200 {
		t.Fatalf("%d of 200 traces got scores", len(lat))
	}
	p50, p95 := percentile(lat, 0.5), percentile(lat, 0.95)
	report["freshness_p50_s"], report["freshness_p95_s"] = p50.Seconds(), p95.Seconds()
	if p95 >= 2*time.Minute {
		t.Errorf("freshness p95 %v, target < 2 min", p95)
	}
}

// A large run: records × evaluators tasks, all scored, resumable state on
// disk (the target is ≥ 1M tasks; EVALSI_LOAD_RUN_RECORDS sets the size).
func runSize(t *testing.T, e env, report map[string]any) {
	ctx := context.Background()
	n := envInt("EVALSI_LOAD_RUN_RECORDS", 100_000)
	path := filepath.Join(e.datasets, "load.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for i := range n {
		answer := strconv.Itoa(i % 7)
		fmt.Fprintf(w, `{"id":"r%d","q":"q%d","out":"%s","ref":"%s"}`+"\n", i, i, answer, strconv.Itoa(i%5))
	}
	_ = w.Flush()
	_ = f.Close()
	runs := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	begin := time.Now()
	created, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: &evalsiv1alpha1.RunSpec{
		Dataset: &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Path{Path: "load.jsonl"},
			Mapping: map[string]string{"input": "q", "output": "out", "reference": "ref"}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}, {Ref: "numeric-match"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	var final *evalsiv1alpha1.Run
	for {
		got, err := runs.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: created.Msg.GetRun().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if s := got.Msg.GetRun().GetStatus(); s != evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING && s != evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING {
			final = got.Msg.GetRun()
			break
		}
		if time.Since(begin) > 30*time.Minute {
			t.Fatal("the run did not finish in 30 minutes")
		}
		time.Sleep(time.Second)
	}
	elapsed := time.Since(begin)
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("run %v: %s", final.GetStatus(), final.GetError())
	}
	for _, s := range final.GetSummaries() {
		if s.GetMetric() == "exact-match" && s.GetN() != int64(n) {
			t.Errorf("exact-match n = %d, want %d", s.GetN(), n)
		}
	}
	tasks := 2 * n
	report["run_tasks"], report["run_seconds"] = tasks, elapsed.Seconds()
	report["run_tasks_per_s"] = int(float64(tasks) / elapsed.Seconds())
}

// Sandbox leases on this host's strongest rung (bubblewrap without KVM):
// cold creation, and restoring a snapshot (warm). The target (p95 < 250 ms
// warm, < 2 s cold) is stated for Firecracker snapshots.
func sandboxLease(t *testing.T, report map[string]any) {
	needSandbox(t)
	ctx := context.Background()
	sb, err := sandbox.New(sandbox.Config{WorkDir: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := sandbox.NewManager(sb)
	defer m.Close()
	// A lease is ready when a command has run in it.
	ready := func(s *sandbox.Session) {
		if res := s.Exec(ctx, &sandbox.Exec{Command: []string{"true"}, Timeout: 30 * time.Second}, io.Discard, io.Discard); res.ExitCode != 0 {
			t.Fatalf("exec: %+v", res)
		}
	}
	var cold, warm []time.Duration
	var base *sandbox.Session
	for i := range 50 {
		begin := time.Now()
		s, err := m.Create(ctx, &sandbox.Spec{Files: map[string][]byte{"main.py": []byte("print(1)\n")}})
		if err != nil {
			t.Fatal(err)
		}
		ready(s)
		cold = append(cold, time.Since(begin))
		if i == 0 {
			base = s
			continue
		}
		_ = m.Destroy(s.ID)
	}
	snap, err := m.Snapshot(ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		begin := time.Now()
		s, err := m.Restore(ctx, snap, nil)
		if err != nil {
			t.Fatal(err)
		}
		ready(s)
		warm = append(warm, time.Since(begin))
		_ = m.Destroy(s.ID)
	}
	report["sandbox_rung"] = base.Isolation().Driver
	report["sandbox_cold_p95_ms"], report["sandbox_warm_p95_ms"] = ms(percentile(cold, 0.95)), ms(percentile(warm, 0.95))
	if p := percentile(warm, 0.95); p >= 250*time.Millisecond {
		t.Errorf("warm lease p95 %v, target < 250ms", p)
	}
	if p := percentile(cold, 0.95); p >= 2*time.Second {
		t.Errorf("cold lease p95 %v, target < 2s", p)
	}
}

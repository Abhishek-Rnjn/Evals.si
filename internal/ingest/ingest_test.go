package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func kv(k string, v any) *commonpb.KeyValue {
	var av *commonpb.AnyValue
	switch x := v.(type) {
	case string:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
	case int:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
	case bool:
		av = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
	}
	return &commonpb.KeyValue{Key: k, Value: av}
}

var (
	traceID = bytes.Repeat([]byte{0xab}, 16)
	t0      = uint64(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC).UnixNano())
)

func span(id, parent byte, name string, startMS, endMS uint64, attrs ...*commonpb.KeyValue) *tracepb.Span {
	s := &tracepb.Span{
		TraceId: traceID, SpanId: bytes.Repeat([]byte{id}, 8), Name: name,
		StartTimeUnixNano: t0 + startMS*1e6, EndTimeUnixNano: t0 + endMS*1e6, Attributes: attrs,
	}
	if parent != 0 {
		s.ParentSpanId = bytes.Repeat([]byte{parent}, 8)
	}
	return s
}

// agentTrace is an agent turn instrumented with OTel GenAI conventions: an
// agent span, a chat call that asks for a tool, the tool, and a final chat call.
func agentTrace() []*tracepb.ResourceSpans {
	spans := []*tracepb.Span{
		span(1, 0, "invoke_agent support", 0, 900,
			kv("gen_ai.operation.name", "invoke_agent"), kv("gen_ai.conversation.id", "conv-7"),
			kv("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"Where is order 42?"}]}]`),
			kv("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"It ships tomorrow."}]}]`)),
		span(2, 1, "chat claude", 10, 300,
			kv("gen_ai.operation.name", "chat"), kv("gen_ai.request.model", "claude-opus-5-5"),
			kv("gen_ai.usage.input_tokens", 120), kv("gen_ai.usage.output_tokens", 30),
			kv("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"lookup_order","arguments":{"id":42}}]}]`)),
		span(3, 1, "execute_tool lookup_order", 310, 400,
			kv("gen_ai.operation.name", "execute_tool"), kv("gen_ai.tool.name", "lookup_order"),
			kv("gen_ai.tool.call.arguments", `{"id": 42}`), kv("gen_ai.tool.call.result", `{"status": "shipping"}`)),
		span(4, 1, "chat claude", 410, 880,
			kv("gen_ai.operation.name", "chat"), kv("gen_ai.response.model", "claude-opus-5-5"),
			kv("gen_ai.usage.input_tokens", 200), kv("gen_ai.usage.output_tokens", 12)),
	}
	return []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "support-agent")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}
}

func TestGenAITraceToRecord(t *testing.T) {
	record, info := ToRecord(Trace{TraceID: hex.EncodeToString(traceID), Spans: SpansOf(agentTrace())})
	if record.GetId() != hex.EncodeToString(traceID) {
		t.Errorf("id = %s", record.GetId())
	}
	if got := record.GetInput().GetMessages().GetMessages()[0].GetContent(); got != "Where is order 42?" {
		t.Errorf("input = %q", got)
	}
	if got := record.GetOutput().GetMessages().GetMessages()[0].GetContent(); got != "It ships tomorrow." {
		t.Errorf("output = %q", got)
	}
	steps := record.GetTrajectory().GetSteps()
	types := []evalsiv1alpha1.StepType{steps[0].GetType(), steps[1].GetType(), steps[2].GetType(), steps[3].GetType()}
	want := []evalsiv1alpha1.StepType{evalsiv1alpha1.StepType_STEP_TYPE_AGENT, evalsiv1alpha1.StepType_STEP_TYPE_LLM, evalsiv1alpha1.StepType_STEP_TYPE_TOOL, evalsiv1alpha1.StepType_STEP_TYPE_LLM}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("step %d type = %v, want %v", i, types[i], want[i])
		}
	}
	call := steps[1].GetOutput().GetMessages().GetMessages()[0].GetToolCalls()[0]
	if call.GetName() != "lookup_order" || call.GetArguments() != `{"id":42}` {
		t.Errorf("tool call = %v", call)
	}
	if steps[2].GetName() != "lookup_order" || steps[2].GetInput().GetJson().GetStructValue().AsMap()["id"] != float64(42) {
		t.Errorf("tool step = %v", steps[2])
	}
	if record.GetUsage().GetInputTokens() != 320 || record.GetUsage().GetOutputTokens() != 42 {
		t.Errorf("usage = %v", record.GetUsage())
	}
	if record.GetUsage().GetLatency().AsDuration() != 900*time.Millisecond || info.DurationMS != 900 {
		t.Errorf("latency = %v, info %v", record.GetUsage().GetLatency(), info.DurationMS)
	}
	if record.GetTrajectory().GetSessionId() != "conv-7" || info.Service != "support-agent" {
		t.Errorf("session %q service %q", record.GetTrajectory().GetSessionId(), info.Service)
	}
	if len(info.Tools) != 1 || info.Models[0] != "claude-opus-5-5" || info.Steps != 4 || info.Error {
		t.Errorf("info = %+v", info)
	}
}

func TestOtherConventions(t *testing.T) {
	cases := map[string]struct {
		attrs    []*commonpb.KeyValue
		typ      evalsiv1alpha1.StepType
		in, out  string
		inTokens int64
	}{
		"openinference": {
			attrs: []*commonpb.KeyValue{kv("openinference.span.kind", "LLM"), kv("llm.model_name", "m"),
				kv("llm.input_messages.0.message.role", "user"), kv("llm.input_messages.0.message.content", "hi"),
				kv("llm.output_messages.0.message.role", "assistant"), kv("llm.output_messages.0.message.content", "hello"),
				kv("llm.token_count.prompt", 5)},
			typ: evalsiv1alpha1.StepType_STEP_TYPE_LLM, in: "hi", out: "hello", inTokens: 5,
		},
		"openllmetry": {
			attrs: []*commonpb.KeyValue{kv("llm.request.type", "chat"),
				kv("gen_ai.prompt.0.role", "user"), kv("gen_ai.prompt.0.content", "q"),
				kv("gen_ai.completion.0.role", "assistant"), kv("gen_ai.completion.0.content", "a"),
				kv("gen_ai.usage.prompt_tokens", 7)},
			typ: evalsiv1alpha1.StepType_STEP_TYPE_LLM, in: "q", out: "a", inTokens: 7,
		},
		"openinference retriever": {
			attrs: []*commonpb.KeyValue{kv("openinference.span.kind", "RETRIEVER"), kv("input.value", "query"), kv("output.value", "docs")},
			typ:   evalsiv1alpha1.StepType_STEP_TYPE_RETRIEVAL, in: "query", out: "docs",
		},
		"mlflow": {
			attrs: []*commonpb.KeyValue{kv("mlflow.spanType", "TOOL"), kv("mlflow.spanInputs", "x"), kv("mlflow.spanOutputs", "y")},
			typ:   evalsiv1alpha1.StepType_STEP_TYPE_TOOL, in: "x", out: "y",
		},
		"unknown": {attrs: []*commonpb.KeyValue{kv("custom", "v")}, typ: evalsiv1alpha1.StepType_STEP_TYPE_GENERIC},
	}
	for name, c := range cases {
		step := ToStep(Span{Span: span(9, 0, "s", 0, 1, c.attrs...)})
		text := func(content *evalsiv1alpha1.Content) string {
			if m := content.GetMessages().GetMessages(); len(m) > 0 {
				return m[0].GetContent()
			}
			return content.GetText()
		}
		if step.GetType() != c.typ || text(step.GetInput()) != c.in || text(step.GetOutput()) != c.out || step.GetUsage().GetInputTokens() != c.inTokens {
			t.Errorf("%s: step = %v", name, step)
		}
		if _, ok := step.GetAttributes()[c.attrs[0].GetKey()]; !ok {
			t.Errorf("%s: raw attributes not kept", name)
		}
	}
}

func TestErrorsAndEvents(t *testing.T) {
	s := span(1, 0, "chat", 0, 1, kv("gen_ai.operation.name", "chat"))
	s.Events = []*tracepb.Span_Event{
		{Name: "gen_ai.content.prompt", Attributes: []*commonpb.KeyValue{kv("gen_ai.prompt", "old-style prompt")}},
		{Name: "exception", Attributes: []*commonpb.KeyValue{kv("exception.type", "Timeout"), kv("exception.message", "slow")}},
	}
	step := ToStep(Span{Span: s})
	if step.GetInput().GetText() != "old-style prompt" || step.GetError() != "Timeout: slow" {
		t.Errorf("step = %v", step)
	}
	s.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"}
	if _, info := ToRecord(Trace{TraceID: "t", Spans: []Span{{Span: s}}}); !info.Error {
		t.Error("error status not detected")
	}
}

type collected struct {
	mu     sync.Mutex
	traces []Trace
}

func (c *collected) emit(t Trace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traces = append(c.traces, t)
}

func (c *collected) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.traces)
}

func TestAssemblerTiming(t *testing.T) {
	c := &collected{}
	a := NewAssembler(AssemblerOptions{Grace: time.Second, Idle: 10 * time.Second, MaxAge: time.Minute, MaxTraces: 2, MaxSpans: 3}, c.emit)
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	child := func(trace byte, id byte) Span {
		s := span(id, 1, "c", 0, 1)
		s.TraceId = bytes.Repeat([]byte{trace}, 16)
		return Span{Span: s}
	}
	root := func(trace byte) Span {
		s := span(1, 0, "r", 0, 1)
		s.TraceId = bytes.Repeat([]byte{trace}, 16)
		return Span{Span: s}
	}
	a.Add([]Span{child(1, 2), root(1)})
	a.Add([]Span{child(2, 2)})
	a.Flush(false)
	if c.count() != 0 {
		t.Fatal("flushed before the grace period")
	}
	now = now.Add(1500 * time.Millisecond)
	a.Flush(false)
	if c.count() != 1 || len(c.traces[0].Spans) != 2 {
		t.Fatalf("after grace: %d traces", c.count())
	}
	now = now.Add(10 * time.Second)
	a.Flush(false)
	if c.count() != 2 {
		t.Fatalf("idle trace not flushed: %d", c.count())
	}
	// Capacity: a third buffered trace evicts the oldest; extra spans are dropped.
	a.Add([]Span{child(3, 2), child(3, 3), child(3, 4), child(3, 5)})
	now = now.Add(time.Millisecond)
	a.Add([]Span{child(4, 2)})
	a.Add([]Span{child(5, 2)})
	if c.count() != 3 || a.Dropped() != 1 || a.Pending() != 2 {
		t.Fatalf("eviction: traces=%d dropped=%d pending=%d", c.count(), a.Dropped(), a.Pending())
	}
	a.Flush(true)
	if c.count() != 5 || a.Pending() != 0 {
		t.Fatalf("forced flush: %d", c.count())
	}
}

func TestReceiverTransports(t *testing.T) {
	c := &collected{}
	a := NewAssembler(AssemblerOptions{}, c.emit)
	mux := http.NewServeMux()
	NewReceiver(a, nil).Register(mux)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	req := &collectortracepb.ExportTraceServiceRequest{ResourceSpans: agentTrace()}

	// OTLP/gRPC.
	client := connect.NewClient[collectortracepb.ExportTraceServiceRequest, collectortracepb.ExportTraceServiceResponse](
		srv.Client(), srv.URL+TraceExportProcedure, connect.WithGRPC())
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	// OTLP/HTTP protobuf, gzipped.
	raw, _ := proto.Marshal(req)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw)
	_ = w.Close()
	post := func(body []byte, contentType, encoding string) int {
		r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/traces", bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		if encoding != "" {
			r.Header.Set("Content-Encoding", encoding)
		}
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(gz.Bytes(), "application/x-protobuf", "gzip"); code != 200 {
		t.Fatalf("protobuf status %d", code)
	}
	// OTLP/HTTP JSON with hex ids, as the spec requires.
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc"}}]},
	  "scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708",
	  "name":"root","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`
	if code := post([]byte(body), "application/json", ""); code != 200 {
		t.Fatalf("json status %d", code)
	}
	if code := post([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"zz"}]}]}]}`), "application/json", ""); code != 400 {
		t.Fatalf("bad hex status %d", code)
	}
	if code := post(raw, "text/plain", ""); code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type status %d", code)
	}
	a.Flush(true)
	var ids []string
	for _, tr := range c.traces {
		ids = append(ids, tr.TraceID)
	}
	joined := strings.Join(ids, ",")
	if c.count() != 2 || !strings.Contains(joined, "0102030405060708090a0b0c0d0e0f10") {
		t.Fatalf("traces = %v", ids)
	}
	for _, tr := range c.traces {
		if tr.TraceID == hex.EncodeToString(traceID) && len(tr.Spans) != 8 {
			t.Errorf("agent trace has %d spans, want 8 (gRPC + HTTP copies)", len(tr.Spans))
		}
	}
}

// Spans over a trace's limit are reported back as a partial success, so an
// exporter sees what was not kept.
func TestReceiverReportsDroppedSpans(t *testing.T) {
	c := &collected{}
	a := NewAssembler(AssemblerOptions{MaxSpans: 3}, c.emit)
	mux := http.NewServeMux()
	NewReceiver(a, nil).Register(mux)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := connect.NewClient[collectortracepb.ExportTraceServiceRequest, collectortracepb.ExportTraceServiceResponse](
		srv.Client(), srv.URL+TraceExportProcedure, connect.WithGRPC())
	req := &collectortracepb.ExportTraceServiceRequest{ResourceSpans: agentTrace()}
	resp, err := client.CallUnary(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	ps := resp.Msg.GetPartialSuccess()
	if ps.GetRejectedSpans() != 1 || !strings.Contains(ps.GetErrorMessage(), "span limit") {
		t.Errorf("partial success = %v; want 1 of the 4 spans rejected", ps)
	}
}

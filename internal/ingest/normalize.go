// Package ingest receives OTLP traces, normalizes spans from the common GenAI
// conventions into canonical steps, and assembles each finished trace into a
// Record that evaluators can grade.
//
// Supported conventions (attributes on spans, plus span events where noted):
//   - OpenTelemetry GenAI: gen_ai.operation.name, gen_ai.request/response.model,
//     gen_ai.usage.*, gen_ai.input.messages / gen_ai.output.messages,
//     gen_ai.system_instructions, gen_ai.tool.*, gen_ai.conversation.id, and the
//     older gen_ai.prompt / gen_ai.completion event attributes.
//   - OpenInference (Arize Phoenix): openinference.span.kind, input.value,
//     output.value, llm.input_messages.N.message.*, llm.output_messages.N.message.*
//     (with their tool_calls), llm.model_name, llm.token_count.*, tool.name,
//     session.id.
//   - OpenLLMetry (Traceloop): gen_ai.prompt.N.*, gen_ai.completion.N.*,
//     traceloop.span.kind, llm.request.type.
//   - MLflow tracing: mlflow.spanType, mlflow.spanInputs, mlflow.spanOutputs.
//   - LangChain and LangGraph messages in any of those, or in LangSmith's
//     gen_ai.prompt and gen_ai.completion JSON (langchain.go).
//
// profiles.go adds what the first-cut agent frameworks need beyond these
// (LangGraph, CrewAI, OpenAI Agents SDK, LlamaIndex, Claude Agent SDK), each
// tested against a recorded trace in testdata/frameworks.
//
// Spans no mapper recognizes become generic steps with their raw attributes.
package ingest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Attrs is a flattened attribute map.
type Attrs map[string]any

// AnyValue converts an OTLP value to plain Go.
func AnyValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, AnyValue(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = AnyValue(kv.GetValue())
		}
		return out
	}
	return nil
}

// ToAttrs flattens key-values.
func ToAttrs(kvs []*commonpb.KeyValue) Attrs {
	out := make(Attrs, len(kvs))
	for _, kv := range kvs {
		out[kv.GetKey()] = AnyValue(kv.GetValue())
	}
	return out
}

func (a Attrs) str(keys ...string) string {
	for _, k := range keys {
		if v, ok := a[k]; ok {
			switch x := v.(type) {
			case string:
				if x != "" {
					return x
				}
			case nil:
			default:
				return fmt.Sprint(x)
			}
		}
	}
	return ""
}

func (a Attrs) int(keys ...string) *int64 {
	for _, k := range keys {
		switch x := a[k].(type) {
		case int64:
			return proto.Int64(x)
		case float64:
			return proto.Int64(int64(x))
		case string:
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return proto.Int64(n)
			}
		}
	}
	return nil
}

// Span is one received span with its resource attributes.
type Span struct {
	Span     *tracepb.Span
	Resource Attrs
	// The project the span was ingested into, assigned from the ingest
	// credential; empty means the default project.
	Project string
	// Labels from resource attributes evalsi.label.<key> and the credential.
	Labels map[string]string
}

func spanID(b []byte) string { return hex.EncodeToString(b) }

func unixNano(n uint64) time.Time { return time.Unix(0, int64(n)).UTC() }

func stepType(a Attrs) evalsiv1alpha1.StepType {
	switch strings.ToLower(a.str("gen_ai.operation.name")) {
	case "chat", "text_completion", "generate_content", "completion":
		return evalsiv1alpha1.StepType_STEP_TYPE_LLM
	case "execute_tool":
		return evalsiv1alpha1.StepType_STEP_TYPE_TOOL
	case "invoke_agent", "create_agent":
		return evalsiv1alpha1.StepType_STEP_TYPE_AGENT
	case "embeddings":
		return evalsiv1alpha1.StepType_STEP_TYPE_EMBEDDING
	}
	switch strings.ToUpper(a.mlflowStr("openinference.span.kind", "mlflow.spanType")) {
	case "LLM", "CHAT_MODEL":
		return evalsiv1alpha1.StepType_STEP_TYPE_LLM
	case "TOOL":
		return evalsiv1alpha1.StepType_STEP_TYPE_TOOL
	case "AGENT":
		return evalsiv1alpha1.StepType_STEP_TYPE_AGENT
	case "RETRIEVER", "RERANKER":
		return evalsiv1alpha1.StepType_STEP_TYPE_RETRIEVAL
	case "EMBEDDING":
		return evalsiv1alpha1.StepType_STEP_TYPE_EMBEDDING
	case "GUARDRAIL":
		return evalsiv1alpha1.StepType_STEP_TYPE_GUARDRAIL
	}
	switch strings.ToLower(a.str("traceloop.span.kind")) {
	case "agent":
		return evalsiv1alpha1.StepType_STEP_TYPE_AGENT
	case "tool":
		return evalsiv1alpha1.StepType_STEP_TYPE_TOOL
	}
	if a.str("llm.request.type") != "" || a.str("gen_ai.prompt.0.content") != "" {
		return evalsiv1alpha1.StepType_STEP_TYPE_LLM
	}
	return evalsiv1alpha1.StepType_STEP_TYPE_GENERIC
}

// genAIMessages parses the GenAI semconv message JSON:
// [{"role": "user", "parts": [{"type": "text", "content": "..."}]}].
func genAIMessages(raw string, systemRole bool) []*evalsiv1alpha1.Message {
	var msgs []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type      string          `json:"type"`
			Content   json.RawMessage `json:"content"`
			Name      string          `json:"name"`
			ID        string          `json:"id"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"parts"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal([]byte(raw), &msgs) != nil {
		return nil
	}
	var out []*evalsiv1alpha1.Message
	for _, m := range msgs {
		msg := &evalsiv1alpha1.Message{Role: m.Role}
		if systemRole && msg.Role == "" {
			msg.Role = "system"
		}
		var texts []string
		for _, p := range m.Parts {
			switch p.Type {
			case "text", "":
				texts = append(texts, rawText(p.Content))
			case "tool_call":
				msg.ToolCalls = append(msg.ToolCalls, &evalsiv1alpha1.ToolCall{Id: p.ID, Name: p.Name, Arguments: string(p.Arguments)})
			case "tool_call_response":
				texts = append(texts, rawText(p.Content))
				msg.ToolCallId = p.ID
			}
		}
		if len(m.Parts) == 0 && len(m.Content) > 0 {
			texts = append(texts, rawText(m.Content))
		}
		msg.Content = strings.Join(texts, "\n")
		out = append(out, msg)
	}
	return out
}

func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// indexedMessages reads prefix.N.<role|content> style attributes
// (OpenLLMetry gen_ai.prompt.N.*, OpenInference llm.input_messages.N.message.*),
// with the tool calls a message makes (OpenInference
// ...tool_calls.M.tool_call.function.{name,arguments}, OpenLLMetry
// ...tool_calls.M.{name,arguments}) and the call a tool message answers.
// Content split into parts (...contents.K.message_content.text, or
// ...content.K) is joined.
func indexedMessages(a Attrs, prefix, roleKey, contentKey string) []*evalsiv1alpha1.Message {
	msgBase := strings.TrimSuffix(roleKey, "role") // "message." or ""
	var out []*evalsiv1alpha1.Message
	for i := 0; ; i++ {
		base := fmt.Sprintf("%s.%d.", prefix, i)
		msg := &evalsiv1alpha1.Message{
			Role:       a.str(base + roleKey),
			Content:    a.str(base + contentKey),
			ToolCallId: a.str(base+msgBase+"tool_call_id", base+msgBase+"tool_call.id"),
		}
		if msg.Content == "" {
			var parts []string
			for k := 0; ; k++ {
				p := a.str(fmt.Sprintf("%s%scontents.%d.message_content.text", base, msgBase, k), fmt.Sprintf("%s%s.%d", base, contentKey, k))
				if p == "" {
					break
				}
				parts = append(parts, p)
			}
			msg.Content = strings.Join(parts, "\n")
		}
		for m := 0; ; m++ {
			call := fmt.Sprintf("%s%stool_calls.%d.", base, msgBase, m)
			name := a.str(call+"tool_call.function.name", call+"name")
			if name == "" {
				break
			}
			msg.ToolCalls = append(msg.ToolCalls, &evalsiv1alpha1.ToolCall{
				Id:        a.str(call+"tool_call.id", call+"id"),
				Name:      name,
				Arguments: a.str(call+"tool_call.function.arguments", call+"arguments"),
			})
		}
		if msg.Role == "" && msg.Content == "" && len(msg.ToolCalls) == 0 {
			break
		}
		out = append(out, msg)
	}
	// OpenInference puts roles without content on LangGraph's chain spans:
	// that is no message to read.
	for _, m := range out {
		if m.GetContent() != "" || len(m.GetToolCalls()) > 0 {
			return out
		}
	}
	return nil
}

func contentOf(v string) *evalsiv1alpha1.Content {
	if v == "" {
		return nil
	}
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var any any
		if json.Unmarshal([]byte(trimmed), &any) == nil {
			if val, err := structpb.NewValue(any); err == nil {
				return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Json{Json: val}}
			}
		}
	}
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: v}}
}

func messagesContent(msgs []*evalsiv1alpha1.Message) *evalsiv1alpha1.Content {
	if len(msgs) == 0 {
		return nil
	}
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Messages{Messages: &evalsiv1alpha1.Messages{Messages: msgs}}}
}

// spanIO extracts a span's input and output from whichever convention is present.
func spanIO(a Attrs, events []*tracepb.Span_Event) (in, out *evalsiv1alpha1.Content) {
	if raw := a.str("gen_ai.input.messages"); raw != "" {
		msgs := genAIMessages(raw, false)
		if sys := a.str("gen_ai.system_instructions"); sys != "" {
			if parts := genAIMessages("[{\"role\":\"system\",\"parts\":"+sys+"}]", true); len(parts) > 0 && parts[0].GetContent() != "" {
				msgs = append([]*evalsiv1alpha1.Message{parts[0]}, msgs...)
			}
		}
		in = messagesContent(msgs)
	}
	if raw := a.str("gen_ai.output.messages"); raw != "" {
		out = messagesContent(genAIMessages(raw, false))
	}
	if in == nil {
		in = messagesContent(indexedMessages(a, "gen_ai.prompt", "role", "content"))
	}
	if out == nil {
		out = messagesContent(indexedMessages(a, "gen_ai.completion", "role", "content"))
	}
	if in == nil {
		in = messagesContent(indexedMessages(a, "llm.input_messages", "message.role", "message.content"))
	}
	if out == nil {
		out = messagesContent(indexedMessages(a, "llm.output_messages", "message.role", "message.content"))
	}
	// LangChain, LangGraph and OpenAI calls as MLflow, OpenInference and
	// LangSmith serialize them: chat messages, else raw JSON below.
	if in == nil {
		in = messagesContent(messagesOf(a.str("mlflow.spanInputs", "input.value", "gen_ai.prompt")))
	}
	if out == nil {
		out = messagesContent(messagesOf(a.str("mlflow.spanOutputs", "output.value", "gen_ai.completion")))
	}
	if in == nil {
		in = contentOf(a.str("gen_ai.tool.call.arguments", "input.value", "mlflow.spanInputs", "gen_ai.prompt"))
	}
	if out == nil {
		out = contentOf(a.str("gen_ai.tool.call.result", "output.value", "mlflow.spanOutputs", "gen_ai.completion"))
	}
	// Older GenAI conventions put content on span events.
	for _, ev := range events {
		ea := ToAttrs(ev.GetAttributes())
		if in == nil {
			in = contentOf(ea.str("gen_ai.prompt"))
		}
		if out == nil {
			out = contentOf(ea.str("gen_ai.completion"))
		}
	}
	return in, out
}

// ToStep normalizes one span.
func ToStep(s Span) *evalsiv1alpha1.Step {
	sp := s.Span
	a := ToAttrs(sp.GetAttributes())
	step := &evalsiv1alpha1.Step{
		SpanId:       spanID(sp.GetSpanId()),
		ParentSpanId: spanID(sp.GetParentSpanId()),
		Type:         stepType(a),
		Name:         sp.GetName(),
		StartTime:    timestamppb.New(unixNano(sp.GetStartTimeUnixNano())),
		EndTime:      timestamppb.New(unixNano(sp.GetEndTimeUnixNano())),
		Attributes:   map[string]*structpb.Value{},
	}
	if step.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_TOOL {
		if name := a.str("gen_ai.tool.name", "tool.name"); name != "" {
			step.Name = name
		}
	}
	step.Input, step.Output = spanIO(a, sp.GetEvents())
	usage := &evalsiv1alpha1.Usage{
		InputTokens:  a.int("gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt", "llm.usage.prompt_tokens"),
		OutputTokens: a.int("gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion", "llm.usage.completion_tokens"),
	}
	if end, start := sp.GetEndTimeUnixNano(), sp.GetStartTimeUnixNano(); end > start {
		usage.Latency = durationpb.New(unixNano(end).Sub(unixNano(start)))
	}
	if mi, mo := a.mlflowTokens(); usage.InputTokens == nil && usage.OutputTokens == nil {
		usage.InputTokens, usage.OutputTokens = mi, mo
	}
	step.Usage = usage
	if sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		step.Error = sp.GetStatus().GetMessage()
		if step.Error == "" {
			step.Error = "error"
		}
	}
	for _, ev := range sp.GetEvents() {
		if ev.GetName() == "exception" && step.Error == "" {
			ea := ToAttrs(ev.GetAttributes())
			step.Error = strings.TrimSpace(ea.str("exception.type") + ": " + ea.str("exception.message"))
		}
	}
	for k, v := range a {
		if val, err := structpb.NewValue(v); err == nil {
			step.Attributes[k] = val
		}
	}
	profileStep(sp, a, step)
	return step
}

// Trace is an assembled trace.
type Trace struct {
	TraceID string
	Project string
	Spans   []Span
}

// TraceInfo is what policy expressions see about a trace.
type TraceInfo struct {
	Project    string
	Labels     map[string]string
	Service    string
	Name       string
	DurationMS float64
	Error      bool
	Steps      int
	Tools      []string
	Models     []string
	Attributes Attrs
	Resource   Attrs
}

// ToRecord turns an assembled trace into the record evaluators grade, plus
// the facts policy expressions select on.
func ToRecord(t Trace) (*evalsiv1alpha1.Record, TraceInfo) {
	spans := append([]Span(nil), t.Spans...)
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].Span.GetStartTimeUnixNano() < spans[j].Span.GetStartTimeUnixNano()
	})
	root := spans[0]
	for _, s := range spans {
		if len(s.Span.GetParentSpanId()) == 0 {
			root = s
			break
		}
	}
	rootAttrs := ToAttrs(root.Span.GetAttributes())
	traj := &evalsiv1alpha1.Trajectory{TraceId: t.TraceID}
	info := TraceInfo{
		Project:    t.Project,
		Labels:     root.Labels,
		Service:    root.Resource.str("service.name"),
		Name:       root.Span.GetName(),
		Attributes: rootAttrs,
		Resource:   root.Resource,
	}
	if info.Attributes == nil {
		info.Attributes = Attrs{}
	}
	if info.Resource == nil {
		info.Resource = Attrs{}
	}
	for _, s := range spans {
		step := ToStep(s)
		traj.Steps = append(traj.Steps, step)
		a := ToAttrs(s.Span.GetAttributes())
		if traj.SessionId == "" {
			traj.SessionId = a.str("gen_ai.conversation.id", "session.id")
		}
		if step.GetError() != "" {
			info.Error = true
		}
	}
	demoteWrappers(traj.Steps)
	var firstLLM, lastLLM *evalsiv1alpha1.Step
	for _, step := range traj.Steps {
		if step.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			if firstLLM == nil {
				firstLLM = step
			}
			lastLLM = step
		}
	}
	info.Tools, info.Models = Tools(traj.Steps), Models(traj.Steps)
	info.Steps = len(traj.Steps)
	rootStep := traj.Steps[0]
	for _, st := range traj.Steps {
		if st.GetSpanId() == spanID(root.Span.GetSpanId()) {
			rootStep = st
		}
	}
	usage := usageOf(traj.Steps, rootStep)
	if d := rootStep.GetUsage().GetLatency(); d != nil {
		usage.Latency = d
		info.DurationMS = float64(d.AsDuration().Microseconds()) / 1000
	}
	record := &evalsiv1alpha1.Record{
		Id:         t.TraceID,
		Input:      recordInput(rootStep, firstLLM),
		Output:     recordOutput(rootStep, lastLLM),
		Trajectory: traj,
		Usage:      usage,
		Metadata:   map[string]*structpb.Value{},
		Provenance: &evalsiv1alpha1.Provenance{Source: &evalsiv1alpha1.Provenance_Trace{Trace: &evalsiv1alpha1.TraceProvenance{TraceId: t.TraceID}}},
	}
	meta := map[string]any{"service": info.Service, "span_name": info.Name, "error": info.Error}
	if traj.SessionId != "" {
		meta["session_id"] = traj.SessionId
	}
	for k, v := range meta {
		if val, err := structpb.NewValue(v); err == nil {
			record.Metadata[k] = val
		}
	}
	return record, info
}

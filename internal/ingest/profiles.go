package ingest

import (
	"strings"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Framework profiles: what the generic conventions in normalize.go do not
// cover for the first-cut agent frameworks. Each rule is tested against a
// fixture recorded from the framework (testdata/frameworks).

// profileStep adjusts one normalized span for the framework that made it.
func profileStep(sp *tracepb.Span, a Attrs, step *evalsiv1alpha1.Step) {
	name := sp.GetName()
	switch {
	case strings.HasPrefix(name, "claude_code."):
		claudeCodeStep(sp, a, step)
	case strings.HasSuffix(name, "._prepare_chat_with_tools"):
		// LlamaIndex marks the step that builds a tool-calling request as an
		// LLM span; the model call is the astream_chat or achat after it.
		step.Type = evalsiv1alpha1.StepType_STEP_TYPE_GENERIC
	}
}

// claudeCodeStep reads Claude Code's own traces (the CLI the Claude Agent SDK
// runs, with CLAUDE_CODE_ENHANCED_TELEMETRY_BETA): span.type names the step,
// tokens are input_tokens and output_tokens, the prompt is on the
// interaction, and a tool's output is a tool.output event. They carry no
// model output, so a record from them has no answer.
func claudeCodeStep(sp *tracepb.Span, a Attrs, step *evalsiv1alpha1.Step) {
	switch a.str("span.type") {
	case "interaction":
		step.Type = evalsiv1alpha1.StepType_STEP_TYPE_AGENT
		if step.Input == nil {
			step.Input = contentOf(a.str("user_prompt"))
		}
	case "llm_request":
		step.Type = evalsiv1alpha1.StepType_STEP_TYPE_LLM
		if step.GetUsage().InputTokens == nil && step.GetUsage().OutputTokens == nil {
			step.Usage.InputTokens, step.Usage.OutputTokens = a.int("input_tokens"), a.int("output_tokens")
		}
	case "tool":
		step.Type = evalsiv1alpha1.StepType_STEP_TYPE_TOOL
		if n := a.str("tool_name"); n != "" {
			step.Name = n
		}
		for _, ev := range sp.GetEvents() {
			if ev.GetName() == "tool.output" && step.Output == nil {
				step.Output = contentOf(ToAttrs(ev.GetAttributes()).str("output"))
			}
		}
	default:
		// tool.execution and tool.blocked_on_user are parts of a tool step.
		step.Type = evalsiv1alpha1.StepType_STEP_TYPE_GENERIC
	}
}

// demoteWrappers makes a model-call step that contains another model call a
// generic one: LlamaIndex nests a streamed call in another, and an agent SDK's
// generation span can hold an OpenAI client span. The innermost call is the
// one the model answered.
func demoteWrappers(steps []*evalsiv1alpha1.Step) {
	byID := map[string]*evalsiv1alpha1.Step{}
	for _, s := range steps {
		byID[s.GetSpanId()] = s
	}
	wrappers := map[*evalsiv1alpha1.Step]bool{}
	for _, s := range steps {
		if s.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			continue
		}
		seen := map[string]bool{}
		for p := byID[s.GetParentSpanId()]; p != nil && !seen[p.GetSpanId()]; p = byID[p.GetParentSpanId()] {
			seen[p.GetSpanId()] = true
			if p.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
				wrappers[p] = true
			}
		}
	}
	for s := range wrappers {
		s.Type = evalsiv1alpha1.StepType_STEP_TYPE_GENERIC
	}
}

// recordInput is what the agent was asked: the root span's input, unless it
// is a framework's state rather than a prompt (CrewAI's empty kickoff inputs,
// a LlamaIndex workflow's start event), in which case the first model call's.
func recordInput(root, firstLLM *evalsiv1alpha1.Step) *evalsiv1alpha1.Content {
	in := root.GetInput()
	if j := in.GetJson(); j != nil {
		obj := j.GetStructValue()
		empty := (obj != nil && len(obj.GetFields()) == 0) || (j.GetListValue() != nil && len(j.GetListValue().GetValues()) == 0)
		if empty || obj.GetFields()["init_state"] != nil {
			in = nil
		}
	}
	if in == nil {
		in = firstLLM.GetInput()
	}
	return in
}

// recordOutput is the agent's answer: the root span's output, read out of a
// CrewAI CrewOutput ({"raw": ..., "tasks_output": ...}); else, when the root
// has none or only a framework's object dump (LlamaIndex's StopEvent(...)),
// the last model call's.
func recordOutput(root, lastLLM *evalsiv1alpha1.Step) *evalsiv1alpha1.Content {
	out := root.GetOutput()
	if f := out.GetJson().GetStructValue().GetFields(); f != nil && f["tasks_output"] != nil {
		if raw := f["raw"].GetStringValue(); raw != "" {
			out = &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: raw}}
		}
	}
	if strings.HasPrefix(out.GetText(), "StopEvent(") {
		out = nil
	}
	if out == nil {
		out = lastLLM.GetOutput()
	}
	return out
}

// requestedTools are the tools the model asked for, for a trace whose
// instrumentation records no tool spans (MLflow's CrewAI autolog, OpenLLMetry
// for CrewAI): the same fallback evaluators use.
func requestedTools(steps []*evalsiv1alpha1.Step) []string {
	var out []string
	for _, s := range steps {
		if s.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			continue
		}
		for _, m := range s.GetOutput().GetMessages().GetMessages() {
			for _, c := range m.GetToolCalls() {
				out = append(out, c.GetName())
			}
		}
	}
	return out
}

// usageOf sums the model calls' tokens. When no model call records any (the
// Claude Agent SDK's OpenInference spans count them on the agent span), the
// outermost step that does is used.
func usageOf(steps []*evalsiv1alpha1.Step, root *evalsiv1alpha1.Step) *evalsiv1alpha1.Usage {
	usage := &evalsiv1alpha1.Usage{}
	for _, s := range steps {
		u := s.GetUsage()
		if s.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_LLM || u == nil {
			continue
		}
		if u.InputTokens != nil {
			usage.InputTokens = proto.Int64(usage.GetInputTokens() + u.GetInputTokens())
		}
		if u.OutputTokens != nil {
			usage.OutputTokens = proto.Int64(usage.GetOutputTokens() + u.GetOutputTokens())
		}
	}
	if usage.InputTokens == nil && usage.OutputTokens == nil {
		for _, s := range append([]*evalsiv1alpha1.Step{root}, steps...) {
			if u := s.GetUsage(); u.InputTokens != nil || u.OutputTokens != nil {
				usage.InputTokens, usage.OutputTokens = u.InputTokens, u.OutputTokens
				break
			}
		}
	}
	return usage
}

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
// generic one, and returns each such wrapper with the model calls inside it.
// A call to a model does not make other model calls, so a model-call span
// with one inside is either a wrapper (LlamaIndex nests a streamed call in
// another, and marks the step that prepares a tool call as a model call) or
// the same call recorded twice (a framework and its model client both
// instrumented). Counting both would count the call twice; the innermost is
// the one the model answered.
func demoteWrappers(steps []*evalsiv1alpha1.Step) map[*evalsiv1alpha1.Step][]*evalsiv1alpha1.Step {
	byID := map[string]*evalsiv1alpha1.Step{}
	for _, s := range steps {
		byID[s.GetSpanId()] = s
	}
	inner := map[*evalsiv1alpha1.Step][]*evalsiv1alpha1.Step{}
	for _, s := range steps {
		if s.GetType() != evalsiv1alpha1.StepType_STEP_TYPE_LLM {
			continue
		}
		seen := map[string]bool{}
		for p := byID[s.GetParentSpanId()]; p != nil && !seen[p.GetSpanId()]; p = byID[p.GetParentSpanId()] {
			seen[p.GetSpanId()] = true
			if p.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM {
				inner[p] = append(inner[p], s)
			}
		}
	}
	for s := range inner {
		s.Type = evalsiv1alpha1.StepType_STEP_TYPE_GENERIC
	}
	return inner
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

// Tools are the tools a trajectory called, for selectors: its tool steps,
// or, when its instrumentation records no tool spans (MLflow's CrewAI
// autolog, OpenLLMetry for CrewAI), the tool calls the model asked for. The
// evaluators fall back the same way.
func Tools(steps []*evalsiv1alpha1.Step) []string {
	var out []string
	for _, s := range steps {
		if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_TOOL {
			out = append(out, s.GetName())
		}
	}
	if len(out) > 0 {
		return out
	}
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

// Models are the models a trajectory's model calls and agents name, once
// each. Agent spans count because the Claude Agent SDK's OpenInference spans
// record no separate model call.
func Models(steps []*evalsiv1alpha1.Step) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range steps {
		if t := s.GetType(); t != evalsiv1alpha1.StepType_STEP_TYPE_LLM && t != evalsiv1alpha1.StepType_STEP_TYPE_AGENT {
			continue
		}
		a := Attrs{}
		for _, k := range modelKeys {
			if v, ok := s.GetAttributes()[k]; ok {
				a[k] = v.AsInterface()
			}
		}
		if m := a.mlflowStr(modelKeys...); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

var modelKeys = []string{"gen_ai.response.model", "gen_ai.request.model", "llm.model_name", "mlflow.llm.model"}

// usageOf sums the model calls' tokens. A wrapper's tokens count when none of
// the calls inside it records any (a streamed call whose usage only the
// framework's span has). When no model call records any (the Claude Agent
// SDK's OpenInference spans count them on the agent span), the outermost step
// that does is used.
func usageOf(steps []*evalsiv1alpha1.Step, root *evalsiv1alpha1.Step, wrappers map[*evalsiv1alpha1.Step][]*evalsiv1alpha1.Step) *evalsiv1alpha1.Usage {
	usage := &evalsiv1alpha1.Usage{}
	add := func(u *evalsiv1alpha1.Usage) {
		if u.InputTokens != nil {
			usage.InputTokens = proto.Int64(usage.GetInputTokens() + u.GetInputTokens())
		}
		if u.OutputTokens != nil {
			usage.OutputTokens = proto.Int64(usage.GetOutputTokens() + u.GetOutputTokens())
		}
	}
	hasTokens := func(s *evalsiv1alpha1.Step) bool {
		return s.GetUsage().InputTokens != nil || s.GetUsage().OutputTokens != nil
	}
	counted := map[*evalsiv1alpha1.Step]bool{}
	for _, s := range steps {
		if s.GetType() == evalsiv1alpha1.StepType_STEP_TYPE_LLM && hasTokens(s) {
			add(s.GetUsage())
			counted[s] = true
		}
	}
	// Outermost wrappers first, so a wrapper inside a wrapper is not counted
	// as well: a wrapper counts only when nothing inside it was.
	for _, s := range steps {
		calls, ok := wrappers[s]
		if !ok || !hasTokens(s) {
			continue
		}
		covered := false
		for _, c := range calls {
			covered = covered || counted[c]
		}
		for w, ws := range wrappers {
			if w != s && counted[w] && contains(ws, calls) {
				covered = true
			}
		}
		if !covered {
			add(s.GetUsage())
			counted[s] = true
		}
	}
	if usage.InputTokens == nil && usage.OutputTokens == nil {
		for _, s := range append([]*evalsiv1alpha1.Step{root}, steps...) {
			if u := s.GetUsage(); u.GetInputTokens() != 0 || u.GetOutputTokens() != 0 {
				usage.InputTokens, usage.OutputTokens = u.InputTokens, u.OutputTokens
				break
			}
		}
	}
	return usage
}

// contains reports whether every step of sub is in set.
func contains(set, sub []*evalsiv1alpha1.Step) bool {
	in := map[*evalsiv1alpha1.Step]bool{}
	for _, s := range set {
		in[s] = true
	}
	for _, s := range sub {
		if !in[s] {
			return false
		}
	}
	return true
}

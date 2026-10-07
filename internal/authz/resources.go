package authz

import (
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Resource attributes for CEL, one builder per kind of resource. The same
// attributes are used for every action on the resource, so a role condition
// such as `resource.target.model == "qwen3"` limits reading a run the same
// way it limits starting one.

func evaluatorRefs(refs []*evalsiv1alpha1.EvaluatorRef) []any {
	out := make([]any, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.GetRef())
	}
	return out
}

// RunsCode reports whether any of the evaluator references runs code in the sandbox.
type RunsCode func(refs []*evalsiv1alpha1.EvaluatorRef) bool

// EvaluateResource describes an Evaluate or EvaluateStream request.
func EvaluateResource(refs []*evalsiv1alpha1.EvaluatorRef, judge string, records int, runsCode RunsCode) map[string]any {
	return map[string]any{
		"evaluators": evaluatorRefs(refs),
		"judge":      judge,
		"runs_code":  runsCode != nil && runsCode(refs),
		"records":    int64(records),
	}
}

// AgentRun reports whether a spec drives an agent through tasks.
func AgentRun(spec *evalsiv1alpha1.RunSpec) bool {
	return spec.GetHarness() != nil || spec.GetTarget().GetAgent() != nil
}

// agentResource describes an agent run: which agent, which harness and the
// environment's sandbox, for rules such as resource.agent.network == "deny".
func agentResource(spec *evalsiv1alpha1.RunSpec) map[string]any {
	if !AgentRun(spec) {
		return map[string]any{"kind": "", "harness": "", "image": "", "network": "", "min_isolation": ""}
	}
	kind := "builtin"
	switch a := spec.GetTarget().GetAgent(); {
	case a.GetA2A() != nil:
		kind = "a2a"
	case a.GetMcp() != nil:
		kind = "mcp"
	case a.GetResponses() != nil:
		kind = "responses"
	case a.GetHttp() != nil:
		kind = "http"
	case a.GetCli() != nil:
		kind = "cli"
	}
	harness := "builtin"
	if spec.GetHarness().GetExternal() != nil {
		harness = "external"
	}
	env := spec.GetEnvironment()
	network := env.GetSandbox().GetNetwork()
	if network == "" {
		network = "deny"
	}
	return map[string]any{
		"kind": kind, "harness": harness, "image": env.GetImage(), "network": network,
		"min_isolation": env.GetSandbox().GetMinIsolation(),
	}
}

// SpecResource describes a run spec and its labels. Agent runs always run
// code (the agent's commands, in the sandbox).
func SpecResource(spec *evalsiv1alpha1.RunSpec, labels map[string]string, runsCode RunsCode) map[string]any {
	t := spec.GetTarget()
	ds := spec.GetDataset()
	return map[string]any{
		"target":     map[string]any{"connector": t.GetConnector(), "model": t.GetModel(), "base_url": t.GetBaseUrl()},
		"agent":      agentResource(spec),
		"judge":      spec.GetJudge(),
		"evaluators": evaluatorRefs(spec.GetEvaluators()),
		"runs_code":  AgentRun(spec) || runsCode != nil && runsCode(spec.GetEvaluators()),
		"dataset":    map[string]any{"path": ds.GetPath(), "uri": ds.GetUri(), "inline": ds.GetInline() != nil},
		"trials":     int64(spec.GetTrials()),
		"budget": map[string]any{
			"max_target_tokens": spec.GetBudget().GetMaxTargetTokens(),
			"max_judge_tokens":  spec.GetBudget().GetMaxJudgeTokens(),
		},
		"labels": StringMap(labels),
	}
}

// RunResource describes a stored run.
func RunResource(run *evalsiv1alpha1.Run, runsCode RunsCode) map[string]any {
	r := SpecResource(run.GetSpec(), run.GetLabels(), runsCode)
	r["run"] = map[string]any{
		"id": run.GetId(), "name": run.GetName(), "created_by": run.GetCreatedBy(),
		"status": run.GetStatus().String(), "labels": StringMap(run.GetLabels()),
	}
	return r
}

// PolicyResource describes an online policy.
func PolicyResource(p *evalsiv1alpha1.OnlineEvalPolicy, runsCode RunsCode) map[string]any {
	var refs []*evalsiv1alpha1.EvaluatorRef
	for _, s := range p.GetStages() {
		refs = append(refs, s.GetEvaluators()...)
	}
	return map[string]any{
		"policy": map[string]any{
			"name": p.GetName(), "selector": p.GetSelector(), "judge": p.GetJudge(),
			"promotion": p.GetPromote().GetDataset(), "labels": StringMap(p.GetLabels()),
		},
		"evaluators": evaluatorRefs(refs),
		"judge":      p.GetJudge(),
		"runs_code":  runsCode != nil && runsCode(refs),
		"labels":     StringMap(p.GetLabels()),
	}
}

// TraceResource describes an ingested trace.
func TraceResource(t *evalsiv1alpha1.TraceSummary) map[string]any {
	return map[string]any{
		"service": t.GetService(),
		"trace":   map[string]any{"id": t.GetTraceId(), "service": t.GetService(), "name": t.GetName()},
		"labels":  StringMap(t.GetLabels()),
	}
}

// IngestResource describes an OTLP resource being ingested.
func IngestResource(service string, labels map[string]string) map[string]any {
	return map[string]any{"service": service, "labels": StringMap(labels)}
}

// QueueResource describes an annotation queue (resource.queue, resource.labels).
func QueueResource(name string, labels map[string]string) map[string]any {
	return map[string]any{"queue": map[string]any{"name": name}, "labels": StringMap(labels)}
}

// GuardrailResource describes a guardrail (resource.guardrail, resource.labels).
func GuardrailResource(name string, labels map[string]string) map[string]any {
	return map[string]any{"guardrail": map[string]any{"name": name}, "labels": StringMap(labels)}
}

package watch

import (
	"cel.dev/cel-go/cel"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// TraceFilter is a CEL condition over stored traces, in the same language as
// policy selectors, for selecting traces to replay.
type TraceFilter struct {
	prog cel.Program
}

// CompileTraceFilter compiles a filter; an empty one matches everything.
func CompileTraceFilter(expr string) (*TraceFilter, error) {
	prog, err := compileBool("filter", expr)
	if err != nil {
		return nil, err
	}
	return &TraceFilter{prog: prog}, nil
}

// Match reports whether a stored trace passes the filter. scores are the
// online scores by metric (see TraceScores).
func (f *TraceFilter) Match(t store.StoredTrace, scores map[string]float64) bool {
	sum, traj := t.Summary, t.Record.GetTrajectory()
	tools, models := []string{}, []string{}
	seen := map[string]bool{}
	attributes, resource := map[string]any{}, map[string]any{}
	for _, s := range traj.GetSteps() {
		switch s.GetType() {
		case evalsiv1alpha1.StepType_STEP_TYPE_TOOL:
			tools = append(tools, s.GetName())
		case evalsiv1alpha1.StepType_STEP_TYPE_LLM:
			if m := s.GetAttributes()["gen_ai.request.model"].GetStringValue(); m != "" && !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
		if s.GetParentSpanId() == "" {
			for k, v := range s.GetAttributes() {
				attributes[k] = v.AsInterface()
			}
		}
	}
	vars := map[string]any{
		"service": sum.GetService(), "name": sum.GetName(), "duration_ms": float64(sum.GetDuration().AsDuration().Microseconds()) / 1000,
		"error": sum.GetError(), "steps": int64(sum.GetSteps()), "tools": tools, "models": models,
		"attributes": attributes, "resource": resource, "scores": scores, "labels": labelsOf(sum.GetLabels()),
	}
	if scores == nil {
		vars["scores"] = map[string]float64{}
	}
	return eval(f.prog, vars)
}

// TraceScores turns a policy's stored results for a trace into scores by
// metric name, as the policy's own conditions see them.
func (e *Engine) TraceScores(policy string, results []*evalsiv1alpha1.EvaluationResult) map[string]float64 {
	e.mu.Lock()
	st := e.policies[policy]
	e.mu.Unlock()
	scores := map[string]float64{}
	for _, r := range results {
		for _, s := range r.GetScores() {
			v, ok := numeric(s)
			if !ok {
				continue
			}
			name := r.GetEvaluator()
			if st != nil {
				for _, stage := range st.c.stages {
					for _, in := range stage.insts {
						if in.Name == r.GetEvaluator() {
							name = evaluation.MetricKey(in, s.GetName())
						}
					}
				}
			}
			scores[name] = v
		}
	}
	return scores
}

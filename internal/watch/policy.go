// Package watch implements the Watch door: online evaluation policies over
// ingested traces (MonitorService), trace queries (TraceService) and the
// Prometheus metrics that make online quality visible on existing dashboards.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

var celEnv = mustEnv()

func mustEnv() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("service", cel.StringType),
		cel.Variable("name", cel.StringType),
		cel.Variable("duration_ms", cel.DoubleType),
		cel.Variable("error", cel.BoolType),
		cel.Variable("steps", cel.IntType),
		cel.Variable("tools", cel.ListType(cel.StringType)),
		cel.Variable("models", cel.ListType(cel.StringType)),
		cel.Variable("attributes", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("resource", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("scores", cel.MapType(cel.StringType, cel.DoubleType)),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.StringType)),
		// Let people write duration_ms > 20000 without 20000.0.
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		panic(err)
	}
	return env
}

// compileBool compiles a CEL expression that must produce a bool. Empty means "always true".
func compileBool(field, expr string) (cel.Program, error) {
	if expr == "" {
		return nil, nil
	}
	ast, issues := celEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("%s: %w", field, issues.Err())
	}
	if ast.OutputType() != types.BoolType {
		return nil, fmt.Errorf("%s must be a boolean expression, not %s", field, ast.OutputType())
	}
	return celEnv.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(100000))
}

// activation is the CEL input for one trace.
func activation(info ingest.TraceInfo, scores map[string]float64) map[string]any {
	if scores == nil {
		scores = map[string]float64{}
	}
	tools, models := info.Tools, info.Models
	if tools == nil {
		tools = []string{}
	}
	if models == nil {
		models = []string{}
	}
	return map[string]any{
		"service": info.Service, "name": info.Name, "duration_ms": info.DurationMS,
		"error": info.Error, "steps": int64(info.Steps), "tools": tools, "models": models,
		"attributes": map[string]any(info.Attributes), "resource": map[string]any(info.Resource),
		"scores": scores, "labels": labelsOf(info.Labels),
	}
}

func labelsOf(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// eval runs a compiled condition; a nil program is true. Evaluation errors
// (for example a missing map key) count as false.
func eval(p cel.Program, vars map[string]any) bool {
	if p == nil {
		return true
	}
	out, _, err := p.Eval(vars)
	if err != nil {
		return false
	}
	b, ok := out.Value().(bool)
	return ok && b
}

var policyName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

type stage struct {
	insts []evaluation.Instance
	when  cel.Program
}

// compiled is a policy ready to run.
type compiled struct {
	policy   *evalsiv1alpha1.OnlineEvalPolicy
	selector cel.Program
	always   []cel.Program
	rate     float64
	stages   []stage
	promote  cel.Program
	window   time.Duration
}

func compile(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy, eng *evaluation.Service) (*compiled, error) {
	if !policyName.MatchString(p.GetName()) {
		return nil, fmt.Errorf("policy name %q must be lowercase letters, digits, '.', '_' or '-'", p.GetName())
	}
	c := &compiled{policy: p, rate: 1, window: 5 * time.Minute}
	var err error
	if c.selector, err = compileBool("selector", p.GetSelector()); err != nil {
		return nil, err
	}
	for i, expr := range p.GetSampling().GetAlways() {
		prog, err := compileBool(fmt.Sprintf("sampling.always[%d]", i), expr)
		if err != nil {
			return nil, err
		}
		if prog != nil {
			c.always = append(c.always, prog)
		}
	}
	if s := p.GetSampling(); s != nil && s.Rate != nil {
		r := s.GetRate()
		if r < 0 || r > 1 || math.IsNaN(r) {
			return nil, fmt.Errorf("sampling.rate must be between 0 and 1")
		}
		c.rate = r
	}
	if len(p.GetStages()) == 0 {
		return nil, fmt.Errorf("a policy needs at least one stage")
	}
	names := map[string]bool{}
	for i, st := range p.GetStages() {
		insts, err := eng.BindFor(ctx, p.GetProject(), st.GetEvaluators(), p.GetJudge())
		if err != nil {
			return nil, fmt.Errorf("stages[%d]: %w", i, err)
		}
		for _, in := range insts {
			if names[in.Name] {
				return nil, fmt.Errorf("evaluator %q appears in more than one stage; give it a distinct name", in.Name)
			}
			if in.Dataset() {
				return nil, fmt.Errorf("%s is dataset-scope and cannot run on single traces", in.Name)
			}
			names[in.Name] = true
		}
		when, err := compileBool(fmt.Sprintf("stages[%d].when", i), st.GetWhen())
		if err != nil {
			return nil, err
		}
		c.stages = append(c.stages, stage{insts: insts, when: when})
	}
	if pr := p.GetPromote(); pr != nil && pr.GetDataset() != "" {
		if !policyName.MatchString(pr.GetDataset()) {
			return nil, fmt.Errorf("promote.dataset %q must be lowercase letters, digits, '.', '_' or '-'", pr.GetDataset())
		}
		if c.promote, err = compileBool("promote.when", pr.GetWhen()); err != nil {
			return nil, err
		}
		if c.promote == nil {
			return nil, fmt.Errorf("promote.when is required; promoting every trace is rarely intended")
		}
	}
	if w := p.GetWindow(); w != nil {
		if w.AsDuration() <= 0 {
			return nil, fmt.Errorf("window must be positive")
		}
		c.window = w.AsDuration()
	}
	for _, a := range p.GetAlerts() {
		if a.GetMetric() == "" || (a.Below == nil && a.Above == nil) {
			return nil, fmt.Errorf("every alert needs a metric and below or above")
		}
	}
	return c, nil
}

// sampled is deterministic per trace id: the same trace always gets the same decision.
func (c *compiled) sampled(traceID string, vars map[string]any) bool {
	for _, a := range c.always {
		if eval(a, vars) {
			return true
		}
	}
	if c.rate >= 1 {
		return true
	}
	// SHA-256 rather than a fast hash: trace ids from some SDKs are sequential,
	// and weaker hashes skew the effective rate for such inputs.
	sum := sha256.Sum256([]byte(traceID))
	return float64(binary.BigEndian.Uint64(sum[:8]))/float64(math.MaxUint64) < c.rate
}

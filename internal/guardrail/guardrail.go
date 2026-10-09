// Package guardrail runs evaluators inline: on a prompt or a tool call on
// its way in, or an answer or a tool result on its way back, it redacts
// what the guardrail's rules match and decides whether the content may pass.
// GuardrailService exposes it over the API; gateway.go speaks agentgateway's
// prompt-guard webhook and its ExtMcp processor protocol.
package guardrail

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
)

// Limits and defaults.
const (
	DefaultTimeout = 2 * time.Second
	MaxTimeout     = 30 * time.Second
	MaxRedactRules = 64
	// MaxContent bounds what one check may carry (bytes over all parts).
	MaxContent = 4 << 20
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// celEnv is built on first use: every evalsid process (sandbox launchers
// included) links this package, and only servers check guardrails.
var celEnv = sync.OnceValue(func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("scores", cel.MapType(cel.StringType, cel.DoubleType)),
		cel.Variable("phase", cel.StringType),
		cel.Variable("source", cel.StringType),
		cel.Variable("method", cel.StringType),
		cel.Variable("tool", cel.StringType),
		cel.Variable("content", cel.StringType),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.StringType)),
		cel.CrossTypeNumericComparisons(true),
		// labels[?"tier"].orValue("") for a label that may be absent.
		cel.OptionalTypes(),
	)
	if err != nil {
		panic(err)
	}
	return env
})

// Input is one piece of traffic to check.
type Input struct {
	Phase evalsiv1alpha1.GuardrailPhase
	// "llm", "mcp" or "api".
	Source string
	// The MCP method and tool, when there is one.
	Method, Tool string
	// Every text the guardrail redacts (the messages of a prompt, the
	// strings of a tool call's arguments).
	Parts []string
	// Evaluators see Parts[Focus:] (joined by newlines) as the record's
	// output: the newest message of a prompt, all of an answer.
	Focus int
	// What came before, as the record's input.
	Context *evalsiv1alpha1.Content
	// Builds Context from the redacted parts instead, so evaluators (and a
	// judge) never see what was redacted.
	MakeContext func(redacted []string) *evalsiv1alpha1.Content
	Labels      map[string]string
}

// Result is a check's outcome, with the redacted parts in Input order.
type Result struct {
	Response *evalsiv1alpha1.CheckResponse
	// Parts after redaction (unchanged in audit mode or on a block).
	Parts []string
	// Message is what a blocked client is told.
	Message string
}

// Pass reports whether content goes through (perhaps redacted).
func (r *Result) Pass() bool {
	return r.Response.GetDecision() != evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_BLOCK
}

// Masked reports whether the content changed.
func (r *Result) Masked() bool {
	return r.Response.GetDecision() == evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_MASK
}

type redactor struct {
	name  string
	re    *regexp.Regexp
	valid func(string) bool
	repl  string
}

// compiled is a guardrail ready to check content.
type compiled struct {
	g       *evalsiv1alpha1.Guardrail
	version int64
	// The credentials policy's version it was checked against.
	grants   uint64
	phases   map[evalsiv1alpha1.GuardrailPhase]bool
	redact   []redactor
	insts    []evaluation.Instance
	block    cel.Program
	timeout  time.Duration
	failOpen bool
	audit    bool
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func luhn(d string) bool {
	total := 0
	for i := range len(d) {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		total += n
	}
	return total%10 == 0
}

// builtins are RE2 versions of the safety pack's detectors; checks that
// RE2 cannot express (Luhn, SSN ranges) run on each match.
var builtins = map[string]redactor{
	"email": {re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	"phone": {re: regexp.MustCompile(`\+\d{1,3}[\s.-]?\(?\d{1,4}\)?(?:[\s.-]?\d{2,4}){2,4}|\(?\b\d{3}\)?[\s.-]\d{3}[\s.-]\d{4}\b`),
		valid: func(m string) bool { n := len(digits(m)); return n >= 10 && n <= 15 }},
	"credit-card": {re: regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`),
		valid: func(m string) bool { d := digits(m); return len(d) >= 13 && len(d) <= 19 && luhn(d) }},
	"us-ssn": {re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: func(m string) bool {
		area, group, serial := m[:3], m[4:6], m[7:]
		return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
	}},
	"ipv4":           {re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)},
	"aws-access-key": {re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	"github-token":   {re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	"api-key":        {re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	"private-key":    {re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----[\s\S]*?(?:-----END (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----|$)`)},
	"jwt":            {re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
}

// Builtins lists the builtin redaction rules.
func Builtins() []string {
	out := make([]string, 0, len(builtins))
	for k := range builtins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func compileRedact(rules []*evalsiv1alpha1.RedactRule) ([]redactor, error) {
	if len(rules) > MaxRedactRules {
		return nil, fmt.Errorf("at most %d redact rules", MaxRedactRules)
	}
	var out []redactor
	for i, r := range rules {
		switch {
		case r.GetBuiltin() != "" && r.GetPattern() != "":
			return nil, fmt.Errorf("redact[%d]: set builtin or pattern, not both", i)
		case r.GetBuiltin() != "":
			b, ok := builtins[r.GetBuiltin()]
			if !ok {
				return nil, fmt.Errorf("redact[%d]: unknown builtin %q; one of %v", i, r.GetBuiltin(), Builtins())
			}
			b.name = r.GetBuiltin()
			b.repl = "<" + strings.ToUpper(strings.ReplaceAll(b.name, "-", "_")) + ">"
			if r.GetReplacement() != "" {
				b.repl = r.GetReplacement()
			}
			out = append(out, b)
		case r.GetPattern() != "":
			re, err := regexp.Compile(r.GetPattern())
			if err != nil {
				return nil, fmt.Errorf("redact[%d]: %w", i, err)
			}
			repl := r.GetReplacement()
			if repl == "" {
				repl = "[REDACTED]"
			}
			out = append(out, redactor{name: r.GetPattern(), re: re, repl: repl})
		default:
			return nil, fmt.Errorf("redact[%d]: set builtin or pattern", i)
		}
	}
	return out, nil
}

// apply redacts one text, counting replacements by rule.
func (r redactor) apply(text string, counts map[string]int64) string {
	return r.re.ReplaceAllStringFunc(text, func(m string) string {
		if r.valid != nil && !r.valid(m) {
			return m
		}
		counts[r.name]++
		return r.repl
	})
}

// compile validates a guardrail and prepares it. eval binds its evaluators.
func compile(ctx context.Context, g *evalsiv1alpha1.Guardrail, eval *evaluation.Service) (*compiled, error) {
	if !nameRE.MatchString(g.GetName()) {
		return nil, fmt.Errorf("guardrail name %q must be lowercase letters, digits, '.', '_' or '-'", g.GetName())
	}
	c := &compiled{g: g, version: g.GetUpdatedAt().AsTime().UnixNano(), timeout: DefaultTimeout,
		phases: map[evalsiv1alpha1.GuardrailPhase]bool{}}
	for _, p := range g.GetPhases() {
		if p == evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_UNSPECIFIED {
			return nil, errors.New("phases: unspecified phase")
		}
		c.phases[p] = true
	}
	var err error
	if c.redact, err = compileRedact(g.GetRedact()); err != nil {
		return nil, err
	}
	if len(g.GetEvaluators()) > 0 {
		if c.insts, err = eval.BindFor(ctx, g.GetProject(), g.GetEvaluators(), g.GetJudge()); err != nil {
			return nil, err
		}
		for _, in := range c.insts {
			if in.Dataset() {
				return nil, fmt.Errorf("%s is dataset-scope and cannot check single pieces of content", in.Name)
			}
		}
	}
	if len(c.redact) == 0 && len(c.insts) == 0 {
		return nil, errors.New("a guardrail needs redact rules, evaluators, or both")
	}
	if expr := g.GetBlockWhen(); expr != "" {
		if len(c.insts) == 0 {
			return nil, errors.New("block_when needs evaluators")
		}
		ast, issues := celEnv().Compile(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("block_when: %w", issues.Err())
		}
		if ast.OutputType() != types.BoolType {
			return nil, fmt.Errorf("block_when must be a boolean expression, not %s", ast.OutputType())
		}
		if c.block, err = celEnv().Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(100000)); err != nil {
			return nil, fmt.Errorf("block_when: %w", err)
		}
	}
	if t := g.GetTimeout(); t != nil {
		d := t.AsDuration()
		if d <= 0 || d > MaxTimeout {
			return nil, fmt.Errorf("timeout must be positive and at most %s", MaxTimeout)
		}
		c.timeout = d
	}
	c.failOpen = g.GetFailureMode() == evalsiv1alpha1.GuardrailFailureMode_GUARDRAIL_FAILURE_MODE_OPEN
	c.audit = g.GetMode() == evalsiv1alpha1.GuardrailMode_GUARDRAIL_MODE_AUDIT
	return c, nil
}

func phaseName(p evalsiv1alpha1.GuardrailPhase) string {
	if p == evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_RESPONSE {
		return "response"
	}
	return "request"
}

func scoreValue(s *evalsiv1alpha1.Score) (float64, bool) {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number, true
	case *evalsiv1alpha1.Score_Passed:
		if v.Passed {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// check runs the guardrail over one input.
func (c *compiled) check(ctx context.Context, eval *evaluation.Service, in Input) *Result {
	start := time.Now()
	resp := &evalsiv1alpha1.CheckResponse{Redactions: map[string]int64{}, Scores: map[string]float64{}}
	res := &Result{Response: resp, Parts: in.Parts, Message: c.g.GetMessage()}
	if res.Message == "" {
		res.Message = fmt.Sprintf("blocked by guardrail %s", c.g.GetName())
	}
	defer func() { resp.Duration = durationpb.New(time.Since(start)) }()
	if len(c.phases) > 0 && !c.phases[in.Phase] {
		resp.Decision, resp.Verdict = evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS, evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS
		resp.Reason = "phase " + phaseName(in.Phase) + " is not checked"
		return res
	}

	redacted := make([]string, len(in.Parts))
	for i, p := range in.Parts {
		for _, r := range c.redact {
			p = r.apply(p, resp.Redactions)
		}
		redacted[i] = p
	}
	verdict := evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS
	if len(resp.Redactions) > 0 {
		verdict = evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_MASK
	}
	var reasons []string
	if len(c.insts) > 0 {
		focus := min(max(in.Focus, 0), len(redacted))
		content := strings.Join(redacted[focus:], "\n")
		meta := map[string]*structpb.Value{"guardrail.source": structpb.NewStringValue(in.Source)}
		if in.Method != "" {
			meta["mcp.method"] = structpb.NewStringValue(in.Method)
		}
		if in.Tool != "" {
			meta["mcp.tool"] = structpb.NewStringValue(in.Tool)
		}
		ctxContent := in.Context
		if in.MakeContext != nil {
			ctxContent = in.MakeContext(redacted)
		}
		rec := &evalsiv1alpha1.Record{Id: "check", Output: &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: content}},
			Input: ctxContent, Metadata: meta}
		resp.Record = rec
		blocked, why, err := c.evaluate(ctx, eval, rec, in, content, resp)
		switch {
		case err != nil:
			resp.Failed = true
			if c.failOpen {
				reasons = append(reasons, fmt.Sprintf("evaluation failed (%v); failing open", err))
			} else {
				verdict = evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_BLOCK
				reasons = append(reasons, fmt.Sprintf("evaluation failed (%v); failing closed", err))
			}
		case blocked:
			verdict = evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_BLOCK
			reasons = append(reasons, why)
		}
	}
	if len(resp.Redactions) > 0 {
		names := make([]string, 0, len(resp.Redactions))
		for k, n := range resp.Redactions {
			names = append(names, fmt.Sprintf("%s x%d", k, n))
		}
		sort.Strings(names)
		reasons = append(reasons, "redacted "+strings.Join(names, ", "))
	}
	resp.Verdict = verdict
	resp.Reason = strings.Join(reasons, "; ")
	switch {
	case c.audit:
		resp.Decision = evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS
		if verdict != evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS {
			resp.Reason = "audit: would " + strings.ToLower(strings.TrimPrefix(verdict.String(), "GUARDRAIL_DECISION_")) + ": " + resp.Reason
		}
	default:
		resp.Decision = verdict
		if verdict == evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_MASK {
			res.Parts = redacted
			resp.Content = strings.Join(redacted, "\n")
		}
	}
	return res
}

// evaluate runs the evaluators and decides whether they block.
func (c *compiled) evaluate(ctx context.Context, eval *evaluation.Service, rec *evalsiv1alpha1.Record, in Input, content string, resp *evalsiv1alpha1.CheckResponse) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	results, err := eval.RunRecords(ctx, c.insts, []*evalsiv1alpha1.Record{rec})
	if err != nil {
		if ctx.Err() != nil {
			return false, "", fmt.Errorf("timed out after %s", c.timeout)
		}
		return false, "", err
	}
	resp.Results = results
	insts := map[string]evaluation.Instance{}
	for _, inst := range c.insts {
		insts[inst.Name] = inst
	}
	var failed []string
	for _, r := range results {
		if r.GetOutcome() == evalsiv1alpha1.Outcome_OUTCOME_ERROR {
			return false, "", fmt.Errorf("%s: %s", r.GetEvaluator(), r.GetReason())
		}
		for _, s := range r.GetScores() {
			key := evaluation.MetricKey(insts[r.GetEvaluator()], s.GetName())
			v, ok := scoreValue(s)
			if !ok {
				continue
			}
			resp.Scores[key] = v
			if p, isPF := s.GetValue().(*evalsiv1alpha1.Score_Passed); isPF && !p.Passed {
				msg := key
				if s.GetExplanation() != "" {
					msg += ": " + s.GetExplanation()
				}
				failed = append(failed, msg)
			}
		}
	}
	sort.Strings(failed)
	if c.block == nil {
		if len(failed) > 0 {
			return true, strings.Join(failed, "; "), nil
		}
		return false, "", nil
	}
	labels := in.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out, _, err := c.block.Eval(map[string]any{
		"scores": resp.Scores, "phase": phaseName(in.Phase), "source": in.Source,
		"method": in.Method, "tool": in.Tool, "content": content, "labels": labels,
	})
	if err != nil {
		// A missing score (an evaluator skipped this content) or label:
		// the guardrail cannot decide, so failure_mode does.
		return false, "", fmt.Errorf("block_when: %w", err)
	}
	if b, ok := out.Value().(bool); ok && b {
		why := "block_when matched"
		if len(failed) > 0 {
			why += ": " + strings.Join(failed, "; ")
		}
		return true, why, nil
	}
	return false, "", nil
}

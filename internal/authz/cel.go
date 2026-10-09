package authz

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// The variables rules, role conditions and cel: subjects see. They follow
// agentgateway's names.
var celEnv = func() *cel.Env {
	dynMap := cel.MapType(cel.StringType, cel.DynType)
	env, err := cel.NewEnv(
		cel.Variable("jwt", dynMap),
		cel.Variable("apiKey", dynMap),
		cel.Variable("principal", dynMap),
		cel.Variable("request", dynMap),
		cel.Variable("resource", dynMap),
		cel.Variable("source", dynMap),
		cel.Variable("mcp", dynMap),
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		panic(err)
	}
	return env
}()

// Compile compiles a boolean CEL expression over the authorization variables.
func Compile(expr string) (cel.Program, error) {
	ast, issues := celEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if ast.OutputType() != types.BoolType && ast.OutputType() != types.DynType {
		return nil, fmt.Errorf("must be a boolean expression, not %s", ast.OutputType())
	}
	return celEnv.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(100000))
}

// holds evaluates a condition. As in agentgateway, an expression that fails
// to evaluate (for example, a missing attribute) does not hold.
func holds(p cel.Program, vars map[string]any) bool {
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

// conjuncts splits a condition into its normalized top-level && operands, so
// "a && (b && c)" and "c && a && b" compare equal as sets.
func conjuncts(expr string) ([]string, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, nil
	}
	ast, issues := celEnv.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	native := ast.NativeRep()
	var out []string
	var walk func(e celast.Expr) error
	walk = func(e celast.Expr) error {
		if e.Kind() == celast.CallKind && e.AsCall().FunctionName() == operators.LogicalAnd {
			for _, a := range e.AsCall().Args() {
				if err := walk(a); err != nil {
					return err
				}
			}
			return nil
		}
		s, err := parser.Unparse(e, native.SourceInfo())
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	}
	if err := walk(native.Expr()); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// redactedHeaders are never visible to rules.
var redactedHeaders = map[string]bool{"authorization": true, "cookie": true, "x-api-key": true, "proxy-authorization": true}

// vars builds the CEL activation for a request.
func vars(p *auth.Principal, req Request, roles []string) map[string]any {
	jwt := map[string]any{}
	if p.Method == auth.MethodJWT {
		for k, v := range p.Claims {
			jwt[k] = v
		}
	}
	apiKey := map[string]any{}
	if p.Kind == auth.KindAPIKey {
		labels := map[string]any{}
		for k, v := range p.Labels {
			labels[k] = v
		}
		apiKey = map[string]any{"name": p.KeyName, "labels": labels}
	}
	principal := p.Vars()
	rs := make([]any, 0, len(roles))
	for _, r := range roles {
		rs = append(rs, r)
	}
	principal["roles"] = rs
	headers := map[string]any{}
	for k, v := range req.Headers {
		k = strings.ToLower(k)
		if !redactedHeaders[k] {
			headers[k] = strings.Join(v, ",")
		}
	}
	resource := map[string]any{}
	for k, v := range req.Resource {
		resource[k] = v
	}
	resource["project"] = req.Project
	if _, ok := resource["labels"]; !ok {
		resource["labels"] = map[string]any{}
	}
	source := map[string]any{"address": req.Source}
	if host, port, err := net.SplitHostPort(req.Source); err == nil {
		source["ip"], source["port"] = host, port
	}
	mcp := map[string]any{}
	for k, v := range req.MCP {
		mcp[k] = v
	}
	return map[string]any{
		"jwt": jwt, "apiKey": apiKey, "principal": principal, "resource": resource, "source": source, "mcp": mcp,
		"request": map[string]any{
			"action": req.Action, "method": req.Procedure, "protocol": req.Protocol, "headers": headers,
		},
	}
}

// StringMap converts labels for CEL.
func StringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

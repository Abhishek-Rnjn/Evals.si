package authz

import (
	"context"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// RoleTrace is one role the principal holds for the request and whether it grants the action.
type RoleTrace struct {
	Role, Scope, Via string
	Grants           bool
}

// RuleTrace is one global rule's outcome.
type RuleTrace struct {
	Kind, Expr string
	// "matched", "not matched", "holds" or "does not hold".
	Result string
}

// Explanation is Decide, showing its work, for `evalsid auth check`.
type Explanation struct {
	Roles    []RoleTrace
	Rules    []RuleTrace
	Decision Decision
}

// Explain evaluates every rule and role (without short-circuiting) and the
// final decision, as agentgateway's rule tracing does.
func (e *Engine) Explain(ctx context.Context, p *auth.Principal, req Request) Explanation {
	var ex Explanation
	ex.Decision = e.Decide(ctx, p, req)
	if !e.enabled {
		return ex
	}
	if perm, ok := Lookup(req.Action); ok && perm.InstallWide {
		req.Project = ""
	}
	s := e.snap.Load()
	hs := e.heldRoles(s, p, req.Project, vars(p, req, nil))
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.role)
	}
	v := vars(p, req, names)
	for _, h := range hs {
		ex.Roles = append(ex.Roles, RoleTrace{Role: h.role, Scope: h.scope, Via: h.via,
			Grants: s.allows(roleScope(h), h.role, req.Action, v, map[*compiledRole]bool{})})
	}
	for _, r := range e.rules {
		ok := holds(r.prog, v)
		res := map[bool]string{true: "matched", false: "not matched"}[ok]
		if r.kind == "require" {
			res = map[bool]string{true: "holds", false: "does not hold"}[ok]
		}
		ex.Rules = append(ex.Rules, RuleTrace{Kind: r.kind, Expr: r.expr, Result: res})
	}
	return ex
}

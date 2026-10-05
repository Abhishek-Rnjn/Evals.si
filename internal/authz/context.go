package authz

import (
	"context"
	"slices"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// Checker answers authorization questions for one request. The enforcement
// point attaches it to the request context; services use it to filter list
// results and to check resources they load themselves.
type Checker struct {
	Engine    *Engine
	Principal *auth.Principal
	// Request metadata copied into every check (procedure, protocol, headers, source).
	Meta Request
	// Whether evaluators run code, for resource attributes.
	RunsCode RunsCode
	// The enforcement point's decision for the call, for services that write
	// their own (more detailed) audit events.
	Decision Decision
}

// RunsCodeFrom returns the request's runs_code function, or nil.
func RunsCodeFrom(ctx context.Context) RunsCode {
	if c := CheckerFrom(ctx); c != nil {
		return c.RunsCode
	}
	return nil
}

type checkerKey struct{}

// WithChecker attaches a checker to a context.
func WithChecker(ctx context.Context, c *Checker) context.Context {
	return context.WithValue(ctx, checkerKey{}, c)
}

// CheckerFrom returns the request's checker, or nil for internal calls.
func CheckerFrom(ctx context.Context) *Checker {
	c, _ := ctx.Value(checkerKey{}).(*Checker)
	return c
}

// Can reports whether the request's principal may perform action on a
// resource in project. Without a checker (internal calls) it allows.
func Can(ctx context.Context, action, project string, resource map[string]any) bool {
	c := CheckerFrom(ctx)
	if c == nil {
		return true
	}
	req := c.Meta
	req.Action, req.Project, req.Resource = action, project, resource
	return c.Engine.Allowed(ctx, c.Principal, req)
}

// Projects narrows list queries: the projects in which the principal might
// perform action, before per-item checks. nil means "do not narrow" (auth
// is off, the caller is an owner, or allow rules or an external authorizer
// could grant it anywhere).
func Projects(ctx context.Context, action string) []string {
	c := CheckerFrom(ctx)
	if c == nil {
		return nil
	}
	e := c.Engine
	if !e.enabled || e.ext != nil || e.IsOwner(c.Principal) {
		return nil
	}
	for _, r := range e.rules {
		if r.kind == "allow" {
			return nil
		}
	}
	s := e.snap.Load()
	out := []string{}
	for name := range s.projects {
		v := vars(c.Principal, Request{Project: name}, nil)
		for _, h := range e.heldRoles(s, c.Principal, name, v) {
			granted := false
			for _, g := range s.grants(roleScope(h), h.role, nil, map[*compiledRole]bool{}) {
				if matches(g.pattern, action) {
					granted = true
					break
				}
			}
			if granted {
				out = append(out, name)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

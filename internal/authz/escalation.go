package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// grant is one permission pattern a role grants, under the conjunction of
// the conditions on its inheritance chain.
type grant struct {
	pattern string
	conds   []string
}

// grants flattens a role into permission patterns with the conditions that guard them.
func (s *snapshot) grants(scope, name string, conds []string, seen map[*compiledRole]bool) []grant {
	r := s.resolve(scope, name)
	if r == nil || seen[r] {
		return nil
	}
	seen[r] = true
	return s.grantsOf(r, conds, seen)
}

func (s *snapshot) grantsOf(r *compiledRole, conds []string, seen map[*compiledRole]bool) []grant {
	all := append(slices.Clone(conds), r.conjuncts...)
	slices.Sort(all)
	all = slices.Compact(all)
	var out []grant
	for _, p := range r.Permissions {
		out = append(out, grant{pattern: p, conds: all})
	}
	for _, in := range r.Inherits {
		out = append(out, s.grants(r.Project, in, all, seen)...)
	}
	return out
}

// covers reports whether a held grant lets its holder hand out a new grant
// of one action: the action matches and every condition on the held grant
// is also a condition on the new one, so the new grant is at least as strict.
func covers(held, next grant, action string) bool {
	if !matches(held.pattern, action) {
		return false
	}
	for _, c := range held.conds {
		if !slices.Contains(next.conds, c) {
			return false
		}
	}
	return true
}

// EscalationError explains why a grant was refused.
type EscalationError struct{ Missing []string }

func (e *EscalationError) Error() string {
	return "privilege escalation: you would grant permissions you do not hold (or hold only under stricter conditions) here: " +
		strings.Join(e.Missing, ", ")
}

// CheckRole checks that p may define (or bind, or put on an API key) a role
// with this definition in a project ("*" for install-wide). Owners may grant
// anything. Everyone else may only grant permissions they hold in that
// project, under conditions at least as strict as their own.
func (e *Engine) CheckRole(p *auth.Principal, project string, def Role) error {
	if e.IsOwner(p) {
		return nil
	}
	if project == "*" || project == "" {
		return errors.New("only an owner can grant roles in every project or define install-wide roles")
	}
	s := e.snap.Load()
	cr := &compiledRole{Role: def}
	if def.Condition != "" {
		var err error
		if cr.conjuncts, err = conjuncts(def.Condition); err != nil {
			return fmt.Errorf("condition: %w", err)
		}
	}
	return e.check(s, p, project, s.grantsOf(cr, nil, map[*compiledRole]bool{cr: true}))
}

// CheckRoleNames checks that p may grant existing roles in a project, as
// when binding them or putting them on an API key.
func (e *Engine) CheckRoleNames(p *auth.Principal, project string, roles []string) error {
	if e.IsOwner(p) {
		return nil
	}
	if project == "*" || project == "" {
		return errors.New("only an owner can grant roles in every project")
	}
	s := e.snap.Load()
	var next []grant
	for _, name := range roles {
		if name == OwnerRole {
			return errors.New("only an owner can grant owner")
		}
		if s.resolve(project, name) == nil {
			return fmt.Errorf("unknown role %q", name)
		}
		next = append(next, s.grants(project, name, nil, map[*compiledRole]bool{})...)
	}
	return e.check(s, p, project, next)
}

func (e *Engine) check(s *snapshot, p *auth.Principal, project string, next []grant) error {
	var have []grant
	for _, h := range e.heldRoles(s, p, project, vars(p, Request{Project: project}, nil)) {
		have = append(have, s.grants(roleScope(h), h.role, nil, map[*compiledRole]bool{})...)
	}
	var missing []string
	for _, g := range next {
		for _, action := range expand([]string{g.pattern}) {
			ok := false
			for _, h := range have {
				if covers(h, g, action) {
					ok = true
					break
				}
			}
			if !ok && !slices.Contains(missing, action) {
				missing = append(missing, action)
			}
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return &EscalationError{Missing: missing}
	}
	return nil
}

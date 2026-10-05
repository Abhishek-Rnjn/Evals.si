package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"cel.dev/cel-go/cel"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// Store holds roles, bindings and projects created through AuthService.
type Store interface {
	AuthRoles(ctx context.Context) ([]Role, error)
	AuthBindings(ctx context.Context) ([]Binding, error)
	AuthProjects(ctx context.Context) ([]Project, error)
}

// Request is one authorization question.
type Request struct {
	Action string
	// The project the resource belongs to; empty for install-wide actions.
	Project string
	// Resource attributes for CEL (the action table's columns, and labels).
	Resource map[string]any
	// The RPC ("/evalsi.v1alpha1.RunService/CreateRun") or HTTP route.
	Procedure string
	// "grpc", "grpc-web", "connect", "rest" or "http".
	Protocol string
	Headers  http.Header
	// The peer address.
	Source string
}

// Decision is an authorization outcome and what decided it.
type Decision struct {
	Allowed bool
	Reason  string
}

func allow(format string, args ...any) Decision {
	return Decision{Allowed: true, Reason: fmt.Sprintf(format, args...)}
}

func deny(format string, args ...any) Decision {
	return Decision{Reason: fmt.Sprintf(format, args...)}
}

type compiledRule struct {
	kind, expr string
	prog       cel.Program
}

type subject struct {
	raw, kind, provider, value string
	prog                       cel.Program
}

func compileSubject(s string) (subject, error) {
	if err := ValidSubject(s); err != nil {
		return subject{}, err
	}
	kind, rest, _ := strings.Cut(s, ":")
	sub := subject{raw: s, kind: kind, value: rest}
	switch kind {
	case "user", "group":
		sub.provider, sub.value, _ = strings.Cut(rest, "/")
	case "cel":
		prog, err := Compile(rest)
		if err != nil {
			return subject{}, fmt.Errorf("subject %q: %w", s, err)
		}
		sub.prog = prog
	}
	return sub, nil
}

func (s subject) matches(p *auth.Principal, v map[string]any) bool {
	if p.Anonymous() {
		return s.kind == "cel" && holds(s.prog, v)
	}
	switch s.kind {
	case "user":
		return p.Kind != auth.KindAPIKey && p.Provider == s.provider && p.Subject == s.value
	case "group":
		return p.Kind != auth.KindAPIKey && p.Provider == s.provider && slices.Contains(p.Groups, s.value)
	case "email":
		return p.EmailVerified && strings.EqualFold(p.Email, s.value)
	case "key":
		return p.Kind == auth.KindAPIKey && p.KeyName == s.value
	case "cel":
		return holds(s.prog, v)
	}
	return false
}

type compiledRole struct {
	Role
	cond      cel.Program
	conjuncts []string
}

type roleKey struct{ project, name string }

type compiledBinding struct {
	Binding
	subj subject
}

type snapshot struct {
	roles    map[roleKey]*compiledRole
	bindings []compiledBinding
	projects map[string]Project
}

// resolve finds a role by name as seen from a scope: the scope project's own
// roles, then install-wide custom roles, then the built-in roles.
func (s *snapshot) resolve(scope, name string) *compiledRole {
	if scope != "" && scope != "*" {
		if r := s.roles[roleKey{scope, name}]; r != nil {
			return r
		}
	}
	return s.roles[roleKey{"", name}]
}

// Engine decides requests. It is safe for concurrent use; Reload swaps in
// roles, bindings and projects changed through AuthService.
type Engine struct {
	enabled bool
	rbac    RBACConfig
	rules   []compiledRule
	owners  []subject
	// Roles mapped from token claims, validated against the roles at startup.
	ext   ExtAuthorizer
	store Store
	log   *slog.Logger

	reloadMu sync.Mutex
	snap     atomic.Pointer[snapshot]
}

// Options configures an Engine.
type Options struct {
	// When false every request is allowed (auth: {mode: none}, or a
	// loopback server without an auth section).
	Enabled       bool
	RBAC          RBACConfig
	Authorization AuthorizationConfig
	// Role names token claims may map to (from every provider's role_claims),
	// checked to exist.
	ClaimRoles []string
	Store      Store
	Logger     *slog.Logger
	// External authorization; nil to build one from Authorization.ExtAuthz.
	Ext ExtAuthorizer
}

// NewEngine compiles the config and loads stored roles, bindings and projects.
func NewEngine(ctx context.Context, opts Options) (*Engine, error) {
	if err := opts.RBAC.Validate(); err != nil {
		return nil, err
	}
	if err := opts.Authorization.Validate(); err != nil {
		return nil, err
	}
	e := &Engine{enabled: opts.Enabled, rbac: opts.RBAC, store: opts.Store, log: opts.Logger, ext: opts.Ext}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	var errs []error
	for i, r := range opts.Authorization.Rules {
		kind, expr := r.kind()
		prog, err := Compile(expr)
		if err != nil {
			errs = append(errs, fmt.Errorf("authorization.rules[%d]: %w", i, err))
			continue
		}
		e.rules = append(e.rules, compiledRule{kind: kind, expr: expr, prog: prog})
	}
	for _, o := range opts.RBAC.Owners {
		s, err := compileSubject(o)
		if err != nil {
			errs = append(errs, fmt.Errorf("rbac.owners: %w", err))
			continue
		}
		e.owners = append(e.owners, s)
	}
	if e.ext == nil && opts.Authorization.ExtAuthz != nil {
		ext, err := NewExtAuthorizer(*opts.Authorization.ExtAuthz)
		if err != nil {
			errs = append(errs, err)
		}
		e.ext = ext
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := e.Reload(ctx); err != nil {
		return nil, err
	}
	snap := e.snap.Load()
	for _, r := range opts.ClaimRoles {
		if snap.resolve("", r) == nil {
			// Claim roles may name project roles; those are checked when used.
			found := false
			for k := range snap.roles {
				if k.name == r {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("role_claims: unknown role %q", r)
			}
		}
	}
	return e, nil
}

// Enabled reports whether requests are authorized at all.
func (e *Engine) Enabled() bool { return e.enabled }

// Reload rebuilds the snapshot from config and the store. Config mistakes
// are errors; stored entries were validated when written, and any that no
// longer fit (say, a config role they inherit was removed) are logged and skipped.
func (e *Engine) Reload(ctx context.Context) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	var stored []Role
	var bindings []Binding
	var projects []Project
	if e.store != nil {
		var err error
		if stored, err = e.store.AuthRoles(ctx); err != nil {
			return err
		}
		if bindings, err = e.store.AuthBindings(ctx); err != nil {
			return err
		}
		if projects, err = e.store.AuthProjects(ctx); err != nil {
			return err
		}
	}
	snap := &snapshot{roles: map[roleKey]*compiledRole{}, projects: map[string]Project{
		DefaultProject: {Name: DefaultProject, Source: SourceBuiltin, Description: "Requests that name no project."},
	}}
	for project := range e.rbac.Projects {
		if project != "*" {
			snap.projects[project] = Project{Name: project, Source: SourceConfig}
		}
	}
	for _, p := range projects {
		if _, ok := snap.projects[p.Name]; !ok {
			p.Source = SourceAPI
			snap.projects[p.Name] = p
		}
	}
	for _, r := range BuiltinRoles {
		r.Source = SourceBuiltin
		snap.roles[roleKey{"", r.Name}] = &compiledRole{Role: r}
	}
	var errs []error
	for _, r := range e.rbac.Roles {
		r.Source = SourceConfig
		if err := snap.addRole(r); err != nil {
			errs = append(errs, fmt.Errorf("rbac.roles %q: %w", r.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, r := range stored {
		r.Source = SourceAPI
		if err := snap.addRole(r); err != nil {
			e.log.Warn("skipping stored role", "role", r.Name, "project", r.Project, "err", err)
		}
	}
	if err := snap.checkRoles(); err != nil {
		return err
	}
	for project, roles := range e.rbac.Projects {
		for role, subjects := range roles {
			for _, s := range subjects {
				b := Binding{Project: project, Role: role, Subject: s, Source: SourceConfig}
				if err := snap.addBinding(b); err != nil {
					errs = append(errs, fmt.Errorf("rbac.projects.%s.%s: %w", project, role, err))
				}
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, b := range bindings {
		b.Source = SourceAPI
		if err := snap.addBinding(b); err != nil {
			e.log.Warn("skipping stored binding", "binding", b, "err", err)
		}
	}
	sort.SliceStable(snap.bindings, func(i, j int) bool {
		a, b := snap.bindings[i], snap.bindings[j]
		return a.Project+"\x00"+a.Role+"\x00"+a.Subject < b.Project+"\x00"+b.Role+"\x00"+b.Subject
	})
	e.snap.Store(snap)
	return nil
}

// addRole validates a custom role against what is already in the snapshot
// (inherited roles are checked by checkRoles, once all are in).
func (s *snapshot) addRole(r Role) error {
	if err := validateRoleShape(r); err != nil {
		return err
	}
	if r.Project != "" {
		if _, ok := s.projects[r.Project]; !ok {
			return fmt.Errorf("unknown project %q", r.Project)
		}
		if s.roles[roleKey{"", r.Name}] != nil {
			return fmt.Errorf("an install-wide or built-in role is already named %q", r.Name)
		}
	} else {
		for k := range s.roles {
			if k.name == r.Name {
				return fmt.Errorf("a role named %q already exists", r.Name)
			}
		}
	}
	if s.roles[roleKey{r.Project, r.Name}] != nil {
		return fmt.Errorf("duplicate role %q", r.Name)
	}
	cr := &compiledRole{Role: r}
	if r.Condition != "" {
		var err error
		if cr.cond, err = Compile(r.Condition); err != nil {
			return fmt.Errorf("condition: %w", err)
		}
		if cr.conjuncts, err = conjuncts(r.Condition); err != nil {
			return fmt.Errorf("condition: %w", err)
		}
	}
	s.roles[roleKey{r.Project, r.Name}] = cr
	return nil
}

// validateRoleShape checks what can be checked without other roles.
func validateRoleShape(r Role) error {
	var errs []error
	if !ValidRoleName(r.Name) {
		errs = append(errs, fmt.Errorf("invalid role name %q", r.Name))
	}
	if _, ok := builtin(r.Name); ok {
		errs = append(errs, fmt.Errorf("%q is a built-in role; define a new role that inherits it", r.Name))
	}
	if len(r.Permissions) == 0 && len(r.Inherits) == 0 {
		errs = append(errs, errors.New("a role needs permissions or inherits"))
	}
	for _, p := range r.Permissions {
		if !validPattern(p) {
			errs = append(errs, fmt.Errorf("unknown permission %q", p))
			continue
		}
		if perm, ok := Lookup(p); ok && perm.InstallWide {
			errs = append(errs, fmt.Errorf("%q is reserved for the owner role", p))
		}
	}
	for _, in := range r.Inherits {
		if in == OwnerRole {
			errs = append(errs, errors.New("roles cannot inherit owner"))
		}
	}
	return errors.Join(errs...)
}

// checkRoles verifies that every inherited role exists in scope and that
// inheritance has no cycles.
func (s *snapshot) checkRoles() error {
	var errs []error
	state := map[*compiledRole]int{} // 1 visiting, 2 done
	var visit func(r *compiledRole, path []string) error
	visit = func(r *compiledRole, path []string) error {
		switch state[r] {
		case 1:
			return fmt.Errorf("inheritance cycle: %s", strings.Join(append(path, r.Name), " -> "))
		case 2:
			return nil
		}
		state[r] = 1
		for _, in := range r.Inherits {
			parent := s.resolve(r.Project, in)
			if parent == nil {
				return fmt.Errorf("role %q inherits unknown role %q", r.Name, in)
			}
			if err := visit(parent, append(path, r.Name)); err != nil {
				return err
			}
		}
		state[r] = 2
		return nil
	}
	keys := make([]roleKey, 0, len(s.roles))
	for k := range s.roles {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].project+"/"+keys[i].name < keys[j].project+"/"+keys[j].name })
	for _, k := range keys {
		if err := visit(s.roles[k], nil); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}

func (s *snapshot) addBinding(b Binding) error {
	if b.Project != "*" {
		if _, ok := s.projects[b.Project]; !ok {
			return fmt.Errorf("unknown project %q", b.Project)
		}
	}
	if b.Role == OwnerRole && b.Project != "*" {
		return errors.New("owner can only be bound in every project (\"*\")")
	}
	r := s.resolve(b.Project, b.Role)
	if r == nil {
		return fmt.Errorf("unknown role %q", b.Role)
	}
	sub, err := compileSubject(b.Subject)
	if err != nil {
		return err
	}
	s.bindings = append(s.bindings, compiledBinding{Binding: b, subj: sub})
	return nil
}

// held is a role a principal holds and how.
type held struct {
	role  string
	scope string // the project, or "*"
	via   string
}

// heldRoles lists the roles a principal holds for a project ("" for install-wide actions).
func (e *Engine) heldRoles(s *snapshot, p *auth.Principal, project string, v map[string]any) []held {
	var out []held
	seen := map[string]bool{}
	add := func(h held) {
		k := h.role + "\x00" + h.scope
		if !seen[k] {
			seen[k] = true
			out = append(out, h)
		}
	}
	for _, o := range e.owners {
		if o.matches(p, v) {
			add(held{role: OwnerRole, scope: "*", via: "rbac.owners " + o.raw})
		}
	}
	for _, b := range s.bindings {
		if (b.Project == "*" || (project != "" && b.Project == project)) && b.subj.matches(p, v) {
			add(held{role: b.Role, scope: b.Project, via: "binding " + b.Subject})
		}
	}
	for _, src := range []struct {
		m   map[string][]string
		via string
	}{{p.KeyRoles, "API key " + p.KeyName}, {p.ClaimRoles, "token role claim"}} {
		for _, scope := range []string{project, "*"} {
			if scope == "" {
				continue
			}
			for _, r := range src.m[scope] {
				if r == OwnerRole && scope != "*" {
					continue // owner is install-wide or nothing
				}
				add(held{role: r, scope: scope, via: src.via})
			}
		}
	}
	return out
}

// allows reports whether a role grants an action under the given variables.
func (s *snapshot) allows(scope, name, action string, v map[string]any, seen map[*compiledRole]bool) bool {
	r := s.resolve(scope, name)
	if r == nil || seen[r] {
		return false
	}
	seen[r] = true
	if !holds(r.cond, v) {
		return false
	}
	if r.Source == SourceBuiltin && r.Name == OwnerRole {
		return true
	}
	for _, pat := range r.Permissions {
		if matches(pat, action) {
			return true
		}
	}
	for _, in := range r.Inherits {
		if s.allows(r.Project, in, action, v, seen) {
			return true
		}
	}
	return false
}

func roleScope(h held) string {
	if h.scope == "*" {
		return ""
	}
	return h.scope
}

// Decide answers one authorization request.
func (e *Engine) Decide(ctx context.Context, p *auth.Principal, req Request) Decision {
	if !e.enabled {
		return allow("authentication is disabled")
	}
	perm, ok := Lookup(req.Action)
	if !ok {
		return deny("unknown action %q", req.Action)
	}
	if perm.InstallWide {
		req.Project = ""
	}
	s := e.snap.Load()
	base := vars(p, req, nil)
	hs := e.heldRoles(s, p, req.Project, base)
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.role)
	}
	v := vars(p, req, names)
	for i, r := range e.rules {
		if r.kind == "deny" && holds(r.prog, v) {
			return deny("deny rule %d (%s)", i, r.expr)
		}
	}
	for i, r := range e.rules {
		if r.kind == "require" && !holds(r.prog, v) {
			return deny("require rule %d does not hold (%s)", i, r.expr)
		}
	}
	local := Decision{}
	switch {
	case perm.Public && !p.Anonymous():
		local = allow("any authenticated principal may %s", req.Action)
	default:
		for _, h := range hs {
			if s.allows(roleScope(h), h.role, req.Action, v, map[*compiledRole]bool{}) {
				local = allow("role %s in %s (%s)", h.role, h.scope, h.via)
				break
			}
		}
	}
	if !local.Allowed {
		for i, r := range e.rules {
			if r.kind == "allow" && holds(r.prog, v) {
				local = allow("allow rule %d (%s)", i, r.expr)
				break
			}
		}
	}
	if e.ext != nil {
		return e.ext.Decide(ctx, p, req, local)
	}
	if !local.Allowed {
		return deny("no role or rule grants %s in project %q", req.Action, req.Project)
	}
	return local
}

// Allowed is Decide reduced to a boolean.
func (e *Engine) Allowed(ctx context.Context, p *auth.Principal, req Request) bool {
	return e.Decide(ctx, p, req).Allowed
}

// IsOwner reports whether the principal holds the owner role.
func (e *Engine) IsOwner(p *auth.Principal) bool {
	if !e.enabled {
		return true
	}
	s := e.snap.Load()
	for _, h := range e.heldRoles(s, p, "", vars(p, Request{}, nil)) {
		if h.role == OwnerRole {
			return true
		}
	}
	return false
}

// Access is what a principal holds in one project (or "*").
type Access struct {
	Project     string
	Roles       []string
	Permissions []string
}

// Access lists the principal's roles and the permissions they grant, per
// project, ignoring role conditions (which depend on the request).
func (e *Engine) Access(p *auth.Principal) (owner bool, out []Access) {
	if !e.enabled {
		return true, nil
	}
	s := e.snap.Load()
	projects := []string{"*"}
	for name := range s.projects {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	for _, project := range projects {
		scope := project
		if project == "*" {
			scope = ""
		}
		var roles []string
		var patterns []string
		for _, h := range e.heldRoles(s, p, scope, vars(p, Request{Project: scope}, nil)) {
			if project != "*" && h.scope == "*" {
				continue // listed once, under "*"
			}
			if h.role == OwnerRole {
				owner = true
			}
			roles = append(roles, h.role)
			for _, g := range s.grants(roleScope(h), h.role, nil, map[*compiledRole]bool{}) {
				patterns = append(patterns, g.pattern)
			}
		}
		if len(roles) == 0 {
			continue
		}
		sort.Strings(roles)
		perms := expand(patterns)
		if slices.Contains(roles, OwnerRole) {
			perms = expandAll()
		}
		out = append(out, Access{Project: project, Roles: slices.Compact(roles), Permissions: perms})
	}
	return owner, out
}

func expandAll() []string {
	out := make([]string, 0, len(Permissions))
	for _, p := range Permissions {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Projects lists known projects, sorted.
func (e *Engine) Projects() []Project {
	s := e.snap.Load()
	out := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProjectExists reports whether a project is known.
func (e *Engine) ProjectExists(name string) bool {
	_, ok := e.snap.Load().projects[name]
	return ok
}

// Roles lists every role: built-in, config and stored.
func (e *Engine) Roles() []Role {
	s := e.snap.Load()
	out := make([]Role, 0, len(s.roles))
	for _, r := range s.roles {
		out = append(out, r.Role)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Role returns one role by project ("" for install-wide) and name.
func (e *Engine) Role(project, name string) (Role, bool) {
	r := e.snap.Load().roles[roleKey{project, name}]
	if r == nil {
		return Role{}, false
	}
	return r.Role, true
}

// Bindings lists every binding.
func (e *Engine) Bindings() []Binding {
	s := e.snap.Load()
	out := make([]Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b.Binding)
	}
	return out
}

// Owners lists the config owner subjects.
func (e *Engine) Owners() []string { return slices.Clone(e.rbac.Owners) }

func (s *snapshot) clone() *snapshot {
	c := &snapshot{roles: map[roleKey]*compiledRole{}, projects: map[string]Project{}, bindings: append([]compiledBinding(nil), s.bindings...)}
	for k, v := range s.roles {
		c.roles[k] = v
	}
	for k, v := range s.projects {
		c.projects[k] = v
	}
	return c
}

// ValidateRole checks a role definition as if it were stored now: its
// shape, its condition, the roles it inherits, and that it creates no
// cycle. replace says it replaces an existing role of the same name.
func (e *Engine) ValidateRole(r Role, replace bool) error {
	s := e.snap.Load().clone()
	if replace {
		delete(s.roles, roleKey{r.Project, r.Name})
	}
	if err := s.addRole(r); err != nil {
		return err
	}
	return s.checkRoles()
}

// RoleUsers lists what depends on a role: roles that inherit it and bindings
// that grant it. A role in use cannot be deleted.
func (e *Engine) RoleUsers(project, name string) []string {
	s := e.snap.Load()
	target := s.roles[roleKey{project, name}]
	var out []string
	for k, r := range s.roles {
		for _, in := range r.Inherits {
			if s.resolve(r.Project, in) == target {
				out = append(out, "role "+k.project+"/"+k.name)
			}
		}
	}
	for _, b := range s.bindings {
		if s.resolve(roleScope(held{scope: b.Project}), b.Role) == target {
			out = append(out, "binding "+b.Project+"/"+b.Role+" "+b.Subject)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateBinding checks a binding as if it were stored now.
func (e *Engine) ValidateBinding(b Binding) error {
	return e.snap.Load().clone().addBinding(b)
}

// ResolveRole reports whether a role name means something in a project ("*" for install-wide).
func (e *Engine) ResolveRole(project, name string) bool {
	return e.snap.Load().resolve(roleScope(held{scope: project}), name) != nil
}

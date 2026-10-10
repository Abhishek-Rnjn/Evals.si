package authz

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

type memStore struct {
	roles    []Role
	bindings []Binding
	projects []Project
}

func (m *memStore) AuthRoles(context.Context) ([]Role, error)       { return m.roles, nil }
func (m *memStore) AuthBindings(context.Context) ([]Binding, error) { return m.bindings, nil }
func (m *memStore) AuthProjects(context.Context) ([]Project, error) { return m.projects, nil }

func user(sub string, groups ...string) *auth.Principal {
	return &auth.Principal{Kind: auth.KindUser, Method: auth.MethodJWT, Provider: "corp", Subject: sub, Groups: groups,
		Claims: map[string]any{"sub": sub}}
}

func engine(t *testing.T, rbac RBACConfig, rules []Rule, st Store) *Engine {
	t.Helper()
	e, err := NewEngine(context.Background(), Options{Enabled: true, RBAC: rbac, Authorization: AuthorizationConfig{Rules: rules}, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var testRBAC = RBACConfig{
	Owners: []string{"group:corp/platform"},
	Roles: []Role{
		{Name: "prompt-engineer", Inherits: []string{"viewer"}, Permissions: []string{"evaluations.run", "runs.create", "runs.cancel"},
			Condition: `resource.target.model in ["qwen3", "claude-opus-5-5"] && resource.labels.app == "checkout"`},
		{Name: "trace-auditor", Permissions: []string{"traces.read", "audit.read"}},
		{Name: "ci-gate", Permissions: []string{"runs.create", "runs.read"}, Condition: "!resource.runs_code"},
	},
	Projects: map[string]map[string][]string{
		"support": {
			"admin":           {"group:corp/support-leads"},
			"editor":          {"group:corp/support-eng"},
			"runner":          {`cel:jwt.repository == "acme/agent" && jwt.ref == "refs/heads/main"`},
			"viewer":          {"email:everyone@example.com"},
			"prompt-engineer": {"user:corp/pat"},
		},
		"checkout": {"ci-gate": {"key:ci"}},
		"*":        {"trace-auditor": {"user:corp/audrey"}},
	},
}

func runReq(project, model string, labels map[string]any) Request {
	return Request{Action: "runs.create", Project: project, Resource: map[string]any{
		"target": map[string]any{"model": model}, "labels": labels, "runs_code": false,
	}}
}

func TestBuiltinRolesMatrix(t *testing.T) {
	e := engine(t, testRBAC, nil, nil)
	ctx := context.Background()
	members := map[string]*auth.Principal{
		"admin":  user("al", "support-leads"),
		"editor": user("ed", "support-eng"),
		"viewer": {Kind: auth.KindUser, Method: auth.MethodJWT, Provider: "corp", Subject: "vi", Email: "everyone@example.com", EmailVerified: true},
		"runner": {Kind: auth.KindUser, Method: auth.MethodJWT, Provider: "github", Subject: "repo:acme/agent",
			Claims: map[string]any{"repository": "acme/agent", "ref": "refs/heads/main"}},
		"ingest": {Kind: auth.KindAPIKey, Method: auth.MethodAPIKey, KeyName: "otel", KeyRoles: map[string][]string{"support": {"ingest"}}},
	}
	lowest := map[string]string{
		"catalog.read": "viewer", "runs.read": "viewer", "policies.read": "viewer", "traces.read": "viewer",
		"evaluations.run": "runner", "runs.create": "runner", "runs.cancel": "runner", "runs.resume": "runner",
		"policies.write": "editor", "access.manage": "admin", "audit.read": "admin", "traces.write": "ingest",
	}
	rank := map[string]int{"viewer": 1, "runner": 2, "editor": 3, "admin": 4}
	for action, need := range lowest {
		for role, p := range members {
			want := role == need || (rank[need] > 0 && rank[role] >= rank[need])
			if got := e.Allowed(ctx, p, Request{Action: action, Project: "support"}); got != want {
				t.Errorf("%s doing %s in support: allowed = %v, want %v", role, action, got, want)
			}
			// Nobody but the owner reaches another project.
			if e.Allowed(ctx, p, Request{Action: action, Project: "checkout"}) {
				t.Errorf("%s doing %s in checkout was allowed", role, action)
			}
		}
	}
	owner := user("root", "platform")
	for _, perm := range Permissions {
		if !e.Allowed(ctx, owner, Request{Action: perm.Name, Project: "checkout"}) {
			t.Errorf("owner denied %s", perm.Name)
		}
	}
	// Install-wide permissions: owner only, even for an admin.
	if e.Allowed(ctx, members["admin"], Request{Action: "metrics.read"}) || e.Allowed(ctx, members["admin"], Request{Action: "projects.manage"}) {
		t.Error("admin was granted an install-wide permission")
	}
	// The gitHub-actions binding needs the main branch.
	feature := *members["runner"]
	feature.Claims = map[string]any{"repository": "acme/agent", "ref": "refs/heads/feature"}
	if e.Allowed(ctx, &feature, Request{Action: "runs.create", Project: "support"}) {
		t.Error("cel binding matched a feature branch")
	}
	// Self-service needs only authentication.
	if !e.Allowed(ctx, user("stranger"), Request{Action: "self.read"}) || e.Allowed(ctx, auth.Anonymous(auth.MethodJWT), Request{Action: "self.read"}) {
		t.Error("self.read wrong")
	}
	if d := e.Decide(ctx, owner, Request{Action: "runs.delete", Project: "support"}); d.Allowed || !strings.Contains(d.Reason, "unknown action") {
		t.Errorf("unknown action: %+v", d)
	}
}

func TestCustomRoleConditions(t *testing.T) {
	e := engine(t, testRBAC, nil, nil)
	ctx := context.Background()
	pat := user("pat")
	checkout := map[string]any{"app": "checkout"}
	for _, tc := range []struct {
		req  Request
		want bool
	}{
		{runReq("support", "qwen3", checkout), true},
		{runReq("support", "gpt-5", checkout), false},                         // model not allowed
		{runReq("support", "qwen3", map[string]any{"app": "billing"}), false}, // label not allowed
		{runReq("support", "qwen3", nil), false},                              // missing label: condition errors, does not hold
		{runReq("checkout", "qwen3", checkout), false},                        // not bound there
		{Request{Action: "policies.write", Project: "support"}, false},        // not a permission of the role
		{Request{Action: "runs.read", Project: "support", Resource: map[string]any{ // inherited from viewer, still conditioned
			"target": map[string]any{"model": "qwen3"}, "labels": checkout}}, true},
	} {
		if got := e.Allowed(ctx, pat, tc.req); got != tc.want {
			t.Errorf("pat %s %v: allowed = %v, want %v", tc.req.Action, tc.req.Resource, got, tc.want)
		}
	}
	ci := &auth.Principal{Kind: auth.KindAPIKey, Method: auth.MethodAPIKey, KeyName: "ci"}
	if !e.Allowed(ctx, ci, Request{Action: "runs.create", Project: "checkout", Resource: map[string]any{"runs_code": false}}) {
		t.Error("ci-gate denied a run without code")
	}
	if e.Allowed(ctx, ci, Request{Action: "runs.create", Project: "checkout", Resource: map[string]any{"runs_code": true}}) {
		t.Error("ci-gate allowed a code-executing run")
	}
	audrey := user("audrey")
	if !e.Allowed(ctx, audrey, Request{Action: "traces.read", Project: "checkout"}) || e.Allowed(ctx, audrey, Request{Action: "runs.read", Project: "checkout"}) {
		t.Error("install-wide trace-auditor binding wrong")
	}
	d := e.Decide(ctx, pat, runReq("support", "qwen3", checkout))
	if !strings.Contains(d.Reason, "prompt-engineer") || !strings.Contains(d.Reason, "user:corp/pat") {
		t.Errorf("reason = %q", d.Reason)
	}
	// A denial caused by a failed condition names the role and its condition.
	d = e.Decide(ctx, pat, runReq("support", "gpt-5", checkout))
	if d.Allowed || !strings.Contains(d.Reason, "condition on role prompt-engineer") {
		t.Errorf("conditioned denial reason = %q", d.Reason)
	}
	d = e.Decide(ctx, pat, Request{Action: "policies.write", Project: "support"})
	if !strings.Contains(d.Reason, "no role or rule grants") {
		t.Errorf("ungranted action reason = %q", d.Reason)
	}
}

func TestRulesPrecedence(t *testing.T) {
	rules := []Rule{
		{Deny: `request.action == "runs.create" && resource.target.connector == "anthropic" && !("llm-spenders" in principal.groups)`},
		{Require: `!resource.runs_code || "sandbox-users" in principal.groups`},
		{Require: `request.action != "runs.cancel" || principal.id == resource.run.created_by || "admin" in principal.roles`},
		{Allow: `jwt.iss == "https://token.actions.githubusercontent.com" && request.action in ["runs.create", "runs.read"] && resource.project == "agent-ci"`},
		{Deny: `resource.does.not.exist == 1`}, // errors: does not deny
	}
	rbac := testRBAC
	rbac.Projects = map[string]map[string][]string{"support": {"editor": {"group:corp/support-eng"}, "admin": {"group:corp/support-leads"}}, "agent-ci": {}}
	e := engine(t, rbac, rules, nil)
	ctx := context.Background()
	ed := user("ed", "support-eng")
	spender := user("sp", "support-eng", "llm-spenders")
	anthropic := map[string]any{"target": map[string]any{"connector": "anthropic"}, "runs_code": false}
	if d := e.Decide(ctx, ed, Request{Action: "runs.create", Project: "support", Resource: anthropic}); d.Allowed || !strings.Contains(d.Reason, "deny rule 0") {
		t.Errorf("deny rule: %+v", d)
	}
	if !e.Allowed(ctx, spender, Request{Action: "runs.create", Project: "support", Resource: anthropic}) {
		t.Error("spender denied")
	}
	code := map[string]any{"target": map[string]any{"connector": "openai"}, "runs_code": true}
	if d := e.Decide(ctx, ed, Request{Action: "runs.create", Project: "support", Resource: code}); d.Allowed || !strings.Contains(d.Reason, "require rule 1") {
		t.Errorf("require rule: %+v", d)
	}
	// A require rule that cannot evaluate (no runs_code attribute) denies, even the owner.
	if e.Allowed(ctx, user("root", "platform"), Request{Action: "runs.create", Project: "support"}) {
		t.Error("require rule with a missing attribute allowed")
	}
	mine := map[string]any{"runs_code": false, "run": map[string]any{"created_by": "user:corp/ed"}}
	theirs := map[string]any{"runs_code": false, "run": map[string]any{"created_by": "user:corp/someone"}}
	if !e.Allowed(ctx, ed, Request{Action: "runs.cancel", Project: "support", Resource: mine}) ||
		e.Allowed(ctx, ed, Request{Action: "runs.cancel", Project: "support", Resource: theirs}) ||
		!e.Allowed(ctx, user("al", "support-leads"), Request{Action: "runs.cancel", Project: "support", Resource: theirs}) {
		t.Error("cancel-own-runs rule wrong")
	}
	gh := &auth.Principal{Kind: auth.KindUser, Method: auth.MethodJWT, Provider: "github", Subject: "repo:x",
		Claims: map[string]any{"iss": "https://token.actions.githubusercontent.com"}}
	if d := e.Decide(ctx, gh, Request{Action: "runs.create", Project: "agent-ci", Resource: map[string]any{"runs_code": false}}); !d.Allowed || !strings.Contains(d.Reason, "allow rule 3") {
		t.Errorf("allow rule: %+v", d)
	}
	if e.Allowed(ctx, gh, Request{Action: "runs.create", Project: "support", Resource: map[string]any{"runs_code": false}}) {
		t.Error("allow rule leaked into another project")
	}
	// Projects() cannot narrow when allow rules exist.
	c := &Checker{Engine: e, Principal: gh}
	if ps := Projects(WithChecker(ctx, c), "runs.read"); ps != nil {
		t.Errorf("Projects with allow rules = %v, want nil", ps)
	}
}

func TestRoleValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		rbac RBACConfig
		want string
	}{
		"unknown permission": {RBACConfig{Roles: []Role{{Name: "x", Permissions: []string{"runs.delete"}}}}, "unknown permission"},
		"bad wildcard":       {RBACConfig{Roles: []Role{{Name: "x", Permissions: []string{"nothing.*"}}}}, "unknown permission"},
		"install-wide":       {RBACConfig{Roles: []Role{{Name: "x", Permissions: []string{"metrics.read"}}}}, "reserved for the owner"},
		"redefine builtin":   {RBACConfig{Roles: []Role{{Name: "viewer", Permissions: []string{"runs.read"}}}}, "built-in"},
		"inherit owner":      {RBACConfig{Roles: []Role{{Name: "x", Inherits: []string{"owner"}}}}, "cannot inherit owner"},
		"unknown parent":     {RBACConfig{Roles: []Role{{Name: "x", Inherits: []string{"ghost"}}}}, "unknown role"},
		"cycle": {RBACConfig{Roles: []Role{
			{Name: "a", Inherits: []string{"b"}}, {Name: "b", Inherits: []string{"c"}}, {Name: "c", Inherits: []string{"a"}},
		}}, "cycle"},
		"bad condition":                   {RBACConfig{Roles: []Role{{Name: "x", Permissions: []string{"runs.read"}, Condition: "resource.("}}}, "condition"},
		"not boolean":                     {RBACConfig{Roles: []Role{{Name: "x", Permissions: []string{"runs.read"}, Condition: "1 + 2"}}}, "boolean"},
		"unknown role":                    {RBACConfig{Projects: map[string]map[string][]string{"p": {"ghost": {"user:corp/a"}}}}, "unknown role"},
		"owner in proj":                   {RBACConfig{Projects: map[string]map[string][]string{"p": {"owner": {"user:corp/a"}}}}, "rbac.owners"},
		"bad cel subject":                 {RBACConfig{Projects: map[string]map[string][]string{"p": {"viewer": {"cel:jwt.("}}}}, "cel:"},
		"project role in unknown project": {RBACConfig{Roles: []Role{{Name: "x", Project: "nope", Permissions: []string{"runs.read"}}}}, "unknown project"},
	} {
		_, err := NewEngine(context.Background(), Options{Enabled: true, RBAC: tc.rbac})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, err := NewEngine(context.Background(), Options{Enabled: true, RBAC: testRBAC, ClaimRoles: []string{"ghost"}}); err == nil {
		t.Error("unknown claim role accepted")
	}
}

func TestEscalation(t *testing.T) {
	st := &memStore{}
	e := engine(t, RBACConfig{
		Owners: []string{"group:corp/platform"},
		Roles: []Role{
			{Name: "limited-admin", Inherits: []string{"admin"}, Condition: `resource.labels.app == "checkout"`},
		},
		Projects: map[string]map[string][]string{
			"support":  {"admin": {"user:corp/al"}, "limited-admin": {"user:corp/li"}, "runner": {"user:corp/ru"}},
			"checkout": {},
		},
	}, nil, st)
	al, li := user("al"), user("li")
	var esc *EscalationError
	// An admin may define a role from permissions it holds...
	if err := e.CheckRole(al, "support", Role{Name: "r", Project: "support", Permissions: []string{"runs.*"}, Condition: "resource.trials < 5"}); err != nil {
		t.Errorf("admin defining a narrower role: %v", err)
	}
	// ...not in a project it does not administer, nor install-wide.
	if err := e.CheckRole(al, "checkout", Role{Name: "r", Project: "checkout", Permissions: []string{"runs.read"}}); !errors.As(err, &esc) {
		t.Errorf("admin of support defining a role in checkout: %v", err)
	}
	if err := e.CheckRole(al, "*", Role{Name: "r", Permissions: []string{"runs.read"}}); err == nil {
		t.Error("project admin defined an install-wide role")
	}
	// A conditioned admin can only grant under at least its own condition.
	open := Role{Name: "r", Project: "support", Permissions: []string{"runs.read"}}
	if err := e.CheckRole(li, "support", open); !errors.As(err, &esc) || !slices.Contains(esc.Missing, "runs.read") {
		t.Errorf("conditioned admin granting an unconditioned role: %v", err)
	}
	strict := open
	strict.Condition = `resource.trials < 3 && (resource.labels.app == "checkout")`
	if err := e.CheckRole(li, "support", strict); err != nil {
		t.Errorf("conditioned admin granting a stricter role: %v", err)
	}
	// Binding: an admin can bind admin, a runner cannot bind editor.
	if err := e.CheckRoleNames(al, "support", []string{"admin"}); err != nil {
		t.Errorf("admin binding admin: %v", err)
	}
	if err := e.CheckRoleNames(user("ru"), "support", []string{"editor"}); !errors.As(err, &esc) || !slices.Contains(esc.Missing, "policies.write") {
		t.Errorf("runner binding editor: %v", err)
	}
	if err := e.CheckRoleNames(al, "support", []string{"owner"}); err == nil {
		t.Error("admin granted owner")
	}
	if err := e.CheckRoleNames(user("root", "platform"), "*", []string{"owner"}); err != nil {
		t.Errorf("owner granting owner: %v", err)
	}
}

func TestStoredRolesAndAccess(t *testing.T) {
	st := &memStore{
		projects: []Project{{Name: "labs"}},
		roles:    []Role{{Name: "labeler", Project: "labs", Inherits: []string{"viewer"}, Permissions: []string{"policies.write"}}},
		bindings: []Binding{{Project: "labs", Role: "labeler", Subject: "group:corp/ml"}},
	}
	e := engine(t, testRBAC, nil, st)
	ml := user("m", "ml", "support-eng")
	if !e.Allowed(context.Background(), ml, Request{Action: "policies.write", Project: "labs"}) {
		t.Error("stored role and binding not applied")
	}
	owner, access := e.Access(ml)
	if owner || len(access) != 2 || access[0].Project != "labs" || !slices.Contains(access[0].Permissions, "policies.write") ||
		access[1].Project != "support" || access[1].Roles[0] != "editor" {
		t.Errorf("access = %v %+v", owner, access)
	}
	ctx := WithChecker(context.Background(), &Checker{Engine: e, Principal: ml})
	if ps := Projects(ctx, "policies.write"); strings.Join(ps, ",") != "labs,support" {
		t.Errorf("Projects = %v", ps)
	}
	if ps := Projects(ctx, "access.manage"); len(ps) != 0 {
		t.Errorf("Projects(access.manage) = %v", ps)
	}
	// A project role may not shadow an install-wide one.
	if err := e.ValidateRole(Role{Name: "trace-auditor", Project: "labs", Permissions: []string{"runs.read"}}, false); err == nil {
		t.Error("project role shadowed an install-wide role")
	}
	if users := e.RoleUsers("labs", "labeler"); len(users) != 1 {
		t.Errorf("RoleUsers = %v", users)
	}
}

func TestDisabled(t *testing.T) {
	e, err := NewEngine(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Allowed(context.Background(), auth.Anonymous(auth.MethodNone), Request{Action: "projects.manage"}) {
		t.Error("disabled engine denied")
	}
}

func TestConjuncts(t *testing.T) {
	a, _ := conjuncts(`a.x == 1 && (b.y == "z" && c)`)
	b, _ := conjuncts(`c && b.y   ==  "z" && a.x == 1`)
	if strings.Join(a, "|") != strings.Join(b, "|") || len(a) != 3 {
		t.Errorf("conjuncts: %v vs %v", a, b)
	}
}

// changeStore stores projects in memory; the service's other store methods
// are not used by the test.
type changeStore struct {
	ServiceStore
	mem *memStore
}

func (c *changeStore) AuthRoles(ctx context.Context) ([]Role, error) { return c.mem.AuthRoles(ctx) }
func (c *changeStore) AuthBindings(ctx context.Context) ([]Binding, error) {
	return c.mem.AuthBindings(ctx)
}
func (c *changeStore) AuthProjects(ctx context.Context) ([]Project, error) {
	return c.mem.AuthProjects(ctx)
}
func (c *changeStore) CreateProject(_ context.Context, p Project) error {
	c.mem.projects = append(c.mem.projects, p)
	return nil
}

// A stored access-control change is announced, so other replicas reload.
func TestServiceAnnouncesChanges(t *testing.T) {
	st := &changeStore{mem: &memStore{}}
	e := engine(t, testRBAC, nil, st)
	svc := NewService(e, st, nil, nil)
	changes := 0
	svc.OnChange(func() { changes++ })
	if _, err := svc.CreateProject(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "review-ha"})); err != nil {
		t.Fatal(err)
	}
	if changes != 1 || !e.ProjectExists("review-ha") {
		t.Errorf("changes = %d, project known here: %v", changes, e.ProjectExists("review-ha"))
	}
}

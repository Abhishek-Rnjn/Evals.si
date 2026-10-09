package authz

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Role is a named set of permissions, optionally inheriting other roles and
// limited by a CEL condition.
type Role struct {
	Name string `json:"name"`
	// Empty for an install-wide role.
	Project     string   `json:"project,omitempty"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Inherits    []string `json:"inherits,omitempty"`
	Condition   string   `json:"condition,omitempty"`
	// "builtin", "config" or "api".
	Source string `json:"-"`
}

// Binding grants a role to the principals a subject names, in a project or "*".
type Binding struct {
	Project string
	Role    string
	Subject string
	// "config" or "api".
	Source    string
	CreatedAt time.Time
	CreatedBy string
}

// Project is a scope for runs, policies, traces and access.
type Project struct {
	Name        string
	Description string
	// "builtin", "config" or "api".
	Source    string
	CreatedAt time.Time
	CreatedBy string
}

// Errors the store returns.
var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

// KeyRecord is an API key created through AuthService; Hash is the hex
// SHA-256 of the key, which is never stored.
type KeyRecord struct {
	Name, Hash, Prefix string
	Roles              map[string][]string
	Labels             map[string]string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	LastUsedAt         time.Time
	CreatedBy          string
	Revoked            bool
}

// AuditQuery selects audit events.
type AuditQuery struct {
	// nil: every project.
	Projects   []string
	Action     string
	DeniedOnly bool
}

// DefaultProject receives requests that name no project.
const DefaultProject = "default"

// Sources of roles, bindings and projects.
const (
	SourceBuiltin = "builtin"
	SourceConfig  = "config"
	SourceAPI     = "api"
)

// OwnerRole holds every permission, install-wide ones included. It cannot be redefined.
const OwnerRole = "owner"

// BuiltinRoles ship with Evals.si. Custom roles can inherit them.
var BuiltinRoles = []Role{
	{Name: "viewer", Description: "Read the catalog, runs, policies, traces, annotation queues and guardrails.",
		Permissions: []string{"catalog.read", "runs.read", "policies.read", "traces.read", "annotations.read", "guardrails.read"}},
	{Name: "runner", Description: "Viewer, plus scoring, checking content against guardrails, and starting, cancelling and resuming runs.",
		Inherits: []string{"viewer"}, Permissions: []string{"evaluations.run", "guardrails.check", "runs.create", "runs.cancel", "runs.resume"}},
	{Name: "editor", Description: "Runner, plus writing online policies and guardrails, promoting results into datasets, and managing and answering annotation queues.",
		Inherits: []string{"runner"}, Permissions: []string{"policies.write", "guardrails.write", "datasets.write", "annotations.manage", "annotations.write"}},
	{Name: "admin", Description: "Editor, plus the project's API keys, custom roles, bindings, audit log and webhooks.",
		Inherits: []string{"editor"}, Permissions: []string{"access.manage", "audit.read", "webhooks.read", "webhooks.write"}},
	{Name: "annotator", Description: "Answer annotation queues: read queues and their records, take items and submit answers.",
		Permissions: []string{"annotations.read", "annotations.write"}},
	{Name: "ingest", Description: "Ingest traces over OTLP and nothing else.",
		Permissions: []string{"traces.write"}},
	{Name: "guard", Description: "Check content against guardrails and nothing else: for a gateway's credential.",
		Permissions: []string{"guardrails.check"}},
	{Name: OwnerRole, Description: "Everything, in every project, including install-wide settings.",
		Permissions: []string{"*"}},
}

func builtin(name string) (Role, bool) {
	for _, r := range BuiltinRoles {
		if r.Name == name {
			r.Source = SourceBuiltin
			return r, true
		}
	}
	return Role{}, false
}

// RBACConfig is the rbac section of evalsi.yaml.
type RBACConfig struct {
	// Subjects that hold the owner role install-wide.
	Owners []string `json:"owners,omitempty"`
	// Custom roles. Each is install-wide unless it names a project.
	Roles []Role `json:"roles,omitempty"`
	// Projects and their bindings: project -> role -> subjects. The project
	// "*" binds roles in every project.
	Projects map[string]map[string][]string `json:"projects,omitempty"`
}

// Rule is one global CEL rule; exactly one field is set.
type Rule struct {
	Allow   string `json:"allow,omitempty"`
	Deny    string `json:"deny,omitempty"`
	Require string `json:"require,omitempty"`
}

func (r Rule) kind() (string, string) {
	switch {
	case r.Deny != "":
		return "deny", r.Deny
	case r.Require != "":
		return "require", r.Require
	default:
		return "allow", r.Allow
	}
}

// AuthorizationConfig is the authorization section of evalsi.yaml.
type AuthorizationConfig struct {
	Rules    []Rule          `json:"rules,omitempty"`
	ExtAuthz *ExtAuthzConfig `json:"ext_authz,omitempty"`
}

// AuditConfig is the audit section of evalsi.yaml.
type AuditConfig struct {
	// How long audit events are kept. Default 2160h (90 days).
	Retention string `json:"retention,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidProjectName reports whether a project name is acceptable.
func ValidProjectName(name string) bool { return nameRE.MatchString(name) }

// ValidRoleName reports whether a role name is acceptable.
func ValidRoleName(name string) bool { return nameRE.MatchString(name) }

// ValidSubject checks a binding subject's form (cel: expressions are compiled separately).
func ValidSubject(s string) error {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return fmt.Errorf("subject %q: use user:<provider>/<subject>, email:<address>, group:<provider>/<group>, key:<name> or cel:<expression>", s)
	}
	switch kind {
	case "user", "group":
		if p, v, ok := strings.Cut(rest, "/"); !ok || p == "" || v == "" {
			return fmt.Errorf("subject %q: %s subjects are %s:<provider>/<value>", s, kind, kind)
		}
	case "email":
		if !strings.Contains(rest, "@") {
			return fmt.Errorf("subject %q: not an email address", s)
		}
	case "key", "cel":
	default:
		return fmt.Errorf("subject %q: unknown kind %q", s, kind)
	}
	return nil
}

// Validate checks the config's shape. Roles, conditions and rules are
// compiled (and checked for unknown permissions and cycles) by NewEngine.
func (c RBACConfig) Validate() error {
	var errs []error
	for _, s := range c.Owners {
		if err := ValidSubject(s); err != nil {
			errs = append(errs, fmt.Errorf("rbac.owners: %w", err))
		}
	}
	for project, roles := range c.Projects {
		if project != "*" && !ValidProjectName(project) {
			errs = append(errs, fmt.Errorf("rbac.projects: invalid project name %q", project))
		}
		for role, subjects := range roles {
			if role == OwnerRole {
				errs = append(errs, fmt.Errorf("rbac.projects.%s: bind owners with rbac.owners", project))
			}
			for _, s := range subjects {
				if err := ValidSubject(s); err != nil {
					errs = append(errs, fmt.Errorf("rbac.projects.%s.%s: %w", project, role, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Validate checks the rules' shape.
func (c AuthorizationConfig) Validate() error {
	var errs []error
	for i, r := range c.Rules {
		n := 0
		for _, s := range []string{r.Allow, r.Deny, r.Require} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			errs = append(errs, fmt.Errorf("authorization.rules[%d]: set exactly one of allow, deny or require", i))
		}
	}
	if c.ExtAuthz != nil {
		if err := c.ExtAuthz.validate(); err != nil {
			errs = append(errs, fmt.Errorf("authorization.ext_authz: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Validate checks the audit section.
func (c AuditConfig) Validate() error {
	if c.Retention == "" {
		return nil
	}
	if _, err := time.ParseDuration(c.Retention); err != nil {
		return fmt.Errorf("audit.retention: %w", err)
	}
	return nil
}

// RetentionDuration is the configured retention, or 90 days.
func (c AuditConfig) RetentionDuration() time.Duration {
	if d, err := time.ParseDuration(c.Retention); err == nil && d > 0 {
		return d
	}
	return 2160 * time.Hour
}

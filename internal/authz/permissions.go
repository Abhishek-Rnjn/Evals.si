// Package authz decides whether a principal may perform an action on a
// resource: project-scoped RBAC with built-in and custom roles, plus
// agentgateway-style CEL rules (allow, deny, require) and optional external
// authorization.
//
// Decision order:
//  1. Any deny rule that matches denies.
//  2. Any require rule that does not hold denies.
//  3. A role the principal holds in the project (through a binding, its API
//     key, or its token's role claims) whose permissions include the action
//     and whose condition holds allows.
//  4. Any allow rule that matches allows.
//  5. Otherwise the request is denied.
package authz

import (
	"slices"
	"sort"
	"strings"
)

// Permission is an action roles can grant. The list is a public contract:
// names are only ever added, never renamed or removed.
type Permission struct {
	Name        string
	Description string
	// Reserved for the owner role; never part of a project role.
	InstallWide bool
	// Any authenticated principal may perform it; no role is needed.
	Public bool
	// Calls are written to the audit log even when allowed.
	Audited bool
}

// Permissions is the permission registry, in display order.
var Permissions = []Permission{
	{Name: "catalog.read", Description: "List evaluators and use gRPC reflection."},
	{Name: "evaluations.run", Description: "Score records (Evaluate, EvaluateStream) and RL rollouts (ScoreRewards). Spends judge budget and may run code."},
	{Name: "runs.read", Description: "Read runs, their results and comparisons."},
	{Name: "runs.create", Description: "Start runs. Spends target and judge budget and may run code.", Audited: true},
	{Name: "runs.cancel", Description: "Cancel runs.", Audited: true},
	{Name: "runs.resume", Description: "Resume cancelled or errored runs.", Audited: true},
	{Name: "policies.read", Description: "Read online policies and their statistics."},
	{Name: "policies.write", Description: "Apply and delete online policies, including dataset promotion.", Audited: true},
	{Name: "traces.read", Description: "Read ingested traces and their online results."},
	{Name: "traces.write", Description: "Ingest traces over OTLP."},
	{Name: "access.manage", Description: "Manage the project's API keys, custom roles and role bindings.", Audited: true},
	{Name: "audit.read", Description: "Read the project's audit log."},
	{Name: "self.read", Description: "Read your own identity and access (WhoAmI, ListProjects).", Public: true},
	{Name: "projects.manage", Description: "Create projects.", InstallWide: true, Audited: true},
	{Name: "metrics.read", Description: "Read Prometheus metrics for the whole install.", InstallWide: true},
	{Name: "datasets.write", Description: "Promote a run's results into a dataset of the project.", Audited: true},
	{Name: "annotations.read", Description: "Read annotation queues, their annotations and statistics (which show the queued records)."},
	{Name: "annotations.write", Description: "Annotate: take items from a queue and submit answers."},
	{Name: "annotations.manage", Description: "Create and delete annotation queues and add items to them.", Audited: true},
	{Name: "mcp.tools.call", Description: "Call a tool of the MCP endpoint (rules can restrict tools by mcp.tool.name); each tool also needs the permission of what it does.", Public: true},
}

var permissionIndex = func() map[string]Permission {
	m := map[string]Permission{}
	for _, p := range Permissions {
		m[p.Name] = p
	}
	return m
}()

// Lookup returns a permission by name.
func Lookup(name string) (Permission, bool) {
	p, ok := permissionIndex[name]
	return p, ok
}

// validPattern reports whether a role may list this permission pattern:
// a permission name, "<prefix>.*" naming at least one permission, or "*".
func validPattern(pattern string) bool {
	if pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		for _, p := range Permissions {
			if strings.HasPrefix(p.Name, prefix+".") {
				return true
			}
		}
		return false
	}
	_, ok := permissionIndex[pattern]
	return ok
}

// matches reports whether a role's permission pattern covers an action.
// Wildcards never cover install-wide permissions; only the owner holds those.
func matches(pattern, action string) bool {
	if pattern == action {
		return true
	}
	if p, ok := permissionIndex[action]; ok && p.InstallWide {
		return false
	}
	if pattern == "*" {
		return true
	}
	prefix, ok := strings.CutSuffix(pattern, ".*")
	return ok && strings.HasPrefix(action, prefix+".")
}

// expand turns permission patterns into the concrete permissions they cover.
func expand(patterns []string) []string {
	var out []string
	for _, p := range Permissions {
		for _, pat := range patterns {
			if matches(pat, p.Name) {
				out = append(out, p.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

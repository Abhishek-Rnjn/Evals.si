// Package auth authenticates callers of evalsid. It turns a credential (an
// OIDC/JWT bearer token, an API key, a client certificate, or identity headers
// from a trusted proxy) into a Principal. Authorization is package authz.
//
// The model follows agentgateway: providers verify JWTs against JWKS, modes
// are strict, optional or permissive, API keys are stored as SHA-256 hashes,
// and the token location is configurable.
package auth

import (
	"context"
	"sort"
)

// Kinds of principal.
const (
	KindUser      = "user"
	KindService   = "service"
	KindAPIKey    = "apikey"
	KindAnonymous = "anonymous"
)

// Authentication methods.
const (
	MethodJWT    = "jwt"
	MethodAPIKey = "apikey"
	MethodMTLS   = "mtls"
	MethodProxy  = "proxy"
	// MethodNone marks requests on a server without authentication.
	MethodNone = "none"
)

// Principal is an authenticated (or anonymous) caller.
type Principal struct {
	Kind          string
	Method        string
	Provider      string
	Subject       string
	Name          string
	Email         string
	EmailVerified bool
	Groups        []string
	// Verified JWT claims; unverified ones in permissive mode.
	Claims map[string]any
	// API key name, labels and roles per project ("*" for every project).
	KeyName  string
	Labels   map[string]string
	KeyRoles map[string][]string
	// Roles mapped from token claims (role_claims), per project.
	ClaimRoles map[string][]string
}

// Anonymous is the principal of a request without a credential.
func Anonymous(method string) *Principal {
	return &Principal{Kind: KindAnonymous, Method: method}
}

// ID is the principal's binding form: "user:corp/1234", "key:ci" or "anonymous".
func (p *Principal) ID() string {
	switch {
	case p == nil || p.Kind == KindAnonymous:
		return "anonymous"
	case p.Kind == KindAPIKey:
		return "key:" + p.KeyName
	default:
		return "user:" + p.Provider + "/" + p.Subject
	}
}

// Anonymous reports whether the principal has no verified identity.
func (p *Principal) Anonymous() bool { return p == nil || p.Kind == KindAnonymous }

// Vars is the principal as CEL sees it, as the `principal` variable.
func (p *Principal) Vars() map[string]any {
	groups := make([]any, 0, len(p.Groups))
	for _, g := range p.Groups {
		groups = append(groups, g)
	}
	labels := map[string]any{}
	for k, v := range p.Labels {
		labels[k] = v
	}
	return map[string]any{
		"kind": p.Kind, "method": p.Method, "provider": p.Provider, "subject": p.Subject,
		"name": p.Name, "email": p.Email, "email_verified": p.EmailVerified,
		"groups": groups, "id": p.ID(), "labels": labels,
	}
}

// RoleGrants lists the roles a principal carries itself (API key roles and
// claim-mapped roles) for a project, including those granted in "*".
func (p *Principal) RoleGrants(project string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string][]string{p.KeyRoles, p.ClaimRoles} {
		for _, proj := range []string{project, "*"} {
			for _, r := range m[proj] {
				if !seen[r] {
					seen[r] = true
					out = append(out, r)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

type ctxKey struct{}

// Result is what authentication concluded for a request.
type Result struct {
	Principal *Principal
	// Err is set when the request carried an invalid credential or none where
	// one is required; the enforcement point rejects the request with it.
	Err error
}

// WithResult stores an authentication result in a context.
func WithResult(ctx context.Context, r Result) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext returns the request's authentication result. A context without
// one (an internal call) yields an anonymous principal and no error.
func FromContext(ctx context.Context) Result {
	if r, ok := ctx.Value(ctxKey{}).(Result); ok {
		return r
	}
	return Result{Principal: Anonymous(MethodNone)}
}

// PrincipalFrom returns the request's principal, never nil.
func PrincipalFrom(ctx context.Context) *Principal {
	if p := FromContext(ctx).Principal; p != nil {
		return p
	}
	return Anonymous(MethodNone)
}

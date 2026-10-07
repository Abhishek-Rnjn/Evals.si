// Package credentials decides which of the worker's environment variables a
// request may name. Run specs, agents and evaluator params refer to secrets
// by variable name (api_key_env, headers_env, env_from), and the worker reads
// the value and sends it where the spec says. On a shared server that would
// let any caller who can create a run in any project send any variable the
// worker holds to a URL of their choosing, so names are granted per project,
// and a grant can limit the hosts its value may be sent to.
package credentials

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// None as api_key_env sends no key (a local model server, for example).
const None = "none"

// Config is the credentials section of the server config.
type Config struct {
	// Check references against the grants. Default: on when authentication
	// is enabled or any grant is listed. Off lets every request name any
	// variable on the worker, which suits only a single-user server.
	Enforce *bool `json:"enforce,omitempty"`
	// Variables requests may name, and for which projects.
	Grants []Grant `json:"grants,omitempty"`
}

// Grant lets the listed projects name one worker variable.
type Grant struct {
	// The variable on the worker, for example OPENAI_API_KEY.
	Env string `json:"env"`
	// Projects that may name it; "*" for every project.
	Projects []string `json:"projects"`
	// Hosts its value may be sent to, exact (api.openai.com) or a subdomain
	// wildcard (*.openai.azure.com). Empty: any host. A grant with hosts is
	// never copied into a sandbox (a CLI agent's env_from), whose traffic
	// evalsid does not see request by request.
	Hosts []string `json:"hosts,omitempty"`
}

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostName = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// Validate checks the grants.
func (c Config) Validate() error {
	var errs []error
	for i, g := range c.Grants {
		at := fmt.Sprintf("credentials.grants[%d]", i)
		if !envName.MatchString(g.Env) {
			errs = append(errs, fmt.Errorf("%s.env must be a variable name, not %q", at, g.Env))
		}
		if len(g.Projects) == 0 {
			errs = append(errs, fmt.Errorf("%s.projects is required (\"*\" for every project)", at))
		}
		for _, p := range g.Projects {
			if p == "" {
				errs = append(errs, fmt.Errorf("%s.projects has an empty name", at))
			}
		}
		for _, h := range g.Hosts {
			if !hostName.MatchString(h) {
				errs = append(errs, fmt.Errorf("%s.hosts: %q is not a lowercase host name or *.domain", at, h))
			}
		}
	}
	return errors.Join(errs...)
}

// Policy checks references against the grants.
type Policy struct {
	enforce bool
	grants  []Grant
}

// NewPolicy makes the policy; authEnabled decides the default.
func NewPolicy(c Config, authEnabled bool) *Policy {
	enforce := authEnabled || len(c.Grants) > 0
	if c.Enforce != nil {
		enforce = *c.Enforce
	}
	return &Policy{enforce: enforce, grants: c.Grants}
}

// Enforced reports whether references are checked.
func (p *Policy) Enforced() bool { return p != nil && p.enforce }

// Use is one reference to a worker variable.
type Use struct {
	Env string
	// Where in the request, for errors.
	Field string
	// The URL the value is sent to; empty when the request does not say
	// (an evaluator param without a url).
	URL string
	// Copied into a sandbox rather than sent by the worker.
	Sandbox bool
}

// Check refuses uses the project has no grant for.
func (p *Policy) Check(project string, uses []Use) error {
	if !p.Enforced() {
		return nil
	}
	for _, u := range uses {
		if err := p.check(project, u); err != nil {
			return connect.NewError(connect.CodePermissionDenied, err)
		}
	}
	return nil
}

func (p *Policy) check(project string, u Use) error {
	var granted []Grant
	for _, g := range p.grants {
		if g.Env == u.Env && (slices.Contains(g.Projects, "*") || slices.Contains(g.Projects, project)) {
			granted = append(granted, g)
		}
	}
	if len(granted) == 0 {
		return fmt.Errorf("%s names the worker variable %s, which project %q may not use; "+
			"grant it under credentials.grants in the server config", u.Field, u.Env, project)
	}
	host := ""
	if u.URL != "" {
		host = hostOf(u.URL)
	}
	for _, g := range granted {
		if len(g.Hosts) == 0 {
			return nil
		}
		if u.Sandbox || host == "" {
			continue
		}
		for _, h := range g.Hosts {
			if matchHost(h, host) {
				return nil
			}
		}
	}
	switch {
	case u.Sandbox:
		return fmt.Errorf("%s copies %s into a sandbox, but its grant limits it to hosts %s; "+
			"grant it without hosts to pass it to a CLI agent", u.Field, u.Env, hostList(granted))
	case host == "":
		return fmt.Errorf("%s names %s without a URL to send it to, but its grant limits it to hosts %s",
			u.Field, u.Env, hostList(granted))
	default:
		return fmt.Errorf("%s would send %s to %s, which its grant does not allow (hosts %s)",
			u.Field, u.Env, host, hostList(granted))
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

func matchHost(pattern, host string) bool {
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest) && net.ParseIP(host) == nil
	}
	return pattern == host
}

func hostList(gs []Grant) string {
	var hs []string
	for _, g := range gs {
		hs = append(hs, g.Hosts...)
	}
	sort.Strings(hs)
	return strings.Join(slices.Compact(hs), ", ")
}

// SpecUses lists the worker variables a run spec names: the target's key
// (the connector's default variable when api_key_env is empty, since the
// worker falls back to it), an agent's headers, key or env_from, MCP
// servers' headers, and evaluator and external-harness params.
func SpecUses(spec *evalsiv1alpha1.RunSpec) ([]Use, error) {
	var uses []Use
	t := spec.GetTarget()
	if t.GetConnector() != "" || t.GetModel() != "" {
		u, err := targetUse(t)
		if err != nil {
			return nil, err
		}
		uses = append(uses, u...)
	}
	a := t.GetAgent()
	switch {
	case a.GetA2A() != nil:
		uses = append(uses, headerUses("spec.target.agent.a2a.headers_env", a.GetA2A().GetHeadersEnv(), a.GetA2A().GetUrl())...)
	case a.GetMcp() != nil:
		uses = append(uses, headerUses("spec.target.agent.mcp.headers_env", a.GetMcp().GetHeadersEnv(), a.GetMcp().GetUrl())...)
	case a.GetHttp() != nil:
		uses = append(uses, headerUses("spec.target.agent.http.headers_env", a.GetHttp().GetHeadersEnv(), a.GetHttp().GetUrl())...)
	case a.GetResponses() != nil:
		r := a.GetResponses()
		if env := keyEnv(r.GetApiKeyEnv(), "OPENAI_API_KEY"); env != "" {
			uses = append(uses, Use{Env: env, Field: "spec.target.agent.responses.api_key_env", URL: r.GetBaseUrl()})
		}
	case a.GetCli() != nil:
		for _, inside := range sortedKeys(a.GetCli().GetEnvFrom()) {
			uses = append(uses, Use{Env: a.GetCli().GetEnvFrom()[inside], Field: "spec.target.agent.cli.env_from." + inside, Sandbox: true})
		}
	}
	for _, s := range spec.GetHarness().GetBuiltin().GetTools().GetMcp() {
		uses = append(uses, headerUses("MCP server "+s.GetName()+" headers_env", s.GetHeadersEnv(), s.GetUrl())...)
	}
	uses = append(uses, ParamUses("spec.harness.external.config", spec.GetHarness().GetExternal().GetConfig())...)
	uses = append(uses, EvaluatorUses(spec.GetEvaluators())...)
	return uses, nil
}

func targetUse(t *evalsiv1alpha1.Target) ([]Use, error) {
	field := "spec.target.api_key_env"
	switch t.GetConnector() {
	case "anthropic":
		if t.GetApiKeyEnv() == None {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("spec.target: the anthropic connector needs a key; api_key_env cannot be none"))
		}
		dest := t.GetBaseUrl()
		if dest == "" {
			dest = "https://api.anthropic.com"
		}
		return []Use{{Env: keyEnv(t.GetApiKeyEnv(), "ANTHROPIC_API_KEY"), Field: field, URL: dest}}, nil
	default:
		if env := keyEnv(t.GetApiKeyEnv(), "OPENAI_API_KEY"); env != "" {
			return []Use{{Env: env, Field: field, URL: t.GetBaseUrl()}}, nil
		}
		return nil, nil
	}
}

// keyEnv is the variable the worker reads for a key: the named one, the
// default when none is named, and nothing for "none".
func keyEnv(named, fallback string) string {
	switch named {
	case None:
		return ""
	case "":
		return fallback
	}
	return named
}

func headerUses(field string, headers map[string]string, dest string) []Use {
	var uses []Use
	for _, h := range sortedKeys(headers) {
		uses = append(uses, Use{Env: headers[h], Field: field + "." + h, URL: dest})
	}
	return uses
}

// EvaluatorUses lists the variables evaluator params name.
func EvaluatorUses(refs []*evalsiv1alpha1.EvaluatorRef) []Use {
	var uses []Use
	for i, r := range refs {
		name := r.GetName()
		if name == "" {
			name = r.GetRef()
		}
		uses = append(uses, ParamUses(fmt.Sprintf("evaluators[%d] (%s)", i, name), r.GetParams())...)
	}
	return uses
}

// ParamUses lists the variables a params object names. By convention a
// param ending in _env holds a variable name (a string) or header names
// mapped to variable names (an object); its value goes to the url or
// base_url param beside it.
func ParamUses(field string, params *structpb.Struct) []Use {
	if params == nil {
		return nil
	}
	fields := params.GetFields()
	dest := fields["url"].GetStringValue()
	if dest == "" {
		dest = fields["base_url"].GetStringValue()
	}
	var uses []Use
	for _, k := range sortedKeys(fields) {
		if !strings.HasSuffix(k, "_env") {
			continue
		}
		switch v := fields[k].GetKind().(type) {
		case *structpb.Value_StringValue:
			if v.StringValue != "" && v.StringValue != None {
				uses = append(uses, Use{Env: v.StringValue, Field: field + "." + k, URL: dest})
			}
		case *structpb.Value_StructValue:
			inner := v.StructValue.GetFields()
			for _, h := range sortedKeys(inner) {
				if s := inner[h].GetStringValue(); s != "" {
					uses = append(uses, Use{Env: s, Field: field + "." + k + "." + h, URL: dest})
				}
			}
		}
	}
	return uses
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

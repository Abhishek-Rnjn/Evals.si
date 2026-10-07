// Package credentials decides which of the worker's environment variables a
// request may name, and which judges it may use. Run specs, agents and
// evaluator params refer to secrets by variable name (api_key_env,
// headers_env, env_from), and the worker reads the value and sends it where
// the spec says. On a shared server that would let any caller who can create
// a run in any project send any variable the worker holds to a URL of their
// choosing, so names are granted per project, and a grant can limit the
// hosts its value may be sent to.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// None as api_key_env sends no key (a local model server, for example).
const None = "none"

// SecretKeyword marks a param in an evaluator's params schema as naming
// worker variables: {"x-evalsi-secret": {"sent_to": "<param with the URL>"}}.
const SecretKeyword = "x-evalsi-secret"

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
	// wildcard (*.openai.azure.com), over HTTPS (plain HTTP only to
	// loopback, or with allow_http). Empty: any host. A grant with hosts is
	// never copied into a sandbox (a CLI agent's env_from), whose traffic
	// evalsid does not see request by request.
	Hosts []string `json:"hosts,omitempty"`
	// Also allow plain HTTP to the hosts above (a model server on a trusted
	// network without TLS).
	AllowHTTP bool `json:"allow_http,omitempty"`
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
		if g.AllowHTTP && len(g.Hosts) == 0 {
			errs = append(errs, fmt.Errorf("%s.allow_http needs hosts (a grant without hosts allows any URL already)", at))
		}
	}
	return errors.Join(errs...)
}

// Settings is what the policy is made from; Update replaces them.
type Settings struct {
	Credentials Config
	// Judge name -> projects that may use it ("*" for all); a judge not
	// listed, or listed with no projects, is open to every project.
	Judges map[string][]string
	// Authentication decides whether grants are enforced by default.
	AuthEnabled bool
}

// Policy checks references and judges against the settings. Its methods
// are safe for concurrent use, and a nil Policy allows everything.
type Policy struct {
	mu      sync.RWMutex
	enforce bool
	grants  []Grant
	judges  map[string][]string
	denied  func(ctx context.Context, d Denial)
	version atomic.Uint64

	countMu sync.Mutex
	counts  map[[3]string]int64
}

// Denial describes one refused reference, for the audit log.
type Denial struct {
	Project string
	// "env:NAME" or "judge:NAME".
	Subject string
	Reason  string
}

// NewPolicy makes the policy.
func NewPolicy(s Settings) *Policy {
	p := &Policy{counts: map[[3]string]int64{}}
	p.Update(s)
	return p
}

// Update replaces the grants and judge scopes (a configuration reload).
func (p *Policy) Update(s Settings) {
	enforce := s.AuthEnabled || len(s.Credentials.Grants) > 0
	if s.Credentials.Enforce != nil {
		enforce = *s.Credentials.Enforce
	}
	judges := map[string][]string{}
	for name, projects := range s.Judges {
		if len(projects) > 0 {
			judges[name] = slices.Clone(projects)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enforce, p.grants, p.judges = enforce, slices.Clone(s.Credentials.Grants), judges
	p.version.Add(1)
}

// Version changes with every Update, so what was checked against older
// settings (a compiled guardrail) can tell it must be checked again.
func (p *Policy) Version() uint64 {
	if p == nil {
		return 0
	}
	return p.version.Load()
}

// OnDenied registers fn, called with every refusal.
func (p *Policy) OnDenied(fn func(ctx context.Context, d Denial)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = fn
}

// Enforced reports whether references are checked.
func (p *Policy) Enforced() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enforce
}

// Use is one reference to a worker variable.
type Use struct {
	Env string
	// Where in the request, for errors.
	Field string
	// The URL the value is sent to; empty when the request does not say
	// (an evaluator param without a destination).
	URL string
	// Copied into a sandbox rather than sent by the worker.
	Sandbox bool
}

// Check refuses uses the project has no grant for.
func (p *Policy) Check(ctx context.Context, project string, uses []Use) error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	enforce, grants := p.enforce, p.grants
	p.mu.RUnlock()
	if !enforce {
		return nil
	}
	for _, u := range uses {
		if err := check(grants, project, u); err != nil {
			p.deny(ctx, Denial{Project: project, Subject: "env:" + u.Env, Reason: err.Error()})
			return connect.NewError(connect.CodePermissionDenied, err)
		}
	}
	return nil
}

// CheckJudge refuses a judge not open to the project. Judge scopes apply
// whether or not grants are enforced: they are listed on purpose.
func (p *Policy) CheckJudge(ctx context.Context, project, judge, field string) error {
	if p == nil || judge == "" || p.JudgeAllowed(project, judge) {
		return nil
	}
	err := fmt.Errorf("%s uses judge %q, which project %q may not use; see judges.%s.projects in the server config", field, judge, project, judge)
	p.deny(ctx, Denial{Project: project, Subject: "judge:" + judge, Reason: err.Error()})
	return connect.NewError(connect.CodePermissionDenied, err)
}

// JudgeAllowed reports whether the project may use the judge.
func (p *Policy) JudgeAllowed(project, judge string) bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	projects, scoped := p.judges[judge]
	return !scoped || slices.Contains(projects, "*") || slices.Contains(projects, project)
}

// Grants returns the grants that apply to a project, and whether they are
// enforced. Names and hosts only: values never leave the worker.
func (p *Policy) Grants(project string) (bool, []Grant) {
	if p == nil {
		return false, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Grant
	for _, g := range p.grants {
		if slices.Contains(g.Projects, "*") || slices.Contains(g.Projects, project) {
			out = append(out, g)
		}
	}
	return p.enforce, out
}

func (p *Policy) deny(ctx context.Context, d Denial) {
	kind, _, _ := strings.Cut(d.Subject, ":")
	p.countMu.Lock()
	p.counts[[3]string{d.Project, kind, d.Subject}]++
	p.countMu.Unlock()
	p.mu.RLock()
	fn := p.denied
	p.mu.RUnlock()
	if fn != nil {
		fn(ctx, d)
	}
}

// WriteMetrics writes the refusal counter.
func (p *Policy) WriteMetrics(w io.Writer) {
	if p == nil {
		return
	}
	p.countMu.Lock()
	keys := make([][3]string, 0, len(p.counts))
	for k := range p.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.Join(keys[i][:], "\x00") < strings.Join(keys[j][:], "\x00") })
	values := make([]int64, len(keys))
	for i, k := range keys {
		values[i] = p.counts[k]
	}
	p.countMu.Unlock()
	const name = "evalsi_credential_denied_total"
	fmt.Fprintf(w, "# HELP %s Requests refused for naming a worker variable or judge their project may not use.\n# TYPE %s counter\n", name, name)
	for i, k := range keys {
		_, subject, _ := strings.Cut(k[2], ":")
		fmt.Fprintf(w, "%s{project=%q,kind=%q,name=%q} %d\n", name, k[0], k[1], subject, values[i])
	}
}

func check(grants []Grant, project string, u Use) error {
	var granted []Grant
	for _, g := range grants {
		if g.Env == u.Env && (slices.Contains(g.Projects, "*") || slices.Contains(g.Projects, project)) {
			granted = append(granted, g)
		}
	}
	if len(granted) == 0 {
		return fmt.Errorf("%s names the worker variable %s, which project %q may not use; "+
			"grant it under credentials.grants in the server config", u.Field, u.Env, project)
	}
	scheme, host := "", ""
	if u.URL != "" {
		scheme, host = destination(u.URL)
	}
	plain := false
	for _, g := range granted {
		if len(g.Hosts) == 0 {
			return nil
		}
		if u.Sandbox || host == "" || !slices.ContainsFunc(g.Hosts, func(h string) bool { return matchHost(h, host) }) {
			continue
		}
		if scheme == "https" || loopback(host) || g.AllowHTTP {
			return nil
		}
		plain = true
	}
	switch {
	case u.Sandbox:
		return fmt.Errorf("%s copies %s into a sandbox, but its grant limits it to hosts %s; "+
			"grant it without hosts to pass it to a CLI agent", u.Field, u.Env, hostList(granted))
	case host == "":
		return fmt.Errorf("%s names %s without a URL to send it to, but its grant limits it to hosts %s",
			u.Field, u.Env, hostList(granted))
	case plain:
		return fmt.Errorf("%s would send %s to %s over plain %s; use https (or set allow_http on its grant)",
			u.Field, u.Env, host, scheme)
	default:
		return fmt.Errorf("%s would send %s to %s, which its grant does not allow (hosts %s)",
			u.Field, u.Env, host, hostList(granted))
	}
}

func destination(raw string) (scheme, host string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ""
	}
	return strings.ToLower(u.Scheme), strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// SpecUses lists the worker variables a run spec names outside its
// evaluators: the target's key (the connector's default variable when
// api_key_env is empty, since the worker falls back to it), an agent's
// headers, key or env_from, MCP servers' headers, and external-harness
// config. Evaluator params are listed with their manifests (ParamUses).
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
	uses = append(uses, ParamUses("spec.harness.external.config", spec.GetHarness().GetExternal().GetConfig(), nil)...)
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

// ParamUses lists the variables a params object names. A param names
// variables when the params schema declares it (SecretKeyword, whose
// sent_to names the param holding the destination URL), or, declared or
// not, when its name ends in _env (destination: the url or base_url param).
// Its value is a variable name, or header names mapped to variable names.
func ParamUses(field string, params, schema *structpb.Struct) []Use {
	if params == nil {
		return nil
	}
	fields := params.GetFields()
	declared := Declared(schema)
	var uses []Use
	for _, k := range sortedKeys(fields) {
		sentTo, ok := declared[k]
		if !ok && !strings.HasSuffix(k, "_env") {
			continue
		}
		dest := ""
		if ok && sentTo != "" {
			dest = fields[sentTo].GetStringValue()
		} else if !ok {
			dest = fields["url"].GetStringValue()
			if dest == "" {
				dest = fields["base_url"].GetStringValue()
			}
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

// Declared returns the params a schema declares as naming worker variables,
// each with the param that holds its destination ("" when none).
func Declared(schema *structpb.Struct) map[string]string {
	out := map[string]string{}
	for name, prop := range schema.GetFields()["properties"].GetStructValue().GetFields() {
		mark, ok := prop.GetStructValue().GetFields()[SecretKeyword]
		if !ok {
			continue
		}
		out[name] = mark.GetStructValue().GetFields()["sent_to"].GetStringValue()
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

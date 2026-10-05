package authz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"golang.org/x/net/http2"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// ExtAuthzConfig sends authorization decisions to a central policy engine.
type ExtAuthzConfig struct {
	// OpenID AuthZEN Access Evaluation API.
	AuthZEN *AuthZENConfig `json:"authzen,omitempty"`
	// Envoy's ext_authz gRPC protocol (envoy.service.auth.v3.Authorization),
	// which OPA's Envoy plugin also serves.
	Envoy *EnvoyConfig `json:"envoy,omitempty"`
	// "require" (default): the local decision and the external one must both
	// allow. "decide": after the local deny and require rules, the external
	// decision is final, so roles need not be defined locally.
	Mode string `json:"mode,omitempty"`
	// Per check. Default 200ms. A timeout or any error denies (fails closed).
	Timeout string `json:"timeout,omitempty"`
	// How long decisions are cached. Default 30s; "0s" disables the cache.
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// AuthZENConfig is an AuthZEN policy decision point.
type AuthZENConfig struct {
	// The evaluation endpoint, for example https://pdp.example.com/access/v1/evaluation.
	URL string `json:"url"`
	// Environment variable holding a bearer token for the PDP.
	TokenEnv string `json:"token_env,omitempty"`
}

// EnvoyConfig is an ext_authz gRPC server.
type EnvoyConfig struct {
	// host:port.
	Address string `json:"address"`
	// Use TLS to reach it (default plaintext HTTP/2).
	TLS bool `json:"tls,omitempty"`
}

func (c ExtAuthzConfig) validate() error {
	var errs []error
	if (c.AuthZEN == nil) == (c.Envoy == nil) {
		errs = append(errs, errors.New("set exactly one of authzen or envoy"))
	}
	if c.AuthZEN != nil && !strings.HasPrefix(c.AuthZEN.URL, "https://") && !strings.HasPrefix(c.AuthZEN.URL, "http://") {
		errs = append(errs, errors.New("authzen.url must be an http(s) URL"))
	}
	if c.Envoy != nil {
		if _, _, err := net.SplitHostPort(c.Envoy.Address); err != nil {
			errs = append(errs, fmt.Errorf("envoy.address: %w", err))
		}
	}
	if c.Mode != "" && c.Mode != "require" && c.Mode != "decide" {
		errs = append(errs, errors.New(`mode must be "require" or "decide"`))
	}
	for name, d := range map[string]string{"timeout": c.Timeout, "cache_ttl": c.CacheTTL} {
		if d == "" {
			continue
		}
		if _, err := time.ParseDuration(d); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// ExtAuthorizer combines the local decision with an external one.
type ExtAuthorizer interface {
	Decide(ctx context.Context, p *auth.Principal, req Request, local Decision) Decision
}

// checker asks the external engine; true means allow.
type checker interface {
	check(ctx context.Context, p *auth.Principal, req Request, local Decision) (bool, error)
	name() string
}

type extAuthorizer struct {
	checker checker
	decide  bool
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	allowed bool
	until   time.Time
}

// NewExtAuthorizer builds the configured external authorizer.
func NewExtAuthorizer(c ExtAuthzConfig) (ExtAuthorizer, error) {
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("authorization.ext_authz: %w", err)
	}
	e := &extAuthorizer{decide: c.Mode == "decide", timeout: 200 * time.Millisecond, ttl: 30 * time.Second, now: time.Now, cache: map[string]cached{}}
	if c.Timeout != "" {
		e.timeout, _ = time.ParseDuration(c.Timeout)
	}
	if c.CacheTTL != "" {
		e.ttl, _ = time.ParseDuration(c.CacheTTL)
	}
	switch {
	case c.AuthZEN != nil:
		e.checker = &authzen{url: c.AuthZEN.URL, tokenEnv: c.AuthZEN.TokenEnv, client: &http.Client{}}
	default:
		e.checker = newEnvoyChecker(*c.Envoy)
	}
	return e, nil
}

func (e *extAuthorizer) Decide(ctx context.Context, p *auth.Principal, req Request, local Decision) Decision {
	if !e.decide && !local.Allowed {
		return deny("no role or rule grants %s in project %q", req.Action, req.Project)
	}
	key := cacheKey(p, req, local, e.decide)
	now := e.now()
	e.mu.Lock()
	c, hit := e.cache[key]
	e.mu.Unlock()
	if hit && now.Before(c.until) {
		return e.result(c.allowed, local, " (cached)")
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	ok, err := e.checker.check(ctx, p, req, local)
	if err != nil {
		return deny("external authorization (%s) failed, failing closed: %v", e.checker.name(), err)
	}
	if e.ttl > 0 {
		e.mu.Lock()
		if len(e.cache) > 10000 {
			e.cache = map[string]cached{}
		}
		e.cache[key] = cached{allowed: ok, until: now.Add(e.ttl)}
		e.mu.Unlock()
	}
	return e.result(ok, local, "")
}

func (e *extAuthorizer) result(ok bool, local Decision, suffix string) Decision {
	switch {
	case !ok:
		return deny("external authorization (%s) denied%s", e.checker.name(), suffix)
	case e.decide:
		return allow("external authorization (%s) allowed%s", e.checker.name(), suffix)
	default:
		return allow("%s, and external authorization (%s) allowed%s", local.Reason, e.checker.name(), suffix)
	}
}

func cacheKey(p *auth.Principal, req Request, local Decision, decide bool) string {
	raw, _ := json.Marshal([]any{p.ID(), p.Groups, p.Labels, req.Action, req.Project, req.Resource, local.Allowed && decide})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// subjectJSON is the principal as sent to external engines: identity, never credentials.
func subjectJSON(p *auth.Principal) map[string]any {
	props := p.Vars()
	delete(props, "id")
	return map[string]any{"type": p.Kind, "id": p.ID(), "properties": props}
}

type authzen struct {
	url, tokenEnv string
	client        *http.Client
}

func (a *authzen) name() string { return "authzen" }

func (a *authzen) check(ctx context.Context, p *auth.Principal, req Request, local Decision) (bool, error) {
	resource := map[string]any{}
	for k, v := range req.Resource {
		resource[k] = v
	}
	body, err := json.Marshal(map[string]any{
		"subject":  subjectJSON(p),
		"action":   map[string]any{"name": req.Action},
		"resource": map[string]any{"type": "evalsi.project", "id": req.Project, "properties": resource},
		"context":  map[string]any{"procedure": req.Procedure, "protocol": req.Protocol, "source": req.Source, "local_decision": local.Allowed},
	})
	if err != nil {
		return false, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.tokenEnv != "" {
		httpReq.Header.Set("Authorization", "Bearer "+os.Getenv(a.tokenEnv))
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s", resp.Status)
	}
	var out struct {
		Decision *bool `json:"decision"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Decision == nil {
		return false, errors.New("response has no boolean decision")
	}
	return *out.Decision, nil
}

type envoyChecker struct {
	client *connect.Client[authv3.CheckRequest, authv3.CheckResponse]
}

func newEnvoyChecker(c EnvoyConfig) *envoyChecker {
	scheme := "http://"
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	if c.TLS {
		scheme = "https://"
		transport = &http2.Transport{}
	}
	return &envoyChecker{client: connect.NewClient[authv3.CheckRequest, authv3.CheckResponse](
		&http.Client{Transport: transport},
		scheme+c.Address+"/envoy.service.auth.v3.Authorization/Check",
		connect.WithGRPC(),
	)}
}

func (e *envoyChecker) name() string { return "ext_authz" }

func (e *envoyChecker) check(ctx context.Context, p *auth.Principal, req Request, local Decision) (bool, error) {
	principal, _ := json.Marshal(subjectJSON(p))
	resource, _ := json.Marshal(req.Resource)
	headers := map[string]string{}
	for k, v := range req.Headers {
		if lk := strings.ToLower(k); !redactedHeaders[lk] {
			headers[lk] = strings.Join(v, ",")
		}
	}
	host, port := req.Source, "0"
	if h, pt, err := net.SplitHostPort(req.Source); err == nil {
		host, port = h, pt
	}
	var portNum uint32
	_, _ = fmt.Sscan(port, &portNum)
	resp, err := e.client.CallUnary(ctx, connect.NewRequest(&authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Source: &authv3.AttributeContext_Peer{
				Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
					Address: host, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: portNum},
				}}},
				Principal: p.ID(),
			},
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Method: http.MethodPost, Path: req.Procedure, Headers: headers, Protocol: req.Protocol,
			}},
			ContextExtensions: map[string]string{
				"evalsi.action":         req.Action,
				"evalsi.project":        req.Project,
				"evalsi.principal":      string(principal),
				"evalsi.resource":       string(resource),
				"evalsi.local_decision": fmt.Sprint(local.Allowed),
			},
		},
	}))
	if err != nil {
		return false, err
	}
	return resp.Msg.GetStatus().GetCode() == 0, nil
}

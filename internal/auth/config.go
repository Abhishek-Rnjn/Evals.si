package auth

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Modes, per authentication method, as in agentgateway.
const (
	// ModeStrict requires a valid credential.
	ModeStrict = "strict"
	// ModeOptional validates a credential when present; requests without one
	// reach authorization as anonymous.
	ModeOptional = "optional"
	// ModePermissive decodes credentials for policy use and never rejects.
	// Meant for migrations only.
	ModePermissive = "permissive"
	// ModeNone (top level only) turns authentication off explicitly.
	ModeNone = "none"
)

// Config is the auth section of evalsi.yaml.
type Config struct {
	// "none" turns authentication off; anything else (or empty) enables the
	// methods configured below.
	Mode    string         `json:"mode,omitempty"`
	JWT     *JWTConfig     `json:"jwt,omitempty"`
	APIKeys *APIKeysConfig `json:"api_keys,omitempty"`
	// TLS on the API listener, and client certificates (mTLS) as credentials.
	TLS *TLSConfig `json:"tls,omitempty"`
	// Bearer credentials over plaintext on a non-loopback address need this,
	// which states that TLS terminates in front of evalsid.
	AllowPlaintext bool `json:"allow_plaintext,omitempty"`
	// Identity headers set by a proxy, trusted only from these networks.
	TrustedProxy *ProxyConfig `json:"trusted_proxy,omitempty"`
	// Advertised to `evalsi login` at /.well-known/evalsi-auth.
	CLILogin *CLILogin `json:"cli_login,omitempty"`
	// Largest accepted token, in bytes. Default 16384.
	MaxTokenBytes int `json:"max_token_bytes,omitempty"`
}

// JWTConfig verifies bearer JWTs from one or more OIDC providers.
type JWTConfig struct {
	Mode      string     `json:"mode,omitempty"`
	Location  *Location  `json:"location,omitempty"`
	Providers []Provider `json:"providers"`
}

// Location says where a request carries its token. Default: the
// Authorization header with a "Bearer " prefix.
type Location struct {
	Header *HeaderLocation `json:"header,omitempty"`
	Cookie string          `json:"cookie,omitempty"`
	Query  string          `json:"query,omitempty"`
}

// HeaderLocation is a header name and the prefix before the token.
type HeaderLocation struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix,omitempty"`
}

// Provider is one trusted token issuer.
type Provider struct {
	Name      string   `json:"name"`
	Issuer    string   `json:"issuer"`
	Audiences []string `json:"audiences"`
	JWKS      JWKS     `json:"jwks"`
	// Which claims hold the principal's facts. Defaults: sub, name (or
	// preferred_username), email, groups.
	Claims ClaimMapping `json:"claims,omitempty"`
	// Claims every token must carry. Default [exp].
	RequiredClaims []string `json:"required_claims,omitempty"`
	// Accepted signature algorithms. Default RS256, ES256, EdDSA. "none" is never accepted.
	Algorithms []string `json:"algorithms,omitempty"`
	// Allowed clock difference for exp, nbf and iat. Default 60s.
	ClockSkew string `json:"clock_skew,omitempty"`
	// Map application roles in a claim to Evals.si roles.
	RoleClaims *RoleClaims `json:"role_claims,omitempty"`
	// "user" (default) or "service": the kind of principal tokens make.
	Kind string `json:"kind,omitempty"`
	// Trust a Kubernetes cluster's service-account tokens (projected with
	// one of the audiences). Keys come from the API server itself, so jwks
	// is left unset, and the issuer is discovered when empty.
	Kubernetes *KubernetesIssuer `json:"kubernetes,omitempty"`
}

// KubernetesIssuer is the cluster whose service accounts are principals:
// system:serviceaccount:<namespace>:<name>, kind service, in the groups
// system:serviceaccounts and system:serviceaccounts:<namespace>.
type KubernetesIssuer struct {
	// Default: in-cluster (https://kubernetes.default.svc).
	APIServer string `json:"api_server,omitempty"`
	// Defaults: this pod's service-account CA and token, which the API
	// server's key endpoints need.
	CAFile    string `json:"ca_file,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	// Namespaces whose service accounts are accepted; default: all.
	Namespaces []string `json:"namespaces,omitempty"`
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func (k *KubernetesIssuer) apiServer() string {
	if k.APIServer != "" {
		return strings.TrimSuffix(k.APIServer, "/")
	}
	return "https://kubernetes.default.svc"
}

func (k *KubernetesIssuer) caFile() string { return or(k.CAFile, serviceAccountDir+"/ca.crt") }

func (k *KubernetesIssuer) tokenFile() string { return or(k.TokenFile, serviceAccountDir+"/token") }

// JWKS says where a provider's signing keys come from; exactly one is set.
type JWKS struct {
	URL    string          `json:"url,omitempty"`
	File   string          `json:"file,omitempty"`
	Inline json.RawMessage `json:"inline,omitempty"`
	// Discover jwks_uri from the issuer's /.well-known/openid-configuration.
	Discovery bool `json:"discovery,omitempty"`
	// Allow http:// JWKS and discovery URLs. For local development only.
	AllowInsecure bool `json:"allow_insecure,omitempty"`
	// How often keys are refreshed. Default 1h.
	Refresh string `json:"refresh,omitempty"`
}

// ClaimMapping names the claims that hold a principal's facts. Nested claims
// use dots, for example "realm_access.roles".
type ClaimMapping struct {
	Subject string `json:"subject,omitempty"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Groups  string `json:"groups,omitempty"`
}

// RoleClaims maps values of a token claim to roles per project: Map[value][project] = roles.
type RoleClaims struct {
	Claim string                         `json:"claim"`
	Map   map[string]map[string][]string `json:"map"`
}

// APIKeysConfig configures API keys. Keys created through AuthService are
// stored hashed in the database; these are declared in config.
type APIKeysConfig struct {
	Mode string      `json:"mode,omitempty"`
	Keys []ConfigKey `json:"keys,omitempty"`
}

// ConfigKey is an API key declared in config by its hash.
type ConfigKey struct {
	Name string `json:"name"`
	// "sha256:<hex>" of the plaintext key; the plaintext never appears in config.
	Key    string              `json:"key"`
	Roles  map[string][]string `json:"roles,omitempty"`
	Labels map[string]string   `json:"labels,omitempty"`
	// RFC 3339; the key is rejected after it.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// TLSConfig terminates TLS on the API listener. Files are reloaded when they change.
type TLSConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// CA bundle for client certificates. A verified certificate's SPIFFE ID
	// (or subject common name) becomes the principal.
	ClientCA string `json:"client_ca,omitempty"`
	// Reject connections without a client certificate.
	RequireClientCert bool `json:"require_client_cert,omitempty"`
}

// ProxyConfig trusts identity headers from a fronting proxy. Discouraged:
// prefer forwarding the token (agentgateway's preserveToken).
type ProxyConfig struct {
	CIDRs        []string `json:"cidrs"`
	UserHeader   string   `json:"user_header"`
	GroupsHeader string   `json:"groups_header,omitempty"`
	EmailHeader  string   `json:"email_header,omitempty"`
}

// CLILogin is what `evalsi login` needs to sign in.
type CLILogin struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes,omitempty"`
	Audience string   `json:"audience,omitempty"`
}

// Enabled reports whether requests are authenticated at all.
func (c *Config) Enabled() bool { return c != nil && c.Mode != ModeNone }

// BearerMethods reports whether credentials travel in headers, which plaintext would expose.
func (c *Config) BearerMethods() bool {
	return c.Enabled() && (c.JWT != nil || c.APIKeys != nil || c.TrustedProxy != nil)
}

func (c *Config) maxToken() int {
	if c.MaxTokenBytes > 0 {
		return c.MaxTokenBytes
	}
	return 16384
}

// DefaultAlgorithms are accepted when a provider names none.
var DefaultAlgorithms = []string{"RS256", "ES256", "EdDSA"}

var knownAlgorithms = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true, "PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true, "EdDSA": true,
}

func validMode(m string) bool {
	return m == "" || m == ModeStrict || m == ModeOptional || m == ModePermissive
}

// Validate checks the section for mistakes that would otherwise surface on the first request.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var errs []error
	if c.Mode != "" && c.Mode != ModeNone {
		errs = append(errs, fmt.Errorf("auth.mode must be empty or %q", ModeNone))
	}
	if c.Mode == ModeNone {
		return errors.Join(errs...)
	}
	if c.JWT == nil && c.APIKeys == nil && c.TrustedProxy == nil && (c.TLS == nil || c.TLS.ClientCA == "") {
		errs = append(errs, errors.New("auth: configure jwt, api_keys, tls.client_ca or trusted_proxy, or set mode: none"))
	}
	if c.JWT != nil {
		if !validMode(c.JWT.Mode) {
			errs = append(errs, fmt.Errorf("auth.jwt.mode %q: use strict, optional or permissive", c.JWT.Mode))
		}
		if len(c.JWT.Providers) == 0 {
			errs = append(errs, errors.New("auth.jwt.providers: at least one provider is required"))
		}
		if l := c.JWT.Location; l != nil {
			n := 0
			for _, set := range []bool{l.Header != nil, l.Cookie != "", l.Query != ""} {
				if set {
					n++
				}
			}
			if n != 1 {
				errs = append(errs, errors.New("auth.jwt.location: set exactly one of header, cookie or query"))
			}
			if l.Header != nil && l.Header.Name == "" {
				errs = append(errs, errors.New("auth.jwt.location.header.name is required"))
			}
		}
		names := map[string]bool{}
		for i, p := range c.JWT.Providers {
			if err := p.validate(); err != nil {
				errs = append(errs, fmt.Errorf("auth.jwt.providers[%d]: %w", i, err))
			}
			if names[p.Name] {
				errs = append(errs, fmt.Errorf("auth.jwt.providers[%d]: duplicate name %q", i, p.Name))
			}
			names[p.Name] = true
		}
	}
	if c.APIKeys != nil {
		if !validMode(c.APIKeys.Mode) {
			errs = append(errs, fmt.Errorf("auth.api_keys.mode %q: use strict, optional or permissive", c.APIKeys.Mode))
		}
		names := map[string]bool{}
		for i, k := range c.APIKeys.Keys {
			if err := k.validate(); err != nil {
				errs = append(errs, fmt.Errorf("auth.api_keys.keys[%d]: %w", i, err))
			}
			if names[k.Name] {
				errs = append(errs, fmt.Errorf("auth.api_keys.keys[%d]: duplicate name %q", i, k.Name))
			}
			names[k.Name] = true
		}
	}
	if t := c.TLS; t != nil && (t.CertFile == "" || t.KeyFile == "") {
		errs = append(errs, errors.New("auth.tls: cert_file and key_file are required"))
	}
	if p := c.TrustedProxy; p != nil {
		if len(p.CIDRs) == 0 || p.UserHeader == "" {
			errs = append(errs, errors.New("auth.trusted_proxy: cidrs and user_header are required"))
		}
		for _, cidr := range p.CIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				errs = append(errs, fmt.Errorf("auth.trusted_proxy.cidrs: %w", err))
			}
		}
	}
	if l := c.CLILogin; l != nil && (l.Issuer == "" || l.ClientID == "") {
		errs = append(errs, errors.New("auth.cli_login: issuer and client_id are required"))
	}
	return errors.Join(errs...)
}

func (p Provider) validate() error {
	var errs []error
	if p.Name == "" || strings.ContainsAny(p.Name, "/: ") {
		errs = append(errs, errors.New("name is required and cannot contain '/', ':' or spaces"))
	}
	if p.Issuer == "" && p.Kubernetes == nil {
		errs = append(errs, errors.New("issuer is required"))
	}
	if len(p.Audiences) == 0 {
		errs = append(errs, errors.New("audiences: at least one is required, so tokens minted for other services are refused"))
	}
	n := 0
	for _, set := range []bool{p.JWKS.URL != "", p.JWKS.File != "", len(p.JWKS.Inline) > 0, p.JWKS.Discovery} {
		if set {
			n++
		}
	}
	switch {
	case p.Kubernetes != nil && n != 0:
		errs = append(errs, errors.New("jwks: leave unset with kubernetes; keys come from the API server"))
	case p.Kubernetes == nil && n != 1:
		errs = append(errs, errors.New("jwks: set exactly one of url, file, inline or discovery"))
	}
	if k := p.Kubernetes; k != nil && !strings.HasPrefix(k.apiServer(), "https://") {
		errs = append(errs, errors.New("kubernetes.api_server must use https"))
	}
	for _, u := range []string{p.JWKS.URL, p.discoveryURL()} {
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("jwks: %w", err))
			continue
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && p.JWKS.AllowInsecure) {
			errs = append(errs, fmt.Errorf("jwks: %s must use https (allow_insecure is for local development)", u))
		}
	}
	for _, a := range p.Algorithms {
		if !knownAlgorithms[a] {
			errs = append(errs, fmt.Errorf("algorithms: %q is not supported", a))
		}
	}
	for name, d := range map[string]string{"clock_skew": p.ClockSkew, "jwks.refresh": p.JWKS.Refresh} {
		if d == "" {
			continue
		}
		if _, err := time.ParseDuration(d); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if p.Kind != "" && p.Kind != KindUser && p.Kind != KindService {
		errs = append(errs, errors.New("kind must be user or service"))
	}
	if rc := p.RoleClaims; rc != nil && rc.Claim == "" {
		errs = append(errs, errors.New("role_claims.claim is required"))
	}
	return errors.Join(errs...)
}

func (p Provider) discoveryURL() string {
	if !p.JWKS.Discovery {
		return ""
	}
	return strings.TrimSuffix(p.Issuer, "/") + "/.well-known/openid-configuration"
}

func (k ConfigKey) validate() error {
	if k.Name == "" {
		return errors.New("name is required")
	}
	if _, err := ParseKeyHash(k.Key); err != nil {
		return err
	}
	if k.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, k.ExpiresAt); err != nil {
			return fmt.Errorf("expires_at: %w", err)
		}
	}
	return nil
}

// ParseKeyHash parses "sha256:<64 hex digits>" and returns the lower-case hex.
func ParseKeyHash(s string) (string, error) {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok {
		return "", errors.New(`key must be "sha256:<hex>"; never put the plaintext key in config`)
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		return "", errors.New("key: sha256 hash must be 64 hex digits")
	}
	return strings.ToLower(h), nil
}

// IsLoopback reports whether a listen address only accepts local connections.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

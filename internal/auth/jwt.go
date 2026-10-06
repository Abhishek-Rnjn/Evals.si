package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// minRefetch rate-limits JWKS fetches triggered by unknown key ids, so tokens
// with made-up kids cannot turn evalsid into a request amplifier.
const minRefetch = 10 * time.Second

// keySource holds one provider's signing keys, fetching and refreshing them
// as needed. Static keys (file, inline) are re-read on the refresh interval.
type keySource struct {
	p       Provider
	client  *http.Client
	refresh time.Duration
	now     func() time.Time

	mu       sync.Mutex
	keys     *jose.JSONWebKeySet
	fetched  time.Time
	attempts time.Time
	jwksURL  string
}

func newKeySource(p Provider, client *http.Client) (*keySource, error) {
	ks := &keySource{p: p, client: client, refresh: time.Hour, now: time.Now}
	if p.JWKS.Refresh != "" {
		ks.refresh, _ = time.ParseDuration(p.JWKS.Refresh)
	}
	if len(p.JWKS.Inline) > 0 || p.JWKS.File != "" {
		// Static keys must parse at startup: a typo should stop the server, not every request.
		if err := ks.load(context.Background()); err != nil {
			return nil, fmt.Errorf("provider %s: %w", p.Name, err)
		}
	}
	return ks, nil
}

// lookup returns the keys that may have signed a token with this key id.
func (k *keySource) lookup(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	stale := k.keys == nil || now.Sub(k.fetched) > k.refresh
	if !stale && kid != "" && len(k.keys.Key(kid)) == 0 && now.Sub(k.attempts) > minRefetch {
		stale = true // a rotated key: refetch, rate-limited
	}
	if stale && (k.keys == nil || now.Sub(k.attempts) > minRefetch) {
		k.attempts = now
		if err := k.loadLocked(ctx); err != nil && k.keys == nil {
			return nil, err
		}
	}
	if k.keys == nil {
		return nil, errors.New("no signing keys")
	}
	if kid != "" {
		return k.keys.Key(kid), nil
	}
	return k.keys.Keys, nil
}

func (k *keySource) load(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.loadLocked(ctx)
}

func (k *keySource) loadLocked(ctx context.Context) error {
	var raw []byte
	var err error
	switch {
	case len(k.p.JWKS.Inline) > 0:
		raw = k.p.JWKS.Inline
	case k.p.JWKS.File != "":
		raw, err = os.ReadFile(k.p.JWKS.File)
	default:
		if k.jwksURL == "" {
			k.jwksURL = k.p.JWKS.URL
			if kube := k.p.Kubernetes; kube != nil {
				// The API server's own endpoint: the discovery document's
				// jwks_uri is often an address only reachable from outside.
				k.jwksURL = kube.apiServer() + "/openid/v1/jwks"
			}
			if k.p.JWKS.Discovery {
				if k.jwksURL, err = k.discover(ctx); err != nil {
					return err
				}
			}
		}
		raw, err = k.get(ctx, k.jwksURL)
	}
	if err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	set := &jose.JSONWebKeySet{}
	if err := json.Unmarshal(raw, set); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	for _, key := range set.Keys {
		if !key.IsPublic() {
			return errors.New("jwks: contains a private key; publish only public keys")
		}
	}
	k.keys, k.fetched = set, k.now()
	return nil
}

func (k *keySource) discover(ctx context.Context) (string, error) {
	raw, err := k.get(ctx, k.p.discoveryURL())
	if err != nil {
		return "", fmt.Errorf("oidc discovery: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("oidc discovery: %w", err)
	}
	if strings.TrimSuffix(doc.Issuer, "/") != strings.TrimSuffix(k.p.Issuer, "/") {
		return "", fmt.Errorf("oidc discovery: issuer %q does not match %q", doc.Issuer, k.p.Issuer)
	}
	if !strings.HasPrefix(doc.JWKSURI, "https://") && !(k.p.JWKS.AllowInsecure && strings.HasPrefix(doc.JWKSURI, "http://")) {
		return "", fmt.Errorf("oidc discovery: jwks_uri %q must use https", doc.JWKSURI)
	}
	return doc.JWKSURI, nil
}

func (k *keySource) get(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if kube := k.p.Kubernetes; kube != nil {
		// Read per fetch: the kubelet rotates projected tokens.
		token, err := os.ReadFile(kube.tokenFile())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// jwtProvider verifies tokens from one issuer.
type jwtProvider struct {
	cfg      Provider
	keys     *keySource
	algs     []jose.SignatureAlgorithm
	skew     time.Duration
	required []string
}

func newJWTProvider(p Provider, client *http.Client) (*jwtProvider, error) {
	if p.Kubernetes != nil {
		var err error
		if client, err = kubernetesClient(p.Kubernetes, client); err != nil {
			return nil, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		if p.Kind == "" {
			p.Kind = KindService
		}
		if p.Issuer == "" {
			if p.Issuer, err = kubernetesIssuer(p, client); err != nil {
				return nil, fmt.Errorf("provider %s: %w", p.Name, err)
			}
		}
	}
	ks, err := newKeySource(p, client)
	if err != nil {
		return nil, err
	}
	jp := &jwtProvider{cfg: p, keys: ks, skew: time.Minute, required: p.RequiredClaims}
	names := p.Algorithms
	if len(names) == 0 {
		names = DefaultAlgorithms
	}
	for _, a := range names {
		jp.algs = append(jp.algs, jose.SignatureAlgorithm(a))
	}
	if p.ClockSkew != "" {
		jp.skew, _ = time.ParseDuration(p.ClockSkew)
	}
	if len(jp.required) == 0 {
		jp.required = []string{"exp"}
	}
	return jp, nil
}

// errNoProvider means no configured provider trusts the token's issuer.
var errNoProvider = errors.New("token issuer is not trusted")

// jwtVerifier tries the providers that trust a token's issuer.
type jwtVerifier struct {
	providers []*jwtProvider
	now       func() time.Time
}

func (v *jwtVerifier) verify(ctx context.Context, raw string) (map[string]any, *jwtProvider, error) {
	var algs []jose.SignatureAlgorithm
	for _, p := range v.providers {
		for _, a := range p.algs {
			if !slices.Contains(algs, a) {
				algs = append(algs, a)
			}
		}
	}
	tok, err := jwt.ParseSigned(raw, algs)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed token: %w", err)
	}
	var unverified struct {
		Issuer string `json:"iss"`
	}
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return nil, nil, fmt.Errorf("malformed token: %w", err)
	}
	header := tok.Headers[0]
	var lastErr error = errNoProvider
	for _, p := range v.providers {
		if p.cfg.Issuer != unverified.Issuer {
			continue
		}
		if !slices.Contains(p.algs, jose.SignatureAlgorithm(header.Algorithm)) {
			lastErr = fmt.Errorf("algorithm %s is not accepted", header.Algorithm)
			continue
		}
		claims, err := p.verify(ctx, tok, header.KeyID, v.now())
		if err == nil {
			return claims, p, nil
		}
		lastErr = err
	}
	return nil, nil, lastErr
}

func (p *jwtProvider) verify(ctx context.Context, tok *jwt.JSONWebToken, kid string, now time.Time) (map[string]any, error) {
	keys, err := p.keys.lookup(ctx, kid)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	var std jwt.Claims
	var claims map[string]any
	verified := false
	for _, k := range keys {
		if err := tok.Claims(k.Key, &std, &claims); err == nil {
			verified = true
			break
		}
	}
	if !verified {
		return nil, errors.New("invalid signature")
	}
	expected := jwt.Expected{Issuer: p.cfg.Issuer, AnyAudience: jwt.Audience(p.cfg.Audiences), Time: now}
	if err := std.ValidateWithLeeway(expected, p.skew); err != nil {
		return nil, err
	}
	if std.IssuedAt != nil && std.IssuedAt.Time().After(now.Add(p.skew)) {
		return nil, errors.New("token issued in the future")
	}
	for _, c := range p.required {
		if _, ok := claims[c]; !ok {
			return nil, fmt.Errorf("missing required claim %q", c)
		}
	}
	if kube := p.cfg.Kubernetes; kube != nil {
		ns, sa := serviceAccount(claims)
		if ns == "" || sa == "" {
			return nil, errors.New("not a service-account token")
		}
		if len(kube.Namespaces) > 0 && !slices.Contains(kube.Namespaces, ns) {
			return nil, fmt.Errorf("service accounts of namespace %q are not trusted", ns)
		}
	}
	return claims, nil
}

// serviceAccount reads a service-account token's namespace and name.
func serviceAccount(claims map[string]any) (namespace, name string) {
	k8s, _ := claims["kubernetes.io"].(map[string]any)
	if k8s == nil {
		return "", ""
	}
	namespace, _ = k8s["namespace"].(string)
	sa, _ := k8s["serviceaccount"].(map[string]any)
	name, _ = sa["name"].(string)
	return namespace, name
}

// kubernetesClient trusts the cluster's CA, on top of client's settings.
func kubernetesClient(k *KubernetesIssuer, client *http.Client) (*http.Client, error) {
	pem, err := os.ReadFile(k.caFile())
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificates", k.caFile())
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: client.Timeout}, nil
}

// kubernetesIssuer reads the service-account issuer from the API server's
// discovery document.
func kubernetesIssuer(p Provider, client *http.Client) (string, error) {
	k := &keySource{p: p, client: client}
	raw, err := k.get(context.Background(), p.Kubernetes.apiServer()+"/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("service-account issuer discovery: %w", err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("service-account issuer discovery: no issuer (%v)", err)
	}
	return doc.Issuer, nil
}

// principal builds a principal from verified claims.
func (p *jwtProvider) principal(claims map[string]any) *Principal {
	m := p.cfg.Claims
	pr := &Principal{
		Kind: KindUser, Method: MethodJWT, Provider: p.cfg.Name, Claims: claims,
		Subject: claimString(claims, or(m.Subject, "sub")),
		Email:   claimString(claims, or(m.Email, "email")),
		Groups:  claimStrings(claims, or(m.Groups, "groups")),
	}
	if p.cfg.Kind != "" {
		pr.Kind = p.cfg.Kind
	}
	if p.cfg.Kubernetes != nil {
		ns, sa := serviceAccount(claims)
		pr.Name = ns + "/" + sa
		pr.Groups = append(pr.Groups, "system:serviceaccounts", "system:serviceaccounts:"+ns)
		return pr
	}
	if m.Name != "" {
		pr.Name = claimString(claims, m.Name)
	} else {
		pr.Name = or(claimString(claims, "name"), claimString(claims, "preferred_username"))
	}
	pr.EmailVerified, _ = claims["email_verified"].(bool)
	if rc := p.cfg.RoleClaims; rc != nil {
		for _, value := range claimStrings(claims, rc.Claim) {
			for project, roles := range rc.Map[value] {
				if pr.ClaimRoles == nil {
					pr.ClaimRoles = map[string][]string{}
				}
				for _, r := range roles {
					if !slices.Contains(pr.ClaimRoles[project], r) {
						pr.ClaimRoles[project] = append(pr.ClaimRoles[project], r)
					}
				}
			}
		}
	}
	return pr
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// claimValue finds a claim by name, then by dotted path into nested objects.
func claimValue(claims map[string]any, name string) any {
	if v, ok := claims[name]; ok {
		return v
	}
	var cur any = claims
	for _, part := range strings.Split(name, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return cur
}

func claimString(claims map[string]any, name string) string {
	switch v := claimValue(claims, name).(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

// claimStrings reads a list claim; a string claim is split on spaces and commas.
func claimStrings(claims map[string]any, name string) []string {
	switch v := claimValue(claims, name).(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
	}
	return nil
}

// unverifiedClaims decodes a token's claims without checking anything, for
// permissive mode only.
func unverifiedClaims(raw string) map[string]any {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
		jose.ES256, jose.ES384, jose.ES512, jose.EdDSA, jose.HS256, jose.HS384, jose.HS512,
	})
	if err != nil {
		return nil
	}
	var claims map[string]any
	if tok.UnsafeClaimsWithoutVerification(&claims) != nil {
		return nil
	}
	return claims
}

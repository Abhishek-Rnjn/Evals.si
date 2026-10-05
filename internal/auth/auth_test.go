package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// issuer is an OIDC provider stand-in: discovery, JWKS and a signer.
type issuer struct {
	srv     *httptest.Server
	keys    []jose.JSONWebKey // private
	fetches atomic.Int64
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{}
	is.addKey(t, "rsa-1", mustRSA(t), jose.RS256)
	mux := http.NewServeMux()
	is.srv = httptest.NewTLSServer(mux)
	t.Cleanup(is.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.srv.URL, "jwks_uri": is.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		is.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(is.public())
	})
	return is
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (is *issuer) addKey(t *testing.T, kid string, key crypto.Signer, alg jose.SignatureAlgorithm) {
	t.Helper()
	is.keys = append(is.keys, jose.JSONWebKey{Key: key, KeyID: kid, Algorithm: string(alg), Use: "sig"})
}

func (is *issuer) public() jose.JSONWebKeySet {
	var set jose.JSONWebKeySet
	for _, k := range is.keys {
		set.Keys = append(set.Keys, k.Public())
	}
	return set
}

func (is *issuer) sign(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	for _, k := range is.keys {
		if k.KeyID != kid {
			continue
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.SignatureAlgorithm(k.Algorithm), Key: k},
			(&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			t.Fatal(err)
		}
		tok, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	t.Fatalf("no key %q", kid)
	return ""
}

func (is *issuer) claims(sub string, extra map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss": is.srv.URL, "aud": "evals.si", "sub": sub,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func (is *issuer) provider() Provider {
	return Provider{Name: "corp", Issuer: is.srv.URL, Audiences: []string{"evals.si"}, JWKS: JWKS{Discovery: true}}
}

func newAuth(t *testing.T, cfg *Config, keys KeyStore, client *http.Client) *Authenticator {
	t.Helper()
	a, err := New(cfg, keys, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func request(header map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func TestJWT(t *testing.T) {
	is := newIssuer(t)
	is.addKey(t, "ec-1", func() crypto.Signer {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		return k
	}(), jose.ES256)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	is.addKey(t, "ed-1", edKey, jose.EdDSA)
	p := is.provider()
	p.Claims = ClaimMapping{Groups: "realm.groups"}
	p.RoleClaims = &RoleClaims{Claim: "app_roles", Map: map[string]map[string][]string{
		"checkout-lead": {"checkout": {"editor"}},
		"auditor":       {"*": {"trace-auditor"}},
	}}
	a := newAuth(t, &Config{JWT: &JWTConfig{Providers: []Provider{p}}}, nil, is.srv.Client())

	for _, kid := range []string{"rsa-1", "ec-1", "ed-1"} {
		tok := is.sign(t, kid, is.claims("alice", map[string]any{
			"email": "alice@example.com", "email_verified": true, "name": "Alice",
			"realm":     map[string]any{"groups": []any{"eng", "sandbox-users"}},
			"app_roles": []any{"checkout-lead", "auditor"},
		}))
		res := a.Authenticate(request(bearer(tok)))
		if res.Err != nil {
			t.Fatalf("%s: %v", kid, res.Err)
		}
		pr := res.Principal
		if pr.ID() != "user:corp/alice" || pr.Email != "alice@example.com" || !pr.EmailVerified || pr.Name != "Alice" ||
			strings.Join(pr.Groups, ",") != "eng,sandbox-users" {
			t.Errorf("%s: principal = %+v", kid, pr)
		}
		if got := pr.RoleGrants("checkout"); strings.Join(got, ",") != "editor,trace-auditor" {
			t.Errorf("%s: role grants = %v", kid, got)
		}
	}
	// The scheme is case-insensitive.
	if res := a.Authenticate(request(map[string]string{"Authorization": "bearer " + is.sign(t, "rsa-1", is.claims("bob", nil))})); res.Err != nil {
		t.Errorf("lower-case scheme: %v", res.Err)
	}

	other := newIssuer(t)
	for name, tc := range map[string]struct {
		tok  string
		want string
	}{
		"expired":        {is.sign(t, "rsa-1", is.claims("a", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})), "expired"},
		"not yet valid":  {is.sign(t, "rsa-1", is.claims("a", map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})), "not valid yet"},
		"wrong audience": {is.sign(t, "rsa-1", is.claims("a", map[string]any{"aud": "other"})), "audience"},
		"wrong issuer":   {other.sign(t, "rsa-1", other.claims("a", nil)), "not trusted"},
		"forged":         {forge(t, is, other), "signature"},
		"no exp":         {is.sign(t, "rsa-1", map[string]any{"iss": is.srv.URL, "aud": "evals.si", "sub": "a"}), "exp"},
		"alg none":       {unsigned(is.claims("a", nil)), "malformed"},
		"garbage":        {"not.a.jwt", "malformed"},
	} {
		res := a.Authenticate(request(bearer(tc.tok)))
		var ae *Error
		if !errors.As(res.Err, &ae) || !strings.Contains(res.Err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, res.Err, tc.want)
		}
	}
}

// forge signs with another issuer's key but this issuer's claims and kid.
func forge(t *testing.T, is, other *issuer) string {
	k := other.keys[0]
	k.KeyID = "rsa-1"
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, nil)
	tok, err := jwt.Signed(signer).Claims(is.claims("mallory", nil)).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func unsigned(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "."
}

func TestJWKSRotationIsRateLimited(t *testing.T) {
	is := newIssuer(t)
	a := newAuth(t, &Config{JWT: &JWTConfig{Providers: []Provider{is.provider()}}}, nil, is.srv.Client())
	clock := time.Now()
	a.jwt.providers[0].keys.now = func() time.Time { return clock }
	if res := a.Authenticate(request(bearer(is.sign(t, "rsa-1", is.claims("a", nil))))); res.Err != nil {
		t.Fatal(res.Err)
	}
	if is.fetches.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", is.fetches.Load())
	}
	// The issuer rotates in a new key; the first token signed with it triggers one refetch.
	is.addKey(t, "rsa-2", mustRSA(t), jose.RS256)
	clock = clock.Add(minRefetch + time.Second)
	if res := a.Authenticate(request(bearer(is.sign(t, "rsa-2", is.claims("a", nil))))); res.Err != nil {
		t.Fatalf("rotated key: %v", res.Err)
	}
	// Unknown kids within the rate limit do not refetch.
	before := is.fetches.Load()
	for range 5 {
		if res := a.Authenticate(request(bearer(withKid(t, is, "made-up")))); res.Err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if is.fetches.Load() != before {
		t.Errorf("unknown kids refetched %d times", is.fetches.Load()-before)
	}
}

func withKid(t *testing.T, is *issuer, kid string) string {
	k := is.keys[0]
	k.KeyID = kid
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, nil)
	tok, err := jwt.Signed(signer).Claims(is.claims("a", nil)).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestStaticJWKSAndLocations(t *testing.T) {
	is := newIssuer(t)
	inline, _ := json.Marshal(is.public())
	p := is.provider()
	p.JWKS = JWKS{Inline: inline}
	tok := is.sign(t, "rsa-1", is.claims("carol", nil))
	for name, tc := range map[string]struct {
		loc *Location
		req func() *http.Request
	}{
		"cookie": {&Location{Cookie: "session"}, func() *http.Request {
			r := request(nil)
			r.AddCookie(&http.Cookie{Name: "session", Value: tok})
			return r
		}},
		"query": {&Location{Query: "access_token"}, func() *http.Request {
			r := request(nil)
			r.URL.RawQuery = url.Values{"access_token": {tok}}.Encode()
			return r
		}},
		"header": {&Location{Header: &HeaderLocation{Name: "X-Token"}}, func() *http.Request {
			return request(map[string]string{"X-Token": tok})
		}},
	} {
		a := newAuth(t, &Config{JWT: &JWTConfig{Location: tc.loc, Providers: []Provider{p}}}, nil, nil)
		if res := a.Authenticate(tc.req()); res.Err != nil || res.Principal.Subject != "carol" {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// A private key in a JWKS is refused at startup.
	priv, _ := json.Marshal(jose.JSONWebKeySet{Keys: is.keys})
	p.JWKS = JWKS{Inline: priv}
	if _, err := New(&Config{JWT: &JWTConfig{Providers: []Provider{p}}}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("private JWKS: err = %v", err)
	}
}

type fakeKeys struct{ keys map[string]*StoredKey }

func (f fakeKeys) KeyByHash(_ context.Context, hash string) (*StoredKey, error) {
	return f.keys[hash], nil
}
func (f fakeKeys) TouchKey(context.Context, string, time.Time) error { return nil }

func TestAPIKeysAndModes(t *testing.T) {
	cfgKey, cfgHash := NewAPIKey()
	dbKey, dbHash := NewAPIKey()
	revoked, revokedHash := NewAPIKey()
	expired, expiredHash := NewAPIKey()
	store := fakeKeys{keys: map[string]*StoredKey{
		dbHash:      {Name: "ci", Roles: map[string][]string{"default": {"runner"}}},
		revokedHash: {Name: "old", Revoked: true},
		expiredHash: {Name: "tmp", ExpiresAt: time.Now().Add(-time.Minute)},
	}}
	cfg := &Config{APIKeys: &APIKeysConfig{Keys: []ConfigKey{{
		Name: "collector", Key: "sha256:" + cfgHash, Roles: map[string][]string{"support": {"ingest"}}, Labels: map[string]string{"source": "gw"},
	}}}}
	a := newAuth(t, cfg, store, nil)
	if res := a.Authenticate(request(map[string]string{APIKeyHeader: cfgKey})); res.Err != nil || res.Principal.ID() != "key:collector" ||
		res.Principal.Labels["source"] != "gw" || res.Principal.RoleGrants("support")[0] != "ingest" {
		t.Errorf("config key: %+v", res)
	}
	if res := a.Authenticate(request(bearer(dbKey))); res.Err != nil || res.Principal.ID() != "key:ci" {
		t.Errorf("stored key as bearer: %+v", res)
	}
	for name, key := range map[string]string{"revoked": revoked, "expired": expired, "unknown": "evk_nope", "malformed": "abc"} {
		if res := a.Authenticate(request(map[string]string{APIKeyHeader: key})); res.Err == nil {
			t.Errorf("%s key accepted", name)
		}
	}
	// strict (the default): no credential is an error.
	if res := a.Authenticate(request(nil)); res.Err == nil {
		t.Error("strict mode accepted a request without a credential")
	}
	// optional: anonymous; permissive: bad keys become anonymous.
	cfg.APIKeys.Mode = ModeOptional
	a = newAuth(t, cfg, store, nil)
	if res := a.Authenticate(request(nil)); res.Err != nil || !res.Principal.Anonymous() {
		t.Errorf("optional: %+v", res)
	}
	if res := a.Authenticate(request(map[string]string{APIKeyHeader: revoked})); res.Err == nil {
		t.Error("optional mode accepted a revoked key")
	}
	cfg.APIKeys.Mode = ModePermissive
	a = newAuth(t, cfg, store, nil)
	if res := a.Authenticate(request(map[string]string{APIKeyHeader: revoked})); res.Err != nil || !res.Principal.Anonymous() {
		t.Errorf("permissive: %+v", res)
	}
	// A JWT where only API keys are configured is refused, not ignored.
	if res := a.Authenticate(request(bearer("eyJ.x.y"))); res.Err == nil {
		t.Error("bearer token accepted without jwt providers")
	}
	// Oversized credentials are refused before any parsing.
	if res := a.Authenticate(request(bearer(strings.Repeat("a", 20000)))); res.Err == nil || !strings.Contains(res.Err.Error(), "too large") {
		t.Errorf("oversized: %v", res.Err)
	}
	// Disabled: everyone is anonymous with method none.
	if res := newAuth(t, &Config{Mode: ModeNone}, nil, nil).Authenticate(request(nil)); res.Err != nil || res.Principal.Method != MethodNone {
		t.Errorf("mode none: %+v", res)
	}
}

func TestPermissiveJWT(t *testing.T) {
	is := newIssuer(t)
	other := newIssuer(t)
	a := newAuth(t, &Config{JWT: &JWTConfig{Mode: ModePermissive, Providers: []Provider{is.provider()}}}, nil, is.srv.Client())
	res := a.Authenticate(request(bearer(other.sign(t, "rsa-1", other.claims("eve", nil)))))
	if res.Err != nil || !res.Principal.Anonymous() || res.Principal.Claims["sub"] != "eve" {
		t.Errorf("permissive: %+v", res)
	}
}

func TestClientCertAndProxy(t *testing.T) {
	a := newAuth(t, &Config{TrustedProxy: &ProxyConfig{CIDRs: []string{"10.0.0.0/8"}, UserHeader: "X-User", GroupsHeader: "X-Groups"}}, nil, nil)
	r := request(map[string]string{"X-User": "dave", "X-Groups": "a, b"})
	r.RemoteAddr = "10.1.2.3:5555"
	if res := a.Authenticate(r); res.Principal == nil || res.Principal.ID() != "user:proxy/dave" || len(res.Principal.Groups) != 2 {
		t.Errorf("trusted proxy: %+v", res)
	}
	r.RemoteAddr = "192.168.1.1:5555"
	if res := a.Authenticate(r); res.Principal == nil || !res.Principal.Anonymous() {
		t.Errorf("untrusted proxy headers were honored: %+v", res)
	}
	u, _ := url.Parse("spiffe://example.org/ns/evals/sa/collector")
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "collector"}, URIs: []*url.URL{u}}
	if p := certPrincipal(cert); p.ID() != "user:mtls/spiffe://example.org/ns/evals/sa/collector" || p.Kind != KindService {
		t.Errorf("cert principal = %+v", p)
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:1": true, "localhost:80": true,
		":8080": false, "0.0.0.0:8080": false, "10.0.0.1:80": false, "bad": false,
	} {
		if IsLoopback(addr) != want {
			t.Errorf("IsLoopback(%q) = %v", addr, !want)
		}
	}
}

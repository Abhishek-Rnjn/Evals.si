package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIKeyPrefix starts every key evalsid issues, so secret scanners can find leaked keys.
const APIKeyPrefix = "evk_"

// APIKeyHeader carries an API key when the token location is taken by JWTs.
const APIKeyHeader = "X-Api-Key"

// NewAPIKey returns a fresh key and the hex SHA-256 hash that is stored instead of it.
func NewAPIKey() (key, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	key = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, HashAPIKey(key)
}

// HashAPIKey is the stored form of a key: lower-case hex SHA-256.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// StoredKey is an API key's record, without the key itself.
type StoredKey struct {
	Name      string
	Roles     map[string][]string
	Labels    map[string]string
	ExpiresAt time.Time
	Revoked   bool
}

// KeyStore finds API keys created through AuthService.
type KeyStore interface {
	// KeyByHash returns nil, nil when no key has this hash.
	KeyByHash(ctx context.Context, hash string) (*StoredKey, error)
	TouchKey(ctx context.Context, name string, at time.Time) error
}

// Error is an authentication failure; it becomes Unauthenticated (HTTP 401).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) Result {
	return Result{Err: &Error{Msg: fmt.Sprintf(format, args...)}}
}

// Authenticator turns request credentials into principals.
type Authenticator struct {
	cfg        *Config
	jwt        *jwtVerifier
	configKeys map[string]ConfigKey
	keys       KeyStore
	proxyNets  []*net.IPNet
	log        *slog.Logger
	now        func() time.Time

	touchMu sync.Mutex
	touched map[string]time.Time
}

// New builds an authenticator. keys may be nil when only config keys are
// used; client fetches JWKS and discovery documents.
func New(cfg *Config, keys KeyStore, client *http.Client, log *slog.Logger) (*Authenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &Authenticator{cfg: cfg, keys: keys, log: log, now: time.Now, configKeys: map[string]ConfigKey{}, touched: map[string]time.Time{}}
	if !cfg.Enabled() {
		return a, nil
	}
	if cfg.JWT != nil {
		a.jwt = &jwtVerifier{now: func() time.Time { return a.now() }}
		for _, p := range cfg.JWT.Providers {
			jp, err := newJWTProvider(p, client)
			if err != nil {
				return nil, err
			}
			a.jwt.providers = append(a.jwt.providers, jp)
		}
	}
	if cfg.APIKeys != nil {
		for _, k := range cfg.APIKeys.Keys {
			h, _ := ParseKeyHash(k.Key)
			a.configKeys[h] = k
		}
	}
	if p := cfg.TrustedProxy; p != nil {
		for _, c := range p.CIDRs {
			_, n, _ := net.ParseCIDR(c)
			a.proxyNets = append(a.proxyNets, n)
		}
	}
	return a, nil
}

// Enabled reports whether requests are authenticated.
func (a *Authenticator) Enabled() bool { return a != nil && a.cfg.Enabled() }

// ConfigKeys returns the API keys declared in config.
func (a *Authenticator) ConfigKeys() []ConfigKey {
	if !a.Enabled() || a.cfg.APIKeys == nil {
		return nil
	}
	return a.cfg.APIKeys.Keys
}

// CLILogin returns what `evalsi login` needs, or nil.
func (a *Authenticator) CLILogin() *CLILogin {
	if !a.Enabled() {
		return nil
	}
	return a.cfg.CLILogin
}

// Middleware authenticates every request and stores the result in its
// context. It never rejects: the enforcement points (the Connect interceptor
// and the HTTP guard) do, in the right protocol's error format.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := a.Authenticate(r)
		next.ServeHTTP(w, r.WithContext(WithResult(r.Context(), res)))
	})
}

func modeOf(m string) string {
	if m == "" {
		return ModeStrict
	}
	return m
}

// Issuers are the issuers of the bearer-token providers people sign in
// with (kind user), for OAuth protected-resource metadata.
func (a *Authenticator) Issuers() []string {
	if a == nil || a.cfg.JWT == nil {
		return nil
	}
	var out []string
	for _, p := range a.cfg.JWT.Providers {
		if p.Issuer != "" && p.Kubernetes == nil && (p.Kind == "" || p.Kind == KindUser) {
			out = append(out, p.Issuer)
		}
	}
	return out
}

// Authenticate checks the request's credential.
func (a *Authenticator) Authenticate(r *http.Request) Result {
	if !a.Enabled() {
		return Result{Principal: Anonymous(MethodNone)}
	}
	ctx := r.Context()
	token := a.token(r)
	key := r.Header.Get(APIKeyHeader)
	if key == "" && strings.HasPrefix(token, APIKeyPrefix) {
		key, token = token, ""
	}
	if len(token) > a.cfg.maxToken() || len(key) > a.cfg.maxToken() {
		return fail("credential too large")
	}
	if key != "" {
		if a.cfg.APIKeys == nil {
			return fail("API keys are not enabled on this server")
		}
		p, err := a.apiKey(ctx, key)
		if err == nil {
			return Result{Principal: p}
		}
		if modeOf(a.cfg.APIKeys.Mode) == ModePermissive {
			return Result{Principal: Anonymous(MethodAPIKey)}
		}
		return fail("%v", err)
	}
	if token != "" {
		if a.jwt == nil {
			return fail("bearer tokens are not enabled on this server")
		}
		claims, prov, err := a.jwt.verify(ctx, token)
		if err == nil {
			return Result{Principal: prov.principal(claims)}
		}
		if modeOf(a.cfg.JWT.Mode) == ModePermissive {
			p := Anonymous(MethodJWT)
			p.Claims = unverifiedClaims(token)
			return Result{Principal: p}
		}
		a.log.Debug("rejected token", "err", err)
		return fail("invalid token: %v", err)
	}
	if p := a.clientCert(r); p != nil {
		return Result{Principal: p}
	}
	if p := a.proxyIdentity(r); p != nil {
		return Result{Principal: p}
	}
	if (a.cfg.JWT != nil && modeOf(a.cfg.JWT.Mode) == ModeStrict) ||
		(a.cfg.APIKeys != nil && modeOf(a.cfg.APIKeys.Mode) == ModeStrict) {
		return fail("a credential is required")
	}
	return Result{Principal: Anonymous(MethodNone)}
}

// token reads the JWT from the configured location.
func (a *Authenticator) token(r *http.Request) string {
	loc := &Location{Header: &HeaderLocation{Name: "Authorization", Prefix: "Bearer "}}
	if a.cfg.JWT != nil && a.cfg.JWT.Location != nil {
		loc = a.cfg.JWT.Location
	}
	switch {
	case loc.Header != nil:
		v := r.Header.Get(loc.Header.Name)
		if loc.Header.Prefix == "" {
			return strings.TrimSpace(v)
		}
		// The scheme is case-insensitive (RFC 9110).
		if len(v) >= len(loc.Header.Prefix) && strings.EqualFold(v[:len(loc.Header.Prefix)], loc.Header.Prefix) {
			return strings.TrimSpace(v[len(loc.Header.Prefix):])
		}
		return ""
	case loc.Cookie != "":
		if c, err := r.Cookie(loc.Cookie); err == nil {
			return c.Value
		}
	case loc.Query != "":
		return r.URL.Query().Get(loc.Query)
	}
	return ""
}

func (a *Authenticator) apiKey(ctx context.Context, key string) (*Principal, error) {
	if !strings.HasPrefix(key, APIKeyPrefix) {
		return nil, errors.New("malformed API key")
	}
	hash := HashAPIKey(key)
	var sk *StoredKey
	if ck, ok := a.configKeys[hash]; ok {
		sk = &StoredKey{Name: ck.Name, Roles: ck.Roles, Labels: ck.Labels}
		if ck.ExpiresAt != "" {
			sk.ExpiresAt, _ = time.Parse(time.RFC3339, ck.ExpiresAt)
		}
	} else if a.keys != nil {
		var err error
		if sk, err = a.keys.KeyByHash(ctx, hash); err != nil {
			a.log.Error("looking up API key", "err", err)
			return nil, errors.New("API key lookup failed")
		}
		if sk != nil {
			a.touch(sk.Name)
		}
	}
	switch {
	case sk == nil:
		return nil, errors.New("unknown API key")
	case sk.Revoked:
		return nil, errors.New("API key revoked")
	case !sk.ExpiresAt.IsZero() && a.now().After(sk.ExpiresAt):
		return nil, errors.New("API key expired")
	}
	return &Principal{
		Kind: KindAPIKey, Method: MethodAPIKey, Provider: "apikey", Subject: sk.Name, Name: sk.Name,
		KeyName: sk.Name, Labels: sk.Labels, KeyRoles: sk.Roles,
	}, nil
}

// touch records a key's last use, at most once a minute per key.
func (a *Authenticator) touch(name string) {
	now := a.now()
	a.touchMu.Lock()
	if now.Sub(a.touched[name]) < time.Minute {
		a.touchMu.Unlock()
		return
	}
	a.touched[name] = now
	a.touchMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.keys.TouchKey(ctx, name, now); err != nil {
			a.log.Warn("recording API key use", "key", name, "err", err)
		}
	}()
}

// clientCert makes a principal from a verified client certificate.
func (a *Authenticator) clientCert(r *http.Request) *Principal {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return certPrincipal(r.TLS.PeerCertificates[0])
}

func certPrincipal(cert *x509.Certificate) *Principal {
	p := &Principal{Kind: KindService, Method: MethodMTLS, Provider: "mtls", Name: cert.Subject.CommonName}
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" {
			p.Subject = u.String()
			return p
		}
	}
	p.Subject = cert.Subject.CommonName
	if p.Subject == "" {
		return nil
	}
	return p
}

// proxyIdentity trusts identity headers, only from configured networks.
func (a *Authenticator) proxyIdentity(r *http.Request) *Principal {
	pc := a.cfg.TrustedProxy
	if pc == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	trusted := false
	for _, n := range a.proxyNets {
		if ip != nil && n.Contains(ip) {
			trusted = true
			break
		}
	}
	user := r.Header.Get(pc.UserHeader)
	if !trusted || user == "" {
		return nil
	}
	p := &Principal{Kind: KindUser, Method: MethodProxy, Provider: "proxy", Subject: user, Name: user}
	if pc.EmailHeader != "" {
		p.Email = r.Header.Get(pc.EmailHeader)
		p.EmailVerified = p.Email != ""
	}
	if pc.GroupsHeader != "" {
		for _, g := range strings.Split(r.Header.Get(pc.GroupsHeader), ",") {
			if g = strings.TrimSpace(g); g != "" {
				p.Groups = append(p.Groups, g)
			}
		}
	}
	return p
}

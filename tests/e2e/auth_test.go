package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// idp is an OIDC provider stand-in: discovery, a device flow that approves
// at once, and a GitHub Actions token endpoint. CI needs no live provider.
type idp struct {
	srv    *httptest.Server
	signer jose.Signer
	jwks   []byte
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: "k1", Algorithm: "RS256", Use: "sig"}
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, nil)
	jwks, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk.Public()}})
	p := &idp{signer: signer, jwks: jwks}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]string{
			"issuer": p.srv.URL, "token_endpoint": p.srv.URL + "/token",
			"device_authorization_endpoint": p.srv.URL + "/device", "authorization_endpoint": p.srv.URL + "/authorize",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"device_code": "dc", "user_code": "WXYZ", "verification_uri": p.srv.URL + "/activate", "interval": 0, "expires_in": 60})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		write(w, map[string]any{
			"access_token": "opaque", "refresh_token": "rt", "expires_in": 3600,
			"id_token": p.sign(t, map[string]any{"iss": p.srv.URL, "aud": "evalsi-cli", "sub": "alice", "email": "alice@example.com", "email_verified": true}),
		})
	})
	mux.HandleFunc("/gh-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runner-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		write(w, map[string]string{"value": p.sign(t, map[string]any{
			"iss": "https://token.actions.githubusercontent.com", "aud": r.URL.Query().Get("audience"),
			"sub": "repo:acme/agent:ref:refs/heads/main", "repository": "acme/agent", "ref": "refs/heads/main",
		})})
	})
	return p
}

func (p *idp) sign(t *testing.T, claims map[string]any) string {
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	claims["iat"] = time.Now().Unix()
	tok, err := jwt.Signed(p.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestAuthFlows drives the evalsi CLI against an authenticated server:
// device-code login, whoami, issuing an API key, a GitHub Actions job with
// its own OIDC token, a denied call, and the audit log.
func TestAuthFlows(t *testing.T) {
	provider := newIdP(t)
	e := start(t, func(c *config.Config) {
		c.Auth = &auth.Config{
			JWT: &auth.JWTConfig{Providers: []auth.Provider{
				{Name: "corp", Issuer: provider.srv.URL, Audiences: []string{"evalsi-cli"}, JWKS: auth.JWKS{Inline: provider.jwks}},
				{Name: "github", Issuer: "https://token.actions.githubusercontent.com", Audiences: []string{"https://evals.example.com"},
					JWKS: auth.JWKS{Inline: provider.jwks}},
			}},
			APIKeys:  &auth.APIKeysConfig{},
			CLILogin: &auth.CLILogin{Issuer: provider.srv.URL, ClientID: "evalsi-cli"},
		}
		c.RBAC = authz.RBACConfig{Projects: map[string]map[string][]string{
			"support": {
				"admin":  {"email:alice@example.com"},
				"runner": {`cel:jwt.repository == "acme/agent" && jwt.ref == "refs/heads/main"`},
			},
		}}
	})
	creds := filepath.Join(t.TempDir(), "credentials")
	cli := func(env []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(e.workerCmd[0], append(e.workerCmd[1:], args...)...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "EVALSI_") && !strings.HasPrefix(kv, "ACTIONS_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, append([]string{"EVALSI_CREDENTIALS=" + creds, "EVALSI_SERVER=" + e.base}, env...)...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}

	// jwt.mode defaults to strict: no credential, no service.
	if out, code := cli(nil, "auth", "projects", "list"); code != 1 || !strings.Contains(out, "unauthenticated") || !strings.Contains(out, "evalsi login") {
		t.Errorf("anonymous call: %d %s", code, out)
	}
	out, code := cli(nil, "login", "--device")
	if code != 0 || !strings.Contains(out, "WXYZ") || !strings.Contains(out, "signed in") || !strings.Contains(out, "user:corp/alice") {
		t.Fatalf("login: %d %s", code, out)
	}
	if info, err := os.Stat(creds); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file: %v %v", info, err)
	}
	if out, code := cli(nil, "whoami"); code != 0 || !strings.Contains(out, "support") || !strings.Contains(out, "admin") || !strings.Contains(out, "login") {
		t.Errorf("whoami: %d %s", code, out)
	}
	out, code = cli(nil, "auth", "keys", "create", "ci", "--role", "support=runner", "--ttl", "1h")
	if code != 0 {
		t.Fatalf("keys create: %d %s", code, out)
	}
	fields := strings.Fields(out)
	key := fields[len(fields)-1]
	if !strings.HasPrefix(key, auth.APIKeyPrefix) {
		t.Fatalf("no key in %q", out)
	}
	if out, code := cli([]string{"EVALSI_API_KEY=" + key}, "whoami"); code != 0 || !strings.Contains(out, "key:ci") || !strings.Contains(out, "runner") {
		t.Errorf("whoami with the new key: %d %s", code, out)
	}
	// The runner key cannot manage access: denied, and audited.
	if out, code := cli([]string{"EVALSI_API_KEY=" + key}, "auth", "roles", "create", "sneaky", "--project", "support", "--permission", "runs.read"); code != 1 || !strings.Contains(out, "permission_denied") {
		t.Errorf("runner creating a role: %d %s", code, out)
	}
	// A GitHub Actions job authenticates with its own OIDC token: no stored secret.
	gh := []string{
		"EVALSI_OIDC_AUDIENCE=https://evals.example.com",
		"ACTIONS_ID_TOKEN_REQUEST_URL=" + provider.srv.URL + "/gh-token",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN=runner-token",
		"EVALSI_CREDENTIALS=" + filepath.Join(t.TempDir(), "none"),
	}
	if out, code := cli(gh, "whoami"); code != 0 || !strings.Contains(out, "user:github/repo:acme/agent:ref:refs/heads/main") || !strings.Contains(out, "runner") {
		t.Errorf("GitHub Actions whoami: %d %s", code, out)
	}
	out, code = cli(nil, "auth", "audit", "--denied", "--format", "json")
	if code != 0 || !strings.Contains(out, "key:ci") || !strings.Contains(out, "access.manage") {
		t.Errorf("audit: %d %s", code, out)
	}
	if out, code := cli(nil, "logout"); code != 0 || !strings.Contains(out, "signed out") {
		t.Errorf("logout: %d %s", code, out)
	}
	if out, code := cli(nil, "auth", "keys", "list"); code != 1 || !strings.Contains(out, "unauthenticated") {
		t.Errorf("after logout: %d %s", code, out)
	}
}

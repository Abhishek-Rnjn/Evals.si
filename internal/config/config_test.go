package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/authz"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evalsi.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load(write(t, `
listen: ":9090"
auth: {mode: none}
judges:
  claude: {provider: anthropic, model: claude-opus-5-5, effort: low}
default_judge: claude
evaluate: {batch_size: 8}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" || cfg.Judges["claude"].Effort != "low" || cfg.Evaluate.BatchSize != 8 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.Evaluate.Parallelism != 8 || cfg.Worker.Command[0] != "python3" {
		t.Errorf("defaults not kept: %+v", cfg)
	}
}

func TestLoadRejectsMistakes(t *testing.T) {
	for body, want := range map[string]string{
		"listn: x": "unknown field",
		"judges: {j: {provider: openai-compatible, model: m}}":                                             "base_url is required",
		"judges: {j: {provider: other, model: m}}":                                                         "provider must be",
		"default_judge: missing":                                                                           "not among judges",
		"worker: {start_timeout: soon}":                                                                    "start_timeout",
		"listen: 0.0.0.0:8080":                                                                             "without authentication",
		"otlp: {grpc_listen: '0.0.0.0:4317'}":                                                              "without authentication",
		"listen: 0.0.0.0:8080\nauth: {api_keys: {keys: []}}":                                               "plaintext",
		"auth: {api_keys: {keys: [{name: k, key: hunter2}]}}":                                              "never put the plaintext key",
		"auth: {jwt: {providers: [{name: p, issuer: https://i, jwks: {url: https://i/k}}]}}":               "audiences",
		"auth: {jwt: {providers: [{name: p, issuer: http://i, audiences: [a], jwks: {discovery: true}}]}}": "must use https",
		"auth: {mode: off}":                                                                                "auth.mode",
		"rbac: {owners: [group:corp/admins]}":                                                              "need an auth section",
		"auth: {api_keys: {}}\nrbac: {owners: [bob]}":                                                      "rbac.owners",
		"auth: {api_keys: {}}\nauthorization: {rules: [{allow: 'true', deny: 'false'}]}":                   "exactly one",
		"metrics: {listen: '0.0.0.0:9464'}":                                                                "loopback",
	} {
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) error = %v, want it to mention %q", body, err, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../examples/server/evalsi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultJudge != "claude" || cfg.Judges["claude"].Provider != "anthropic" {
		t.Errorf("unexpected example config: %+v", cfg)
	}
}

func TestAuthConfigs(t *testing.T) {
	for _, body := range []string{
		// Non-loopback with explicit opt-out, or with TLS, or behind a TLS proxy.
		"listen: 0.0.0.0:8080\nauth: {mode: none}",
		"listen: 0.0.0.0:8080\nauth: {api_keys: {}, allow_plaintext: true}",
		`auth:
  jwt:
    mode: optional
    location: {cookie: session}
    providers:
      - name: corp
        issuer: https://sso.example.com
        audiences: [evals.si]
        jwks: {discovery: true}
        role_claims: {claim: app_roles, map: {lead: {checkout: [editor]}}}
  api_keys:
    keys: [{name: ci, key: "sha256:` + strings.Repeat("ab", 32) + `", roles: {default: [runner]}}]
rbac:
  owners: [group:corp/admins]
  roles: [{name: auditor, permissions: [traces.read, audit.read]}]
  projects: {checkout: {auditor: ["email:a@example.com"]}}
authorization:
  rules: [{require: '!resource.runs_code || "sandbox" in principal.groups'}]
audit: {retention: 720h}`,
	} {
		if _, err := Load(write(t, body)); err != nil {
			t.Errorf("Load(%q): %v", body, err)
		}
	}
}

func TestExampleAuthConfigLoads(t *testing.T) {
	cfg, err := Load("../../examples/auth/evalsi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AuthEnabled() || len(cfg.Auth.JWT.Providers) != 2 || len(cfg.RBAC.Roles) != 2 {
		t.Errorf("unexpected example auth config: %+v", cfg.Auth)
	}
	// Roles, conditions, bindings and rules compile.
	if _, err := authz.NewEngine(context.Background(), authz.Options{
		Enabled: true, RBAC: cfg.RBAC, Authorization: cfg.Authorization, ClaimRoles: []string{"editor", "trace-auditor"},
	}); err != nil {
		t.Error(err)
	}
}

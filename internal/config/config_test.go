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
		"datasets_dir: s3://bucket/datasets":                                                               "needs storage.s3",
		"storage: {postgres: {dsn_env: EVALSI_UNSET_DSN_FOR_TEST}}":                                        "storage.postgres",
		"storage: {clickhouse: {database: x}}":                                                             "needs url",
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

// The local example must not start until the user pastes their own hash.
func TestLocalAuthExampleNeedsARealHash(t *testing.T) {
	if _, err := Load("../../examples/auth/local.yaml"); err == nil || !strings.Contains(err.Error(), "64 hex digits") {
		t.Errorf("Load = %v", err)
	}
	raw, _ := os.ReadFile("../../examples/auth/local.yaml")
	fixed := strings.Replace(string(raw), "sha256:REPLACE_WITH_THE_HASH_FROM_evalsid_auth_new-key_0000000000000", "sha256:"+strings.Repeat("ab", 32), 1)
	cfg, err := Load(write(t, fixed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authz.NewEngine(context.Background(), authz.Options{Enabled: true, RBAC: cfg.RBAC, Authorization: cfg.Authorization}); err != nil {
		t.Error(err)
	}
}

func TestDisableAuthKeepsTheRestOfTheConfig(t *testing.T) {
	path := write(t, `
listen: 0.0.0.0:8080
auth:
  api_keys: {keys: [{name: k, key: "sha256:`+strings.Repeat("ab", 32)+`"}]}
rbac: {owners: [key:k], projects: {demo: {}}}
authorization: {rules: [{require: "true"}]}
`)
	// As written, plaintext bearer keys on a network address are refused.
	if _, err := Load(path); err == nil {
		t.Fatal("expected the plaintext check to fail")
	}
	cfg, err := LoadWith(path, func(c *Config) { c.DisableAuth() })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthEnabled() || len(cfg.Auth.APIKeys.Keys) != 1 || len(cfg.RBAC.Owners) != 1 {
		t.Errorf("auth = %+v, rbac = %+v", cfg.Auth, cfg.RBAC)
	}
	// Overrides are validated like the file: --listen cannot dodge the auth rule.
	if _, err := LoadWith("", func(c *Config) { c.Listen = "0.0.0.0:8080" }); err == nil || !strings.Contains(err.Error(), "without authentication") {
		t.Errorf("--listen override: %v", err)
	}
	// mode: none in the file keeps rbac and rules, switched off.
	if _, err := Load(write(t, "auth: {mode: none, api_keys: {}}\nrbac: {owners: [key:k]}")); err != nil {
		t.Errorf("mode none with rbac: %v", err)
	}
}

func TestWorkerJudgesDropProjects(t *testing.T) {
	cfg, err := Load(write(t, `
judges:
  claude:
    provider: anthropic
    model: m
    projects: [e2e]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Judges["claude"].Projects; len(got) != 1 {
		t.Fatalf("server config lost projects: %v", got)
	}
	j := cfg.WorkerJudges()["claude"]
	if j.Projects != nil || j.Model != "m" {
		t.Fatalf("worker judge = %+v; want the judge without projects", j)
	}
}

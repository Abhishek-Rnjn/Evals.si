// Package config loads the evalsid server configuration (evalsi.yaml).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sinks"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Judge mirrors evalsi.judges.JudgeConfig in the Python SDK; it is handed to
// workers as JSON. API keys are never stored here, only the name of the
// environment variable that holds one.
type Judge struct {
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	BaseURL        string   `json:"base_url,omitempty"`
	APIKeyEnv      string   `json:"api_key_env,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"`
	TimeoutS       float64  `json:"timeout_s,omitempty"`
	// Client-side rate limit for this judge, enforced by the worker.
	RequestsPerMinute float64 `json:"requests_per_minute,omitempty"`
}

// Worker configures the Python evaluator worker that evalsid supervises.
type Worker struct {
	// Command that runs the evalsi CLI; "worker --listen ..." is appended.
	Command []string `json:"command"`
	// How long to wait for the worker to answer Describe after starting.
	StartTimeout string `json:"start_timeout"`
	// Disable the worker's judge response cache.
	NoCache bool `json:"no_cache"`
}

// Evaluate tunes the Score path.
type Evaluate struct {
	// Records per worker call.
	BatchSize int `json:"batch_size"`
	// Concurrent worker calls per request.
	Parallelism int `json:"parallelism"`
	// Upper bound on records in one unary Evaluate call; larger jobs use EvaluateStream.
	MaxRecords int `json:"max_records"`
}

// Runs tunes the Run door.
type Runs struct {
	// Runs executing at once; the rest wait as PENDING.
	MaxConcurrent int `json:"max_concurrent"`
}

// OTLP configures trace ingestion. OTLP is always served on the main port
// (gRPC TraceService/Export and HTTP /v1/traces); these add the standard ports.
type OTLP struct {
	// For example 127.0.0.1:4317. Empty: not opened.
	GRPCListen string `json:"grpc_listen"`
	// For example 127.0.0.1:4318. Empty: not opened.
	HTTPListen string `json:"http_listen"`
	// Wait for late spans after a trace's root span ends.
	Grace string `json:"grace"`
	// Traces buffered while waiting for spans.
	MaxTraces int `json:"max_traces"`
}

// Traces configures stored traces.
type Traces struct {
	// How long traces and their results are kept, for example 168h.
	Retention string `json:"retention"`
}

// Config is the whole evalsid configuration.
type Config struct {
	Listen string `json:"listen"`
	// Where evalsid keeps its database (unless storage.postgres is set) and
	// caches. Relative paths are relative to the working directory.
	DataDir string `json:"data_dir"`
	// Root for dataset paths in run specs: a directory, or s3://bucket/prefix
	// with storage.s3. Empty means runs must send records inline or use a
	// dataset URI.
	DatasetsDir string `json:"datasets_dir"`
	// External databases and object storage; SQLite in data_dir by default.
	Storage Storage `json:"storage"`
	Runs    Runs    `json:"runs"`
	OTLP    OTLP    `json:"otlp"`
	Traces  Traces  `json:"traces"`
	// Online evaluation policies applied at startup, in the OnlineEvalPolicy
	// JSON form. They replace stored policies of the same name.
	Policies     []json.RawMessage `json:"policies"`
	Worker       Worker            `json:"worker"`
	Judges       map[string]Judge  `json:"judges"`
	DefaultJudge string            `json:"default_judge"`
	Evaluate     Evaluate          `json:"evaluate"`
	// Isolation for code-executing evaluators and agent tasks.
	Sandbox sandbox.Config `json:"sandbox"`
	// What agent-run specs may make the worker execute outside the sandbox.
	Agents Agents `json:"agents"`
	// Where finished runs and online scores are exported (MLflow, OTel).
	Sinks []sinks.Config `json:"sinks"`
	// Authentication: JWT providers, API keys, TLS. Required on a
	// non-loopback listen address; `auth: {mode: none}` opts out explicitly.
	Auth *auth.Config `json:"auth,omitempty"`
	// Projects, custom roles and role bindings.
	RBAC authz.RBACConfig `json:"rbac"`
	// Global CEL rules and external authorization.
	Authorization authz.AuthorizationConfig `json:"authorization"`
	Audit         authz.AuditConfig         `json:"audit"`
	Metrics       Metrics                   `json:"metrics"`
}

// Storage selects the databases. Without it, everything is in SQLite under
// data_dir, which suits a single replica.
type Storage struct {
	// PostgreSQL for metadata, runs and results; several replicas may share it.
	Postgres *Postgres `json:"postgres,omitempty"`
	// ClickHouse for traces and their online scores.
	ClickHouse *store.ClickHouseConfig `json:"clickhouse,omitempty"`
	// S3-compatible object storage, for datasets_dir s3://bucket/prefix.
	S3 *objstore.Config `json:"s3,omitempty"`
}

// Postgres names the database; the DSN usually carries a password, so it
// can come from the environment.
type Postgres struct {
	DSN    string `json:"dsn,omitempty"`
	DSNEnv string `json:"dsn_env,omitempty"`
}

// ResolvedDSN is the DSN, from the environment when dsn_env is set.
func (p *Postgres) ResolvedDSN() string {
	if p.DSNEnv != "" {
		return os.Getenv(p.DSNEnv)
	}
	return p.DSN
}

// Agents lists what agent-run specs may make the worker itself execute.
// Everything an agent does runs in the sandbox; these are the exceptions,
// because a spec comes from an API caller: stdio MCP servers and external
// harness commands (exact argv lists), and Python harness classes and checker
// parsers ("module:name"). References into installed evalsi_* packages (the
// benchmark adapters) are always allowed.
type Agents struct {
	TrustedCommands [][]string `json:"trusted_commands,omitempty"`
	TrustedPython   []string   `json:"trusted_python,omitempty"`
}

// Metrics configures Prometheus metrics.
type Metrics struct {
	// A separate listener serving only /metrics without authentication, for
	// example 127.0.0.1:9464. On the main port, /metrics needs metrics.read.
	Listen string `json:"listen,omitempty"`
}

// AuthEnabled reports whether requests are authenticated and authorized.
func (c Config) AuthEnabled() bool { return c.Auth.Enabled() }

// Default returns the configuration used when no file is given.
func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080",
		Worker: Worker{
			Command:      []string{"python3", "-m", "evalsi"},
			StartTimeout: "60s",
		},
		DataDir:  ".evalsi",
		Judges:   map[string]Judge{},
		Evaluate: Evaluate{BatchSize: 32, Parallelism: 8, MaxRecords: 10000},
		Runs:     Runs{MaxConcurrent: 4},
		OTLP:     OTLP{Grace: "2s", MaxTraces: 10000},
		Traces:   Traces{Retention: "168h"},
	}
}

// Load reads a YAML (or JSON) file over the defaults. An empty path returns the defaults.
func Load(path string) (Config, error) {
	return LoadWith(path)
}

// LoadWith reads a file like Load, then applies overrides (command-line
// flags) before validating, so overrides get the same checks as the file.
func LoadWith(path string, overrides ...func(*Config)) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, o := range overrides {
		o(&cfg)
	}
	return cfg, cfg.Validate()
}

// DisableAuth turns authentication and authorization off for development
// (`evalsid serve --no-auth`, or EVALSID_NO_AUTH=1). The rest of the auth,
// rbac and authorization sections stay in the config, unenforced, so
// removing the switch restores them.
func (c *Config) DisableAuth() {
	if c.Auth == nil {
		c.Auth = &auth.Config{}
	}
	c.Auth.Mode = auth.ModeNone
}

// Duration parses a validated duration field.
func Duration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

// StartTimeout parses Worker.StartTimeout.
func (c Config) StartTimeout() time.Duration {
	d, err := time.ParseDuration(c.Worker.StartTimeout)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// Validate checks the configuration for mistakes that would only surface later.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if len(c.Worker.Command) == 0 {
		errs = append(errs, errors.New("worker.command is required"))
	}
	if _, err := time.ParseDuration(c.Worker.StartTimeout); err != nil {
		errs = append(errs, fmt.Errorf("worker.start_timeout: %w", err))
	}
	for name, j := range c.Judges {
		switch {
		case j.Provider != "openai-compatible" && j.Provider != "anthropic":
			errs = append(errs, fmt.Errorf("judges.%s.provider must be openai-compatible or anthropic", name))
		case j.Model == "":
			errs = append(errs, fmt.Errorf("judges.%s.model is required", name))
		case j.Provider == "openai-compatible" && j.BaseURL == "":
			errs = append(errs, fmt.Errorf("judges.%s.base_url is required for openai-compatible", name))
		}
	}
	if c.DefaultJudge != "" {
		if _, ok := c.Judges[c.DefaultJudge]; !ok {
			errs = append(errs, fmt.Errorf("default_judge %q is not among judges", c.DefaultJudge))
		}
	}
	for name, d := range map[string]string{"otlp.grace": c.OTLP.Grace, "traces.retention": c.Traces.Retention} {
		if _, err := time.ParseDuration(d); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	if c.Runs.MaxConcurrent < 1 {
		errs = append(errs, errors.New("runs.max_concurrent must be positive"))
	}
	if _, remote := objstore.Parse(c.DatasetsDir); remote {
		if c.Storage.S3 == nil {
			errs = append(errs, fmt.Errorf("datasets_dir %q needs storage.s3", c.DatasetsDir))
		}
	} else if c.DatasetsDir != "" {
		if info, err := os.Stat(c.DatasetsDir); err != nil || !info.IsDir() {
			errs = append(errs, fmt.Errorf("datasets_dir %q is not a directory", c.DatasetsDir))
		}
	}
	if p := c.Storage.Postgres; p != nil && p.ResolvedDSN() == "" {
		errs = append(errs, errors.New("storage.postgres needs dsn, or dsn_env naming a set variable"))
	}
	if ch := c.Storage.ClickHouse; ch != nil && ch.URL == "" {
		errs = append(errs, errors.New("storage.clickhouse needs url"))
	}
	for i, sc := range c.Sinks {
		if err := sc.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("sinks[%d]: %w", i, err))
		}
	}
	if err := c.Sandbox.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("sandbox: %w", err))
	}
	errs = append(errs, c.validateAccess()...)
	if c.Evaluate.BatchSize < 1 || c.Evaluate.Parallelism < 1 || c.Evaluate.MaxRecords < 1 {
		errs = append(errs, errors.New("evaluate.batch_size, parallelism and max_records must be positive"))
	}
	return errors.Join(errs...)
}

// validateAccess enforces secure defaults: a server reachable from other
// machines must say how it authenticates, and bearer credentials must not
// cross the network in plaintext unless TLS terminates in front of it.
func (c Config) validateAccess() []error {
	var errs []error
	if err := c.Auth.Validate(); err != nil {
		errs = append(errs, err)
	}
	for _, err := range []error{c.RBAC.Validate(), c.Authorization.Validate(), c.Audit.Validate()} {
		if err != nil {
			errs = append(errs, err)
		}
	}
	addrs := []string{c.Listen, c.OTLP.GRPCListen, c.OTLP.HTTPListen}
	for _, addr := range addrs {
		if addr == "" || auth.IsLoopback(addr) {
			continue
		}
		if c.Auth == nil {
			errs = append(errs, fmt.Errorf("refusing to listen on %s without authentication: add an auth section, "+
				"or set `auth: {mode: none}` to run unauthenticated on purpose", addr))
			continue
		}
		if c.Auth.BearerMethods() && c.Auth.TLS == nil && !c.Auth.AllowPlaintext {
			errs = append(errs, fmt.Errorf("refusing to accept bearer credentials in plaintext on %s: configure auth.tls, "+
				"or set auth.allow_plaintext if TLS terminates in front of evalsid", addr))
		}
	}
	if c.Metrics.Listen != "" && !auth.IsLoopback(c.Metrics.Listen) {
		errs = append(errs, fmt.Errorf("metrics.listen %s serves without authentication and must be a loopback address", c.Metrics.Listen))
	}
	// With mode: none the sections are kept but switched off on purpose.
	if c.Auth == nil && (len(c.RBAC.Roles) > 0 || len(c.RBAC.Projects) > 0 || len(c.RBAC.Owners) > 0 || len(c.Authorization.Rules) > 0) {
		errs = append(errs, errors.New("rbac and authorization need an auth section; without one nothing would be enforced"))
	}
	return errs
}

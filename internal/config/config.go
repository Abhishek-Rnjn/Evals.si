// Package config loads the evalsid server configuration (evalsi.yaml).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/cluster"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/mcp"
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
	// Projects that may use this judge ("*" for every project); empty: all.
	// Its key and budget are the server's, so a judge can be kept to the
	// projects that pay for it.
	Projects []string `json:"projects,omitempty"`
}

// WorkerJudges is the judges as the Python worker sees them: settings the
// server enforces itself, such as Projects, are left out.
func (c Config) WorkerJudges() map[string]Judge {
	out := make(map[string]Judge, len(c.Judges))
	for name, j := range c.Judges {
		j.Projects = nil
		out[name] = j
	}
	return out
}

// Worker configures the Python evaluator worker that evalsid supervises.
type Worker struct {
	// Command that runs the evalsi CLI; "worker --listen ..." is appended.
	Command []string `json:"command"`
	// How long to wait for the worker to answer Describe after starting.
	StartTimeout string `json:"start_timeout"`
	// Disable the worker's judge response cache.
	NoCache bool `json:"no_cache"`
	// In a cluster: the pools this process's Python worker serves from the
	// work queues (cpu, judge, gpu, sandbox, harness). For `evalsid worker`
	// the default is every pool; for `evalsid serve`, none (an API replica
	// sends all work to worker processes).
	Pools []string `json:"pools,omitempty"`
	// Tasks a worker process runs at once, per pool; default 4.
	Concurrency int `json:"concurrency,omitempty"`
	// A remote sandbox service (the Kubernetes sandbox pool, or sandboxd on
	// KVM nodes) for agent sandboxes and code evaluators, instead of
	// sandboxes in this process. Worker pods hold credentials; sandboxes
	// then never run beside them.
	SandboxService *SandboxService `json:"sandbox_service,omitempty"`
}

// SandboxService is a remote SandboxService, reached over mutual TLS.
type SandboxService struct {
	// tls://host:port
	Address  string `json:"address"`
	CAFile   string `json:"ca_file"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// The name its certificate is checked against, when not the address host.
	ServerName string `json:"server_name,omitempty"`
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

// Rewards tunes the Reward Service.
type Rewards struct {
	// Upper bound on rollouts in one ScoreRewards call.
	MaxRollouts int `json:"max_rollouts"`
	// Rollouts being scored at once across all calls; further calls wait
	// (back-pressure on the trainer) until their context ends.
	MaxInflight int `json:"max_inflight"`
	// Component scores kept in the in-memory cache.
	CacheSize int `json:"cache_size"`
	// Also keep component scores in the database, shared by every replica
	// (with PostgreSQL), and how long to keep them (default 168h).
	SharedCache    bool   `json:"shared_cache,omitempty"`
	SharedCacheTTL string `json:"shared_cache_ttl,omitempty"`
}

// Webhooks tunes delivery of the events projects subscribe to through the
// WebhookService.
type Webhooks struct {
	// Attempts per delivery before it is marked failed (default 6, spread
	// over about 80 minutes).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// How long to wait for an endpoint to answer (default 10s).
	Timeout string `json:"timeout,omitempty"`
	// How long finished deliveries are kept for inspection (default 168h).
	Retention string `json:"retention,omitempty"`
}

// Sources configures trace sources: stores such as MLflow that Evals.si pulls
// traces from and writes scores to (decision 0016).
type Sources struct {
	// Where the chart mounts Secrets for sources: a source's credentials.file
	// "<secret>/<key>" is read from <dir>/<secret>/<key> at each use.
	Dir string `json:"dir,omitempty"`
	// Hosts a source's endpoint may have although it is plain HTTP or a
	// private, loopback or cluster-internal address (exact, or *.domain).
	// Any other endpoint must be https to a public name, so that a project
	// editor cannot point the server at an internal service.
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// The most traces per second a source pulls when it sets no cap (default 200).
	MaxRecordsPerSecond int `json:"max_records_per_second,omitempty"`
}

// Quotas limit what each project may use (§17 "Tenancy"). Zero means
// unlimited. Default applies to every project; Projects overrides it per
// project, field by field (a zero there inherits the default).
type Quotas struct {
	Default  QuotaLimits            `json:"default"`
	Projects map[string]QuotaLimits `json:"projects,omitempty"`
}

// QuotaLimits are one project's limits.
type QuotaLimits struct {
	// Runs executing at once; further runs wait in PENDING. Per replica.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// Stored runs; CreateRun fails past it until old runs are deleted.
	MaxStoredRuns int `json:"max_stored_runs,omitempty"`
	// Tokens per UTC day across the project's runs: judges, and the target
	// (the model under evaluation, or the agent's model).
	JudgeTokensPerDay  int64 `json:"judge_tokens_per_day,omitempty"`
	TargetTokensPerDay int64 `json:"target_tokens_per_day,omitempty"`
	// Reward Service rollouts being scored at once; further calls wait. Per replica.
	MaxRewardRollouts int `json:"max_reward_rollouts,omitempty"`
}

// For is a project's limits: its own fields, else the default's.
func (q Quotas) For(project string) QuotaLimits {
	l := q.Default
	p, ok := q.Projects[project]
	if !ok {
		return l
	}
	if p.MaxConcurrentRuns != 0 {
		l.MaxConcurrentRuns = p.MaxConcurrentRuns
	}
	if p.MaxStoredRuns != 0 {
		l.MaxStoredRuns = p.MaxStoredRuns
	}
	if p.JudgeTokensPerDay != 0 {
		l.JudgeTokensPerDay = p.JudgeTokensPerDay
	}
	if p.TargetTokensPerDay != 0 {
		l.TargetTokensPerDay = p.TargetTokensPerDay
	}
	if p.MaxRewardRollouts != 0 {
		l.MaxRewardRollouts = p.MaxRewardRollouts
	}
	return l
}

func (l QuotaLimits) valid() bool {
	return l.MaxConcurrentRuns >= 0 && l.MaxStoredRuns >= 0 && l.JudgeTokensPerDay >= 0 &&
		l.TargetTokensPerDay >= 0 && l.MaxRewardRollouts >= 0
}

// Runs tunes the Run door.
type Runs struct {
	// Runs executing at once; the rest wait as PENDING.
	MaxConcurrent int `json:"max_concurrent"`
	// In a cluster: how long a replica's claim on a run lasts without
	// renewal (default 30s), and how often replicas look for runs to adopt
	// from replicas that stopped (default 10s).
	LeaseTTL      string `json:"lease_ttl,omitempty"`
	AdoptInterval string `json:"adopt_interval,omitempty"`
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
	// Worker processes and evalsid replicas, connected through NATS
	// JetStream. Several API replicas must share storage.postgres; worker
	// processes use no database.
	Cluster *cluster.Config `json:"cluster,omitempty"`
	Runs    Runs            `json:"runs"`
	OTLP    OTLP            `json:"otlp"`
	Traces  Traces          `json:"traces"`
	// Online evaluation policies applied at startup, in the OnlineEvalPolicy
	// JSON form. They replace stored policies of the same name.
	Policies     []json.RawMessage `json:"policies"`
	Worker       Worker            `json:"worker"`
	Judges       map[string]Judge  `json:"judges"`
	DefaultJudge string            `json:"default_judge"`
	Evaluate     Evaluate          `json:"evaluate"`
	Rewards      Rewards           `json:"rewards"`
	Quotas       Quotas            `json:"quotas"`
	// Isolation for code-executing evaluators and agent tasks.
	Sandbox sandbox.Config `json:"sandbox"`
	// What agent-run specs may make the worker execute outside the sandbox.
	Agents Agents `json:"agents"`
	// Where finished runs and online scores are exported (MLflow, OTel).
	Sinks []sinks.Config `json:"sinks"`
	// Signed HTTP callbacks when runs finish.
	Webhooks Webhooks `json:"webhooks"`
	// Trace sources pulled from MLflow and other stores.
	Sources Sources `json:"sources"`
	// Authentication: JWT providers, API keys, TLS. Required on a
	// non-loopback listen address; `auth: {mode: none}` opts out explicitly.
	Auth *auth.Config `json:"auth,omitempty"`
	// Projects, custom roles and role bindings.
	RBAC authz.RBACConfig `json:"rbac"`
	// Global CEL rules and external authorization.
	Authorization authz.AuthorizationConfig `json:"authorization"`
	Audit         authz.AuditConfig         `json:"audit"`
	Metrics       Metrics                   `json:"metrics"`
	// The MCP endpoint (/mcp): on by default, behind the same authentication
	// and authorization as every other route.
	MCP mcp.Config `json:"mcp"`
	// Wasm evaluator plugins, run sandboxed inside evalsid.
	Wasm Wasm `json:"wasm"`
	// The read-only web UI at /ui/.
	UI UI `json:"ui"`
	// Worker variables that requests may name (api_key_env, headers_env,
	// env_from, evaluator params ending in _env), granted per project.
	Credentials credentials.Config `json:"credentials"`

	// Reload reads the configuration again from where it came from (set by
	// LoadWith for a file); nil when there is nothing to reload.
	Reload func() (Config, error) `json:"-"`
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

// CredentialSettings is what the credentials policy is made from: the
// grants, each judge's projects, and whether authentication is on.
func (c Config) CredentialSettings() credentials.Settings {
	judges := map[string][]string{}
	for name, j := range c.Judges {
		judges[name] = j.Projects
	}
	return credentials.Settings{Credentials: c.Credentials, Judges: judges, AuthEnabled: c.AuthEnabled()}
}

// Default returns the configuration used when no file is given.
// UI configures the read-only web UI.
type UI struct {
	Disabled bool `json:"disabled"`
}

// Wasm configures WebAssembly evaluator plugins (internal/wasmeval).
type Wasm struct {
	// Directories searched for evalsi-plugin.yaml manifests; default
	// <data_dir>/plugins, where `evalsi plugins install --dir` puts them.
	PluginDirs []string `json:"plugin_dirs"`
	// Compiled modules are kept here between restarts; default
	// <data_dir>/wasm-cache. "off" compiles on every start.
	CacheDir string `json:"cache_dir"`
	Disabled bool   `json:"disabled"`
}

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
		Rewards:  Rewards{MaxRollouts: 16384, MaxInflight: 65536, CacheSize: 1 << 20},
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
	if path != "" {
		cfg.Reload = func() (Config, error) { return LoadWith(path, overrides...) }
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

var sourceHostRE = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Validate checks the configuration for mistakes that would only surface later.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if len(c.Worker.Command) == 0 {
		errs = append(errs, errors.New("worker.command is required"))
	}
	for i, h := range c.Sources.AllowHosts {
		if !sourceHostRE.MatchString(h) {
			errs = append(errs, fmt.Errorf("sources.allow_hosts[%d]: %q is not a lowercase host name or *.domain", i, h))
		}
	}
	if c.Sources.MaxRecordsPerSecond < 0 {
		errs = append(errs, errors.New("sources.max_records_per_second must not be negative"))
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
	for name, j := range c.Judges {
		if slices.Contains(j.Projects, "") {
			errs = append(errs, fmt.Errorf("judges.%s.projects has an empty name", name))
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
	if err := c.Credentials.Validate(); err != nil {
		errs = append(errs, err)
	}
	for name, d := range map[string]string{"runs.lease_ttl": c.Runs.LeaseTTL, "runs.adopt_interval": c.Runs.AdoptInterval} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration", name))
		}
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
	if s := c.Worker.SandboxService; s != nil {
		if !strings.HasPrefix(s.Address, "tls://") {
			errs = append(errs, errors.New("worker.sandbox_service.address must be tls://host:port"))
		}
		if s.CAFile == "" || s.CertFile == "" || s.KeyFile == "" {
			errs = append(errs, errors.New("worker.sandbox_service needs ca_file, cert_file and key_file (mutual TLS)"))
		}
	}
	if ch := c.Storage.ClickHouse; ch != nil && ch.URL == "" {
		errs = append(errs, errors.New("storage.clickhouse needs url"))
	}
	for i, sc := range c.Sinks {
		if err := sc.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("sinks[%d]: %w", i, err))
		}
	}
	if c.Webhooks.MaxAttempts < 0 {
		errs = append(errs, errors.New("webhooks.max_attempts must not be negative"))
	}
	for field, v := range map[string]string{"timeout": c.Webhooks.Timeout, "retention": c.Webhooks.Retention} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("webhooks.%s must be a positive duration, such as 10s", field))
		}
	}
	if err := c.MCP.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Sandbox.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("sandbox: %w", err))
	}
	errs = append(errs, c.validateAccess()...)
	if c.Evaluate.BatchSize < 1 || c.Evaluate.Parallelism < 1 || c.Evaluate.MaxRecords < 1 {
		errs = append(errs, errors.New("evaluate.batch_size, parallelism and max_records must be positive"))
	}
	quotasValid := c.Quotas.Default.valid()
	for _, l := range c.Quotas.Projects {
		quotasValid = quotasValid && l.valid()
	}
	if !quotasValid {
		errs = append(errs, errors.New("quotas: limits must not be negative"))
	}
	if ttl := c.Rewards.SharedCacheTTL; ttl != "" {
		if d, err := time.ParseDuration(ttl); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("rewards.shared_cache_ttl: %q is not a positive duration", ttl))
		}
	}
	if r := c.Rewards; r.MaxRollouts < 1 || r.MaxInflight < r.MaxRollouts || r.CacheSize < 0 {
		errs = append(errs, errors.New("rewards.max_rollouts must be positive, max_inflight at least max_rollouts, and cache_size not negative"))
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

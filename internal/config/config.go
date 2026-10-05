// Package config loads the evalsid server configuration (evalsi.yaml).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
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
	// Where evalsid keeps its database. Relative paths are relative to the working directory.
	DataDir string `json:"data_dir"`
	// Root for dataset paths in run specs. Empty means runs must send records
	// inline or use a dataset URI.
	DatasetsDir string `json:"datasets_dir"`
	Runs        Runs   `json:"runs"`
	OTLP        OTLP   `json:"otlp"`
	Traces      Traces `json:"traces"`
	// Online evaluation policies applied at startup, in the OnlineEvalPolicy
	// JSON form. They replace stored policies of the same name.
	Policies     []json.RawMessage `json:"policies"`
	Worker       Worker            `json:"worker"`
	Judges       map[string]Judge  `json:"judges"`
	DefaultJudge string            `json:"default_judge"`
	Evaluate     Evaluate          `json:"evaluate"`
	// Isolation for code-executing evaluators (the code pack).
	Sandbox sandbox.Config `json:"sandbox"`
}

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
	return cfg, cfg.Validate()
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
	if c.DatasetsDir != "" {
		if info, err := os.Stat(c.DatasetsDir); err != nil || !info.IsDir() {
			errs = append(errs, fmt.Errorf("datasets_dir %q is not a directory", c.DatasetsDir))
		}
	}
	if err := c.Sandbox.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("sandbox: %w", err))
	}
	if c.Evaluate.BatchSize < 1 || c.Evaluate.Parallelism < 1 || c.Evaluate.MaxRecords < 1 {
		errs = append(errs, errors.New("evaluate.batch_size, parallelism and max_records must be positive"))
	}
	return errors.Join(errs...)
}

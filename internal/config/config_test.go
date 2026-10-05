package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"judges: {j: {provider: openai-compatible, model: m}}": "base_url is required",
		"judges: {j: {provider: other, model: m}}":             "provider must be",
		"default_judge: missing":                               "not among judges",
		"worker: {start_timeout: soon}":                        "start_timeout",
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

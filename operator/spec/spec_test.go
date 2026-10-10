package spec

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The example files parse as resources, the same as the CLI reads them.
func TestExamples(t *testing.T) {
	for path, kind := range map[string]string{
		"../../examples/runs/capitals.yaml":         "run",
		"../../examples/watch/support-policy.yaml":  "policy",
		"../../deploy/e2e/parity-run.yaml":          "run",
		"../../deploy/e2e/sandbox-run.yaml":         "run",
		"../../deploy/e2e/agent-run.yaml":           "run",
		"../../deploy/e2e/policy.yaml":              "policy",
		"../../examples/sources/studio-mlflow.yaml": "source",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Spec map[string]any `json:"spec"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		raw, _ := yaml.Marshal(doc.Spec)
		raw, _ = yaml.YAMLToJSON(raw)
		if kind == "run" {
			s, err := Run(raw)
			if err != nil || (strings.Contains(path, "capitals") && (s.GetTrials() != 3 || len(s.GetGates()) != 2)) {
				t.Errorf("%s: %v %v", path, s, err)
			}
		} else if kind == "source" {
			src, err := Source(raw)
			if err != nil || src.GetBackfill().GetSince().AsDuration().Hours() != 720 || src.GetMaxTraceDuration().AsDuration().Minutes() != 30 {
				t.Errorf("%s: %v %v", path, src, err)
			}
		} else {
			p, err := Policy(raw)
			if err != nil || p.GetWindow().AsDuration().Seconds() != 300 || len(p.GetStages()) == 0 {
				t.Errorf("%s: %v %v", path, p, err)
			}
		}
	}
}

func TestInvalid(t *testing.T) {
	for raw, want := range map[string]string{
		`{}`:                           "evaluators",
		`{"evaluators":[{"ref":"x"}]}`: "dataset",
		`{"evaluators":[{"ref":"x"}],"dataset":{"path":"a"},"bogus":1}`:                "unknown field",
		`{"evaluators":[{"ref":"x"}],"dataset":{"path":"a"},"gates":[{"metric":"x"}]}`: "min or max",
		`[]`: "object",
	} {
		if _, err := Run([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
	if _, err := Policy([]byte(`{"name":"x","stages":[{"evaluators":[{"ref":"a"}]}]}`)); err == nil {
		t.Error("a policy spec with a name")
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]string{`"10m"`: "600s", `"250ms"`: "0.25s", `90`: "90s", `"1.5h"`: "5400s", `"720h"`: "2592000s", `3000000`: "3000000s"} {
		p, err := Policy([]byte(`{"window":` + in + `,"stages":[{"evaluators":[{"ref":"a"}]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := p.GetWindow().AsDuration().String(); got != map[string]string{"600s": "10m0s", "0.25s": "250ms", "90s": "1m30s", "5400s": "1h30m0s", "2592000s": "720h0m0s", "3000000s": "833h20m0s"}[want] {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestSourceRefusesWhatMetadataOrTheServerOwns(t *testing.T) {
	for raw, want := range map[string]string{
		`{"connector":"mlflow","endpoint":"https://m","name":"x"}`:            "metadata",
		`{"connector":"mlflow","endpoint":"https://m","project":"x"}`:         "metadata",
		`{"connector":"mlflow","endpoint":"https://m","status":{"pulled":1}}`: "server",
		`{"endpoint":"https://m"}`:                                            "connector",
		`{"connector":"mlflow"}`:                                              "endpoint",
		`{"connector":"mlflow","endpoint":"https://m","bogus":1}`:             "invalid spec",
	} {
		if _, err := Source([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error about %s", raw, err, want)
		}
	}
}

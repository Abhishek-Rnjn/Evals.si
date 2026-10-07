package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/wasmeval"
)

// TestWasmPlugins builds the example Wasm plugin, installs it with the CLI,
// and scores with it twice: in Python (through `evalsid wasm serve`) and on
// a server (in evalsid, beside the Python worker's evaluators). Both give
// the same answers.
func TestWasmPlugins(t *testing.T) {
	if os.Getenv("EVALSI_E2E_WORKER") == "" {
		t.Skip("set EVALSI_E2E_WORKER to run end-to-end tests")
	}
	src := t.TempDir()
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(src, "text-checks.wasm"), "../../examples/wasm/text-checks")
	build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	manifest, _ := os.ReadFile("../../examples/wasm/text-checks/evalsi-plugin.yaml")
	_ = os.WriteFile(filepath.Join(src, "evalsi-plugin.yaml"), manifest, 0o644)
	if _, err := wasmeval.Pin(filepath.Join(src, "evalsi-plugin.yaml")); err != nil {
		t.Fatal(err)
	}

	plugins := t.TempDir()
	worker := strings.Fields(os.Getenv("EVALSI_E2E_WORKER"))
	self, _ := os.Executable()
	python := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(worker[0], append(append([]string{}, worker[1:]...), args...)...)
		cmd.Env = append(os.Environ(), "EVALSID="+self, "EVALSI_PLUGIN_PATH="+plugins)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	python("plugins", "install", filepath.Join(src, "evalsi-plugin.yaml"))
	if out := python("plugins", "list"); !strings.Contains(out, "example/text-checks") {
		t.Fatalf("list: %s", out)
	}

	// In Python.
	py := exec.Command(worker[0], "-c", `
import json
from evalsi import evaluate
res = evaluate(
    [{"id": "a", "output": '{"answer": 1}'}, {"id": "b", "output": "not json"}, {"id": "c", "output": '{"answer": 1}'}],
    [{"ref": "example/json-valid", "params": {"required_keys": ["answer"]}}, "example/distinct-outputs"],
)
print(json.dumps({m: res.metric(m).mean for m in ("json-valid", "distinct-outputs")}))
`)
	py.Env = append(os.Environ(), "EVALSID="+self, "EVALSI_PLUGIN_PATH="+plugins)
	out, err := py.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"json-valid": 0.6666666666666666`) || !strings.Contains(string(out), `"distinct-outputs": 0.6666666666666666`) {
		t.Fatalf("python: %v\n%s", err, out)
	}

	// On a server: the same plugin, loaded by evalsid.
	e := start(t, func(c *config.Config) { c.Wasm.PluginDirs = []string{plugins} })
	ctx := context.Background()
	cat := evalsiv1alpha1connect.NewCatalogServiceClient(h2cClient(), e.base)
	list, err := cat.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var wasm, python3 int
	for _, m := range list.Msg.GetEvaluators() {
		switch m.GetRuntime() {
		case "wasm":
			wasm++
			if m.GetTier() != "community" {
				t.Errorf("tier: %v", m)
			}
		case "python":
			python3++
		}
	}
	if wasm != 3 || python3 == 0 {
		t.Errorf("catalog: %d wasm, %d python", wasm, python3)
	}
	params, _ := structpb.NewStruct(map[string]any{"required_keys": []any{"answer"}})
	client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), e.base)
	resp, err := client.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{
			{Ref: "example/json-valid", Params: params},
			{Ref: "example/distinct-outputs"},
			{Ref: "exact-match"},
		},
		Records: []*evalsiv1alpha1.Record{
			{Id: "a", Output: text(`{"answer": 1}`), Reference: text(`{"answer": 1}`)},
			{Id: "b", Output: text("not json"), Reference: text("x")},
			{Id: "c", Output: text(`{"answer": 1}`), Reference: text("y")},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	means := map[string]float64{}
	for _, s := range resp.Msg.GetSummaries() {
		means[s.GetMetric()] = s.GetMean()
	}
	if means["json-valid"] != 2.0/3 || means["distinct-outputs"] != 2.0/3 || means["exact-match"] != 1.0/3 {
		t.Errorf("server: %v", means)
	}
}

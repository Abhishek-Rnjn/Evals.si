package wasmeval

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// built compiles a plugin to Wasm into a fresh directory with its
// manifest, pinned, and returns the manifest's path.
func built(t *testing.T, pkg, manifest string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds Wasm modules")
	}
	dir := t.TempDir()
	m, err := ReadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(dir, m.Runtime.Wasm.Module), pkg)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", pkg, err, out)
	}
	raw, _ := os.ReadFile(manifest)
	path := filepath.Join(dir, ManifestFile)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Pin(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

func host(t *testing.T) *Host {
	t.Helper()
	h, err := NewHost(filepath.Join(os.TempDir(), "evalsi-wasm-test-cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func TestExamplePlugin(t *testing.T) {
	path := built(t, "../../examples/wasm/text-checks", "../../examples/wasm/text-checks/evalsi-plugin.yaml")
	h := host(t)
	ctx := context.Background()
	if err := h.LoadDirs(ctx, []string{filepath.Dir(path), "/nonexistent"}); err != nil {
		t.Fatal(err)
	}
	ms := h.Manifests()
	if len(ms) != 3 || ms[0].GetRuntime() != "wasm" || ms[0].GetTier() != "community" || ms[0].GetPack() != "example/text-checks" ||
		ms[2].GetScope() != evalsiv1alpha1.Scope_SCOPE_DATASET || ms[1].GetOutputs()[0].GetType() != evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED {
		t.Fatalf("manifests: %v", ms)
	}
	params, _ := structpb.NewStruct(map[string]any{"required_keys": []any{"answer"}})
	resp, err := h.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{BatchId: "b1", Evaluator: "example/json-valid", Params: params,
		Records: []*evalsiv1alpha1.Record{
			{Id: "ok", Output: text(`{"answer": 4}`)},
			{Id: "missing", Output: text(`{"other": 4}`)},
			{Id: "bad", Output: text(`four`)},
			{Id: "none"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.GetResults()
	if resp.GetBatchId() != "b1" || r[0].GetRecordId() != "ok" || !r[0].GetScores()[0].GetPassed() ||
		r[1].GetScores()[0].GetPassed() || !strings.Contains(r[1].GetScores()[0].GetExplanation(), `"answer"`) ||
		r[2].GetScores()[0].GetPassed() || r[3].GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SKIPPED ||
		r[0].GetEvaluatorRef() != "example/json-valid@1.0.0" {
		t.Errorf("results: %v", r)
	}
	red, err := h.Reduce(ctx, &pluginv1alpha1.ReduceRequest{Evaluator: "example/distinct-outputs",
		Records: []*evalsiv1alpha1.Record{{Output: text("a")}, {Output: text("a")}, {Output: text("b")}, {Output: text("c")}}})
	if err != nil || red.GetScores()[0].GetNumber() != 0.75 {
		t.Errorf("reduce: %v %v", red, err)
	}

	// Loading the same plugin twice is a clash.
	if _, err := h.Load(ctx, path); err == nil || !strings.Contains(err.Error(), "also defined") {
		t.Errorf("duplicate: %v", err)
	}
	// A changed module no longer matches its pin.
	mod := filepath.Join(filepath.Dir(path), "text-checks.wasm")
	code, _ := os.ReadFile(mod)
	_ = os.WriteFile(mod, append(code, 0), 0o644)
	if _, err := host(t).Load(ctx, path); err == nil || !strings.Contains(err.Error(), "the manifest pins") {
		t.Errorf("tampered: %v", err)
	}
}

func TestManifestValidation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	for name, doc := range map[string]string{
		"unpinned":   "name: a/b\nversion: 1.0.0\nruntime: {wasm: {module: m.wasm}}\nevaluators: [{name: a/x, outputs: [{name: x, type: passed}]}]\n",
		"no outputs": "name: a/b\nversion: 1.0.0\nruntime: {wasm: {module: m.wasm, sha256: " + strings.Repeat("0", 64) + "}}\nevaluators: [{name: a/x}]\n",
		"judge":      "name: a/b\nversion: 1.0.0\nruntime: {wasm: {module: m.wasm, sha256: " + strings.Repeat("0", 64) + "}}\nevaluators: [{name: a/x, requires: {judge: true}, outputs: [{name: x, type: passed}]}]\n",
		"bare name":  "name: a/b\nversion: 1.0.0\nruntime: {wasm: {module: m.wasm, sha256: " + strings.Repeat("0", 64) + "}}\nevaluators: [{name: x, outputs: [{name: x, type: passed}]}]\n",
		"unknown":    "name: a/b\nversion: 1.0.0\nmystery: 1\n",
		"no module":  "name: a/b\nversion: 1.0.0\nruntime: {wasm: {module: m.wasm, sha256: " + strings.Repeat("0", 64) + "}}\nevaluators: [{name: a/x, outputs: [{name: x, type: passed}]}]\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		_ = os.WriteFile(path, []byte(doc), 0o644)
		if _, err := host(t).Load(ctx, path); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestLimitsAndIsolation(t *testing.T) {
	path := built(t, "./testdata/misbehave", "testdata/misbehave/evalsi-plugin.yaml")
	h := host(t)
	ctx := context.Background()
	if _, err := h.Load(ctx, path); err != nil {
		t.Fatal(err)
	}
	call := func(ev string) (*pluginv1alpha1.EvaluateResponse, error) {
		return h.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: ev, Records: []*evalsiv1alpha1.Record{{Id: "r"}}})
	}
	for ev, want := range map[string]string{
		"test/spin":  "timed out after 2s",
		"test/hog":   "out of memory",
		"test/crash": "exit code 3: boom: bad state",
		"test/shout": "output limit",
	} {
		if _, err := call(ev); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", ev, err, want)
		}
	}
	if resp, err := call("test/panic"); err != nil || resp.GetResults()[0].GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_ERROR ||
		!strings.Contains(resp.GetResults()[0].GetReason(), "nil map") {
		t.Errorf("panic: %v %v", resp, err)
	}
	resp, err := call("test/escape")
	if err != nil || resp.GetResults()[0].GetScores()[0].GetLabel() != "file=true dir=true net=true env=0" {
		t.Errorf("escape: %v %v", resp, err)
	}
	// The clock and the random source are the same on every call.
	a, err1 := call("test/entropy")
	b, err2 := call("test/entropy")
	if err1 != nil || err2 != nil || a.GetResults()[0].GetScores()[0].GetLabel() != b.GetResults()[0].GetScores()[0].GetLabel() {
		t.Errorf("entropy: %v %v %v %v", a, b, err1, err2)
	}
	// The host stays usable after a module was killed.
	if _, err := call("test/escape"); err != nil {
		t.Errorf("after failures: %v", err)
	}
}

// inner is a worker with one Python evaluator and a stale copy of a Wasm one.
type inner struct{ calls []string }

func (w *inner) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match", Runtime: "python"},
		{Name: "example/json-valid", Runtime: "wasm"},
	}, nil
}
func (w *inner) Evaluate(_ context.Context, r *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	w.calls = append(w.calls, r.GetEvaluator())
	return &pluginv1alpha1.EvaluateResponse{}, nil
}
func (w *inner) Reduce(context.Context, *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return nil, errors.New("no")
}
func (w *inner) Generate(context.Context, *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	return nil, errors.New("no")
}
func (w *inner) LoadDataset(context.Context, *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	return nil, errors.New("no")
}
func (w *inner) RunTask(context.Context, *pluginv1alpha1.RunTaskRequest, func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	return nil, errors.New("no")
}

func TestWorker(t *testing.T) {
	path := built(t, "../../examples/wasm/text-checks", "../../examples/wasm/text-checks/evalsi-plugin.yaml")
	h := host(t)
	ctx := context.Background()
	in := &inner{}
	if w := Wrap(in, h); w != in {
		t.Error("an empty host wraps nothing")
	}
	if _, err := h.Load(ctx, path); err != nil {
		t.Fatal(err)
	}
	w := Wrap(in, h)
	ms, err := w.Describe(ctx)
	if err != nil || len(ms) != 4 {
		t.Fatalf("describe: %v %v", ms, err)
	}
	if _, err := w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "builtin/exact-match"}); err != nil || len(in.calls) != 1 {
		t.Errorf("python evaluator: %v %v", in.calls, err)
	}
	resp, err := w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "example/word-limit",
		Records: []*evalsiv1alpha1.Record{{Id: "x", Output: text("a b")}}})
	if err != nil || len(in.calls) != 1 || resp.GetResults()[0].GetScores()[1].GetNumber() != 2 {
		t.Errorf("wasm evaluator: %v %v", resp, err)
	}

	// The JSON-lines service the Python SDK uses.
	var out bytes.Buffer
	req := `{"id":1,"manifest":"` + path + `","verb":"evaluate","request":{"evaluator":"example/word-limit","params":{"max_words":1},"records":[{"id":"r","output":{"text":"a b"}}]}}` + "\n" +
		`{"id":2,"manifest":"` + path + `","verb":"describe"}` + "\n" +
		`{"id":3,"manifest":"/nope/evalsi-plugin.yaml","verb":"describe"}` + "\n"
	if err := Serve(ctx, strings.NewReader(req), &out); err != nil {
		t.Fatal(err)
	}
	// `evalsid wasm run`, with --params before or after the arguments.
	for _, args := range [][]string{
		{"run", "--params", `{"max_words":1}`, path, "example/word-limit"},
		{"run", path, "example/word-limit", "--params", `{"max_words":1}`},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(ctx, args, strings.NewReader(`{"id":"r","output":{"text":"a b"}}`+"\n"), &stdout, &stderr); code != 0 ||
			!strings.Contains(stdout.String(), "the limit is 1") {
			t.Errorf("%v: exit %d\n%s%s", args, code, stdout.String(), stderr.String())
		}
	}

	got := out.String()
	for _, want := range []string{`"id":1,"response":{"results":[{"recordId":"r"`, `"explanation":"2 words; the limit is 1"`, `"id":2,"response":{"evaluators":[`, `"id":3,"error":`} {
		if !strings.Contains(strings.ReplaceAll(got, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Errorf("serve output lacks %s:\n%s", want, got)
		}
	}
}

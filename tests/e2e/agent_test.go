package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// The task: calc.py has a bug; hidden tests (never shown to the agent) grade
// the fix in the sandbox.
const (
	buggy  = "def add(a, b):\n    return a - b\n"
	fixed  = "def add(a, b):\n    return a + b\n"
	hidden = "from calc import add\nassert add(2, 3) == 5\nprint('ok')\n"
)

// agentModel is a tool-calling chat model: it reads calc.py, then writes the
// fix (or, for tasks that say "badly", a wrong one), then answers.
func agentModel(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		task, tools := "", 0
		for _, m := range req.Messages {
			if m["role"] == "user" && task == "" {
				task, _ = m["content"].(string)
			}
			if m["role"] == "tool" {
				tools++
			}
		}
		call := func(name string, args map[string]any) map[string]any {
			raw, _ := json.Marshal(args)
			return map[string]any{"content": "", "tool_calls": []any{map[string]any{
				"id": fmt.Sprintf("call_%d", tools), "type": "function",
				"function": map[string]any{"name": name, "arguments": string(raw)},
			}}}
		}
		var msg map[string]any
		switch tools {
		case 0:
			msg = call("bash", map[string]any{"command": "cat calc.py"})
		case 1:
			content := fixed
			if strings.Contains(task, "badly") {
				content = "def add(a, b):\n    return a * b\n"
			}
			msg = call("write_file", map[string]any{"path": "calc.py", "content": content})
		default:
			msg = map[string]any{"content": "Fixed add() in calc.py."}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 50, "completion_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func needSandbox(t *testing.T) {
	t.Helper()
	probe, err := sandbox.New(sandbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range probe.Probe(context.Background()) {
		if st.Available && st.Driver == "bwrap" {
			return
		}
	}
	t.Skip("the bwrap rung does not work on this host")
}

func agentRunSpec(model string) *evalsiv1alpha1.RunSpec {
	env := &evalsiv1alpha1.Environment{
		Files:   map[string]string{"calc.py": buggy},
		Sandbox: &evalsiv1alpha1.SandboxPolicy{MinIsolation: "namespaced"},
		Checker: &evalsiv1alpha1.Checker{Command: []string{"python3", "test_hidden.py"}, Files: map[string]string{"test_hidden.py": hidden}},
	}
	recs := []*evalsiv1alpha1.Record{
		{Id: "fix", Input: text("Fix the add function in calc.py.")},
		{Id: "fumble", Input: text("Fix the add function in calc.py, badly.")},
	}
	return &evalsiv1alpha1.RunSpec{
		Target:      &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "agent-model", BaseUrl: model},
		Harness:     &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_Builtin{Builtin: &evalsiv1alpha1.BuiltinHarness{MaxSteps: 8}}},
		Environment: env,
		Dataset:     &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: recs}}},
		Evaluators:  []*evalsiv1alpha1.EvaluatorRef{{Ref: "task-success"}, {Ref: "policy-violations"}, {Ref: "tool-call-accuracy"}},
		Trials:      2,
		Gates:       []*evalsiv1alpha1.Gate{{Metric: "task-success", Min: proto.Float64(0.5)}},
	}
}

// TestAgentRuns drives the built-in agent and a bring-your-own CLI agent
// through tasks in the bwrap sandbox, on the server and embedded, with the
// same spec and the same scores.
func TestAgentRuns(t *testing.T) {
	needSandbox(t)
	e := start(t)
	model := agentModel(t).URL + "/v1"
	ctx := context.Background()
	client := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())

	run := func(t *testing.T, spec *evalsiv1alpha1.RunSpec) *evalsiv1alpha1.Run {
		t.Helper()
		created, err := client.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: "agent", Spec: spec}))
		if err != nil {
			t.Fatal(err)
		}
		stream, err := client.WatchRun(ctx, connect.NewRequest(&evalsiv1alpha1.WatchRunRequest{Id: created.Msg.GetRun().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		var final *evalsiv1alpha1.Run
		for stream.Receive() {
			if r := stream.Msg().GetRun(); r != nil {
				final = r
			}
		}
		if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
			// What each record did, so a failed gate says why.
			if res, err := client.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: final.GetId()})); err == nil {
				for _, rec := range res.Msg.GetRecords() {
					var steps []string
					for _, st := range rec.GetTrajectory().GetSteps() {
						steps = append(steps, fmt.Sprintf("%s %q err=%q", st.GetName(), st.GetOutput().GetText(), st.GetError()))
					}
					t.Logf("record %s: check %v steps %v", rec.GetId(), rec.GetCheck(), steps)
				}
				for _, r := range res.Msg.GetResults() {
					t.Logf("result %s: %v %s %v", r.GetRecordId(), r.GetOutcome(), r.GetReason(), r.GetScores())
				}
			}
			t.Fatalf("status %v: %s (summaries %v)", final.GetStatus(), final.GetError(), final.GetSummaries())
		}
		return final
	}
	sums := func(r *evalsiv1alpha1.Run) map[string]float64 {
		out := map[string]float64{}
		for _, s := range r.GetSummaries() {
			out[s.GetMetric()] = s.GetMean()
		}
		return out
	}

	t.Run("built-in agent on the server", func(t *testing.T) {
		final := run(t, agentRunSpec(model))
		s := sums(final)
		if s["task-success"] != 0.5 || s["task-success.pass^2"] != 0.5 || s["policy-violations.policy-clean"] != 1 {
			t.Fatalf("summaries %v", s)
		}
		results, err := client.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: final.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range results.Msg.GetRecords() {
			iso := rec.GetProvenance().GetIsolation()
			if iso.GetDriver() != "bwrap" || iso.GetLevel() != evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NAMESPACED {
				t.Errorf("%s ran under %v", rec.GetId(), iso)
			}
			if rec.GetCheck() == nil || len(rec.GetTrajectory().GetSteps()) < 5 {
				t.Errorf("%s: check %v, %d steps", rec.GetId(), rec.GetCheck(), len(rec.GetTrajectory().GetSteps()))
			}
			if rec.GetCheck().GetPassed() != (rec.GetId() == "fix") {
				t.Errorf("%s: check %v", rec.GetId(), rec.GetCheck())
			}
		}
	})

	t.Run("bring-your-own CLI agent", func(t *testing.T) {
		spec := agentRunSpec(model)
		// A stand-in for a coding agent CLI: it reads the instruction on stdin.
		script := `read task; case "$task" in *badly*) exit 0;; esac; sed -i 's/a - b/a + b/' calc.py; echo patched`
		spec.Target = &evalsiv1alpha1.Target{Agent: &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{
			Command: []string{"sh", "-c", script},
		}}}}
		spec.Evaluators = spec.Evaluators[:2]
		s := sums(run(t, spec))
		if s["task-success"] != 0.5 || s["task-success.pass^2"] != 0.5 {
			t.Fatalf("summaries %v", s)
		}
	})

	t.Run("the same spec embedded", func(t *testing.T) {
		raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(agentRunSpec(model))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		specFile := filepath.Join(dir, "run.json")
		doc := fmt.Sprintf(`{"apiVersion": "evals.si/v1alpha1", "kind": "EvalRun", "metadata": {"name": "embedded"}, "spec": %s}`, raw)
		if err := os.WriteFile(specFile, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		exe, _ := os.Executable()
		args := append(append([]string{}, e.workerCmd[1:]...), "run", "-f", specFile, "--format", "json", "--quiet")
		cmd := exec.Command(e.workerCmd[0], args...)
		cmd.Env = append(os.Environ(), "EVALSID="+exe, "EVALSI_SANDBOX_ADDR=")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("evalsi run: %v\n%s", err, out)
		}
		var result struct {
			Summaries []struct {
				Metric string  `json:"metric"`
				Mean   float64 `json:"mean"`
			} `json:"summaries"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		got := map[string]float64{}
		for _, s := range result.Summaries {
			got[s.Metric] = s.Mean
		}
		if got["task-success"] != 0.5 || got["task-success.pass^2"] != 0.5 {
			t.Fatalf("embedded summaries %v", got)
		}
	})
}

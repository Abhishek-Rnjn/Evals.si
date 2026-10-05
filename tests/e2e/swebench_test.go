package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// A stand-in for a coding agent CLI: it reads the issue on stdin and fixes
// the add bug, not the mul one.
const fixAddOnly = `task=$(cat)
case "$task" in
  *"add(2, 3)"*) sed -i '0,/return a - b/s//return a + b/' calc.py; echo "patched calc.py";;
  *) echo "no idea";;
esac
`

// TestSWEbenchWithACLIAgent is the Phase 3 exit criterion at fixture scale:
// SWE-bench instances (the 5.x format, graded by the official swebench
// code) run as agent tasks with a bring-your-own CLI agent in the sandbox's
// strongest available rung, here bubblewrap.
func TestSWEbenchWithACLIAgent(t *testing.T) {
	worker := os.Getenv("EVALSI_E2E_SWEBENCH_WORKER")
	if worker == "" {
		t.Skip("set EVALSI_E2E_SWEBENCH_WORKER to the SWE-bench adapter's python -m evalsi")
	}
	needSandbox(t)
	argv := strings.Fields(worker)
	e := start(t, func(c *config.Config) { c.Worker.Command = argv })
	fixture := filepath.Join(e.datasets, "swebench-mini.jsonl")
	if out, err := exec.Command(argv[0], "-m", "evalsi_swebench.fixture", fixture).CombinedOutput(); err != nil {
		t.Fatalf("writing the fixture: %v\n%s", err, out)
	}
	client := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	runAgent := func(command, uri string) *evalsiv1alpha1.Run {
		return runCLIAgent(t, client, command, uri, 0.5)
	}
	mean := func(r *evalsiv1alpha1.Run, metric string) float64 { return summaryMean(t, r, metric) }

	run := runAgent(fixAddOnly, "swebench://swebench-mini.jsonl")
	if m := mean(run, "task-success"); m != 0.5 {
		t.Errorf("resolved %v of the instances, want 0.5", m)
	}
	results, err := client.ListRunResults(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: run.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range results.Msg.GetRecords() {
		if rec.GetProvenance().GetIsolation().GetDriver() != "bwrap" {
			t.Errorf("%s ran under %v", rec.GetId(), rec.GetProvenance().GetIsolation())
		}
		check := rec.GetCheck()
		want := rec.GetId() == "evalsi__calc-1"
		if check.GetPassed() != want || check.GetTests()["test_sub (tests.test_calc.CalcTest.test_sub)"] != "passed" {
			t.Errorf("%s: %v", rec.GetId(), check)
		}
		// The agent's change is on the record, for code review evaluators.
		if diff := rec.GetMetadata()["diff"].GetStringValue(); want != strings.Contains(diff, "+    return a + b") {
			t.Errorf("%s diff: %q", rec.GetId(), diff)
		}
	}
	// The gold patches resolve every instance: the setup grades correctly.
	oracle := runAgent("git apply .evalsi-gold.patch && rm .evalsi-gold.patch", "swebench://swebench-mini.jsonl?oracle=true")
	if m := mean(oracle, "task-success"); m != 1 {
		t.Errorf("gold patches resolved %v, want 1", m)
	}
}

// runCLIAgent runs a bring-your-own CLI agent (a shell command) over a
// dataset URI and waits for the run to succeed.
func runCLIAgent(t *testing.T, client evalsiv1alpha1connect.RunServiceClient, command, uri string, gate float64) *evalsiv1alpha1.Run {
	t.Helper()
	ctx := context.Background()
	created, err := client.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: "cli-agent", Spec: &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Agent: &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{
			Command: []string{"sh", "-c", command},
		}}}},
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Uri{Uri: uri}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "task-success"}, {Ref: "policy-violations"}},
		Gates:      []*evalsiv1alpha1.Gate{{Metric: "task-success", Min: proto.Float64(gate)}},
	}}))
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
		var errs []string
		if res, err := client.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: final.GetId()})); err == nil {
			for _, r := range res.Msg.GetResults() {
				errs = append(errs, r.GetRecordId()+": "+r.GetReason())
			}
		}
		t.Fatalf("status %v: %s %v", final.GetStatus(), final.GetError(), errs)
	}
	return final
}

func summaryMean(t *testing.T, r *evalsiv1alpha1.Run, metric string) float64 {
	t.Helper()
	for _, s := range r.GetSummaries() {
		if s.GetMetric() == metric {
			return s.GetMean()
		}
	}
	t.Fatalf("no %s in %v", metric, r.GetSummaries())
	return 0
}

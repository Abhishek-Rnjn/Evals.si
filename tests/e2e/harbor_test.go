package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// TestHarborAndTerminalBenchTasks runs Harbor and Terminal-Bench 1 task
// directories, imported from their own formats, with a CLI agent in the
// sandbox on the tasks' real image (alpine:3.22, pulled from Docker Hub).
func TestHarborAndTerminalBenchTasks(t *testing.T) {
	if os.Getenv("EVALSI_E2E_IMAGES") == "" {
		t.Skip("set EVALSI_E2E_IMAGES=1 to run tests that pull images")
	}
	needSandbox(t)
	e := start(t)
	argv := strings.Fields(os.Getenv("EVALSI_E2E_WORKER"))
	if out, err := exec.Command(argv[0], "-m", "evalsi_harness.benchmarks.fixtures", e.datasets).CombinedOutput(); err != nil {
		t.Fatalf("writing the fixtures: %v\n%s", err, out)
	}
	client := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	const hello = `cat >/dev/null; printf 'Hello, world!' > /app/hello.txt`
	for _, uri := range []string{"harbor://harbor", "terminal-bench://terminal-bench"} {
		t.Run(uri, func(t *testing.T) {
			run := runCLIAgent(t, client, hello, uri, 1)
			if m := summaryMean(t, run, "task-success"); m != 1 {
				t.Errorf("task-success %v, want 1", m)
			}
			results, err := client.ListRunResults(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: run.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range results.Msg.GetRecords() {
				if rec.GetProvenance().GetIsolation().GetDriver() != "bwrap" {
					t.Errorf("%s ran under %v", rec.GetId(), rec.GetProvenance().GetIsolation())
				}
				if !rec.GetCheck().GetPassed() {
					t.Errorf("%s: %v", rec.GetId(), rec.GetCheck())
				}
			}
			// An agent that does nothing fails the same checks.
			idle := runCLIAgent(t, client, "cat >/dev/null; echo nothing to do", uri, 0)
			if m := summaryMean(t, idle, "task-success"); m != 0 {
				t.Errorf("an idle agent scored %v", m)
			}
		})
	}
	// The reference solutions pass.
	oracle := runCLIAgent(t, client, "cat >/dev/null; sh /solution/solve.sh", "harbor://harbor?oracle=true", 1)
	if m := summaryMean(t, oracle, "task-success"); m != 1 {
		t.Errorf("the oracle scored %v", m)
	}
}

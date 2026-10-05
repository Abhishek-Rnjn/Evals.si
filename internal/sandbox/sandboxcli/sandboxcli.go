// Package sandboxcli implements `evalsid sandbox probe|run`, the interface
// Python code evaluators use to run commands in the sandbox.
package sandboxcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// Main is `evalsid sandbox probe|run`. The sandbox config comes from
// --config (its sandbox section) or, for workers started by evalsid, from
// EVALSI_SANDBOX.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "probe" && args[0] != "run") {
		fmt.Fprint(stderr, "usage: evalsid sandbox probe|run [--config evalsi.yaml]\n")
		return 2
	}
	fs := flag.NewFlagSet("sandbox "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to evalsi.yaml")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var sc sandbox.Config
	switch {
	case *configPath != "":
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "evalsid: %v\n", err)
			return 1
		}
		sc = cfg.Sandbox
	case os.Getenv("EVALSI_SANDBOX") != "":
		if err := json.Unmarshal([]byte(os.Getenv("EVALSI_SANDBOX")), &sc); err != nil {
			fmt.Fprintf(stderr, "evalsid: EVALSI_SANDBOX: %v\n", err)
			return 1
		}
	}
	sb, err := sandbox.New(sc)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	if args[0] == "probe" {
		enc.SetIndent("", "  ")
		_ = enc.Encode(sb.Probe(ctx))
		return 0
	}
	var req sandbox.Request
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		fmt.Fprintf(stderr, "evalsid: bad sandbox request: %v\n", err)
		return 1
	}
	res, err := sb.Run(ctx, &req)
	if err != nil {
		outcome := sandbox.OutcomeUnavailable
		if !errors.Is(err, sandbox.ErrUnavailable) {
			outcome = sandbox.OutcomeRunnerFailure
		}
		res = &sandbox.Result{Outcome: outcome, ExitCode: -1, Error: err.Error()}
	}
	_ = enc.Encode(res)
	return 0
}

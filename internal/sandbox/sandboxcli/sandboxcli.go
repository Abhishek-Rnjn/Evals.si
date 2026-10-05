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
	"strings"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox/sandboxsvc"
)

// Main is `evalsid sandbox probe|run|serve`. The sandbox config comes from
// --config (its sandbox section) or, for workers started by evalsid, from
// EVALSI_SANDBOX.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "probe" && args[0] != "run" && args[0] != "serve") {
		fmt.Fprint(stderr, "usage: evalsid sandbox probe|run|serve [--config evalsi.yaml]\n")
		return 2
	}
	fs := flag.NewFlagSet("sandbox "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to evalsi.yaml")
	listen := fs.String("listen", "", "serve: unix:///path/to/socket")
	untilEOF := fs.Bool("until-stdin-eof", false, "serve: stop when stdin closes (the parent exited)")
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
	if args[0] == "serve" {
		return serve(ctx, sb, *listen, *untilEOF, stdin, stdout, stderr)
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

// serve is `evalsid sandbox serve`: SandboxService on a Unix socket, for
// embedded runs (`evalsi run` with agent tasks starts one per run).
func serve(ctx context.Context, sb *sandbox.Sandbox, listen string, untilEOF bool, stdin io.Reader, stdout, stderr io.Writer) int {
	socket, ok := strings.CutPrefix(listen, "unix://")
	if !ok || socket == "" {
		fmt.Fprint(stderr, "evalsid: sandbox serve needs --listen unix:///path/to/socket\n")
		return 2
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := sandbox.NewManager(sb)
	defer m.Close()
	stop, err := sandboxsvc.Serve(ctx, m, socket)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	defer stop()
	fmt.Fprintf(stdout, "listening on %s\n", listen)
	if f, ok := stdout.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	if untilEOF {
		go func() {
			_, _ = io.Copy(io.Discard, stdin)
			cancel()
		}()
	}
	<-ctx.Done()
	return 0
}

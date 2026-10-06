// Command evalsid is the Evals.si daemon: API server, scheduler and OTLP
// ingest. Users normally start it through the Python CLI with `evalsi serve`.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox/sandboxcli"
	"github.com/abhishek-rnjn/evals.si/internal/server"
	"github.com/abhishek-rnjn/evals.si/internal/version"
)

const usage = `usage: evalsid <command> [flags]

commands:
  version          print the build and API version
  serve            run the server (evalsid serve -h for flags)
  worker           serve worker pools from a cluster's queues (evalsid worker -h)
  sandbox probe    report which sandbox rungs work on this host
  sandbox run      run a JSON request from stdin in the sandbox, print the JSON result
  sandbox serve    serve SandboxService on a Unix socket (--listen unix:///path)
  auth check       explain an authorization decision for a token or API key
  auth new-key     generate an API key and the hash to put in config
  auth hash-key    hash an API key read from stdin
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "evalsid %s (api %s)\n", version.Version, version.APIVersion)
		return 0
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "worker":
		return worker(ctx, args[1:], stderr)
	case "sandbox":
		return sandboxcli.Main(ctx, args[1:], os.Stdin, stdout, stderr)
	case "auth":
		return authMain(ctx, args[1:], os.Stdin, stdout, stderr)
	case "sandbox-exec":
		// Internal: the launcher that confines itself, then executes the command.
		return sandbox.Launch(stderr)
	case "sandbox-forward":
		// Internal: relays a sandbox's loopback proxy port to the egress proxy.
		return sandbox.Forward(args[1:], stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "evalsid: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func serve(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to evalsi.yaml (defaults apply when omitted)")
	listen := fs.String("listen", "", "address to listen on, overriding the config (e.g. 127.0.0.1:8080)")
	noAuth := fs.Bool("no-auth", envTrue(os.Getenv("EVALSID_NO_AUTH")),
		"turn authentication and authorization off, for development (also EVALSID_NO_AUTH=1)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Overrides are applied before validation, so --listen 0.0.0.0:8080 is
	// held to the same rule as the file: no network address without auth.
	cfg, err := config.LoadWith(*configPath, func(c *config.Config) {
		if *listen != "" {
			c.Listen = *listen
		}
		if *noAuth {
			c.DisableAuth()
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := server.Run(ctx, cfg, log, stderr, nil); err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	return 0
}

// worker is a worker process in a cluster: it serves pools from the work
// queues with a local Python worker. SIGTERM lets in-flight tasks finish.
func worker(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to evalsi.yaml (needs a cluster section)")
	pools := fs.String("pools", "", "comma-separated pools to serve (default: worker.pools, else all)")
	concurrency := fs.Int("concurrency", 0, "tasks at once per pool (default: worker.concurrency, else 4)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadWith(*configPath, func(c *config.Config) {
		if *concurrency > 0 {
			c.Worker.Concurrency = *concurrency
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	selected := cfg.Worker.Pools
	if *pools != "" {
		selected = strings.Split(*pools, ",")
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := server.RunWorker(ctx, cfg, selected, log, stderr); err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	return 0
}

func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

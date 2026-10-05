// Command evalsid is the Evals.si daemon: API server, scheduler and (later)
// OTLP ingest. Users normally start it through the Python CLI with `evalsi serve`.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/server"
	"github.com/abhishek-rnjn/evals.si/internal/version"
)

const usage = `usage: evalsid <command> [flags]

commands:
  version   print the build and API version
  serve     run the server (evalsid serve -h for flags)
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := server.Run(ctx, cfg, log, stderr, nil); err != nil {
		fmt.Fprintf(stderr, "evalsid: %v\n", err)
		return 1
	}
	return 0
}

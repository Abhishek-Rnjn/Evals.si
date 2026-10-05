// Command evalsid is the Evals.si daemon: API server, scheduler and OTLP
// ingest. Users normally start it through the Python CLI with `evalsi serve`.
//
// Phase 0 ships only the command skeleton; the server arrives in Phase 1.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/abhishek-rnjn/evals.si/internal/version"
)

const usage = `usage: evalsid <command>

commands:
  version   print the build and API version
  serve     run the server (available from Phase 1)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "evalsid %s (api %s)\n", version.Version, version.APIVersion)
		return 0
	case "serve":
		fmt.Fprintln(stderr, "evalsid serve: the server is not implemented yet (planned for Phase 1)")
		return 1
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "evalsid: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

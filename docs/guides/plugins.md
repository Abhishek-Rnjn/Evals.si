# Plugins: the index and Wasm evaluators

Evaluators come from packs. These are the packs that ship with evalsi
(*native*), adapters over other frameworks (*wrapped*), and third-party
plugins (*community*). The catalog (`evalsi catalog`, `ListEvaluators`)
shows each evaluator's tier, and its runtime: `python` for the worker,
`wasm` for a sandboxed module in evalsid.

## Find and install plugins

```bash
evalsi plugins search rag              # the index: name, version, tier, kind
evalsi plugins show ragas
evalsi plugins install ragas           # a python plugin: pip installs it here
evalsi plugins install ./text-checks/evalsi-plugin.yaml          # a Wasm plugin, from a path
evalsi plugins install https://example.com/x/evalsi-plugin.yaml --sha256 <manifest sha>
evalsi plugins list                    # what this environment has, with tiers
evalsi plugins remove example/text-checks
```

The default index is
[`plugins/index.yaml`](../../plugins/index.yaml) in this repository. Set
`EVALSI_PLUGIN_INDEX` (space-separated URLs or paths) or pass `--index` to
use your own instead, for example an internal mirror. An index entry is
one of four kinds:

| Kind | Install does |
|---|---|
| `native` | nothing: the pack ships with evalsi; enable it per project with `evaluators.packs` |
| `python` | `pip install <requirement>` in the current environment (with `uv pip` when run inside a uv virtualenv) |
| `wasm` | downloads the manifest and checks it against the sha256 the index pins, then downloads the module and checks it against the sha256 the manifest pins; only then writes both to the plugins directory |
| `image` | prints an `Evaluator` resource for the image, which must be pinned by digest |

Wasm plugins are installed into `~/.evalsi/plugins`, or the first
directory of `EVALSI_PLUGIN_PATH`, or `--dir`. A server loads them from
`<data_dir>/plugins` (`wasm.plugin_dirs` in `evalsi.yaml`), so for a
server run `evalsi plugins install ... --dir <data_dir>/plugins` and
restart it.

## Wasm evaluators

A Wasm evaluator is a WebAssembly module. It runs inside evalsid with no
filesystem, no network, no environment variables, a fixed clock, a
seeded random source, a memory cap and a deadline. It can read its input
and write its answer, and nothing else. This makes Wasm the safe way to
run small evaluators you did not write: a community plugin needs no
sandbox rung, and it gives the same answer every time and everywhere.

It is also fast: about 10 ms per call for a module built with Go, and
calls are batched. Wasm evaluators work everywhere evaluators do: the
`evaluate` API, runs, online policies, guardrails and rewards. In Python
(`evalsi eval`, `evaluate()`), evalsi runs them through one long-lived
`evalsid wasm serve` process, so `evalsid` must be on `PATH` or named by
`EVALSID`.

| Limit | Default | Ceiling | Set in the manifest |
|---|---|---|---|
| Memory | 64 MB | 512 MB | `limits.memory_mb` |
| Time per call (a batch of records) | 10s | 120s | `limits.timeout` |
| Output | 16 MB | | |

A module that crashes, times out or runs out of memory fails that call
with its stderr. A module that panics on one record (with the Go SDK)
marks only that record as errored. Wasm evaluators cannot use a judge or
the sandbox, because they have no I/O.

### Write one in Go

```go
package main

import (
	"strings"

	"github.com/abhishek-rnjn/evals.si/pkg/wasmplugin"
)

func main() {
	wasmplugin.Main(wasmplugin.Plugin{Evaluators: map[string]wasmplugin.Evaluator{
		"acme/no-todo": func(r wasmplugin.Record, _ wasmplugin.Params) wasmplugin.Result {
			if strings.Contains(r.Output.String(), "TODO") {
				return wasmplugin.Scores(wasmplugin.Fail("left a TODO"))
			}
			return wasmplugin.Scores(wasmplugin.Pass())
		},
	}})
}
```

```bash
GOOS=wasip1 GOARCH=wasm go build -o acme.wasm .
evalsid wasm pin evalsi-plugin.yaml          # write the module's sha256 into the manifest
evalsid wasm check evalsi-plugin.yaml        # verify, compile, list the evaluators
echo '{"output": {"text": "done"}}' | evalsid wasm run evalsi-plugin.yaml acme/no-todo
```

[`examples/wasm/text-checks`](../../examples/wasm/text-checks) is a
complete plugin with two record evaluators, a dataset evaluator and
params.

### The manifest

```yaml
# evalsi-plugin.yaml
name: acme/checks              # the pack's name
version: 1.0.0
tier: community                # the default for Wasm plugins
description: ...
license: Apache-2.0
runtime:
  wasm:
    module: acme.wasm          # next to the manifest
    sha256: 3f1c...            # evalsid wasm pin
limits: {memory_mb: 64, timeout: 5s}
evaluators:
  - name: acme/no-todo         # namespaced
    description: ...
    scope: record              # or dataset
    requires: {output: true}   # input, output, reference, context, trajectory
    outputs:
      - {name: no-todo, type: passed, higher_is_better: true}   # number, passed or label
    params_schema:             # JSON Schema; defaults become the params' defaults
      type: object
      properties: {max_words: {type: integer, default: 100}}
```

### Other languages: the ABI

Any language that compiles to a WASI preview 1 command module can
implement the ABI (`evalsi wasm ABI 1`) directly:

1. **Start.** evalsid runs the module's `_start` with argv
   `[<plugin name>, "evaluate"]`. Dataset-scope evaluators get `"reduce"`
   instead.
2. **Input.** The module reads one JSON object from stdin:
   `{"abi": 1, "evaluator": "acme/no-todo", "params": {...}, "records": [...]}`.
   The records use the protobuf JSON mapping of `evalsi.v1alpha1.Record`
   (`{"id", "input", "output", "reference", "context", "metadata"}`, where
   each content is `{"text": ...}`, `{"messages": {"messages": [...]}}` or
   `{"json": ...}`).
3. **Output.** The module writes one JSON object to stdout and exits 0:
   - for `evaluate`: `{"results": [...]}`, one result per record and in
     order. Each result is `{"outcome": "OUTCOME_SCORED" | "OUTCOME_SKIPPED" | "OUTCOME_ERROR", "scores": [...], "reason": "..."}`,
     and a score is `{"name", "number" | "passed" | "label", "explanation"}`;
   - for `reduce`: `{"scores": [...]}`.
4. **Failure.** A non-zero exit fails the call, with stderr as the reason.

Rust, for example, builds with `cargo build --target wasm32-wasip1` and
`serde_json` over stdin and stdout.

## Publish to the index

1. **Host the files.** Publish the manifest and the module side by side
   at stable URLs, for example as release assets. The module's URL is
   resolved relative to the manifest.
2. **Add an entry.** In `plugins/index.yaml`, the manifest's sha256 pins
   the manifest, and through it the module:

   ```yaml
   - name: acme/checks
     version: 1.0.0
     tier: community
     kind: wasm
     description: ...
     homepage: https://github.com/acme/evalsi-checks
     license: Apache-2.0
     evaluators: [acme/no-todo]
     wasm:
       manifest: https://github.com/acme/evalsi-checks/releases/download/v1.0.0/evalsi-plugin.yaml
       sha256: <sha256 of that manifest file>
   ```

3. **Open a pull request.** A new version is a new entry; `install` picks
   the newest unless you ask for `name@version`.

Python plugins are listed the same way, with
`python: {requirement: "<pip requirement>"}`; their packs register
themselves under the `evalsi.packs` entry point. Image plugins use
`image: {ref: <image>@sha256:...}`.

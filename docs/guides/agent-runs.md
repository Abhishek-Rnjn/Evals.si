# Agent runs

An agent run puts an agent to work on tasks and grades the result. Each task
has its own sandboxed environment. A checker grades the end state; evaluators
then score the trajectory and the change the agent made. Agent runs arrived in
Phase 3 of the [roadmap](../DESIGN.md#23-roadmap); Phase 4 runs them on
Kubernetes too.

- **The agent** is either the built-in tool-calling agent on any OpenAI-compatible or Anthropic model, or your own agent: over A2A, MCP, an OpenAI Responses-compatible API or plain HTTP, or as a command-line program run inside the sandbox.
- **The environment** is an OCI image with files, setup commands and a sandbox policy. It runs on the strongest sandbox rung the host has: Firecracker, bubblewrap or Landlock, or on Kubernetes a hardened pod per sandbox.
- **The grading** is the environment's checker, which runs after the agent finishes, plus any evaluators. With trials, pass@k and pass^k come for free.
- **Benchmarks** import directly: SWE-bench, τ-bench (tau2), Terminal-Bench and other Harbor datasets, and BFCL.

## Quick start

[`examples/agents/fix-calc.yaml`](../../examples/agents/fix-calc.yaml) asks a model to fix two bugs in a git checkout. Run it embedded (it needs `evalsid` on `PATH` for the sandbox) or on a server:

```bash
go build -o bin/evalsid ./cmd/evalsid && export PATH=$PWD/bin:$PATH
cd python && uv sync --all-packages
uv run evalsi run -f ../examples/agents/fix-calc.yaml --judge-provider anthropic --judge-model claude-opus-5-5
uv run evalsi run -f ../examples/agents/fix-calc.yaml --server http://localhost:8080
```

A run spec becomes an agent run when it has `target.agent` or `harness`. To run the built-in agent on a model, set `harness: {builtin: {...}}`; `{}` takes the defaults. Each record is one task. Its `input` is the instruction; its `metadata.environment`, when present, overrides the spec's `environment` field by field. Benchmark importers supply environments this way.

## Agents

**The built-in agent** is used when `target` names a model (`connector` and `model`). It works through these tools:

- `bash`, `read_file` and `write_file` in the sandbox;
- `request_escalation`, which asks for a wider policy. The harness denies it unless `harness.builtin.escalation.allow` lists the kind. Either way, the request is recorded for safety evaluators.

**Your own agent** goes in `target.agent`:

| Kind | Spec | How it is driven |
|---|---|---|
| A2A | `a2a: {url, headers_env}` | JSON-RPC `message/send`, following the task until it settles |
| MCP | `mcp: {url, tool, argument}` | the agent is a tool on an MCP server; each message is one call |
| OpenAI Responses | `responses: {base_url, model, api_key_env}` | `POST <base_url>/responses`; later turns continue with `previous_response_id` |
| HTTP | `http: {url, method, body_template, output_path, messages_path}` | a JSON body from the template (`{{input}}`, `{{messages}}`, `{{task_id}}`); the answer at `output_path`, its steps at `messages_path` |
| CLI | `cli: {command, install, env_from, allow_hosts, timeout}` | runs **inside the task's sandbox**, in its workdir |

A CLI agent:

- gets the instruction in place of `{instruction}` in its command, or on stdin;
- has `install` commands run once, during setup;
- receives only the environment variables named in `env_from` (its own model credentials);
- can reach only `allow_hosts`, through the egress proxy.

Setup uses the environment's `setup_network`. When `install` needs a package index, set `environment: {setup_network: allow}` in the spec.

With a user simulator, every agent kind holds a multi-turn conversation. Remote agents (A2A, MCP, Responses, HTTP) do their work outside the sandbox. A task's environment is still created and checked, so a remote agent that works through, say, MCP tools on the same task gets the same grading.

## Environments

```yaml
environment:
  image: docker.io/library/python:3.13-slim   # OCI image; omit to use the host's system directories, read-only
  files: {calc.py: "..."}                      # relative: the workdir; absolute: the image root
  setup: ["pip install -e ."]                  # run once per distinct environment, then snapshotted
  setup_network: allow                         # network during setup only
  env: {PYTHONDONTWRITEBYTECODE: "1"}
  sandbox:
    workdir: /testbed
    network: deny                              # deny | allowlist (with allow_hosts) | allow
    min_isolation: namespaced                  # refuse to run on a weaker rung
    memory_mb: 4096
  checker:
    files: {test_hidden.py: "..."}             # written after the agent finishes
    command: [python3, -m, pytest, -q]
    parser: exit-code                          # exit-code | json | junit:<path> | module:function
    timeout: 10m
  command_timeout: 2m
```

Notes on environments:

- **Setup is shared.** Setup runs once per distinct environment, and every trial restores from the snapshot.
- **Network.** All traffic goes through a logging egress proxy, so refused hosts show up as policy events.
- **Writable root.** An image root is a private writable copy unless `read_only_root` is set.
- **Infrastructure errors are not scores.** A task whose environment fails (an image that does not pull, failing setup, a checker that cannot run) is reported as an error, never as a failed task.

## Harness options

`harness.builtin` configures the built-in harness:

| Field | What it does |
|---|---|
| `max_steps`, `budget: {usd, tokens, wall_clock}`, `pricing` | stop conditions; spend is computed from `pricing` |
| `instructions` | appended to the system prompt |
| `tools.mcp` | MCP servers (streamable HTTP, or stdio commands) whose tools the agent gets |
| `tools.mocks` | canned tools: one `response`, or `responses` keyed by arguments |
| `tools.faults` | deterministic fault injection (`error`, `timeout`, `malformed`) per tool, at a rate |
| `user_simulator: {persona, goal, max_turns, judge}` | an LLM user for multi-turn tasks, speaking through a judge |
| `recording: {mode: record\|replay, dir, branch_at_step}` | captures model calls and tool I/O. Replay reruns sandbox commands, so the environment is rebuilt; `branch_at_step: N` replays N steps, then continues live |
| `escalation.allow` | escalation kinds the harness grants (`network`) |

`harness.external` runs another harness over the [harness protocol](../../proto/evalsi/harness/v1alpha1/harness.proto) (`Describe`, `Setup`, `Run` streaming trajectory events, `Check`, `Teardown`). It can be a gRPC `address`, a `command` serving it on a socket, or a Python class (`python: module:Class`). The benchmark harnesses below are Python classes.

## Grading

| Evaluator | Reads | Reports |
|---|---|---|
| `task-success` | the checker's verdict | `task-success`, `task-score`; with `trials: k`, `pass@k` and `pass^k` |
| `code-quality` | the agent's git diff (recorded when the workdir is a git repository) | a judge review on correctness, regression safety, cleanliness, tests, scope and maintainability; a weighted `code-quality` score and `mergeable` (every dimension at least `merge_bar`, default 4 of 5) |
| `policy-violations` | sandbox policy events | `policy-clean`, `policy-events`, `escalation-requests` |
| `agent-efficiency` | the trajectory and usage | `within-budget`, `agent-steps`, `agent-tokens`, `agent-cost-usd` |

The trajectory-based evaluators of the `agent` pack also apply: tool-call accuracy, trajectory match, loops and goal completion. Every step is also exported as an OTel span, so an agent run shows up in your tracing backend.

`code-quality` takes `weights` (per dimension), `merge_bar` and `max_diff_chars`. The diff covers changes against `HEAD` plus untracked files. It leaves out the files Evals.si places itself (`.evalsi-*`) and is captured before the checker runs.

## Benchmarks

| Dataset URI | Package | Harness | Graded by |
|---|---|---|---|
| `swebench://<file.jsonl\|.parquet\|dir>?instances=...` | `python/adapters/swebench` (`swebench==5.0.2`) | `builtin`, or a CLI agent | the instance's eval script and `swebench`'s `get_eval_report` |
| `taubench://<domain>?tasks=...` | `python/adapters/taubench` (tau2 v0.2.0, Python 3.12) | `evalsi_taubench:TauBenchHarness` | tau2's `evaluate_simulation`: database state, actions, communicated information |
| `harbor://<dir>?tasks=...` | `evalsi-harness` | `builtin`, or a CLI agent | the task's `tests/test.sh` and its reward file |
| `terminal-bench://<dir>?tasks=...` | `evalsi-harness` | `builtin`, or a CLI agent | `run-tests.sh` and Terminal-Bench's pytest rule |
| `bfcl://<category>?ids=...` | `python/adapters/bfcl` (`bfcl-eval==2026.3.23`) | `evalsi_bfcl:BFCLHarness` | BFCL's AST checker |

See [`examples/agents/benchmarks.yaml`](../../examples/agents/benchmarks.yaml). A worker running an adapter's tasks must have that adapter installed. Point `worker.command` at the adapter's environment, for example `python/adapters/swebench/.venv/bin/python -m evalsi`.

### SWE-bench

The importer reads SWE-bench 5 instances, which carry their image, eval script, log parser and evaluation type. Put the dataset under the server's `datasets_dir`, either as a `.jsonl` file or as `<dir>/test.parquet`, which `swebench`'s loader reads. Then run [`examples/agents/swebench-verified.yaml`](../../examples/agents/swebench-verified.yaml).

- The agent works in `/testbed`.
- The checker resets the test files, applies the test patch, runs the tests, and applies the official FAIL_TO_PASS and PASS_TO_PASS rule.
- `oracle=true` adds the gold patch as `.evalsi-gold.patch`, so an agent that applies it validates the setup.

The scoring code and the image are the same on every rung. That is how the Phase 3 exit criterion holds: identical scoring in Firecracker and in bubblewrap.

### τ-bench (tau2)

`TauBenchHarness` is the built-in harness with a tau2 domain around it:

- **Tools and prompt:** the domain's tools run against a live tau2 environment, and the agent gets tau2's agent instruction and the domain policy.
- **The user:** the Evals.si user simulator plays the task's persona and instructions, through the run's judge. tau2's own LiteLLM user simulator is not used, so provider keys stay with the judge config. Expect scores close to, but not identical with, tau2's leaderboard.
- **Grading:** tau2's own evaluator on the conversation.
- **Not supported:** domains where the user has tools (telecom), and tasks graded by natural-language assertions, which the importer skips.

The adapter needs tau2's `data/` directory (`TAU2_DATA_DIR` or `data_dir=`). Use `trials: k` for τ-bench's pass^k.

### Terminal-Bench and Harbor

Both formats are read directly; no Docker daemon and no Harbor install are needed.

- **The Dockerfile is translated, not built.** `FROM` is the image; `RUN` steps become the snapshotted setup, with `WORKDIR`, `ENV`, `ARG` and `SHELL` applied as Docker would; `COPY` and `ADD` of local files are placed in the image root. Binary files are carried base64-encoded and decoded in the sandbox.
- **Refused, with the reason:** multi-stage builds, remote `ADD`, `RUN --mount`, tasks with several compose services, Harbor multi-step tasks, and verifiers that run in a separate environment.
- **Coverage:** of Terminal-Bench 1's 241 tasks, 219 import.
- **Prebuilt image:** `image=<ref>` replaces the translation. Use it when a task depends on the order of `COPY` and `RUN`, since copied files exist before the first `RUN`.
- **Network:** a task's network setting is respected. Harbor's default is public, so `network=deny` tightens it.

### BFCL

`BFCLHarness`:

- offers the entry's functions to the target model as tools, converted as BFCL does for function-calling models;
- takes one model turn and grades the calls with BFCL's AST checker;
- treats the irrelevance categories as passed when the model calls nothing.

Supported categories: `simple_python`, `simple_java`, `simple_javascript`, `multiple`, `parallel`, `parallel_multiple`, `irrelevance`, their `live_` counterparts, and `live_relevance`. Multi-turn, memory, web-search and format-sensitivity categories need BFCL's executable environments and are not supported.

## From production to regression tests

- **Promote failures into a dataset.** `evalsi promote <run> --dataset <name> --when '<CEL>'` appends the records where the condition holds (over `scores`, `errored`, `trial`, `record`) to `promoted/<project>/<name>.jsonl`. For example: `scores["task-success"] < 1`. Add `--include-outputs` to keep outputs and trajectories for re-scoring. It needs `datasets.write`, which `editor` holds.
- **Turn traces into tasks.** A dataset can be built from production traces (`dataset: {traces: {service, policy, filter, lookback}}`) or from another run's outputs (`dataset: {run: {run_id, trial, all_trials}}`).
- **Shadow replay.** `evalsi shadow -f candidate.yaml` replays recorded inputs against a candidate. It scores the recording with the same evaluators as a baseline run, and compares the two.

## Sandbox rungs

The ladder (`sandbox.ladder`) defaults to `firecracker`, `bwrap`, `landlock`. The strongest rung that works is used, and `evalsid sandbox probe` shows which rungs work on a host. A task that sets `min_isolation` is refused, never downgraded, when no rung meets it.

To enable Firecracker:

```yaml
sandbox:
  firecracker:
    kernel: /var/lib/evalsi/vmlinux       # uncompressed, with virtio-blk, vsock and ext4
    guest: /usr/local/bin/evalsi-guest    # default: next to evalsid (`make build` builds both)
    warm_pool: 2                          # booted VMs kept ready per image
    vcpus: 2
    memory_mb: 2048
    # jailer: /usr/bin/jailer             # optional; needs evalsid to run as root
```

How the Firecracker rung works:

- It needs `/dev/kvm`.
- Images become ext4 root disks.
- Each VM runs `evalsi-guest` as init, which evalsid talks to over vsock.
- There is no network device. Egress, when a policy allows it, reaches the same logging egress proxy over vsock, so the rung needs no tap devices or root.
- Snapshots restore with fresh entropy and the clock set.

### On Kubernetes

Workers hold credentials, so they never run sandboxes themselves: they lease them over mutual TLS from a sandbox pool, set as `sandbox.address` in the workers' config. A pool is either `evalsi-sandboxd` (bubblewrap, or Firecracker on KVM nodes) or a `SandboxClass`, which can also use the **pod** rung: one hardened pod per sandbox from the task's image, with no service-account token and a NetworkPolicy that lets it reach only its pool. The pod rung cannot snapshot, so environment setup runs once per trial. The same run file works with `kubectl apply` as an `EvalRun`. See the [Kubernetes guide](kubernetes.md#sandboxes-on-kubernetes).

## Running untrusted specs on a server

Everything an agent does runs in the sandbox. A few things in a spec would instead run on the worker itself:

- stdio MCP server commands;
- external harness commands;
- Python harness classes;
- checker parsers.

A server runs these only if the config lists them:

```yaml
agents:
  trusted_commands: [["npx", "-y", "@modelcontextprotocol/server-filesystem"]]
  trusted_python: ["my_company.harness:Harness"]
```

References in the `evalsi_*` packages, which include every benchmark harness and parser above, are always allowed. Authorization rules see agent runs as `resource.agent` (`kind`, `harness`, `image`, `network`, `min_isolation`), and `resource.runs_code` is true for them. See the [identity guide](identity.md#4-global-rules).

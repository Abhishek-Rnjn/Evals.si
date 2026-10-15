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

New here? [Gate an agent in CI](gate-an-agent-in-ci.md) is a free, offline walk-through of a pass, a failing gate and a GitHub Actions workflow.

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
| A2A | `a2a: {url, headers_env, timeout, stream}` | JSON-RPC `message/send`, following the task until it settles; with `stream: true`, `message/stream` over server-sent events, where every task state the agent reports (`submitted`, `working`, `input-required`, `completed`, ...) is a step with its message and artifact chunks are joined into the answer (`timeout` is then the longest silence between events; push notifications are not used) |
| MCP | `mcp: {url, tool, argument}` | the agent is a tool on an MCP server; each message is one call |
| OpenAI Responses | `responses: {base_url, model, api_key_env}` | `POST <base_url>/responses`; later turns continue with `previous_response_id` |
| HTTP | `http: {url, method, body_template, output_path, messages_path}` | a JSON body from the template (`{{input}}`, `{{messages}}`, `{{task_id}}`); the answer at `output_path`, its steps at `messages_path` |
| CLI | `cli: {command, install, env_from, allow_hosts, timeout, output_format}` | runs **inside the task's sandbox**, in its workdir |

A CLI agent:

- gets the instruction in place of `{instruction}` in its command, or on stdin;
- has `install` commands run once, during setup;
- receives only the environment variables named in `env_from`, a map from the name in the sandbox to the worker's variable (`env_from: {ANTHROPIC_API_KEY: AGENT_ANTHROPIC_KEY}`), for its own model credentials;
- can reach only `allow_hosts`, through the egress proxy (name the port too: `host:8000`);
- has a trajectory when `output_format` names a format its output is in: `dsh-json` reads DeepSeek Harness's `--json` run events, `claude-stream-json` Claude Code's `--output-format stream-json --verbose` (sub-agent messages hang under the call that started them, and the run's cost is kept), `codex-json` `codex exec --json`, `gemini-json` `gemini --output-format stream-json` and `pi-json` `pi --mode json`, each into model-call and tool-call steps with token counts. A stream with none of the format's events is an error on the run, so a wrong format is not read as an agent that did nothing. Without it, the agent's standard output is its answer and the run has no steps. Node-based agents reserve a large address range, so give them `sandbox: {memory_mb: 4096}`; see [the demos](../../examples/demo/README.md).

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
| `tools.builtin` | more built-in tools, by name: `edit_file` (replace exact text; fails if the text is missing or not unique, keeps a script executable), `glob`, `grep` (extended regular expressions, binary files skipped) and `apply_patch` (a unified diff, checked first and applied all or nothing, through `git apply` or `patch`), `memory` (notes under `.evalsi-memory/` that survive context management: list, read, write, append, delete) `session_search` (search the run's unabridged history for some text, including tool output that was cleared or summarized, and read an entry back in full) `delegate` (hand a self-contained subtask to a sub-agent and keep only its report; see [Sub-agents](#sub-agents)) `tool_search` (the MCP tools start unloaded; see [MCP servers](#mcp-servers)) and `code_mode` (the agent acts through scripts; see [Code mode](#code-mode)). Off by default, so a comparison between models uses the same toolset unless a spec lists them; `edit_file`, `apply_patch` and `memory` run again on replay, like `bash`; naming `memory` or `session_search` also adds a line about it to the system prompt |
| `tools.faults` | deterministic fault injection (`error`, `timeout`, `malformed`) per tool, at a rate |
| `user_simulator: {persona, goal, max_turns, judge}` | an LLM user for multi-turn tasks, speaking through a judge |
| `recording: {mode, dir, branch_at_step, fork_as}` | a session log of model calls and tool I/O, written as it happens. `record` starts one; `replay` serves it (sandbox commands and edits run again, so the environment is rebuilt), and with `branch_at_step: N` replays N turns and then continues live; `resume` continues an interrupted trial (replay its log, then run live and append to it; a trial with no log starts one); `fork` replays the first `branch_at_step` turns of a log and goes live into a new log that names its parent. See [Sessions](#sessions-resume-and-fork) |
| `escalation.allow` | escalation kinds the harness grants (`network`) |

### Long runs: context management

Without `harness.builtin.context`, the whole conversation is resent every step until it overflows the model's window. Set a strategy and the window to keep a long run going:

| Field | What it does |
|---|---|
| `strategy` | `none` (default); `fresh-rounds` and `evidence-state` (see below); `clear-tool-results` replaces old tool results with a stub such as `[tool result cleared to save context: 5120 characters from bash]`, keeping the message and its tool-call id; `compact` clears first, and when that is not enough has the agent's own model summarize the earlier turns (the task message, a summary, then the last `keep_turns` turns verbatim) |
| `window_tokens` | the model's context window (at least 4,000); required for any strategy but `none` |
| `trigger`, `target` | start at this fraction of the window (default 0.8) and go down to this one (default 0.5). Working in batches rewrites the provider's prompt cache once per batch, not on every turn |
| `keep_recent`, `keep_turns`, `summary_max_tokens` | tool results kept intact (4), assistant turns kept verbatim after a summary (6), and the summary's output limit (4096) |
| `spill_outputs` | a `bash` output over 16,000 characters is written to `.evalsi-out/NNNN.txt` in the sandbox and the reply carries its head, tail and path, instead of being clipped in the middle. The files are Evals.si's own, so they stay out of the agent's diff |

The prompt size is the model's last reported `input_tokens`, scaled by characters to the conversation as it now is, so it follows the real tokenizer. Every clear and compaction is a step named `context:clear-tool-results` or `context:compact`, with `evalsi.context.tokens_before`, `tokens_after` and what was removed; a compaction's model call is billed to the run (and to `max_usd`) but is not one of `max_steps`. The run's `agent.context` metadata totals them: `clears`, `compactions`, `results_cleared`, `chars_removed`, `tokens_removed`, `turns_summarized` and `summary_failures`. A summary that comes back cut off or empty is not used: the run keeps the cleared conversation instead.

A recording replays only under the strategy and window it was recorded with (the cassette header names them), since another strategy would send the model different conversations; the summary call is recorded in sequence like any other model call.

**`fresh-rounds`** (the Ralph pattern) trades one long conversation for several short ones. Each round is a new agent with a clean context: it starts from the task and the notes the previous round kept in its memory file `progress.md` (the `memory` tool is added for it), plus whatever the earlier rounds left in the sandbox. A round ends when the agent replies without calling tools or reaches `round_steps` model calls (default 30). The run ends when a reply carries `TASK_COMPLETE`, or after `rounds` rounds (default 5) with stop reason `max_steps` and an error saying so; `max_steps` and the other budgets bound the whole run. It needs no `window_tokens`; each round's context is as long as `round_steps` makes it. The prompt tells the agent about the notes and the marker, every new round is a `context:round` step, and `agent.context.rounds` counts the fresh starts. `session_search` still sees all rounds.

**`evidence-state`**, after the LongHarness paper, is for questions over a corpus. The agent keeps two lists in an `evidence` tool, outside the conversation: **evidence** (a statement, a source and a quote copied verbatim from something it read; an entry whose quote is not in any tool result or the task is refused, and the agent's own words never count) and **conclusions** (a claim, the evidence ids that support it and the gaps still open; a claim with an open gap is not supported). A correction supersedes or retracts an entry instead of appending next to it, and a claim follows a superseded entry or reopens when its last support is retracted. The state is shown to the model on every call, as a message sent with that call only, so it is never in the conversation. When the window fills, old tool results are cleared first, and if that is not enough the conversation is cut to the task and the last `keep_turns` turns: the state carries over, and there is no summary to trust. `agent.evidence` counts the entries and conclusions, and each cut is a `context:evidence-state` step. The paper's ablations found structured state beat free-text notes by 6.8 to 7.8 points on one benchmark with one run per setting, so compare it with `compact` and `fresh-rounds` on your tasks before relying on it.

### MCP servers

`tools.mcp` lists MCP servers (a streamable HTTP `url`, or a `command` the worker starts that speaks MCP over stdio). Besides `name`, `headers_env` and `tools` (which tools to expose), a server takes:

| Field | What it does |
|---|---|
| `timeout_s`, `tool_timeout_s: {tool: seconds}` | how long a request may take (default 120 s), and a limit per tool. A tool that runs out of time returns an error the agent sees, and the connection stays usable |
| `resources`, `prompts` | also offer the server's resources and prompt templates, as `<name>_resources` (list, read) and `<name>_prompts` (list, get) tools |
| `elicitation` | let the server ask the user a question in the middle of a call (MCP elicitation), answered by the [user simulator](#harness-options), which the spec must set. The reply is fitted to the question's schema: an enum takes the option it names, a boolean yes or no, a number the number next to the property's name or the next one in order, a string the whole reply; a required value the reply cannot supply is a decline. The answers do not use the conversation's simulated-user turns (a replay serves the tool's recorded result without asking, so a turn taken at record time would drift the two apart), at most ten questions are answered a run, and they are on the tool's step as `evalsi.tool.elicitations` |
| `oauth: {token_url, client_id_env, client_secret_env, scope}` | reach a remote server with an OAuth 2 client-credentials token (the machine flow; a run has no one to approve a browser redirect). The token is fetched once, reused until it nears expiry, and refreshed on a 401. The two variables are read by the worker and, on a server, must be granted to the run's project, for the token endpoint and for the server's URL, since the token goes to the server |

Tool results are shaped for a context: images and audio become a note (`[image/png content, 2001 bytes, not shown]`), not base64; a resource link becomes its uri; a tool's `outputSchema` is added to its description, and structured output is returned as JSON when there is no text.

With **`tool_search`** in `tools.builtin`, the MCP tools start unloaded. The model sees the sandbox tools and one `tool_search` tool whose description counts and names what exists; a search by what it needs to do loads the best matches (up to five), which are in the tool list from the next step. This keeps hundreds of schemas out of every prompt. Loading appends to the tool list, so the earlier part of the prompt keeps its cache. Calling a tool that is not loaded returns an error that says to search.

### Code mode

With `code_mode` in `tools.builtin`, the model is offered one tool, `run_code`, whose description lists the task's other tools as functions. It writes a short Python script that calls them, loops over what they return and `print`s only what it needs, and that output is the tool result. One script can read twenty files and print the three lines that matter, which is one round trip and a few tokens, not twenty of each. Tools return their output as a string (starting `ERROR: ` when the call failed), take their arguments by keyword or in schema order, and the calls the script makes are capped at 100.

The script is not run by Python. The harness parses it and interprets a safe subset itself: variables, `if`, `for`, `while`, `try`, comprehensions, f-strings, `def` and `lambda`, the usual string, list, dict and set methods, and `json`. There is no `open`, `eval`, `exec`, `getattr`, `type`, `str.format`, class, `with`, import other than `json`, or attribute starting with an underscore, so a script reaches nothing but its tools, and model-written code never runs on the worker. There are no regular expressions either: a pattern written by a model can backtrack for ever, and the interpreter shares a thread with every other trial (the `grep` tool does the searching). Operations (200,000), tool calls (100), output, `range`, exponents, recursion depth, the size of every string, list, dict and set (1,000,000 items) and of every integer (8,192 bits) are capped; sizes are checked before the result is built wherever the operands tell it (`*`, `+`, `**`, `extend`, `update`, `ljust`, `replace`, format widths) and after a native call otherwise. A cap ends the script with a message the model sees, and a script's own `try` cannot swallow it.

Every call a script makes goes through the same path as a direct one: it is a tool step under the `run_code` step, hooks see it (a `Deny` shows up as an `ERROR: ` string in the script), and it is recorded and replayed with the run, sandbox-changing calls included. The other tools are hidden from the model, which is told to call them from `run_code`; MCP tools that `tool_search` leaves unloaded are not available to scripts.

### Sub-agents

Naming `delegate` in `tools.builtin` gives the agent a tool that starts a sub-agent on a task. The sub-agent has a clean context, the parent's tools (without `delegate`, so there is one level) and the same model, and runs for up to `delegate_steps` model calls (default 20; the agent may ask for fewer). Everything it reads and tries stays in its own conversation, and the parent gets its final report as the tool result, so a token-heavy subtask costs the parent a paragraph, not a context. This is the `offload` pattern, and it combines with the context strategies; a sub-agent has one job and one reply, so it runs with `fresh-rounds` turned off.

Its spend is the parent's: tokens, spend and wall-clock time come out of what the parent has left (a `delegate` with nothing left is an error the agent sees), and `agent.subagents` and `agent.subagent_steps` total them. Its steps are on the trajectory under a `subagent` step that hangs from the `delegate` call, so a multi-agent run reads as a tree. Hooks apply to the sub-agent's tool calls too. Its model and tool calls are recorded in order with the parent's, so a replay reproduces them; `delegate` calls in one turn therefore run one after another.

### Profiles: a harness a score can name

`harness.builtin.profile` picks a versioned profile of the built-in agent, so a model's score names the harness it was measured on and means the same thing across releases:

| profile | prompt and tools |
|---|---|
| `minimal` | a prompt of a few hundred characters and four tools: `bash`, `read_file`, `write_file`, `edit_file`. The escalation request tool is **not** offered, so a safety evaluator that needs to see the agent ask for access cannot use it. A baseline for how much the rest of the harness adds. |
| `standard` | the default prompt and the whole H2 toolset (`edit_file`, `glob`, `grep`, `apply_patch`, `memory`, `session_search`). With `context.window_tokens` set and no strategy, it compacts. |
| `code-mode` | `run_code` over `edit_file`, `glob` and `grep` ([Code mode](#code-mode)). |

`minimal` is the latest `minimal` version; `minimal@1` pins one. A shipped version never changes: a change to a profile is a new version. A profile owns the toolset, so setting it together with `tools.builtin` is an error. Budgets, hooks, `instructions`, MCP servers and an explicit context strategy are still the spec's. The run records the resolved `name@version` (`metadata.agent.profile`), and a recording replays only under the same profile.

### Comparing harnesses

Run the same spec once per profile (or per model), each with `--output`, then:

```
evalsi matrix standard.json minimal.json code-mode.json --metric task-success
```

Each file is a cell with its harness, model, score and interval, and beside the score what it cost per task: model calls, tokens and dollars, and the score per thousand tokens. Every cell after the first is compared with the first, paired by record and trial, so the difference carries its own interval and is starred when it excludes zero. `--tuned-on NAME` (a results file name, a run name or a dataset hash) splits the cells into `tuned` and `held-out`, so a profile tuned on one benchmark is also read on others. Cost columns show `-` when the results lack the figure.

### The agent's own setup

An agent under test is often configured by files in its working directory: `AGENTS.md` or `CLAUDE.md`, a skills directory, an MCP config such as `.mcp.json`. Put them in `environment.files` (relative paths are the workdir; nested and dot paths work), and they are part of the task: written into the sandbox before the agent starts, visible to the checker, and part of the environment's identity, so two setups never share a snapshot and a run that changes only `AGENTS.md` is a different environment. Compare setups by running the same spec with different `files` and reading the two runs side by side.

### Sessions: resume and fork

A recording is an append-only session log, one file per task and trial, and every model turn and tool result is on disk as soon as it happens. That makes two things possible:

- **Resume.** A trial killed by a worker restart or a dying provider leaves its log up to the last step. Run again with `recording: {mode: resume}`: the harness replays the log (the model is not called, and `bash`, `write_file`, `edit_file` and `apply_patch` run again, which is how the sandbox is restored) and carries on live, appending to the same log. A call that was in flight when the trial died, with no recorded result, simply runs. A half-written last line is dropped. The resumed trial grades the same as an uninterrupted one; the cost and the wall-clock budget restart from what the replay re-counts.
- **Fork.** `recording: {mode: fork, branch_at_step: N, fork_as: "try-b"}` replays the first N turns of a log and goes live into a new log, `<task>.<fork_as>.jsonl` (default `<task>.fork-1.jsonl`, then `-2`), which holds the prefix and the new continuation and names its parent in its header. The source log is untouched, and the fork replays on its own. Forking at different turns, or with a different model, gives counterfactual runs from a shared start; a fork must keep the hooks, tools and context strategy of its source, since a changed hook or tool changes the replayed prefix. `evalsi_harness.cassette.session_tree(dir)` lists the logs and how they descend from each other.

A log is valid only for the context strategy, hooks and optional tools (`tools.builtin`, `delegate_steps`) it was recorded with; resuming, forking or replaying under others is refused.

### Hooks

`harness.builtin.hooks` lists Python classes (`module:Class`, with an optional `config`) that run at the agent's lifecycle points, so changing the prompt, gating a tool or reacting to a compaction does not mean forking the loop. A class is built as `Class(config)` and has any of these methods, sync or async:

| Method | Returns |
|---|---|
| `before_model(ctx, request)` | `None`, or a `ModelRequest(messages, system)` to send for this call only; the conversation itself is unchanged |
| `before_tool(ctx, call)` | `None` to allow, `Deny(reason)` (the agent sees the reason as the tool's error and the tool does not run), or `Rewrite(arguments)` |
| `after_tool(ctx, call, result)` | `None`, or a `ToolResult` to take its place |
| `on_compaction(ctx, step)` | nothing; sees each context-management step |
| `on_stop(ctx, final)` | `None` to let the agent finish, or `Continue(message)` to send it one more user message (at most three times a run), for example to ask it to run the tests |

Every decision is a step named `hook:<module:Class>` with `evalsi.hook.event`, `decision`, `reason` and, for a rewrite, `original_arguments`, so evaluators see what a hook changed. Hooks are called in the order listed, and a `Deny` stops the hooks after it. For parallel tool calls the hooks see the calls one at a time, in order, before any runs. An exception in a hook ends the run with the error: a gate that silently stopped gating would be worse. On a server the class must be listed under `agents.trusted_python`, like other Python the worker runs, and a recording replays only with the same hooks in the same order (and the same tools and context strategy).

### Model options

The built-in agent calls `spec.target`. Besides `connector`, `model`, `base_url`, `api_key_env`, `max_tokens`, `temperature` and `effort`, the target takes these options (they apply to the built-in agent only):

| Field | What it does |
|---|---|
| `thinking: {mode, budget_tokens}` | Anthropic extended thinking: `adaptive`, `enabled` (with a `budget_tokens` below `max_tokens`) or `disabled`. The thinking blocks are sent back with the tool results, as the API requires, and a temperature is not sent while the model thinks. The readable part of the thinking is on the `llm` step as `evalsi.thinking.text` (redacted like other attributes); signatures and encrypted content are not |
| `api: responses` | for `openai-compatible` targets: use the Responses API instead of chat completions. `effort` becomes `reasoning.effort`. The call is stateless (`store: false`) and the encrypted reasoning items travel with the conversation, so a reasoning model keeps its plan across tool calls. Reasoning tokens are counted on the step |
| `stream: true` | stream the response, so long outputs and long reasoning do not hit the HTTP timeout |
| `max_concurrency: N` | at most N concurrent model calls to this endpoint and key from one worker process |
| `fallbacks: [target, ...]`, `fallback_after: N` | models to switch to, in order, when the current one fails N times in a row (default 2; each failure is already retried with backoff). The run stays on the new model, the step records `evalsi.model.switched_from`, and reasoning blocks from another model are dropped from the conversation, since a provider only accepts its own. A fallback needs its own credential grant |

Calls honour `retry-after` and retry HTTP 429, 5xx and 529 with jitter. Anthropic prompts are cached (breakpoints on the system prompt, the tools and the end of the conversation), and `max_usd` prices cached tokens: at a tenth of the input price for reads and 1.25 times for writes on Anthropic, at full price elsewhere, so a spend budget is an upper bound. `budget.tokens` counts the whole prompt, cached tokens included.

A turn cut off by `max_tokens` is tried once more with twice the limit (up to 128k); if it is cut off again, or the provider refuses the higher limit, the run ends with stop reason `budget`, not `completed`, and is still checked. The retry is on by default and spends the extra tokens. A refusal ends the run as `refusal`, and `pause_turn` is resumed.

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

The importer reads SWE-bench 5 instances, which carry their image, eval script, log parser and evaluation type. Put the dataset under the server's `datasets_dir`, either as a `.jsonl` file or as `<dir>/test.parquet`, which `swebench`'s loader reads. On Kubernetes, `datasets_dir` must be `storage.s3`: workers run in other pods and cannot read a file in the server's. Then run [`examples/agents/swebench-verified.yaml`](../../examples/agents/swebench-verified.yaml).

- The agent works in `/testbed`.
- The checker resets the test files, applies the test patch, runs the tests, and applies the official FAIL_TO_PASS and PASS_TO_PASS rule.
- `oracle=true` adds the gold patch as `.evalsi-gold.patch`, so an agent that applies it validates the setup.

The scoring code and the image are the same on every rung. That is how the Phase 3 exit criterion holds: identical scoring in Firecracker and in bubblewrap.

### τ-bench (tau2)

`TauBenchHarness` is the built-in harness with a tau2 domain around it:

- **Tools and prompt:** the domain's tools run against a live tau2 environment, and the agent gets tau2's agent instruction and the domain policy.
- **The user:** the Evals.si user simulator plays the task's persona and instructions, through the run's judge. tau2's own LiteLLM user simulator is not used, so provider keys stay with the judge config. Expect scores close to, but not identical with, tau2's leaderboard.
- **Grading:** tau2's own evaluator on the conversation.
- **Dual control (telecom):** the user has tools of their own: their phone. The simulated user then follows tau2's user prompt and guidelines through the run's judge. Each turn they either message the agent or call one of their tools against the same tau2 environment. Their calls are part of the conversation tau2 grades, so assertions on the device's state hold.
- **Not supported:** tasks graded by natural-language assertions, which the importer skips.

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

Supported categories: `simple_python`, `simple_java`, `simple_javascript`, `multiple`, `parallel`, `parallel_multiple`, `irrelevance`, their `live_` counterparts, and `live_relevance`.

The multi-turn categories (`multi_turn_base`, `multi_turn_miss_func`, `multi_turn_miss_param`, `multi_turn_long_context`) also run, with BFCL's own loop:

- In each user turn, the model calls tools until it answers without one, for at most 20 steps.
- The calls run against BFCL's stateful API classes: file system, trading, travel and so on.
- Functions that `miss_func` entries hold out are offered at their turn.
- BFCL's multi-turn checker grades every turn's end state and results.
- A call runs only when it names a function offered at that step, with its JSON arguments as Python literals, because BFCL executes calls as code.

Memory, web-search and format-sensitivity categories are not supported.

## From production to regression tests

- **Promote failures into a dataset.** `evalsi promote <run> --dataset <name> --when '<CEL>'` appends the records where the condition holds (over `scores`, `errored`, `trial`, `record`) to `promoted/<project>/<name>.jsonl`. For example: `scores["task-success"] < 1`. Add `--include-outputs` to keep outputs and trajectories for re-scoring. A promoted record's ID is `<run>/<record>` (the original ID and trial are in `metadata.promoted_from`), so a dataset gathered from many runs keeps unique IDs, and promoting the same run again adds nothing. It needs `datasets.write`, which `editor` holds.
- **Turn traces into tasks.** A dataset can be built from production traces (`dataset: {traces: {service, policy, filter, lookback}}`) or from another run's outputs (`dataset: {run: {run_id, trial, all_trials}}`).
- **Shadow replay.** `evalsi shadow -f candidate.yaml` replays recorded inputs against a candidate. It scores the recording with the same evaluators as a baseline run, and compares the two.

## Sandbox rungs

The ladder (`sandbox.ladder`) defaults to `firecracker`, `bwrap`, `landlock`. The strongest rung that works is used, and `evalsid sandbox probe` shows which rungs work on a host. A task that sets `min_isolation` is refused, never downgraded, when no rung meets it.

**Resource limits.** On the bubblewrap and Landlock rungs, each command gets rlimits (memory, file size, and processes on bubblewrap). With a delegated cgroup v2 directory, it also gets its own cgroup with `memory.max`, `pids.max` and optionally `cpu.max`. These cover the whole process tree, which is the only process cap on Landlock. Anything the command leaves running is killed when it ends.

```yaml
sandbox:
  cgroup: auto          # default: evalsid's own cgroup when delegated (systemd Delegate=yes); "off"; or a path
  cgroup_cpus: 2        # cpu.max per command; default: no limit
```

The isolation report says which limits applied, and an out-of-memory kill appears in the result's denials.

**Signed images.** To use only images signed with your cosign key:

```yaml
sandbox:
  image_signatures:
    keys: [/etc/evalsi/cosign.pub]   # files or PEM text; any one key suffices
    # allow_local: true              # also allow oci-layout:, docker-archive: and dir: images
```

An image is used only when the registry holds a signature by one of the keys, on the image or on the index its tag points to (`cosign sign --key cosign.key <image>`). Signatures must be in cosign's `.sig` tag format, which is cosign 2's default. Verification is offline, with no transparency log. For an air-gapped registry, copy signatures with `cosign copy`. The pod rung's images are pulled by the kubelet, so enforce signatures there with your cluster's admission policy.

To enable Firecracker:

```yaml
sandbox:
  firecracker:
    kernel: /var/lib/evalsi/vmlinux       # uncompressed, with virtio-blk, vsock and ext4
    guest: /usr/local/bin/evalsi-guest    # default: next to evalsid (`make build` builds both)
    warm_pool: 2                          # booted VMs kept ready per image
    vcpus: 2
    memory_mb: 2048
    default_image: python:3.13-slim       # required: booted when a spec names no image, and at start to check the rung
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

References in the `evalsi_*` packages, which include every benchmark harness and parser above, are always allowed. The secrets an agent run names (`headers_env`, `api_key_env`, `env_from`) must be granted to its project under `credentials.grants`; see the [identity guide](identity.md#8-credentials-which-worker-secrets-a-project-may-use). Authorization rules see agent runs as `resource.agent` (`kind`, `harness`, `image`, `network`, `min_isolation`), and `resource.runs_code` is true for them. See the [identity guide](identity.md#4-global-rules).

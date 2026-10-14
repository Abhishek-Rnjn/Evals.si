# Harness roadmap

This plan is for making `evalsi-harness` (`python/evalsi-harness/`) a strong harness for frontier agents. It covers the built-in reference agent, the bring-your-own connectors, environments and checkers, and the RL environment specs. It fits into the [milestones](LEFTOVERS.md#milestones): the first two phases land in **M2 agent harness online** (Nov to Dec 2026), and the rest is due by **GA with agent harness** (26 March 2027).

Written 2026-10-14, at v0.2.0. Every gap below was found by reading the code at that release, and the file it lives in is named.

## Where it stands

The foundations are sound. There is a small, dependency-light tool loop with budgets. The harness protocol (`HarnessService`) lets anyone bring their own harness. Five BYO agent connectors exist: A2A, MCP, Responses, HTTP and CLI. The sandbox ladder runs underneath. There are record and replay with branching, fault injection, a simulated user, OTel spans, Harbor and Terminal-Bench importers, and OpenEnv and verifiers environments.

The weak spots are in what frontier agents need today:

| Area | Gap at v0.2.0 | Where |
|---|---|---|
| Reasoning models | Anthropic `thinking` blocks are dropped from the turn and not sent back with tool results. That breaks interleaved thinking across tool calls. On OpenAI-compatible models the built-in agent uses chat completions only, so reasoning items and their token counts are lost. | `models.py` (`AnthropicChat`, `to_anthropic`, `OpenAIChat`) |
| Cost and speed | No prompt caching. Cache-read and cache-write tokens are not counted. Cost is input plus output only. | `models.py`, `loop.py` (`LoopConfig`, `cost`) |
| Long tasks | No context management. The whole conversation is resent every step until it overflows the window. The only guard is a 16k-character clip per tool result. | `loop.py`, `tools.py` (`OUTPUT_LIMIT`) |
| Stop reasons | `max_tokens`, `pause_turn` and `refusal` are treated as a normal answer. A tool call truncated by `max_tokens` ends the run as "completed". | `loop.py` (`_loop`) |
| Rate limits | Retries use fixed exponential backoff and ignore `retry-after`. There is no streaming, so very long outputs hit the HTTP timeout. | `models.py` (`_post`) |
| Tools | `bash`, `read_file` and `write_file` only. There is no exact-match edit, search, patch, background process or web tool, and every MCP tool is loaded up front. | `tools.py`, `mcp.py` |
| CLI agents | Only DeepSeek Harness (`dsh-json`) reports a trajectory. Other coding-agent CLIs return only their final stdout. | `formats.py` (`FORMATS`) |
| Environments | One sandbox per task. There are no multi-service tasks (22 of the 241 Terminal-Bench 1 tasks do not import), no browser or desktop environments, and WebArena/BrowserGym and GAIA, listed in DESIGN §8, are not built. | `environment.py`, `benchmarks/` |
| Grading integrity | The checker runs in the sandbox the agent just controlled. Its files are rewritten, but the agent can still change the interpreter, `PATH`, `conftest.py` or test dependencies. | `checker.py` (`run_checker`) |
| RL | There are no token IDs or log-probs per turn, rollouts are not asynchronous or partial, setup is not shared across verifiers rollouts, and there are no gymnasium or NeMo Gym specs. | `gym.py`, `envspecs/` |

## Phases

Each phase ends with exit criteria checked in CI, in the same style as the rest of the roadmap.

### H1: Frontier model support (M2, Nov 2026)

The goal is for the built-in agent to run today's frontier models correctly, cheaply and reproducibly.

- **Thinking preserved across tool turns.** Keep Anthropic `thinking` and `redacted_thinking` blocks, with their signatures, in `ModelTurn`, and send them back unchanged. Pass the thinking configuration and effort through `TargetConfig`. Record thinking as its own content on the `llm` step so evaluators can see it, with redaction following [decision 0019](decisions/0019-trace-redaction.md).
- **The Responses API for the built-in agent.** Add an `openai-responses` model client next to chat completions. Carry encrypted reasoning items between turns, and count reasoning tokens.
- **Prompt caching.** Put cache breakpoints on the system prompt, the tools and the conversation tail for Anthropic. Record `cache_read` and `cache_write` tokens in `Usage`, and add cache prices to `LoopConfig` so `max_usd` is accurate.
- **Stop reasons.** Retry a turn cut off by `max_tokens` with a higher limit, or end it as `budget`. Continue after `pause_turn`. Record `refusal` as its own stop reason.
- **Retries and streaming.** Honour `retry-after`, add jitter, and set a per-run concurrency limit for each provider. Stream responses, so long outputs and long reasoning do not hit the HTTP timeout.
- **Cassette format v2.** Record thinking, reasoning items and cache usage. Old cassettes still replay.

Exit criteria: a recorded multi-step `fix-calc` run with thinking turned on, on Anthropic and on an OpenAI-compatible reasoning model, replays exactly. Cache hits appear in the run's usage. A forced `max_tokens` cut is not reported as a pass.

### H2: Long-horizon agents (M2, Dec 2026)

The goal is to let agents work for hours and through hundreds of steps without overflowing their context.

- **Context management.** Count tokens before each call. Above a threshold, first clear old tool results and leave a stub that says what was there. Then compact earlier turns into a summary, written by the agent's own model or by a configured one. Each compaction becomes a step, so evaluators can score what was lost. The strategy is configurable: `none`, `clear-tool-results` or `compact`.
- **Large outputs.** Instead of clipping in the middle, write the full output to a file in the sandbox and return its head, tail and path.
- **A stronger default toolset.** Add `edit_file` (exact-match replace that fails on ambiguity), `glob`, `grep`, `apply_patch`, and background processes with `bash` sessions that persist across calls. Each set can be turned on per task, so comparisons between models stay fair.
- **Tool search.** When a task exposes many MCP tools, load them on demand through a search tool instead of putting every schema in the prompt.
- **MCP up to date.** Add OAuth for remote servers, structured tool output, resources and prompts, elicitation answered by the simulated user, and per-tool timeouts.
- **Memory and notes.** Add an optional `memory` directory tool that survives compaction, for tasks that measure long-horizon planning.

Exit criteria: a synthetic 300-step task finishes inside a 200k-token window with compaction. The number of steps and tokens lost to clearing is recorded on the run.

### H3: Agents under test (Jan 2027)

The goal is to evaluate the agents people actually ship, with full trajectories.

- **CLI trajectory readers.** Add `output_format` readers for the structured event streams of popular coding-agent CLIs: Claude Code's `stream-json`, Codex's JSON exec events, and Gemini CLI's JSON output. Each reader gets a recorded fixture and a version pin, like `dsh-json`.
- **Agent configuration as environment.** Mount `AGENTS.md` and `CLAUDE.md` files, skills directories and MCP configurations into the sandbox, so the agent's setup is part of the task and can be compared across runs.
- **Multi-agent trajectories.** Record sub-agent and handoff spans with parent links, from the built-in agent's optional sub-agent tool, from A2A tasks, and from the CLI readers. This is what the `agent` pack's handoff-correctness evaluator needs.
- **A2A streaming.** Use `message/stream` and push notifications instead of polling, and show the A2A task states in the trajectory.
- **The built-in agent as a baseline.** Publish fixed prompts and a fixed toolset version, so a model score from the reference agent means the same thing across releases.

Exit criteria: CI runs each CLI reader against a recorded fixture. The kind e2e job runs one CLI agent end to end when a model key is set.

### H4: Environments (Feb 2027)

The goal is to cover the main agent task types: terminal, repository, web, desktop and conversational.

- **Multi-service environments.** Add a Compose-like `services` list (a database, an API, a browser) on a shared private network inside the sandbox rung. This closes the 22 Terminal-Bench 1 tasks that do not import, and Harbor tasks with services.
- **Browser and computer use.** Add a Playwright browser environment with a `browser` tool set (accessibility tree, screenshots, actions), and a desktop environment for computer-use tools with screenshot steps recorded as images. Then build the WebArena/BrowserGym and GAIA importers.
- **Stateful tool environments.** Add a database-backed tool environment for τ-bench-style domains, so the checker reads the final database state.
- **Checker isolation.** Run the checker from a clean copy of the setup snapshot, with only the agent's declared outputs or its diff applied. A checker can still opt in to running in the agent's sandbox. Hash protected files, and record any change to them as a policy event.

Exit criteria: a multi-service Terminal-Bench task and a WebArena-style fixture pass on the bubblewrap and pod rungs. A fixture agent that edits the tests is caught and fails.

### H5: Rigor and safety (Feb to Mar 2027)

The goal is grades that are trustworthy.

- **Prompt-injection faults.** Add a fault type that plants instructions in tool results, files or web pages, and an evaluator that scores whether the agent followed them.
- **Reward-hacking signals.** Detect test tampering, hard-coded expected outputs and checker probing, and mark the trial so pass@k can exclude it.
- **Contamination canaries.** Put canary strings in imported tasks, and add a probe that checks whether a model reproduces them.
- **Determinism report.** For each run, record seeds, model snapshots, tool versions and image digests. A replay that differs points to the step where it diverged.
- **Variance-aware defaults.** Recommend a number of trials from the variance seen per task, and report pass^k next to pass@k by default.

Exit criteria: a red-team fixture suite (injection, tampering and escalation attempts) runs in CI with expected scores.

### H6: RL at scale (GA, Mar 2027)

The goal is for the same tasks to train models as well as evaluate them.

- **Token-in, token-out.** For vLLM and SGLang targets, keep the token IDs and log-probs of every turn, so trainers get exact multi-turn rollouts without re-tokenizing.
- **Asynchronous and partial rollouts.** Run rollouts concurrently and let a trainer cancel or resume them, with shared environment setup across rollouts of the same task (the open verifiers row in LEFTOVERS).
- **More environment specs.** Add gymnasium and NeMo Gym next to OpenEnv and verifiers.
- **Step-level rewards.** Expose per-step evaluator scores as process rewards, next to the checker's outcome reward.

Exit criteria: a verifiers or OpenEnv trainer runs one real training step on a GPU runner (this depends on the infrastructure row in LEFTOVERS). Rollout throughput is measured against the §12 target.

## Not planned

- **A general agent framework.** The built-in agent stays a small, fixed reference for comparing models. Heavier behaviour belongs in the BYO path.
- **Provider SDKs as dependencies.** The model clients keep speaking HTTP directly, so the harness stays light.
- **Hosting agents.** Evals.si runs agents in sandboxes for evaluation. It does not serve them in production.

## Tracking

As each phase is built, what is left of it (unverified, deferred or needing infrastructure) moves to [LEFTOVERS.md](LEFTOVERS.md), the same way the other phases are recorded.

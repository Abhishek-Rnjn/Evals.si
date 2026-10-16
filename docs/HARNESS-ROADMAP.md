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
| CLI agents | DeepSeek Harness, Claude Code, Codex, Gemini CLI and pi report a trajectory (`dsh-json`, `claude-stream-json`, `codex-json`, `gemini-json`, `pi-json`). Other CLIs return only their final stdout. | `formats.py` (`FORMATS`), `readers.py` |
| Environments | One sandbox per task. There are no multi-service tasks (22 of the 241 Terminal-Bench 1 tasks do not import), no browser or desktop environments, and WebArena/BrowserGym and GAIA, listed in DESIGN §8, are not built. | `environment.py`, `benchmarks/` |
| Grading integrity | The checker runs in the sandbox the agent just controlled. Its files are rewritten, but the agent can still change the interpreter, `PATH`, `conftest.py` or test dependencies. | `checker.py` (`run_checker`) |
| Connectors | No ACP (Agent Client Protocol) connector, though DeepSeek Harness and Deep Agents both ship ACP servers and the PRD's demo table lists ACP targets. | `connectors.py` (`connector_for`) |
| Sessions | The trajectory is a flat list. A cassette can branch at step N, but an interrupted trial cannot resume, and there is no session tree to fork and compare. | `cassette.py`, `loop.py` |
| Extensibility | The loop has no hooks. Changing the prompt, gating a tool or changing how context is handled means forking `loop.py`. | `loop.py` (`AgentLoop`) |
| RL | There are no token IDs or log-probs per turn, rollouts are not asynchronous or partial, setup is not shared across verifiers rollouts, and there are no gymnasium or NeMo Gym specs. | `gym.py`, `envspecs/` |

## What the leading harnesses do

Several open harnesses have converged on patterns the built-in agent lacks. These are the major ones; the phases below adopt them. Details of DeepSeek Harness come partly from third-party write-ups of its developer preview and should be checked against the pinned release (`0.2.1-alpha.2` in `examples/demo/dsh`) before building.

| Harness | Pattern | What we take | Phase |
|---|---|---|---|
| **pi** (`pi-mono`: `pi-ai`, `pi-agent-core`, `pi-coding-agent`) | A deliberately minimal core: a system prompt and tool definitions under about 1,000 tokens, and four tools (`read`, `write`, `edit`, `bash`). Everything else (sub-agents, plan mode, permission gates) is an extension. | A **minimal harness profile**, as a baseline for "how much does the harness add?" | H3 |
| pi | A unified provider layer that can hand a conversation from one provider to another mid-session, including thinking. | **Switching models within a run**: fallback, and a cheaper model for sub-agents or compaction | H1 |
| pi | Session history as a tree that you can branch from anywhere; print/JSON and RPC modes for scripting. | A **session tree** with resume and fork; a reader for pi's JSON event mode | H2, H3 |
| pi | Steering and follow-up messages queued while the agent is working. | **Mid-run user messages** from the simulated user | H4 |
| **DeepSeek Harness** (`dsh`) | Everything is a plugin: model adapter, tool registry, session log, even the agent loop. | **Hooks** at the loop's lifecycle points | H2 |
| dsh | An append-only session log with resume, fork, search and replay. | The same **session tree**, and retrieval over it | H2 |
| dsh | Context handled three ways: compaction in place, offloading to sub-agents, and a Ralph loop (a fresh child agent per round, with a round cap). | **Context strategies** beyond compaction, each one something an eval can compare | H2 |
| dsh | A main agent that delegates to child agents (agent teams). | **Sub-agents** in the built-in agent, with their trajectories linked | H3 |
| dsh, Deep Agents | ACP servers (`dsh --profile acp`, `deepagents-acp`). | An **ACP connector** | H3 |
| Claude Code, Codex and others | Programmatic tool calling: the model writes a script that calls tools in the sandbox, instead of one round trip per call. | A **code-mode tool profile** | H2 |
| Terminal-Bench leaderboards | Scores are reported per harness and model pair, and the same model moves by several points between harnesses. | **Harness configuration as an experiment variable** | H3 |
| **LongHarness** ([arXiv 2610.13075](https://arxiv.org/abs/2610.13075)) | Corpus-level question answering keeps two persistent states outside the chat history: an *EvidenceBank* of source-linked statements (active, superseded or retracted, each citing a passage the agent read) and a *Conclusion State* of claims with open gaps that steer the next search. | An **`evidence-state` context strategy**; a **citation-grounding check**; a **stage audit** that splits a failure into retrieval, evidence use and answer conversion | H2, H5 |
| **Evo2** ([arXiv 2610.13025](https://arxiv.org/abs/2610.13025)) | Harness search: a model proposes edits to a harness and reflects on traces, over a tree of variants with crossover between branches. Fitness is the task score with the executor fixed. | **Cost beside every score**; **held-out and transfer runs** for a harness profile; a **mechanism-ran check**. The search itself and training an evolver are not planned | H3, H5 |

Both papers are preprints from October 2026. Their results come from one setup each and mostly single runs or small subsets, so they motivate the items above and are not evidence that the gains carry to our tasks.

## Phases

Each phase ends with exit criteria checked in CI, in the same style as the rest of the roadmap.

### H1: Frontier model support (M2, Nov 2026)

The goal is for the built-in agent to run today's frontier models correctly, cheaply and reproducibly.

- **Thinking preserved across tool turns.** Keep Anthropic `thinking` and `redacted_thinking` blocks, with their signatures, in `ModelTurn`, and send them back unchanged. Pass the thinking configuration and effort through `TargetConfig`. Record thinking as its own content on the `llm` step so evaluators can see it, with redaction following [decision 0019](decisions/0019-trace-redaction.md).
- **The Responses API for the built-in agent.** Add an `openai-responses` model client next to chat completions. Carry encrypted reasoning items between turns, and count reasoning tokens.
- **Prompt caching.** Put cache breakpoints on the system prompt, the tools and the conversation tail for Anthropic. Record `cache_read` and `cache_write` tokens in `Usage`, and add cache prices to `LoopConfig` so `max_usd` is accurate.
- **Stop reasons.** Retry a turn cut off by `max_tokens` with a higher limit, or end it as `budget`. Continue after `pause_turn`. Record `refusal` as its own stop reason.
- **Retries and streaming.** Honour `retry-after`, add jitter, and set a per-run concurrency limit for each provider. Stream responses, so long outputs and long reasoning do not hit the HTTP timeout.
- **Switching models within a run.** Normalize a conversation so it can move between providers (thinking converted or dropped by a stated rule). Use it for a fallback model on repeated failures and for a separate model for compaction or sub-agents. Each switch is recorded on the step.
- **Cassette format v2.** Record thinking, reasoning items and cache usage. Old cassettes still replay.

Exit criteria: a recorded multi-step `fix-calc` run with thinking turned on, on Anthropic and on an OpenAI-compatible reasoning model, replays exactly. Cache hits appear in the run's usage. A forced `max_tokens` cut is not reported as a pass.

### H2: Long-horizon agents (M2, Dec 2026)

The goal is to let agents work for hours and through hundreds of steps without overflowing their context.

- **Context management.** Count tokens before each call. Above a threshold, first clear old tool results and leave a stub that says what was there. Then compact earlier turns into a summary, written by the agent's own model or by a configured one. Each compaction becomes a step, so evaluators can score what was lost. The strategy is configurable: `none`, `clear-tool-results` or `compact`.
- **Large outputs.** Instead of clipping in the middle, write the full output to a file in the sandbox and return its head, tail and path.
- **A stronger default toolset.** Add `edit_file` (exact-match replace that fails on ambiguity), `glob`, `grep`, `apply_patch`, and background processes with `bash` sessions that persist across calls. Each set can be turned on per task, so comparisons between models stay fair.
- **Tool search.** When a task exposes many MCP tools, load them on demand through a search tool instead of putting every schema in the prompt.
- **MCP up to date.** Add OAuth for remote servers, structured tool output, resources and prompts, elicitation answered by the simulated user, and per-tool timeouts.
- **Context strategies (after dsh).** Besides compaction, add `offload` (hand a token-heavy subtask to a sub-agent and keep only its result) and `fresh-rounds` (the Ralph pattern: each round is a new agent with a clean context, a progress file and a round cap). Record the strategy on the run, so strategies can be compared like models.
- **An `evidence-state` strategy (after LongHarness).** Keep two structured states outside the conversation: an evidence list of source-linked statements, and a conclusion list of claims with open gaps. A new statement must cite a passage the agent has read, and a correction supersedes or retracts the old entry instead of appending to it. Open gaps steer what the agent searches for next. The paper's ablations found structured state beat free-text notes by 6.8 to 7.8 points and append-only evidence cost up to 10.0 points, on one benchmark with one run per setting, so this is a strategy to compare against `compact` and `fresh-rounds`, not a new default.
- **Hooks (after dsh's plugins and pi's extensions).** Add Python hooks at the loop's lifecycle points: before a model call, before and after a tool call (allow, deny or rewrite), on compaction and on stop. Prompts, tool gating and context strategies become hooks, so changing the harness no longer means forking `loop.py`. Every hook decision becomes a step event.
- **Session tree, resume and fork (after pi and dsh).** Store the trajectory as an append-only log of entries, each with a parent. A trial interrupted by a worker restart resumes from its last entry, after the sandbox is restored. Any entry can be forked into a new branch, generalizing the cassette's `branch_at_step`. Add a `session_search` tool so an agent can retrieve what compaction removed.
- **Code-mode tools (programmatic tool calling).** Add an optional profile where the agent writes a Python script that calls the task's tools inside the sandbox, and only the script's output returns to the context. The tool calls inside it are still recorded as steps.
- **Memory and notes.** Add an optional `memory` directory tool that survives compaction, for tasks that measure long-horizon planning.

Exit criteria: a synthetic 300-step task finishes inside a 200k-token window under each context strategy. The number of steps and tokens lost to clearing is recorded on the run. A trial killed mid-run resumes and grades the same as an uninterrupted one.

### H3: Agents under test (Jan 2027)

The goal is to evaluate the agents people actually ship, with full trajectories.

- **CLI trajectory readers.** Add `output_format` readers for the structured event streams of popular coding-agent CLIs: Claude Code's `stream-json`, Codex's JSON exec events, Gemini CLI's JSON output, and pi's JSON event mode. Each reader gets a recorded fixture and a version pin, like `dsh-json`.
- **An ACP connector.** Drive agents over the Agent Client Protocol (`dsh --profile acp`, `deepagents-acp` and other ACP agents). Session updates become steps, and permission requests are answered by the harness policy and recorded as policy events.
- **Agent configuration as environment.** Mount `AGENTS.md` and `CLAUDE.md` files, skills directories and MCP configurations into the sandbox, so the agent's setup is part of the task and can be compared across runs.
- **Sub-agents and multi-agent trajectories (after dsh's agent teams).** Give the built-in agent an optional `task` tool that starts a sub-agent with its own context, tools and budget, taken out of the parent's. Record sub-agent and handoff spans with parent links, from that tool, from A2A tasks, and from the CLI readers. This is what the `agent` pack's handoff-correctness evaluator needs.
- **A2A streaming.** Use `message/stream` and push notifications instead of polling, and show the A2A task states in the trajectory.
- **Harness profiles.** Ship versioned profiles of the built-in agent: `minimal` (pi-style: a prompt of about 1,000 tokens and four tools), `standard` (the H2 toolset and compaction) and `code-mode`. A model score names its profile and version, so it means the same thing across releases.
- **Harness as an experiment variable.** Let a run sweep harness configurations (profile, context strategy, hooks, tools) like it sweeps models. Report a harness × model matrix with confidence intervals, so a team can tell whether a change helped the harness or the model.
- **Cost beside every score (after Evo2).** Report tokens and model calls per task next to each score, and compare harnesses on score for a given budget. Evo2 found a harness that scored higher with 3.6K tokens and 10 calls per task, where two baselines used 6.6K to 6.9K tokens and 26 to 37 calls.
- **Held-out and transfer runs (after Evo2).** A profile tuned on one benchmark is also scored on others it was not tuned on, and the report separates the two. This keeps a profile that only fits its tuning set from looking good.
- **A mechanism-ran check (after Evo2).** When a run changes a harness setting, an evaluator checks the traces for evidence that the changed mechanism actually executed, so a gain is not credited to a change that never ran.

Exit criteria: CI runs each CLI reader and the ACP connector against recorded fixtures. A run compares two harness profiles on the same model and reports the difference with its interval. The kind e2e job runs one CLI agent end to end when a model key is set.

### H4: Environments (Feb 2027)

The goal is to cover the main agent task types: terminal, repository, web, desktop and conversational.

- **Multi-service environments.** Add a Compose-like `services` list (a database, an API, a browser) on a shared private network inside the sandbox rung. This closes the 22 Terminal-Bench 1 tasks that do not import, and Harbor tasks with services.
- **Browser and computer use.** Add a Playwright browser environment with a `browser` tool set (accessibility tree, screenshots, actions), and a desktop environment for computer-use tools with screenshot steps recorded as images. Then build the WebArena/BrowserGym and GAIA importers.
- **Long-document importers (after LongHarness).** Add OfficeQA Pro and AA-LCR importers for tasks over a document corpus that exceeds the context window. They give the H2 context strategies a long-horizon task that is not a coding task. OfficeQA Pro needs corpus hosting in the sandbox, which the multi-service item above provides.
- **Mid-run user messages (after pi's steering queue).** The simulated user can interject while the agent works (a correction, a new constraint, a cancellation), timed by step or by event. Evaluators then score whether the agent took it into account.
- **Stateful tool environments.** Add a database-backed tool environment for τ-bench-style domains, so the checker reads the final database state.
- **Checker isolation.** Run the checker from a clean copy of the setup snapshot, with only the agent's declared outputs or its diff applied. A checker can still opt in to running in the agent's sandbox. Hash protected files, and record any change to them as a policy event.

Exit criteria: a multi-service Terminal-Bench task and a WebArena-style fixture pass on the bubblewrap and pod rungs. A fixture agent that edits the tests is caught and fails.

### H5: Rigor and safety (Feb to Mar 2027)

The goal is grades that are trustworthy.

- **Prompt-injection faults.** Add a fault type that plants instructions in tool results, files or web pages, and an evaluator that scores whether the agent followed them.
- **Reward-hacking signals.** Detect test tampering, hard-coded expected outputs and checker probing, and mark the trial so pass@k can exclude it.
- **Contamination canaries.** Put canary strings in imported tasks, and add a probe that checks whether a model reproduces them.
- **Evidence-stage audit (after LongHarness).** An evaluator in the `agent` pack that splits each trajectory into three stages: was the required evidence retrieved, was it kept and used in the final decision, and was it converted into a correct answer. The last two rates are conditional on the stage before. It tells a team where an agent fails, not only that it fails. LongHarness's version uses an LLM judge, so ours must be checked against human labels before it is trusted, and it reports the judge's agreement.
- **Citation grounding.** A check that every claim an agent submits cites a passage it actually read, and that the citation resolves to the current version of that evidence. It works on any BYO agent's trajectory, and a failed check is also a reward-hacking signal.
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

- **A general agent framework.** The built-in agent stays a small reference for comparing models and harness configurations. Hooks and profiles make it configurable; anything bigger, such as a TUI, an extension marketplace or a plan mode, belongs in the BYO path, where pi, dsh and others already do it well.
- **Provider SDKs as dependencies.** The model clients keep speaking HTTP directly, so the harness stays light.
- **Automated harness search, and training an evolver.** Evo2 shows harness variants can be searched and a model trained to propose edits. We provide the trusted score such a search needs (H3 and H5), but we do not build the search. It would optimize whatever the score rewards, so it waits until checker isolation (H4) and the reward-hacking signals (H5) are in place, and until a team asks for it.
- **Hosting agents.** Evals.si runs agents in sandboxes for evaluation. It does not serve them in production.

## Tracking

**H1 (2026-10-15):** built and tested over mock transports and a local server: thinking preserved across tool turns and configurable on the target, the Responses API client with reasoning items carried between turns, streaming for all three protocols, Anthropic prompt caching with cache-aware cost, `retry-after` and 529 retries, a retry of a turn cut off by `max_tokens`, stop reasons (`refusal`, `pause_turn`), a per-endpoint concurrency limit, fallback models with the switch recorded on the step, and cassette format v2. Not done: none of it has run against a live provider, so the exit test (a recorded multi-step `fix-calc` run with thinking on Anthropic and on an OpenAI reasoning model that replays exactly) is open; it needs model keys. Not built: moving a conversation's reasoning between providers (it is dropped, by a stated rule), a separate model for compaction or sub-agents (they arrive with H2 and H3), Anthropic's `pause_turn` with server-side tools, and a streamed response's partial output on failure (a broken stream is an error, not resumed).

**H2 (2026-10-15):** built and tested against fake models, local servers and a fake MCP server, in this order: context management (`clear-tool-results`, `compact`, `fresh-rounds`, `evidence-state`) with large-output spilling, per-step records and a replay guard; the opt-in toolset (`edit_file`, `glob`, `grep`, `apply_patch`, `memory`, `session_search`); hooks; session logs written as they happen, with resume and fork; `delegate` sub-agents (the `offload` pattern); tool search and the MCP upgrades (timeouts, content types, structured output, resources, prompts, elicitation, OAuth client credentials); and code mode over a restricted interpreter. The synthetic 300-step task in a 200k-token window overflows without a strategy and finishes under each of the four, and an interrupted trial resumes and grades like an uninterrupted one. **Not done:** persistent background processes (the sandbox kills a command's process group when it returns; see [LEFTOVERS](LEFTOVERS.md)); `evidence-state`'s quote check is a text match, and the stage audit that judges it waits for H5. **Not verified:** nothing in H2 has run against a live model or a real tokenizer, OAuth and elicitation have not met a real MCP server, and the token counts rest on a fake model that reports the size of what it was sent.

**H3 (2026-10-15, in progress):** built and tested against fixtures written from the CLIs' published type definitions, and fake models: readers for Claude Code (`stream-json`, with sub-agent parenting and `modelUsage` totals), Codex (`exec --json`), Gemini CLI (`stream-json` and `json`) and pi (`--mode json`); versioned harness profiles (`minimal`, `standard`, `code-mode`) that a run and its recording name; `evalsi matrix`, which puts model calls, tokens and dollars per task beside each score and reports a paired difference with its interval and a tuned/held-out split; agent configuration as `environment.files`, A2A `message/stream` with task states as steps and a mechanism-ran report (`evalsi matrix --mechanisms`). The scored `mechanism-ran` evaluator now uses the same tool/context activity rules, with fixture tests; live-model validation remains open. **Not done:** the ACP connector (it needs an interactive exec in the sandbox), A2A push notifications, a server-side sweep over harness configurations, and the kind e2e run of a CLI agent. **Not verified:** no reader has seen a real CLI's output, and no profile has run against a live model, so the exit criteria (recorded CLI fixtures, an ACP fixture, a measured profile comparison, a live CLI e2e) are open.

**H4 (2026-10-16, in progress):** built and tested against fake models and CLI-agent fixtures on the local sandbox: protected files for the checker (a modified, deleted or new file under `checker.protect` is a guardrail step, and the trial fails or the originals are restored), and scripted mid-run user messages (`interjections`). **Not done:** multi-service environments, the browser and desktop environments with the WebArena/BrowserGym/GAIA importers, the OfficeQA Pro and AA-LCR importers, a domain-neutral database tool environment (the tau2 adapter already grades the final database for its domains), and running the checker from a clean snapshot copy. **Not verified:** nothing ran on the bubblewrap or pod rungs, so the exit criteria are open except that a fixture agent editing the tests is caught, on the local sandbox only.

As each phase is built, what is left of it (unverified, deferred or needing infrastructure) moves to [LEFTOVERS.md](LEFTOVERS.md), the same way the other phases are recorded.

# Harness roadmap

This plan is for making `evalsi-harness` (`python/evalsi-harness/`) a strong harness for frontier agents. It covers the built-in reference agent, the bring-your-own connectors, environments and checkers, and the RL environment specs. It fits into the [milestones](LEFTOVERS.md#milestones): the first two phases land in **M2 agent harness online** (Nov to Dec 2026), and the rest is due by **GA with agent harness** (26 March 2027).

Written 2026-10-14, at v0.2.0. Every gap below was found by reading the code at that release, and the file it lives in is named.

Verification update, 2026-10-17: [live EKS checks](verification/EKS-HARNESS-2026-10-17.md)
now cover signed thinking/streaming/cassette replay, GPU and Anthropic
profiles including real code mode, four bounded context strategies with
mechanism evidence, actual SIGKILL/resume, pinned Claude CLI/MCP and
pod/bwrap checker/interjection controls. Prompt-cache creation was observed,
but direct harness cache-hit controls failed. These checks do not close the
300-step/200k-window, thinking-plus-tool, native OpenAI Responses or full
held-out/transfer exit criteria below.

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
| Environments | One sandbox per task. The pod backend supports per-trial services and an explicit Compose subset; the pinned upstream audit still refuses 22 of 241 Terminal-Bench 1 tasks. There are no browser or desktop environments, and WebArena/BrowserGym and GAIA, listed in DESIGN §8, are not built. | `environment.py`, `benchmarks/` |
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
| **Harness²** ([google-research/harness2](https://github.com/google-research/harness2), [arXiv 2610.14224](https://arxiv.org/abs/2610.14224)) | Keeps model weights fixed and edits eight harness components (system prompt, guardrails, memory, skills, tool guidance, scripts, subagents, plugins) with a Propose–Probe–Compose step that uses no ground truth, plus a persistent improvement harness updated from adopted harnesses. Runs on OpenCode or Codex over JobBench, LAB and WorkBuddy-Bench, and writes adopted harnesses, trajectories and evaluation results under `runs/`. | A **run importer**, a **harness-variant comparison** with a gate, **component ablation**, and **sandboxed, policy-checked execution** of its executors. The refinement loop itself is not built here | H7 |

These papers are preprints from October 2026. Their results come from one setup each and mostly single runs or small subsets, so they motivate the items above and are not evidence that the gains carry to our tasks.

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

- **Multi-service environments.** Add a Compose-like `services` list (a database, an API, a browser) on a shared private network inside the sandbox rung. The pod backend and a restricted importer are built; the pinned audit still refuses 22 original upstream tasks, which also require build/Compose adaptation. Broader backend and importer coverage remains open.
- **Browser and computer use.** Add a Playwright browser environment with a `browser` tool set (accessibility tree, screenshots, actions), and a desktop environment for computer-use tools with screenshot steps recorded as images. Then build the WebArena/BrowserGym and GAIA importers.
- **Long-document importers (after LongHarness).** Add OfficeQA Pro and AA-LCR importers for tasks over a document corpus that exceeds the context window. They give the H2 context strategies a long-horizon task that is not a coding task. OfficeQA Pro needs corpus hosting in the sandbox, which the multi-service item above provides.
- **Mid-run user messages (after pi's steering queue).** The simulated user can interject while the agent works (a correction, a new constraint, a cancellation), timed by step or by event. Evaluators then score whether the agent took it into account.
- **Stateful tool environments.** Add a database-backed tool environment for τ-bench-style domains, so the checker reads the final database state.
- **Checker isolation.** Run the checker from a clean copy of the setup snapshot, with only the agent's declared outputs or its diff applied. A checker can still opt in to running in the agent's sandbox. Hash protected files, and record any change to them as a policy event. *Built (unreleased): `checker.isolation: clean` with declared `outputs`; applying a diff is not built. Run for real on the bubblewrap and Landlock rungs (`tests/test_real_rungs.py`, ladder pinned per rung and the driver asserted; the CI job that builds `evalsid` sets `EVALSI_REQUIRE_SANDBOX` so a missing rung fails). Not run on the pod or Firecracker rungs, so the H4 exit criterion (bubblewrap and pod) is not met.*

Exit criteria: a multi-service Terminal-Bench task and a WebArena-style fixture pass on the bubblewrap and pod rungs. A fixture agent that edits the tests is caught and fails.

### H5: Rigor and safety (Feb to Mar 2027)

The goal is grades that are trustworthy.

- **Prompt-injection faults.** Add a fault type that plants instructions in tool results, files or web pages, and an evaluator that scores whether the agent followed them.
- **Reward-hacking signals.** Detect test tampering, hard-coded expected outputs and checker probing, and mark the trial so pass@k can exclude it. *Built (unreleased): `trial-tampering` with opt-in `summary.exclude_flagged_trials`; honest-run false positives are measured only on the two gold patches of the SWE-bench-format fixture (0 flags, n=2; fixtures, not the benchmark) and not at all on Terminal-Bench. The Harbor and Terminal-Bench importers do not make the workdir a git repository by default, so the diff signals fire there only if the task image ships one; `harbor://...?git=true` (and the same option on `terminal-bench://`) adds a baseline commit so they can, and has been checked only as a setup command run in a real directory, not on a benchmark task.*
- **Contamination canaries.** Put canary strings in imported tasks, and add a probe that checks whether a model reproduces them.
- **Evidence-stage audit (after LongHarness).** An evaluator in the `agent` pack that splits each trajectory into three stages: was the required evidence retrieved, was it kept and used in the final decision, and was it converted into a correct answer. The last two rates are conditional on the stage before. It tells a team where an agent fails, not only that it fails. LongHarness's version uses an LLM judge, so ours must be checked against human labels before it is trusted, and it reports the judge's agreement. *Partly built (unreleased): `builtin/evidence-stage-audit` is the deterministic version, string containment against a reference `{evidence, answer}`, with each stage scored only when the one before passed. `builtin/evidence-stage-judge` is the judge-based version (one call, reads paraphrase), tested with a scripted judge only. Its calibration against human labels, and the judge's reported agreement, are not done.*
- **Citation grounding.** A check that every claim an agent submits cites a passage it actually read, and that the citation resolves to the current version of that evidence. It works on any BYO agent's trajectory, and a failed check is also a reward-hacking signal. *Built: `builtin/citation-grounding` over explicit citations and evidence snapshots (see the H3 paragraph under Tracking).*
- **Determinism report.** For each run, record seeds, model snapshots, tool versions and image digests. A replay that differs points to the step where it diverged. *Partly built: `evalsi determinism` and `determinism.py` describe how stable scores are across trials of the same records, and the dataset hash and target are in a run's manifest. A per-run record of seeds, model snapshots, tool versions and image digests, and a replay that names the step where it diverged, are not confirmed built here and stay open.*
- **Variance-aware defaults.** Recommend a number of trials from the variance seen per task, and report pass^k next to pass@k by default. *Built (unreleased): `evalsi run` prints a variance-bound trial recommendation and writes `trial_advice` (and `evalsi determinism` reports it); `pass^k` already printed next to `pass@k` for `trials` above 1. It is a report: default trials and gates are unchanged.*

Exit criteria: a red-team fixture suite (injection, tampering and escalation attempts) runs in CI with expected scores. *Built (unreleased): `python/evalsi-harness/tests/test_redteam.py` plays an honest fix and 11 repo attacks plus prompt injection, escalation and a canary, each with pinned expected scores, in the Python CI job. The attacks and heuristics were written together, so it is a regression suite and a map of blind spots, not a false-positive rate.*

### H6: RL at scale (GA, Mar 2027)

The goal is for the same tasks to train models as well as evaluate them.

- **Token-in, token-out.** For vLLM and SGLang targets, keep the token IDs and log-probs of every turn, so trainers get exact multi-turn rollouts without re-tokenizing. *Built (unreleased) for vLLM's chat-completions fields, opt-in as `TargetConfig(token_trace=True)`; tested against a fake server only. SGLang is not built or checked, and there is no run-spec field.*
- **Asynchronous and partial rollouts.** Run rollouts concurrently and let a trainer cancel or resume them, with shared environment setup across rollouts of the same task (the open verifiers row in LEFTOVERS). *Built (unreleased): `RolloutPool`, `TaskEnv(pool=)`, pause at a turn boundary and resume from a snapshot on rungs that snapshot (run on real bubblewrap and Landlock sandboxes; not Firecracker); cancellation destroys the sandboxes.*
- **More environment specs.** Add gymnasium and NeMo Gym next to OpenEnv and verifiers. *Built (unreleased), tested against the real packages in the `envspecs` job.*
- **Step-level rewards.** Expose per-step evaluator scores as process rewards, next to the checker's outcome reward. *Built (unreleased): `evalsi_harness.process`.*

Exit criteria: a verifiers or OpenEnv trainer runs one real training step on a GPU runner (this depends on the infrastructure row in LEFTOVERS). Rollout throughput is measured against the §12 target.

### H7: Harness optimization interop (Harness², after H3 and H5; mostly built, see Tracking)

The goal is to measure harnesses that other tools optimize. Harness² refines a harness without ground truth, so it needs an independent, trusted score. We already have the evaluators, gates and sandboxes to supply one. Its README is the only source read so far; the run layout under `runs/` and the executor interfaces must be checked against a pinned release before building, as with DeepSeek Harness above. Its reported gains (up to 11.5 points on WorkBuddy-Bench) come from the authors' setup and are not evidence for our tasks.

Each item is a separate change, in this order:

- **Run importer.** Read a `runs/<bench>/<subset>/…` directory (task summaries, deliverables, adopted harnesses, reflection notes, evaluation results) into trajectories and records, so the existing `tool-call-accuracy`, `tool-errors`, `loop-detection`, step-budget and judge evaluators score them. It follows the Harbor and Terminal-Bench importers: a version pin and a recorded fixture.
- **Harness-variant comparison.** Treat each adopted harness (base, `--k 1`, parallel `--k K`, sequential `--k K`) as a target in `evalsi matrix`. Report pass@k and pass^k with intervals, cost beside score, and a paired per-task difference against the base harness. Add a regression gate that fails when a refined harness is significantly worse than base, or worse on held-out tasks than on the tasks it was refined on.
- **Sandboxed execution of Harness² executors.** Run OpenCode and Codex inside the sandbox ladder with an explicit network policy. Its README notes unrestricted executor network access and a Codex run with its sandbox disabled. Add a policy step for any request outside the allowed hosts.
- **Component ablation.** Given the base and an adopted harness, revert one of the eight component edits at a time, re-run, and attribute the gain per component with an interval. This separates edits that matter from edits that merely came along, and flags an edit that helps its own task and hurts others. It builds on the mechanism-ran check (H3).
- **Harness-edit evaluators.** Score an edit itself: a weakened guardrail, a new tool or network use the task policy forbids, an edit that changes behavior on tasks it was not proposed for, and an edit whose mechanism never ran.
- **Probe-signal calibration.** Harness² chooses between candidates by contrasting executions, with no ground truth. Run our checker and judge on the same candidates and report how often its choice agrees with ours, and the judge's agreement with human labels first, as for the stage audit (H5). This is a measurement of its signal, not a replacement for it.
- **Contamination guard.** Harness² warns that public benchmark rubrics may be online. Flag trajectories that fetch rubric or answer pages, reusing the canary and policy machinery of H5.
- **Benchmark packs.** Add JobBench, LAB and WorkBuddy-Bench (`office`, `web`, `code`) as task packs once their licenses and rubric-handling rules are checked. LAB needs Pandoc and Poppler, and WorkBuddy-Bench needs Docker, so the office and web subsets also depend on the H4 multi-service and browser environments.
- **Live scoring of executor runs.** Emit OpenTelemetry from the executor, or read its session logs, and score them through the watch path with scores written back to MLflow, Phoenix or Langfuse.
- **A Propose–Probe–Compose pattern for the built-in agent (a spike first).** Test whether the pattern transfers to `evalsi-harness` profiles. An edit is adopted only if it passes an H3 gate on held-out tasks. This waits on checker isolation (H4) and the reward-hacking signals (H5), and ends in a written go or no-go. It does not change the "Automated harness search" entry below until the spike says go.

Exit criteria: CI imports a recorded Harness² run and scores it with the shared evaluators. A matrix run compares a base and a refined harness on the same model and reports the paired difference with its interval, and the regression gate fails a deliberately worse fixture harness. The ablation reports a planted no-op edit as zero effect.

## Not planned

- **A general agent framework.** The built-in agent stays a small reference for comparing models and harness configurations. Hooks and profiles make it configurable; anything bigger, such as a TUI, an extension marketplace or a plan mode, belongs in the BYO path (approval gates are the exception: H8 adds them, because an evaluation has to score them), where pi, dsh and others already do it well.
- **Provider SDKs as dependencies.** The model clients keep speaking HTTP directly, so the harness stays light.
- **Automated harness search, and training an evolver.** Evo2 shows harness variants can be searched and a model trained to propose edits. We provide the trusted score such a search needs (H3 and H5), but we do not build the search. It would optimize whatever the score rewards, so it waits until checker isolation (H4) and the reward-hacking signals (H5) are in place, and until a team asks for it. Harness² is such a search; H7 imports, scores, gates and sandboxes its output, and does not run its loop for users.
- **Hosting agents.** Evals.si runs agents in sandboxes for evaluation. It does not serve them in production: no multi-user session store, chat UI or per-conversation pods. H8 evaluates conversational and approval behaviour without serving it.

### H8: Oversight, topologies and conversations (proposed, after H3)

Status: proposed and unassigned, except the approval gate and its evaluators,
which are built (unreleased) and marked below. A comparison with a
serving-oriented harness on 2026-10-21 (README-based, source unread) found three
areas where the built-in agent is thin for *evaluation* purposes: what it does
when it needs approval, what a multi-agent run looks like beyond one level, and
how it behaves across a conversation. Serving agents to users, a session store and
a chat UI stay out of scope (see "Not planned"). The comparison also understated
what exists: one-level `delegate` sub-agents with linked trajectories, scripted
mid-run messages, session resume and fork, and checker isolation are built.

- **Approval gates and human-in-the-loop events.** Today the only decision is
  `request_escalation`, which grants `network` or denies. Add a policy for the
  built-in agent that can require approval before a class of tool call (by tool,
  path glob or command pattern: allow, ask or deny, first match wins). An `ask`
  becomes a `hitl_request` step answered by a scripted or simulated approver
  (approve, deny, approve with an edited argument, or no answer within N steps),
  and the answer is a `hitl_resolved` step. Both are recorded in cassettes, so a
  replay sees the same decisions. This is what an evaluator needs to score "asked
  before the irreversible action", "stopped after a denial" and "did not retry a
  denied call under another name". The ACP connector's permission requests (H3)
  use the same events. Plan mode and a TUI stay with bring-your-own agents.
  *Built (unreleased): the `evalsi_harness.approval:ApprovalGate` hook with
  allow/ask/deny rules by tool, path and command, a scripted approver (approve,
  edit, deny, no-answer) and recorded `hitl_request`/`hitl_resolved` steps that
  replay exactly ([guide](guides/agent-runs.md#approval-gates)). A model or human
  approver and an agent-initiated request tool are not built.*
- **Approval evaluators in the `agent` pack.** `approval-before-sensitive-action`
  (the listed tools were preceded by an approved request), `denial-respected` (no
  equivalent call after a denial, including via `bash`) and `escalation-minimal`
  (the request names the narrowest kind that works). *Built (unreleased):
  `builtin/denial-respected` and `builtin/approval-coverage`; the latter checks a
  gate's coverage of patterns you name, because "approval before action" cannot fail
  under an enforced gate. `escalation-minimal` is not built.* A fixture agent that skips
  approval, one that retries after a denial and one that behaves must score
  differently.
- **Multi-agent topologies.** `delegate` is one level, one model, and runs
  sequentially. Let a run name specialists (a prompt, a tool allowlist, a budget
  and optionally a cheaper model, using the model switching built in H1), allow
  nesting to a configured depth, and run independent `delegate` calls in one turn
  in parallel with the recording order fixed so replay stays exact. Record
  handoffs from A2A tasks and CLI readers with the same parent links. Report
  per-sub-agent cost, steps and outcome beside the total.
- **Topology as an experiment variable.** Sweep single-agent, delegated and
  specialist configurations through `evalsi matrix` next to profiles, so a team
  can tell whether the extra agents bought anything for their tasks at their cost.
  Extend `delegation-links` to check specialist assignment and depth limits.
- **Multi-turn conversation evaluation.** The simulated user is one persona and
  goal. Add a library of scripted and generated users (impatient, ambiguous,
  adversarial, changes their mind), goals that end the conversation when
  met, and conversation-level scoring (resolution, turns to resolve, repeated
  questions, contradicted earlier answers). Add a fixture that interrupts a
  conversation and reattaches to it, so an agent's recovery is measured rather
  than assumed. This evaluates a conversational agent; it does not host one.
- **A live run event stream.** Emit a run's steps, approvals and budget use as
  NDJSON or server-sent events while it runs, so a UI or another tool can follow
  a long trial. Replay and the stored trajectory stay the source of truth.
- **Recipes for agents under test built on common frameworks.** Passing and
  deliberately regressed `agent-run.yaml` controls for LangGraph/Deep Agents
  and for serving-style agents reached over A2A or HTTP, with their session
  handling and approval events mapped to steps. This is the integration work
  PROD-07 asks for, applied to the harness.

Exit criteria: CI runs the approval policy against a fixture agent that asks, one
that skips approval and one that retries after a denial, with expected scores,
and a replay reproduces every `hitl_*` step. A two-level, parallel specialist
fixture replays exactly, and `evalsi matrix` compares it with a single-agent run
and reports cost beside score. A conversation fixture with an interruption scores
the same on replay. The recipes run offline against recorded fixtures.

## Tracking

**H1 (2026-10-15):** built and tested over mock transports and a local server: thinking preserved across tool turns and configurable on the target, the Responses API client with reasoning items carried between turns, streaming for all three protocols, Anthropic prompt caching with cache-aware cost, `retry-after` and 529 retries, a retry of a turn cut off by `max_tokens`, stop reasons (`refusal`, `pause_turn`), a per-endpoint concurrency limit, fallback models with the switch recorded on the step, and cassette format v2. Not done: none of it has run against a live provider, so the exit test (a recorded multi-step `fix-calc` run with thinking on Anthropic and on an OpenAI reasoning model that replays exactly) is open; it needs model keys. Not built: moving a conversation's reasoning between providers (it is dropped, by a stated rule), a separate model for compaction or sub-agents (they arrive with H2 and H3), Anthropic's `pause_turn` with server-side tools (a paused turn with no tool calls is resumed as it stands, in `loop.py`, but no server-side tool is offered), and a streamed response's partial output on failure (a broken stream is an error, not resumed).

**H2 (2026-10-15):** built and tested against fake models, local servers and a fake MCP server, in this order: context management (`clear-tool-results`, `compact`, `fresh-rounds`, `evidence-state`) with large-output spilling, per-step records and a replay guard; the opt-in toolset (`edit_file`, `glob`, `grep`, `apply_patch`, `memory`, `session_search`); hooks; session logs written as they happen, with resume and fork; `delegate` sub-agents (the `offload` pattern); tool search and the MCP upgrades (timeouts, content types, structured output, resources, prompts, elicitation, OAuth client credentials); and code mode over a restricted interpreter. The synthetic 300-step task in a 200k-token window overflows without a strategy and finishes under each of the four, and an interrupted trial resumes and grades like an uninterrupted one. **Not done:** persistent background processes (the sandbox kills a command's process group when it returns; see [LEFTOVERS](LEFTOVERS.md)); `evidence-state`'s quote check is a text match, and the stage audit that judges it waits for H5. **New bounded verification (2026-10-17):** all four context strategies ran with the published ByT5 tokenizer; MCP OAuth/elicitation passed against the published SDK and Authlib; a real worker SIGKILL/resume passed on bubblewrap. Model responses remain scripted, so live-model behavior, summary/evidence quality and long-document results remain unverified ([record](verification/LEFTOVERS-2026-10-17.md)).

**H3 (2026-10-15, in progress):** built and tested against fixtures written from the CLIs' published type definitions, and fake models: readers for Claude Code (`stream-json`, with sub-agent parenting and `modelUsage` totals), Codex (`exec --json`), Gemini CLI (`stream-json` and `json`) and pi (`--mode json`); versioned harness profiles (`minimal`, `standard`, `code-mode`) that a run and its recording name; `evalsi matrix`, which puts model calls, tokens and dollars per task beside each score and reports a paired difference with its interval and a tuned/held-out split; agent configuration as `environment.files`, A2A `message/stream` with task states as steps and a mechanism-ran report (`evalsi matrix --mechanisms`). The scored `mechanism-ran` evaluator now uses the same tool/context activity rules, with fixture tests; live-model validation remains open. **Not done:** the ACP connector (it needs an interactive exec in the sandbox), A2A push notifications, a server-side sweep over harness configurations. Also built and fixture-tested: `citation-grounding` over explicit citations and evidence snapshots; `handoff-correctness` with built-in/Claude/A2A-extension links; an opt-in live Claude CLI kind check and offline artifact verifier. **Not verified:** the live CLI kind check has not completed with a model key; no reader has seen a real CLI's output, and no profile has run against a live model, so the exit criteria (recorded CLI fixtures, an ACP fixture, a measured profile comparison, a live CLI e2e) are open. **Bounded live evidence (2026-10-17, EKS):** the pinned Claude Code 2.1.296 and live GPU and Anthropic profiles ran, and a fresh-pod replay passed; a Qwen code-mode run failed; the other CLIs, ACP and transfer experiments remain open ([record](verification/EKS-HARNESS-2026-10-17.md)). That is a bounded run, not a recorded fixture per reader, so the exit criteria above are still not met.

**H4 (2026-10-16, in progress):** built and tested against fake models and CLI-agent fixtures on the local sandbox: protected files for the checker (a modified, deleted or new file under `checker.protect` is a guardrail step, and the trial fails or the originals are restored), and scripted mid-run user messages (`interjections`). **Also built (2026-10-17):** pod-native per-trial services and a restricted Terminal-Bench Compose importer ([decision 0027](decisions/0027-multi-service-environments.md)). **Not done:** multi-service support on bubblewrap/Firecracker, unrestricted Compose, the browser and desktop environments with the WebArena/BrowserGym/GAIA importers, the OfficeQA Pro and AA-LCR importers, a domain-neutral database tool environment (the tau2 adapter already grades the final database for its domains), and running the checker from a clean snapshot copy. **Verification:** protected-file and interjection checks now pass on real pod and bubblewrap pools with scripted agents ([record](verification/LEFTOVERS-2026-10-17.md)). Service lifecycle evidence is tracked separately in [verification](verification/README.md#multi-service-environments-2026-10-17); the complete H4 exit criteria remain open.

**H7 (2026-10-19, in progress):** built and tested against synthetic fixtures written from the Harness² source (`google-research/harness2`), not from a recorded run: the run importer (`evalsi harness2 import`) and selection calibration with a judge-vs-human kappa helper (`evalsi harness2 calibrate`); the `evalsi matrix --gate` regression gate with held-out checks; component ablation (`evalsi ablate plan|attribute`, with a planted no-op edit giving zero effect); the `harness-edit-*` and `contamination-fetch` evaluators; the WorkBuddy `score.json` verifier option on `harbor://`; `lab://` and `jobbench://` loaders for a directory you supply; and example specs that run the Codex and OpenCode CLIs under the sandbox's per-host egress allowlist. **Not done:** the Propose–Probe–Compose spike (it ends in a go or no-go from live runs); a pinned-release check of the `runs/` layout, so every field is tolerant and unconfirmed; an OpenCode trajectory reader (so OpenCode runs have no steps to score); live scoring of executor telemetry (only post-hoc Codex parsing and OTLP push work today); a per-task significance test for ablation harm (`evalsi ablate attribute --harm` lists helped and hurt tasks and an exact sign test across them, which has little power on few tasks); a faithful grader for LAB and JobBench (`builtin/rubric-grade` grades the final answer text against the carried rubric with a judge, and does not see files the agent left in the workspace; it is uncalibrated); a full Codex or OpenCode task under the egress allowlist (Codex 0.163.0 and OpenCode 1.19.3 both honour the proxy variables, measured with `scripts/probe-cli-proxy.py` without a model; OpenCode also needs `models.opencode.ai` and `registry.npmjs.org`, and Codex also reaches `chatgpt.com` and GitHub, so a one-host allowlist is not enough). WorkBuddy-Bench's licence limits redistribution, so no dataset content is vendored.

**H6 (2026-10-19, in progress):** built: tasks as `reset`/`step` environments (`evalsi_harness.gym`) with a shared setup pool, cancellation that destroys the sandbox, and snapshot and restore of a running episode; adapters that present them as an OpenEnv `Environment`, a verifiers `StatefulToolEnv`, a Gymnasium `Env` and a NeMo Gym `GymnasiumServer`, tested against the real packages in the `envspecs` CI job; step-level rewards (`evalsi_harness.process`) beside the checker's outcome reward; vLLM token IDs and log-probs per turn (`TargetConfig(token_trace=True)`); `RolloutPool` with concurrent, pausable and resumable rollouts; and reward paths for verl, OpenRLHF and TRL, scored in-process or by a server. **Not done:** SGLang token fields (not checked), a run-spec field for `token_trace`, cancel or resume of rollouts across rungs that cannot snapshot (the pod rung), a measured rollout throughput, and the exit criterion (a trainer running one real training step on a GPU runner, which this repository's CI does not do; native verl 0.9.1 and OpenRLHF 0.9.0 updates ran on EKS, [record](verification/EKS-GPU-2026-10-17.md)). Everything new here was tested with scripted models, fake servers and the local sandbox: no live vLLM, no trainer.

As each phase is built, what is left of it (unverified, deferred or needing infrastructure) moves to [LEFTOVERS.md](LEFTOVERS.md), the same way the other phases are recorded.

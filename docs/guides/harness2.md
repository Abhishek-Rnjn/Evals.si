# Measure harnesses optimized by Harness²

[Harness²](https://github.com/google-research/harness2) edits an agent's harness (system prompt, guardrails, memory, skills, tool guidance, scripts, subagents, plugins) without ground truth and keeps the model fixed. That needs an independent score. Evals.si supplies one: it imports Harness²'s runs, compares the harnesses it adopted with a paired interval and a regression gate, ablates its edits one component at a time, scores the edits themselves, and runs its executors (OpenCode and Codex) in a sandbox with a network allowlist. It does not run Harness²'s refinement loop.

**Status: unverified.** Everything here was written from reading Harness²'s source, not from a recorded run, and none of it has run against a live OpenCode or Codex, a real Harness² `runs/` directory or a real benchmark. Fixtures are synthetic. The layout and formats below can be wrong where Harness² differs from its source; the tools say so by reading tolerantly (a missing key is a missing value) rather than guessing. Harness²'s reported gains are the authors' setup and say nothing about your tasks.

## What you can do now

| Step | How |
|---|---|
| Import a run | `evalsi harness2 import` reads a namespace directory (`runs/<bench>/<subset>/<bench>-<substrate>-<mode><k>…`) into one results file per harness: `base` and each adopted mode and k. The importer is `evalsi_harness/benchmarks/harness2.py`. See `evalsi harness2 import --help`. |
| Compare harnesses | `evalsi matrix` puts the files side by side with paired differences against the first (the base). `--gate` exits 1 when a harness is significantly worse than base; with `--tuned-on` it also compares held-out tasks with the tasks the harness was refined on. `--max-regression` and `--gate-strict` tune it. The first input names the baseline harness and model; its first run on each dataset is the reference for that dataset, wherever it sits in the inputs, and a NaN or infinite tolerance is refused. |
| Check the selection signal | `evalsi harness2 calibrate` reports how often Harness²'s chosen candidate is the one your checker or judge scores highest, with an interval, and judge-versus-human agreement. Evaluators in `evalsi/harness2_calibration.py`. |
| Ablate | `evalsi ablate plan` writes one variant per edited component with that edit reverted; run them, then `evalsi ablate attribute` reports each component's effect with an interval. |
| Score the edits | The `harness-edit-*` evaluators (weakened guardrail, policy violation, scope, mechanism-ran) read the base and edited trees from `metadata.harness_edit`. The `contamination-fetch` evaluator flags trajectories that fetch rubric or answer pages. Both are text and path rules: a flag means "look at this", not a verdict. |
| Import the tasks | `harbor://` (with `reward=score.json`), `lab://` and `jobbench://` read a directory you supply. Details below. |
| Run the executors safely | [`examples/harness2/`](https://github.com/Abhishek-Rnjn/Evals.si/tree/main/examples/harness2) has run specs for Codex and OpenCode as CLI agents inside the sandbox. Details below. |

A judge failure is never a zero anywhere in this chain: an ungraded Harness² run is skipped or an error, and a verifier with no usable score is a checker error.

## Licensing and contamination

- **WorkBuddy-Bench** is under a custom Tencent license with a territorial limitation. Read it before downloading. Evals.si ships none of its data, instructions or tests, and the fixtures here are invented. The loaders read a directory you downloaded under that license.
- **JobBench** rubrics (`RUBRICS.json`) are public online. They are a contamination risk for any agent with web access. `jobbench://` copies only `task_folder/` into the sandbox, never the rubric or the `expected/`, `reference/` and `eval_result/` folders; it keeps the rubric in the record's metadata, where `builtin/rubric-grade` reads it: a judge grades the agent's final answer text against each criterion and scores the weighted share met. It does not see files the agent left in the workspace and is uncalibrated, so treat it as a stand-in for the benchmark's own grader. Keep that metadata and your dataset files away from the agent, and add `contamination-fetch` to the run's evaluators.
- **LAB** needs Pandoc and Poppler in the image to read its documents; WorkBuddy-Bench needs Docker for its own runner. Neither is provided here: use `image=` to name an image you built.

## Importing tasks

### WorkBuddy-Bench through `harbor://`

WorkBuddy-Bench tasks have Harbor's shape (`task.toml`, `instruction.md`), so the existing importer reads them, with one gap that is now closed. Harness² reads the verifier's `score.json`, not `reward.json` or `reward.txt`, so a plain `harbor://` import of such a task reports "the tests wrote no reward file". Add `reward=score.json`:

```
harbor://wb-bench-code-v1.0/tasks?reward=score.json
```

The checker then reads `/logs/verifier/score.json` first (and falls back to Harbor's reward files). The score is the first usable of `reward`, `overall`, `test_pass_rate` (each within 0 to 1; an out-of-range value is skipped, not clamped), then `tests_passed / tests_total`, with a score of 1 passing. A file with none of these is a checker error, never a zero. The default import is unchanged.

Grouping carries through: `[metadata]` from `task.toml` is in `metadata.harbor.metadata`, so `role` (code tasks) and `category` (office and web tasks) are at `metadata.harbor.metadata.role` and `.category`.

What is not confirmed, because no WorkBuddy-Bench task was available to read:

- That a task's verifier writes to `/logs/verifier/score.json`. This is inferred from Harness² reading `<trial>/verifier/score.json` and Harbor's log layout.
- That its tasks fit the importer otherwise (`tests/test.sh`, a Dockerfile or `docker_image`, no separate verifier environment, no multi-step). A task outside that is refused with the reason.
- **Judge-scored subsets.** Harness²'s job configuration passes an LLM judge model for the `web` subset, and its adapter calls the `office` subset rubric-scored (whether by a model is not clear). A verifier that calls a model needs the judge's network and credentials inside the checker, which the default `network: deny` blocks. `code` is rule-scored and the most likely to grade offline. There is no default judge here.

### LAB and JobBench

`lab://<dir>` reads `<practice-area>/<task>/task.json` with a `documents/` folder; `jobbench://<dir>` reads `<occupation>/<task>/task_folder/TASK_INSTRUCTIONS.txt` and the optional `RUBRICS.json` beside `task_folder`. The directory is yours; the id is `<group>/<task>`. Options: `tasks` (ids or names, globs allowed), `image`, `workdir` (default `/workspace`), `network` (`deny` or `allow`, default `deny`), and `rubric=false` to leave the rubric out of the records.

Both benchmarks are graded by an LLM judge against a rubric, which cannot be reproduced offline. The tasks therefore have no checker, so `task-success` skips them, and `metadata.grading` says `method: llm-judge`, `reproduced_offline: false` and holds the rubric (`task.json`'s `criteria`, or the parsed `RUBRICS.json`) for a judge evaluator you configure. No judge is chosen for you. The shape of LAB's `criteria` is not interpreted, because the adapter only removes it; turning it into a rubric text is your step. Documents are copied into `<workdir>/documents` (LAB) or `<workdir>/task_folder` (JobBench), and an empty `output/` is created for deliverables. Large document sets travel inside the record, so keep the dataset small or `tasks`-filtered. Not checked: the exact `RUBRICS.json` schema beyond a `rubrics` list with `weight` and `criterion`.

## Running the executors in the sandbox

Harness²'s README notes unrestricted executor network access and a Codex run with its own sandbox disabled. What the sandbox here supports:

- **A per-host egress allowlist exists.** `environment.sandbox.network` is `deny`, `allowlist` (with `allow_hosts`) or `allow`. Entries are `host`, `host:port` or `*.suffix`; a bare host means ports 80 and 443. A CLI agent's `allow_hosts` turns on the allowlist for the agent. A Harbor task defaults to `network: allow` and that task-level setting overrides the spec's, so put `network=deny` in the `harbor://` URI (both examples do); a Harbor task also supplies its own environment variables, which replace `environment.env`, so the examples set the CLI's variables in the command with `env`. Traffic goes through a logging egress proxy that resolves names on the host. Nothing needs to be built for this.
- **The policy step exists.** A request to a host outside the list is refused and recorded as an `egress denied` event, then as an `egress_denied` policy event on the run, and the `policy-violations` evaluator scores it.
- **Caveats.** Only proxy-aware clients are logged (the sandbox sets `HTTP_PROXY` and `HTTPS_PROXY`); a client that ignores them fails closed and unlogged. On the Kubernetes pod rung, enforcement is partial unless the NetworkPolicy is declared and enforced. **Measured 2026-10-19** with `scripts/probe-cli-proxy.py` (the six proxy variables the sandbox sets, a fake key, no model): Codex 0.163.0 sends its API traffic through them (`CONNECT api.openai.com:443`, a websocket attempt then the HTTPS fallback), and also reaches for `chatgpt.com`, `github.com` and `api.github.com`, which an allowlist of only the model host denies and logs as `egress_denied` events that `policy-violations` flags. OpenCode 1.19.3 also uses them (`models.opencode.ai:443`, and, with a model catalog cached, `registry.npmjs.org:443` to install its provider package at run time), so the example's single-host allowlist is not enough for it to start: add those hosts, or pre-seed the catalog and provider package in the image. Neither run got as far as a model call, because no key or model was involved; whether an allowlist that includes those hosts lets a full task run is unverified. Allowlisting a host allows everything on it, so name a model endpoint, not a general host.
- **Codex** runs with `--dangerously-bypass-approvals-and-sandbox`, as in Harness²: its own sandbox is off, so this sandbox is the only boundary. Keep `min_isolation` set and the allowlist narrow. `output_format: codex-json` reads `codex exec --json` into steps and tokens. Whether an API key in the environment alone authenticates `codex exec` is unconfirmed.
- **OpenCode** has no trajectory reader. The available formats are `dsh-json`, `claude-stream-json`, `codex-json`, `gemini-json` and `pi-json`, so an OpenCode run has no steps: its standard output is the answer, and the trajectory evaluators have nothing to score. Writing a reader needs a recorded `opencode run --format json` stream.
- **Install** commands run once during setup with `setup_network: allow`; the agent itself then reaches only the allowlist. Both example specs use placeholders where a version, model or key is needed. They load under the run-spec validator in the test suite, which checks the schema, not that the agents work.

## Live scoring of executor runs

What works today: a CLI agent run by `evalsi run` is scored after it finishes. The Codex stream is parsed into steps once the process exits, so this is a post-hoc score, not live. A harness that sends OpenTelemetry (GenAI, OpenInference, OpenLLMetry or MLflow conventions) to an Evals.si ingest endpoint with an ingest key is scored live, and a [trace source](trace-sources.md) scores traces already kept in MLflow, Phoenix, Langfuse, LangSmith or ClickHouse and writes the scores back.

What does not: nothing reads an executor's session logs or tails its output while it runs, and no reader exists for either executor's own telemetry. Whether Codex or OpenCode emit spans the ingest understands has not been checked, and Harness²'s source has no OpenTelemetry. A sandboxed executor would also need the OTLP host in `allow_hosts`; the proxy carries HTTP(S) only, so OTLP over HTTP is the only variant that could work. Treat live scoring of these executors as not built.

## Not covered

The refinement loop itself, a reader for OpenCode's trajectory, per-rollout cost (the importer carries Harness²'s selection cost only), and trajectories inside Harness² run directories, whose formats are not pinned. See the [roadmap](../HARNESS-ROADMAP.md) for the H7 plan and [agent runs](agent-runs.md) for the sandbox, CLI agents and importers in general.

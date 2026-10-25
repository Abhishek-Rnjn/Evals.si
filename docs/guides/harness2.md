# Measure harnesses optimized by Harness²

[Harness²](https://github.com/google-research/harness2) edits an agent's harness (system prompt, guardrails, memory, skills, tool guidance, scripts, subagents, plugins) without ground truth and keeps the model fixed. That needs an independent score. Evals.si supplies one: it imports Harness²'s runs, compares the harnesses it adopted with a paired interval and a regression gate, ablates its edits one component at a time, scores the edits themselves, and runs its executors (OpenCode and Codex) in a sandbox with a network allowlist. It does not run Harness²'s refinement loop.

**Status: partially verified, not benchmark-qualified.** The [25 October verification](../verification/HARNESS2-2026-10-25.md) exercised the actual pinned upstream deterministic refinement workflow, its on-disk namespace and judge writer, import, comparison gates, missing/error grades and physically executed ablations. Separately, real OpenCode 1.19.3 with Sonnet completed a synthetic task through Evals.si's bwrap sandbox, allowlisted proxy and hidden checker; the follower exported a recorded Sonnet log into a running daemon and an online policy evaluated it. These are **not** a model-driven upstream refinement run, a real benchmark, live Codex or human calibration. Original fixtures remain synthetic. Missing keys remain missing values rather than guesses. Harness²'s reported gains are the authors' setup and say nothing about your tasks.

## Start verification without model credentials

The maintained [`check-harness2-upstream.py`](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/scripts/check-harness2-upstream.py)
acceptance script executes the upstream deterministic demo at a pinned revision,
independently grades its ledger arithmetic, and checks the importer and real
matrix/calibration commands. It also executes script and README-stub ablations
to distinguish a meaningful effect from a no-op. It needs Python 3.12, a
trusted checkout of that upstream revision, and the SDK/harness environment,
but no provider account, benchmark download or Docker. The
[verification report](../verification/HARNESS2-2026-10-25.md#reproduce-the-no-cost-upstream-acceptance)
gives exact commands. This acceptance script and the limit correction below
are unreleased candidate changes, not features already established in PyPI 0.3.1.

## What you can do now

| Step | How |
|---|---|
| Import a run | `evalsi harness2 import` reads a namespace directory (`runs/<bench>/<subset>/<bench>-<substrate>-<mode><k>…`) into one results file per harness: `base` and each adopted mode and k. The importer is `evalsi_harness/benchmarks/harness2.py`. See `evalsi harness2 import --help`. |
| Compare harnesses | `evalsi matrix` puts the files side by side with paired differences against the first (the base). `--gate` exits 1 when a harness is significantly worse than base; with `--tuned-on` it also compares held-out tasks with the tasks the harness was refined on. `--max-regression` and `--gate-strict` tune it. The first input names the baseline harness and model; its first run on each dataset is the reference for that dataset, wherever it sits in the inputs, and a NaN or infinite tolerance is refused. |
| Check the selection signal | `evalsi harness2 calibrate` reports how often Harness²'s chosen candidate is the one your checker or judge scores highest, with an interval, and judge-versus-human agreement. Evaluators in `evalsi/harness2_calibration.py`. |
| Ablate | `evalsi ablate plan` writes one variant per edited component with that edit reverted; run them, then `evalsi ablate attribute` reports each component's effect with an interval. `--by unit` reverts one skill, sub-agent, script or plugin at a time (`c6:fmt`, `c5:coder`, `c7:scripts/a.py`) instead of the whole component, and `attribute --harm` lists the tasks each edit helped and the tasks it hurt, flagging an edit that does both (`--min-effect` sets how small a per-task change counts as unchanged). The harm list adds an exact sign test across the tasks that changed (`sign p`); it is descriptive, and a few tasks give it almost no power (5 helped and 0 hurt is p = 0.0625), and it says nothing about any one task. Beside it, a per-task test compares every trial of the full harness on a task with every trial of the variant (Fisher's exact test for pass/fail metrics, an exact permutation test on the means otherwise), Holm-corrected across the component's tasks; `sig hurt` (`significant_hurt` in JSON, with each task's `p` and `p_adjusted` under `tests`) lists the tasks hurt beyond chance at `--alpha` (default 0.05). It needs several trials per task: with 3 pass/fail trials a side the smallest possible p is 0.1, so nothing can be flagged; with 5 it is about 0.008. |
| Score the edits | The `harness-edit-*` evaluators (weakened guardrail, policy violation, scope, mechanism-ran) read the base and edited trees from `metadata.harness_edit`. The `contamination-fetch` evaluator flags trajectories that fetch rubric or answer pages. Both are text and path rules: a flag means "look at this", not a verdict. |
| Import the tasks | `harbor://` (with `reward=score.json`), `lab://` and `jobbench://` read a directory you supply. Details below. |
| Run the executors safely | [`examples/harness2/`](https://github.com/Abhishek-Rnjn/Evals.si/tree/main/examples/harness2) has run specs for Codex and OpenCode as CLI agents inside the sandbox. Details below. |

A judge failure is never a zero anywhere in this chain: an ungraded Harness² run is skipped or an error, and a verifier with no usable score is a checker error.

## Licensing and contamination

- **WorkBuddy-Bench** is under a custom Tencent license with a territorial limitation. Read it before downloading. Evals.si ships none of its data, instructions or tests, and the fixtures here are invented. The loaders read a directory you downloaded under that license.
- **JobBench** rubrics (`RUBRICS.json`) are public online. They are a contamination risk for any agent with web access. `jobbench://` copies only `task_folder/` into the sandbox, never the rubric or the `expected/`, `reference/` and `eval_result/` folders; it keeps the rubric in the record's metadata, where `builtin/rubric-grade` reads it: a judge grades the agent's final answer text, plus the files the agent left under `output/`, against each criterion and scores the weighted share met. The harness collects those files after the agent finishes (`metadata.grading.workspace.paths`; bounded to 200 files, 20,000 characters per file and 120,000 in total): text files are read as they are, `.docx`, `.xlsx` and `.pptx` from their XML, `.pdf` through `pdftotext` when the image has Poppler. Images, legacy `.doc`/`.xls` and anything else are listed to the judge by name and size, not read, and formatting, charts and embedded images are not seen. It is uncalibrated against human labels and untested on a real LAB or JobBench task, so treat it as a stand-in for the benchmark's own grader. Keep that metadata and your dataset files away from the agent, and add `contamination-fetch` to the run's evaluators.
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
- **OpenCode** runs with `opencode run --format json` and `output_format: opencode-json`: each `step_start`…`step_finish` pair is a model-call step (tokens with cache reads and writes counted as input, cost, finish reason), each `tool_use` a tool step with its output or error, and an `error` event is the run's error. Only the main session's parts are printed, so a sub-agent's work shows only as its `task` tool's output. The reader is pinned to opencode 1.19.3; its fixtures are real 1.19.3 output recorded against a scripted chat-completions server (no live model).
- **Install** commands run once during setup with `setup_network: allow`; the agent itself then reaches only the allowlist. Both example specs use placeholders where a version, model or key is needed. They load under the run-spec validator in the test suite, which checks the schema, not that the agents work.

### Measured OpenCode limits and remaining policy failure

The Linux arm64 OpenCode 1.19.3/Bun 1.3.14 runtime failed before task execution
under 2-GiB and 4-GiB address-space limits. The example now declares
`memory_mb: 8192`: the measured synthetic task passed at that setting, including
a real Sonnet tool call and exact hidden file check. On the measured bwrap
fallback this is a **virtual-address rlimit**, not an RSS guarantee; the outer
development container separately enforced a 4-GiB RAM cap. Do not generalize
that sizing to every platform or runtime version.

Those successful tasks still attempted `registry.npmjs.org:443`. The narrow
allowlist denied and recorded it, so **task-success was 1 but policy-clean was
0**. This is useful enforcement evidence, not a policy-clean result. A deployment
requiring both must gate `policy-violations.policy-clean` as well as
`task-success`. Prefer pre-seeded dependencies to silently widening egress.
The WorkBuddy dataset and verifier in the example remain unverified.

## Live scoring of executor runs

What works today: a CLI agent run by `evalsi run` is scored after it finishes. The Codex stream is parsed into steps once the process exits, so this is a post-hoc score, not live. A harness that sends OpenTelemetry (GenAI, OpenInference, OpenLLMetry or MLflow conventions) to an Evals.si ingest endpoint with an ingest key is scored live, and a [trace source](trace-sources.md) scores traces already kept in MLflow, Phoenix, Langfuse, LangSmith or ClickHouse and writes the scores back.

`evalsi harness2 follow <log> --format codex-json|opencode-json|claude-stream-json|gemini-json|pi-json --task-id ID` closes most of the gap for the executors' event logs. Point the CLI agent's standard output at a file (`... --format json | tee events.jsonl`), set `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS` (an `ingest` key) as for any [OTLP source](end-to-end.md), and run `follow` beside it. It reads the new complete lines every `--poll` seconds with the same reader `output_format` uses, sends the steps that can no longer change as spans of one trace, and sends the root `invoke_agent` span when the log has been quiet for `--idle` seconds. The server's online-evaluation policies then score the trace and write the scores back through the project's sinks (MLflow, Phoenix, Langfuse): `follow` scores nothing itself. A model call and the tool calls after it are held back until the next model call starts or the log ends, so the final answer arrives with the run's last batch; the trace is complete, for policies that need the root span, only at the end. The OTLP host must be in `allow_hosts` if the follower runs inside the sandbox; it normally runs beside it.

Reading Codex's or OpenCode's own OpenTelemetry or session database is not built;
only the printed event stream is read. The original tests use a local collector.
The 25 October follow-up additionally sent an **actual recorded Sonnet executor
log** in two batches through the follower to current-source `evalsid`, fetched
the persisted trace and verified one online latency evaluation. It did not
observe an actively running model concurrently, exercise authenticated ingestion
or verify external sink write-back. The follower still scores nothing itself.

## Not covered

Running the refinement loop from Evals.si, model-driven upstream refinement
verification, per-rollout cost (the importer carries Harness²'s selection cost
only), and benchmark-specific trajectories inside Harness² run directories,
whose formats are not pinned. The standalone executor's JSON event stream is
supported separately. See the [roadmap](../HARNESS-ROADMAP.md) for the H7 plan
and [agent runs](agent-runs.md) for the sandbox, CLI agents and importers.

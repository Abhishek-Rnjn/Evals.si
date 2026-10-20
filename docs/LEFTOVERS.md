# Leftovers from Phases 0 to 6

These items were deferred, left unverified, or are described in [DESIGN.md](DESIGN.md) but not built. Each item names where it was recorded.

The proposed [product roadmap](PRODUCT-ROADMAP.md) turns the [product strategy](PRODUCT-STRATEGY.md)
for production-agent and enterprise AI teams into prioritized work IDs,
dependencies and acceptance criteria. This inventory retains the implementation
and verification evidence behind that plan.

The [runtime follow-up](verification/RUNTIME-VERIFICATION-2026-10-18.md)
retains all 50 original inventory IDs, adds a real-KVM test/workflow with no
simulated fallback, and rechecks free local MLflow/Phoenix score persistence.
KVM, GPU and target-cluster certification remain blocked here. Reproducible
[competitor benchmarks](../tests/benchmarks/README.md) distinguish local SDK
overhead from hosted performance; they do not close production scale targets.

## EKS verification paused, 2026-10-18

The user is deleting the cluster for now. Live checks are paused, not
complete. The [committed handoff](verification/EKS-PAUSED-2026-10-18.md)
records every original inventory row, the GPU-first resume order, exact
unfinished CPU checks, infrastructure/access blockers and reconstruction
requirements. Historical resource-state statements in the run reports
describe the last observation, not a claim that the cluster still exists.

Completed work is in `42d8127` and `cc70404` on `verify/eks-leftovers`, and
the [2026-10-20 AKS continuation](verification/AKS-2026-10-20.md) on
`verify/aks-2026-10-20`. On AKS (NVIDIA A100 80GB PCIe + Intel VT-x KVM +
Istio ambient), native **OpenRLHF 0.11.2** (`vLLM 0.29.0`), native `verl 0.9.1`
(`vLLM 0.24.0`), live `vLLM` `TargetConfig(token_trace=True)` (streaming and
non-streaming, text and tool turns), and trainer-backed `OpenEnv`, `verifiers`
(shared setup), `Gymnasium` and `NeMo Gym` with step-level process rewards
(`EvaluatorReward`, `ToolErrorPenalty`) passed. Real-KVM `TestFirecrackerRealKVM`,
the stock `evalsi-sandboxd` Firecracker chart, clean checking across all four
rungs (`bwrap`, `landlock`, `firecracker`, `pod`), `RolloutPool` pause/resume on
Firecracker and cancel on `pod`, a real SWE-bench Verified task
(`sympy__sympy-20590` on `bwrap` and `firecracker`), all `RECHECK.md` rows,
concurrent PostgreSQL `CreateRun` reconciliation, direct S3 `ImportDataset`,
Production Failure Inbox (`#114`), Shared Dataset Catalog (`#117`) and BYO
agent stop-reason gating (`#116`, `#119`) also passed. AWS S3, external
mesh/provider cases, Kata node runtime installation, manual accessibility and
production recovery/scale remain.

## Milestones

The roadmap's milestones and their exit criteria are in the [PRD](PRD.md#milestones-and-phased-rollout). Where they stand on 2026-10-13:

| Milestone | Planned | Status |
|-----------|---------|--------|
| M1 Release, SDK client | Oct to Nov 2026 | Done: v0.1.0 released and verified; v0.2.0 since |
| M2 agent harness online | Nov to Dec 2026 | In progress: a default policy and a gated deploy built on the demo and run by the kind e2e job; sign-off on the real agent harness needs its repository |
| M3 Pull connectors | Nov 2026 to Jan 2027 | Partly done: MLflow, Phoenix and Langfuse verified; Databricks and SageMaker not run live; a 30-day backfill and the pull lag are now measured (locally and in the kind e2e job), not yet at production scale |
| M4 Real-infra hardening | Jan to Feb 2027 | Not certified: bounded EKS checks passed; load, full upgrade and mesh enforcement remain |
| M5 More sources, SDK | Jan to Mar 2027 | SDK parts done; LangSmith source built, with server/worker fixture coverage and an opt-in live check ([method](verification/LANGSMITH.md)); live verification still needs credentials; ClickHouse Collector Map source built and bounded-live-tested ([method](verification/CLICKHOUSE-SOURCE.md)); ClickHouse Cloud/TLS/distributed/scale checks and Tempo/Kafka/S3 sources remain |
| GA with agent harness | 26 March 2027 | Not started |

The plan for strengthening the agent harness through M2 and GA is in [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md).

The latest release is **v0.3.0** ([release](https://github.com/Abhishek-Rnjn/Evals.si/releases/tag/v0.3.0), [changelog](../CHANGELOG.md#030---2026-10-19)); see [Releases](PRD.md#releases).

The completed checkpoint from `verify/eks-leftovers@42d8127` is now included
in PR #85. See the [EKS run](verification/EKS-2026-10-17.md) and
[harness/checkpoint follow-up](verification/EKS-HARNESS-2026-10-17.md) for
live GPU/model, checkpoint PVC/LoRA, framework, judge and stock bwrap evidence.
The [subsequent GPU run](verification/EKS-GPU-2026-10-17.md) verified actual
native verl 0.9.1 and OpenRLHF 0.9.0 updates, not just inference. Current
OpenRLHF 0.11.2 and trainer-backed environment checks remain unverified.
Direct harness prompt-cache reuse controls failed.

Last updated: 2026-10-18 (cluster-deletion pause, complete remaining-check handoff and native GPU training evidence, reconciled with main's separate kind results); before that 2026-10-17 (all 50 item rows audited; live verification, recovery drills and remaining requirements in [the combined record](verification/LEFTOVERS-2026-10-17.md)); before that 2026-10-16 (production-readiness review: judge prompt fences and dependency scanning fixed, seven rows added, three extended; see the review note below); before that 2026-10-13 (v0.2.0 released and verified; the two buildable rows built: verl and OpenRLHF reward paths and Claude Code over MCP in CI; backfill and lag measured; E6 imports); before that 2026-10-12 (bounded EKS verification, [report](verification/EKS-2026-10-12.md); server-side slicing, run archiving, environment specs, platform wheels, operator checkpoint serving and a hosted Wasm plugin built; the rest of server-side analytics closed as not planned; v0.1.0 released and its signatures verified; the two rows that waited on the tag are closed); before that 2026-10-11 (triage of every row, after the Part C review fixes; annotation claims, the UI browser test and per-release pool isolation); before that 2026-10-10 (trace sources) and 2026-10-09, after the [end-to-end verification](verification/README.md) on kind, AKS and an Istio ambient cluster with KVM.

Trace redaction on push and pull is built and regression-tested. A new kind
push check found the synthetic credential absent from retained NATS bytes,
PostgreSQL record bytes and the authenticated API response. Pull-provider and
ClickHouse coverage retain their previous evidence; this does not certify every
trace path or historical data. See [the live record](verification/LEFTOVERS-2026-10-17.md)
and [decision 0019](decisions/0019-trace-redaction.md).

## Harness² interop (H7, 2026-10-19)

Built from a read of the Harness² source and tested on synthetic fixtures; nothing has run against a recorded Harness² run, a live model or a real WorkBuddy-Bench, LAB or JobBench task. Open: the Propose–Probe–Compose spike (a go or no-go from live runs); the `runs/` layout and `score.json` verifier fields checked against a pinned release; an OpenCode trajectory reader; reading executor telemetry live; a per-task significance test for ablation harm (the harm list carries only a low-power sign test across tasks); grading LAB and JobBench work products (`builtin/rubric-grade` judges the answer text against the rubric, not the files, and is uncalibrated); a human-labelled check of the judge before `calibrate` results are trusted; a full Codex or OpenCode task under the egress allowlist (both honour the proxy variables as of Codex 0.163.0 and OpenCode 1.19.3, measured with `scripts/probe-cli-proxy.py` without a model; OpenCode also needs `models.opencode.ai` and `registry.npmjs.org`, Codex also reaches `chatgpt.com` and GitHub); the false-positive rates of `harness-edit-weakened-guardrail` and `contamination-fetch` on real harness edits and trajectories. Details are in [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md#tracking) and [the guide](guides/harness2.md).

## Harness work built but not verified (H1 to H7, 2026-10-19)

What exists in the code and what has been checked, for the harness roadmap's items. "Verified" here means run against the real thing, not a fixture or a scripted stand-in. Rows for H1 to H4 summarize [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md#tracking) and the EKS rows below; the H5 to H7 rows are from work in this period.

| Item | Built | Not verified |
|------|-------|--------------|
| H1 frontier models | Thinking across tool turns, Responses API client, streaming, prompt caching, retries, fallback models, cassette v2 | Run against a live provider only in the bounded EKS checks; the exit test (a recorded multi-step `fix-calc` run on Anthropic and an OpenAI reasoning model that replays exactly) is open. Direct prompt-cache reads failed in both modes |
| H2 long-horizon agents | Four context strategies, toolset, hooks, session resume and fork, `delegate`, MCP upgrades, code mode | Model responses are scripted; live-model behaviour, summary and evidence quality, and long-document results are unverified. `evidence-state`'s quote check is a text match |
| H3 agents under test | Readers for Claude Code, Codex, Gemini CLI and pi; harness profiles; `evalsi matrix`; A2A `message/stream`; `citation-grounding`; `handoff-correctness` | Readers were written from published type definitions; only the pinned Claude Code run on EKS saw real output. No profile comparison on a live model; the live CLI kind check has not completed with a key |
| H4 environments | Protected files, interjections, pod per-trial services, restricted Compose importer, clean checker isolation (`checker.isolation: clean`) | Clean checking ran on real bubblewrap and Landlock rungs (the `e2e` CI job requires them), **not on the pod or Firecracker rungs**, which the exit criterion names. Applying an agent's diff instead of declared outputs is not built |
| H5 `trial-tampering` | Test-tampering, hard-coded-output and checker-probing signals; opt-in `summary.exclude_flagged_trials`; scores a CLI agent on its diff | False positives measured on **two** gold patches of the SWE-bench-format fixture (0 flagged), none on real honest runs or Terminal-Bench. The importers make no git repo unless asked: `git=true` adds a baseline commit (checked only as a command run in a real directory, with git needed in the image). Exclusion stays off by default. A `sitecustomize.py` shim is a known blind spot; only clean checking stops it |
| H5 red-team suite | 11 repo attacks, injection, escalation and a canary, with pinned scores, in the Python CI job | It is a regression suite written together with the heuristics, not a false-positive or detection rate |
| H5 variance-aware trial advice | Per-task bounds (exact binomial for pass/fail, chi-square for numeric) in `evalsi run` and `evalsi determinism`; a report only | Checked by unit tests and an exact-coverage test; never run over real multi-trial results. The numeric bound assumes roughly normal scores |
| H5 evidence-stage audit | `evidence-stage-audit` (string containment) and `evidence-stage-judge` (one judge call) | Unit tests, and a scripted judge for the judge version. No real `evidence-state` runs; the judge is **uncalibrated** against human labels, and no agreement is reported |
| H5 determinism report | `evalsi determinism` for score stability across trials | A per-run record of seeds, model snapshots, tool versions and image digests, and a replay that names the step where it diverged, are not confirmed built |
| H6 RL environments | `evalsi_harness.gym` (`reset`/`step`, shared setup, cancel, snapshot and resume), OpenEnv, verifiers, Gymnasium and NeMo Gym adapters (the `envspecs` CI job runs the real packages), verl, OpenRLHF and TRL reward paths | Native verl 0.9.1 and OpenRLHF 0.9.0 updates ran on EKS; no trainer-backed OpenEnv, verifiers, Gymnasium or NeMo Gym run (NeMo Gym's own `gymnasium_agent` drives an episode in a test, with a scripted model and the server behind a test client, not over a network or with a trainer; Gymnasium's `TaskGymEnv` has only been run with the local test sandbox, not a real sandbox client on its background loop), and current OpenRLHF 0.11.2 is unverified |
| H6 rollouts | `RolloutPool` (concurrent, pause at a turn boundary, resume from a snapshot), per-turn process rewards | Scripted models. Pause and resume ran for real on the bubblewrap and Landlock rungs (`tests/test_real_rungs.py`, run locally against a built `evalsid`); not on Firecracker or pod (the pod rung cannot snapshot, so it cancels). Rollout throughput has not been measured against the DESIGN §12 target |
| H6 step-level rewards | `EvaluatorReward` (delta or level of any record-scope evaluator on the trajectory so far), `ToolErrorPenalty` | Unit tests with built-in evaluators. No training run has used them, so whether they help is unknown; a judge-backed evaluator makes a call per step |
| H6 token IDs and log-probs | `TargetConfig(token_trace=True)`: vLLM's `return_token_ids` and `return_tokens_as_token_ids` fields, parsed (also from a stream), recorded in cassettes | A fake server only; **no live vLLM** and **no SGLang** (its field names were not checked). Not exposed in the run spec (no proto field). The prompt IDs depend on the server version returning `prompt_token_ids`; a trainer still needs the server's `/tokenize` where it does not |
| H7 `rubric-grade` | Judge scores the weighted share of a LAB or JobBench rubric's criteria met | Scripted judge only; sees the answer text, not files the agent left in the workspace; uncalibrated; never run on real LAB or JobBench data |
| H7 ablation | `evalsi ablate plan|attribute`, `--by unit` (per skill, sub-agent, script, plugin) and `--harm` (helped and hurt tasks) | Synthetic results only. The harm list is descriptive: an exact sign test across the changed tasks (`sign_p`) is reported, but a handful of tasks gives it almost no power, and there is no per-task significance test |
| H7 proxy honouring | `scripts/probe-cli-proxy.py` | Codex 0.163.0 and OpenCode 1.19.3 route through the proxy variables, but no model call was reached: OpenCode also needs `models.opencode.ai` and `registry.npmjs.org`, Codex also reaches `chatgpt.com` and GitHub, so a one-host allowlist is not enough. A full task under an allowlist has not run |

## Production-readiness review (2026-10-16)

This review focused on self-hosted installations, where the customer's identity
provider and platform own many account and infrastructure controls. Enterprise
customers can still require vendor security/procurement evidence and supported
operating procedures for a self-hosted product. The proposed [enterprise roadmap](PRODUCT-ROADMAP.md#enterprise-aiplatform-workflow)
tracks those requirements separately from additional hosted-service work such as
billing and account operations. Certification, native provisioning/federation and
other buyer-specific controls require an explicit scope and evidence.

Two findings were fixed on the review branch:

- **Judge prompt fences could be closed by the text they hold.** `fence()` in `python/evalsi/src/evalsi/packs/judge.py` turns the opening `<` of any fence tag in untrusted text into `&lt;`. The judge, rag and agent packs, the built-in simulated user and the tau-bench simulated user all use it, so a response containing `</response>` can no longer write instructions outside its fence. Records whose text contains a fence tag now reach the judge escaped, so their scores can differ from earlier runs. Whether a judge model resists instructions inside a fence is a property of the model, and is measured by the row "Judge robustness against injected text" below.
- **No dependency vulnerability scanning.** `.github/workflows/security.yml` runs `govulncheck`, `pip-audit` (on the locked `uv` set) and `npm audit` (website and UI test) on pull requests, on dependency changes on `main`, and weekly. `.github/dependabot.yml` opens grouped weekly updates for Go, Python (`uv`), npm, Docker and GitHub Actions.

`pip-audit` found nothing in the locked Python set and `npm audit` found no high or critical issues (the website has 10 moderate ones, in the docs theme's `postcss` dependencies). `govulncheck` could not be run in the build sandbox (`vuln.go.dev` is blocked), but its first CI run on the review PR found 41 reachable standard-library advisories: `go.mod` pinned `go 1.26.0`, which is what CI and the release binaries build with (the Dockerfiles use `golang:1.26`, which floats to the latest patch). It also found five HTTP/2 advisories in `golang.org/x/net` v0.59.0. The review PR raises the directive to `go 1.26.9` and `x/net` to v0.60.0; keep it at a patched 1.26 release, since `setup-go` builds with exactly the version `go.mod` names.

The rest is planned, in the rows below: the new rows are marked "(review 2026-10-16)", and the rows that already existed carry a line from the review. Each says why it is not on the review branch.

## Current verification triage (2026-10-17)

All **50 original item rows** below have a recorded result or remaining requirement in the
[new inventory](verification/LEFTOVERS-2026-10-17.md#every-leftovers-row).
Nineteen have executable evidence in this PR: four bounded checks passed their stated
verification requirement and fifteen have partial results with remaining scope (including the earlier Terminal-Bench checks and imported EKS checkpoint).
The other rows require unavailable access/hardware/scale, missing implementation,
a product decision, or describe an existing intentional limit. This replaces the
stale 42-row count; a partial result is not a production sign-off.

| Outcome | Rows |
| --- | ---: |
| Bounded verification passed | 4 |
| Partial verification | 15 |
| Credentials, external/private access, hardware or scale required | 11 |
| GPU training partial; current-stack verification paused | 1 |
| Implementation required | 7 |
| Product decision required | 3 |
| Existing known limit | 8 |
| Already verified in release evidence | 1 |

Since that inventory, per-caller rate limits (L33) are built ([decision 0029](decisions/0029-per-caller-rate-limits.md)), so six rows need implementation.

## Verified only with stand-ins (needs real infrastructure)

[Bounded EKS verification on 2026-10-12](verification/EKS-2026-10-12.md) closed
the live-KEDA, several-host NATS broker-failure and per-release pool-isolation
rows below. Later checks also closed real agentgateway guards, mounted plugins
and bundled ClickHouse/full second-release coexistence. Stock user-namespaced bwrap worked on the observed EKS hosts, not
on every host; Calico controls without a mesh do not close Calico/Istio-ambient
enforcement. The initial second release was pool-only; a later full application
with separate database/NATS and key-rejection controls passed. M4 remains uncertified.

The default amd64 kind job, including Landlock, passed in
[CI run 119](https://github.com/Abhishek-Rnjn/Evals.si/actions/runs/38135174954)
at `ba2ceff` on 2026-10-11. Docker Desktop's kernel does not enable Landlock;
`SKIP_LANDLOCK=1` remains an explicit local opt-out, not Mac verification of
that rung. The new AKS recheck has not run.

Langfuse `v4.56.0` passed Compose and in-cluster TraceSource checks, including
Secret credentials, policy scores, replacement under the same score ID and an
evalsid-pod NetworkPolicy allow/deny/restore control. The tested images were
amd64 under emulation; Cloud, TLS, production storage and arm64 images remain
unverified. See the [run record](verification/README.md#langfuse-live-recheck-2026-10-10).

The [platform-aware bundle recheck](verification/README.md#platform-aware-bundle-recheck-2026-10-10)
passed arm64 bundle export, kind import and installation, and bootstrap key
authentication across an upgrade. Later runs passed the demo dataset upload,
both small-repo suites, MLflow sink and TraceSource loop, and restricted-namespace
pod-rung code and agent checks. CI run 119 covers Landlock on amd64 kind;
an opted-out Mac run is not a complete Kubernetes verification.

| Item | Why it is open | Where recorded |
|------|----------------|----------------|
| Firecracker with the stock image and chart values | The rung itself is verified (see below), but that run used a patched ConfigMap and a derived image. The fixes that make the stock image and `firecracker.defaultImage` work (PR #12) have not been re-run on a KVM cluster. | [verification](verification/README.md) |
| User-namespaced bubblewrap pools (`mode: bwrap`, `hostUsers: false`) | Stock source chart with hostUsers:false passed mTLS/full-enforcement probe, good/bad execution and eight H4 controls on EKS AL2023/containerd. Nested kind UID-mapping preflight failed; AKS/idmap host limits remain. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [verification](verification/README.md) (VR-D15) |
| Sandbox egress NetworkPolicy on Calico with Istio ambient | The operator's policy did not block a sandbox-labelled pod's egress on such a cluster; the cause is unknown. kind and the AKS cluster tested have no policy engine. The pod rung now verifies a declared policy with a canary connection and reports `partial` isolation and a probe warning when it is not enforced; the new kind/Istio ambient check reproduced the leak and partial report, refused unsafe services, and passed with explicit pool/trial ambient opt-out ([record](verification/LEFTOVERS-2026-10-17.md)). | [verification](verification/README.md) |
| Admission webhooks on EKS with Calico's overlay | Actual chart hostNetwork admission passed on EKS with invalid-gate rejection, unavailable-webhook fail-closed and recovery controls; local kind chart admission also passed. Overlay pod routing is not certified. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [guide](guides/kubernetes.md), [report](verification/EKS-2026-10-12.md) |
| Admission webhooks under mesh-wide STRICT mTLS | Verified on kind with Istio 1.30.6, a real sidecar operator and mesh-wide STRICT. Removing the chart port exception blocked a valid dry run; restoring it admitted the request, and an invalid gate was rejected. This closes the bounded verification gap, not all mesh versions ([record](verification/LEFTOVERS-2026-10-17.md)). | [verification](verification/README.md) (VR-R2) |
| PR #12 fixes on real clusters | The fixes for the verification findings have unit, chart and kind tests, but have not been re-run on AKS or the Istio cluster. | [verification](verification/README.md) |
| Trace sources (milestone M3) | Verified against MLflow 3.17.0/Phoenix 20.20.0 fixtures and live worker loops; R6's MLflow CRD/policy loop passed on kind with PostgreSQL source tables. Langfuse 4.56.0 passed in-cluster Secret/Service polling, score replacement and enforced egress. EKS real MLflow pull redacted API/storage/actual CPU work and wrote assessment feedback. Real Prometheus Operator loaded shipped rules; the unchanged 15-minute failure rule fired, delivered through local Alertmanager and resolved after repair with a fresh healthy scrape. Databricks experiments/Unity Catalog/OAuth and SageMaker SigV4 remain pinned-source/derived-fixture tested, not live-verified. A 30-day MLflow backfill and live pull lag are measured by `scripts/measure-trace-source.py` (locally: 500 traces backfilled in 5 s, live p95 15 s at a 15 s poll; the kind e2e job runs it on the demo MLflow). Not verified: those external providers, Langfuse Cloud/TLS/arm64, backfill and lag at production scale, provider UI visibility and external alert channels. Not built: Databricks table-prefix/experiment-name locations or ambient AWS SageMaker credentials; Azure ML V3 traces refused. Accepted online work now has a durable replay queue that preserves policy selection across restarts; evaluation and external callbacks are at least once. Older unscored traces need a source backfill. | [trace sources guide](guides/trace-sources.md), [decision 0016](decisions/0016-trace-source-connectors.md), [EKS methods](verification/EKS-2026-10-12.md) |
| The reference demos (`examples/demo`) | Both small-repo suites, MLflow sink and Deep Agents pull passed on kind. Native dsh headless GenAI export/pull/assessment also passed; interactive web export remains unverified. Deep-research and Terminal-Bench 2 need a real model/judge and have never run. | [Part C](verification/PART-C.md), [R1 to R9 in the PRD](PRD.md) |
| Frontier-model handling in the built-in agent (H1) | EKS live Anthropic tools/streaming, signed thinking and exact cassette replay passed. Direct prompt-cache reads failed in both modes; native Responses and thinking-plus-tool roundtrip remain open. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h1-frontier-model-support-m2-nov-2026) |
| Long-horizon agents (H2) | Published ByT5 tokenizer exercised four strategies; real MCP SDK/Authlib OAuth/elicitation and local SIGKILL/resume passed. EKS live four-strategy checker/replay and fresh-pod process resume passed; 300-step/context-quality and external-side-effect idempotency remain open. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h2-long-horizon-agents-m2-dec-2026) |
| Persistent background processes for the built-in agent (H2) | `bash` runs each command to completion: the sandbox launcher kills the command's process group, and the cgroup where there is one, when it returns, so a server or watcher started in one call is gone in the next. Sessions that persist across calls need an `evalsid` change (a long-lived shell per sandbox), not a harness one. Not built. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h2-long-horizon-agents-m2-dec-2026), `internal/sandbox/launch.go` |
| Agents under test (H3), in progress | EKS pinned Claude Code 2.1.296, live GPU/Anthropic profiles and fresh-pod replay passed. Qwen code-mode failed. Other CLIs, ACP and transfer experiments remain open; the GitHub live workflow is separate. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h3-agents-under-test-jan-2027) |
| Environments (H4), in progress | Built: protected files for the checker (`checker.protect`, `on_change: fail|restore`) and scripted mid-run user messages (`interjections`). Both now pass with scripted models/CLI fixtures on real bubblewrap and pod pools; this is not live-model quality evidence ([record](verification/LEFTOVERS-2026-10-17.md)). **Also built (2026-10-17):** per-trial services on the pod backend, with dependency readiness, resource limits, bounded lifetime, status/log inspection and a restricted Terminal-Bench Compose importer ([0027](decisions/0027-multi-service-environments.md)). Bubblewrap/Firecracker service groups and unrestricted Compose are not built. **Not built:** the browser and desktop environments and the WebArena/BrowserGym and GAIA importers (they need a browser in the image and screenshot steps); the OfficeQA Pro and AA-LCR importers (they need the multi-service corpus hosting); running the checker on a clean copy with only the agent's *diff* applied (declared-output clean checking is built, unreleased, and was run on the bubblewrap and Landlock rungs but not on the pod rung; protected files guard the workdir); a generic database-backed tool environment (the tau2 adapter already grades the final database for its own domains, through tau2's environment; a domain-neutral one is a design question, not just code); interjections timed by event rather than by step, and for CLI agents. **Exit criteria open:** a multi-service Terminal-Bench task and a WebArena-style fixture on the bubblewrap and pod rungs; the editing-the-tests fixture is now caught on local, bubblewrap and pod sandboxes. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h4-environments-feb-2027) |
| Agent framework mapping profiles | All ten pinned live framework recorder configurations passed mapping contracts in EKS. Native Claude cached-input total fixed. Missing source usage/answers remain absent; native Responses, retries and multi-agent coverage remain open. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | [guide](guides/agent-frameworks.md), PRD scope table |
| 10k+ concurrent agent trials (§14 target) | Needs a cluster sized for it. The in-cluster load test on the Istio cluster reached 1,220 Evaluate calls/s (p95 24 ms), 387k OTLP spans/s and a 1,000-record run at 3,985 records/s; concurrent agent trials were not load-tested. | §23 Phase 4 load test, [verification](verification/README.md) |
| Real SWE-bench Verified images | Several GB each; CI runs fixtures in the same format, graded by the same code. The verification ran the fixtures on the pod, bubblewrap and Firecracker rungs with identical results, but no real task. | §23 Phase 3 limits |
| cgroup v2 limits in an unprivileged container | Tested as root on CI's cgroup v2. In a pod, the cgroup mount is read-only unless the pod is privileged (as the bubblewrap pool is), so other deployments fall back to rlimits. | §13 implementation notes |
| Kata Containers as the `vm` level on the owner's cluster (D13) | Supported as a pod `runtimeClassName`; not run on a cluster with Kata installed. | D13, decision 0007 |

## Verified on real clusters

These were verified only with stand-ins before the [verification run](verification/README.md), which used commit `c5a1de9`.

| Item | Result |
|------|--------|
| Firecracker rung on a KVM host, `evalsi-sandboxd` with `mode: firecracker` | Code evaluation and the SWE-bench fixture ran in microVMs (`ISOLATION_LEVEL_VM`), with the same results as the pod and bubblewrap rungs |
| HA: a replica that owns a run is killed | Another replica adopted the run; every result was stored once |
| Namespace-only install under Pod Security `restricted` | Installed and ran code evaluations on kind, AKS and the Istio cluster (pod rung and Landlock). Agent runs on the pod rung passed on kind and AKS. On the Istio cluster they failed at the waypoint (VR-R1); pod labels to opt out now exist |
| TRL GRPO with Evals.si rewards | Passed on CPU on kind and AKS |
| KEDA scaling a live pool | EKS: genuine 32-record agent backlog scaled harness workers from 0 to 3 Ready and naturally back to 0; bounded functional check, not production load |
| Several-node NATS (stream replicas 3) | EKS: three brokers on distinct physical hosts; active run survived work-leader pod deletion with 32 unique results and both followers current for WORK/SPANS; not EC2 node failure |
| Per-release isolation of sandbox pools | EKS/Calico: two actual Helm pools; symmetric own-pool positive and other-pool/API negative TCP controls. Initial second release was pool-only; the later full application is recorded separately below. Shared SandboxClass behavior and mesh enforcement not certified |
| Bundled ClickHouse and a full second Helm application | EKS: actual stored redacted records; separate full API/CPU/pool release with isolated database/broker, live scoring and symmetric key rejection. Original failed fixture retained; not two independent database instances |
| Mounted Python and Wasm plugins on Kubernetes | EKS: real chart-mounted Python pack scored by name through API/CPU; init-populated read-only Wasm mount ran good/bad/skip records and rejected a wrong pin. Distributed API Wasm execution not claimed |
| Guardrails against a live agentgateway | Real v1.6.0 native LLM and MCP request/result pass/mask/block controls; LLM error fail-closed. Synthetic model/tool, non-streaming tools/call; streaming and real model/agent products remain unverified |

## Deferred features

| Item | Notes | Where recorded |
|------|-------|----------------|
| Webhook URLs open to internal addresses | Run, trace and alert events are built ([webhooks guide](guides/webhooks.md)). Webhook URLs are not restricted to public addresses: the server dials whatever an admin sets, which is how it reaches in-cluster services. `trace.scored` has no filter of its own (a policy's selector and sampling bound it), and is not load-tested at the PRD's 10k traces/min. Review 2026-10-16: trace sources already refuse private, loopback and link-local hosts unless `sources.allow_hosts` lists them (`internal/source/guard.go`); the same guard with an allow list would close this once the default is decided. | P4 in the PRD |
| Server-side SQL over runs | Built: `evalsi analyze slice --on-server` computes a metric per run and metadata value on the server, over the runs the caller may read ([decision 0022](decisions/0022-server-side-slicing.md)). Arbitrary SQL stays in `evalsi analyze query`, on the client: run results are stored as protobuf, not columns, so there is nothing to run SQL on, and a SQL endpoint could not be scoped to the runs a caller may read. ClickHouse holds traces and their scores, not runs. | Decisions 0009, 0022 |
| Platform wheels bundling evalsid | Built ([decision 0024](decisions/0024-platform-wheels.md)): the release builds `evalsi` wheels for Linux amd64 and arm64 with `evalsid` and `bwrap` in `evalsi/_bin`, and CI builds one from the real archive, installs it and runs its sandbox. First published in v0.2.0: the arm64 wheel from PyPI installed in an arm64 Linux container and its bundled `evalsid` reported 0.2.0. Not covered: macOS and Windows (no archive; the sandbox is Linux-only). | Decision 0006 |
| Run archiving | Built: `ArchiveRun` (and `runs.archive_after`, `runs.project_archive_after`) keeps a run's summary after its records, outputs and results go, and automatic deletes and archives write audit events ([decision 0023](decisions/0023-run-archiving.md)). Archiving releases the stored-run quota; authorized, audited terminal `DeleteRun` removes everything. API authorization metadata remains for 24h retry checks after an explicit delete. External sink data and promoted datasets remain; deleting an EvalRun still keeps its server run. Not built: per-label retention rules. Review 2026-10-16: deletes are hard, with no grace period before they take effect and no documented erasure path that reaches backups and logs; add both when the retention rules are built. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), [0023](decisions/0023-run-archiving.md), F31 |
| Limits left after the reviews of items 3 to 6 and Part C | Everything those reviews found that had a safe code fix is fixed: scheduled-window skips name the traces, a lost schedule lease is reported as the cause, idle idempotency scopes are swept hourly, card numbers next to a CVV are hidden, `evalsi.log()` reports a failure once and bounds exit and fork behaviour, checkpoints take the umask's mode and only rank zero saves, SDK metadata is bounded, dataset-upload read errors are client errors and a stalled upload times out, and the dsh plugin closes unfinished turns. What remains: a keyed dataset upload is written to disk twice (spooled for the request fingerprint, then again by the dataset store) and takes an upload slot in both places; `evalsi.log()` does not retry a failed batch (its futures carry the error), and if retries are ever added they must send an `Idempotency-Key`; a scheduled window skips a trace a connector returns nothing for (deleted or empty), so a connector that returned a partial page without an error would lose traces from that window. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), [decision 0019](decisions/0019-trace-redaction.md) |
| Pipeline and schedule certification | New live checks cover MinIO S3 upload/hash, concurrent same-key create/delete across two PostgreSQL-backed API replicas, unique results, archiving and restore. AWS S3, database failover/HA races, production load, MCP tool idempotency and scheduled source write-back remain uncertified or unsupported ([record](verification/LEFTOVERS-2026-10-17.md)). | P6/P7/S9, decision 0020 |
| Sandbox-time quotas: what they do not cover | `sandbox_seconds_per_day` meters runs: agent tasks (start to end) and code evaluators that run in the sandbox pool. It does not meter online policy evaluation, guardrail checks, Wasm evaluators, reward-service rollouts or sandbox use outside a run, and a run's time is counted as it goes, so the run that crosses the line finishes its current batch. Concurrency quotas are per replica. | §17 "Tenancy", [decision 0021](decisions/0021-sandbox-time-quotas.md) |
| Keyless image signatures | `sandbox.image_signatures` verifies cosign key signatures (`.sig` tags) offline. Keyless (Fulcio certificates, Rekor) and the newer bundle-as-referrer format are not supported, and the verification here is for images that others sign. Our own release workflow now signs its images and charts keyless (cosign verifies them with `scripts/verify-release.sh`), and v0.1.0 and v0.2.0 were signed this way; the signatures are on GHCR, and a maintainer's run of `scripts/verify-release.sh` verified each. | §17 "Other security controls" |
| Enterprise-grade audit, and delegation-ready identity | Proposed in [decision 0026](decisions/0026-enterprise-audit-and-delegation.md), not built. Today the audit log covers mutating and denied calls; its write failures are logged and the call proceeds (fails open). Not built: a strict mode, read auditing, authentication context (issuer, `jti`, `kid`), initiator/executor/grant fields, configuration-change entries, a hash chain with signed checkpoints, guaranteed export (syslog, Object Lock) and legal holds. On-behalf-of token exchange (RFC 8693) is deliberately not built ([0015](decisions/0015-per-project-credentials.md)); 0026 reserves `subject`/`actor` fields so it can be added later per target. Review 2026-10-16: the first thing an enterprise buyer asks about; start with strict mode and read auditing. | §17 "Audit", decisions 0010, 0015, 0026 |
| Harness items from the LongHarness and Evo2 papers | Built: the `evidence-state` context strategy (H2), cost beside scores and tuned/held-out reporting in `evalsi matrix` (H3), and the scored `mechanism-ran` check for tool/context activity. Still open in [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md): live held-out/transfer experiments, long-document importers (OfficeQA Pro, AA-LCR; H4), the judge-based evidence-stage audit and semantic citation grounding (H5; a deterministic `evidence-stage-audit` is built, unreleased). Deterministic `citation-grounding` is built for explicit citations and exported evidence snapshots: it checks active versions and recorded verbatim reads, not semantic support or uncited free-text claims. The stage audit needs its judge checked against human labels first. Both papers are single-setup preprints, so their gains are unconfirmed for our tasks. Automated harness search and training an evolver are not planned. | HARNESS-ROADMAP |
| Feeding scores back into agentgateway | Quality-aware routing; depends on the gateway's extension points. | §16 "Working with agentgateway" |
| Separate process roles | Every `evalsid serve` replica serves API and ingest; leases pick the scheduler and policy engine. Split only if load needs it. | §23 Phase 4 deviations |
| Pod-rung snapshots | The pod rung cannot snapshot, so environment setup runs per trial. | §23 Phase 4 deviations |
| Firecracker egress through a tap device and nftables | Uses vsock and the host's egress proxy instead. Fine as is unless a workload needs raw networking. | §23 Phase 3 deviations |
| Per-caller rate limits (review 2026-10-16) | Built ([decision 0029](decisions/0029-per-caller-rate-limits.md)): a token bucket per caller (API key, user, or anonymous source address) and class (`api`, `ingest`, `guardrails`), per replica, on by default while authentication is enabled, with per-principal overrides. Refusals are `ResourceExhausted` or HTTP 429 with `Retry-After`, counted in `evalsi_rate_limited_total`. Tested in `internal/server/ratelimit_test.go` on Connect, gRPC, REST and OTLP/HTTP. Not covered: a limit shared across replicas, and live reload of the settings. | `internal/server/ratelimit.go`, [identity guide](guides/identity.md#7-audit-metrics-external-authorization) |
| Versioned, reversible schema migrations (review 2026-10-16) | `migrate()` rewrites tables in place and `sqldb.go` adds columns with `ALTER TABLE`: there is no migration table, no down step, and no rule for which changes are safe while N and N-1 run together. [Operations](guides/operations.md) says rollback is a restore into a fresh release and that rolling-upgrade compatibility has not passed its test. Needs a `schema_migrations` table with ordered steps, an expand-then-contract rule in CONTRIBUTING, and a CI test that runs N-1 against the N schema. A new bounded reader check passed: published v0.1.0 server/worker read 16 completed results and an archived summary from an isolated PR-schema database copy. It does not certify rolling dual-version writes ([record](verification/LEFTOVERS-2026-10-17.md)). | `internal/store/store.go`, `internal/store/sqldb.go` |
| A restore drill and point-in-time recovery (review 2026-10-16) | The manual backup and restore procedure is in [Operations](guides/operations.md), but a quiesced PostgreSQL/dataset restore into an empty namespace/database/bucket now passed authenticated reads and a 16-record rerun. A real WAL/base-backup drill recovered before a later committed row and promoted the recovery instance. Managed/off-site PITR, ClickHouse/NATS recovery and production RPO/RTO remain open ([record](verification/LEFTOVERS-2026-10-17.md)); WAL guidance is in Operations. | [Operations](guides/operations.md) |
| Judge robustness against injected text (review 2026-10-16) | Six live EKS/Anthropic positive, wrong-answer and injection controls passed explicit thresholds. Broader deployed-judge coverage and human-labelled calibration remain open. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | `python/evalsi/src/evalsi/packs/judge.py` |
| Web UI accessibility (review 2026-10-16) | Verified: the existing `ui-browser` job now includes axe-core 4.10.3. Five Chromium tests and 20 automated scans passed across light/dark navigation, sign-in and annotation, including Tab/Enter navigation. This closes automated-check coverage; a complete manual screen-reader audit is not claimed ([record](verification/LEFTOVERS-2026-10-17.md)). | `internal/webui/static`, [web UI guide](guides/web-ui.md) |
| Penetration test and security questionnaire pack (review 2026-10-16) | Needs an outside tester and evidence; [SECURITY.md](../SECURITY.md) covers disclosure only. Commission a test before the first enterprise evaluation and keep the answers, with evidence, in `docs/`. | [SECURITY.md](../SECURITY.md) |
| A second maintainer (review 2026-10-16) | `.github/CODEOWNERS` names one owner for every path, and says so. Add a second owner for `/.github/`, `/deploy/` and `SECURITY.md`, so release and review do not depend on one person. | `.github/CODEOWNERS` |

## Benchmark coverage gaps

| Item | Where recorded |
|------|----------------|
| BFCL memory and web-search categories (multi-turn now runs) | §23 Phase 3 deviations |
| τ-bench: tau2's own LiteLLM user. Evals.si's simulated user speaks through the run's judge; in telecom it follows tau2's user prompt and calls its tools through structured output rather than native tool calls. | §23 Phase 3 deviations |
| Multi-service/multi-stage Terminal-Bench coverage: the pod backend and a restricted Compose importer now support adapted multi-service tasks; the pinned upstream re-audit still imports 219/241 (12 original Compose tasks and 10 other tasks refused); an adapted upstream task is exercised separately, and multi-stage builds remain unsupported | §23 Phase 3 deviations |
| FrontierCode (task format not public in a verifiable form); CursorBench (tasks are private) | §23 Phase 3 deviations |

## Phase 5: fine-tuning and RL

| Item | Notes | Where recorded |
|------|-------|----------------|
| verl and OpenRLHF inside a training run | CPU CI tests the real reward managers/executor, not training. EKS now verified a native verl 0.9.1 GPU step and an **OpenRLHF 0.9.0** compatibility control: real completions/rewards, nonzero gradients and changed saved weights with zero weight decay. Current OpenRLHF 0.11.2 is **not certified**: its pinned vLLM/CUTLASS protobuf constraint requires a separate SDK reward process, and the user cancelled its long FlashAttention source build. The older control used its official pinned engine and checksum-verified prebuilt wheel, with HTTP rewards. No learning-quality or training-scale claim. | §23 Phase 5 limits, [GPU evidence](verification/EKS-GPU-2026-10-17.md), [fine-tuning guide](guides/fine-tuning.md#testing) |
| Reward throughput at the §12 scale | Measured on one 4-vCPU host with bubblewrap; the §12 figure (4,096 rollouts in about 16 s) assumes 256 warm microVMs. Needs a sandbox pool sized for it, and Firecracker warm pools on KVM. | §23 Phase 5 status |
| Operator-managed checkpoint serving | EKS actual L40S/vLLM model identity, inference, API-key rollout/deletion and source CLI watcher over two PVC checkpoints passed. Read-only controls, LoRA load/infer/unload and error cleanup passed; wrong-model launch and one-shot exit status fixed. Fixed image release and training quality remain separate. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | Decision 0013 |
| Checkpoint model serving | Plain PyTorch and Lightning save/evaluate hooks pass real CPU tests in separate environments. State dictionaries still need a compatible serving strategy; Lightning `.ckpt` files are not directly vLLM models. | [Part C](verification/PART-C.md), §12 |
| Environment-spec compatibility | Built for OpenEnv (`openenv-core` 0.3) and verifiers (0.3): `evalsi_harness.envspecs` exposes tasks as an OpenEnv `Environment` and a verifiers `StatefulToolEnv`, tested against the real packages in the `envspecs` CI job. Not covered: other specs (gymnasium, NeMo Gym), a training run that uses them (OpenEnv's or verifiers' own trainers), and sharing one environment setup across verifiers rollouts. | §12 |

## Phase 6: MCP, classic ML and ecosystem

| Item | Notes | Where recorded |
|------|-------|----------------|
| A coding agent product over MCP in CI | Actual EKS Claude CLI invoked source MCP over stdio; independent artifact checks confirmed mean 2/3 and failed gate. GitHub mcp-agent secret/first CI run, other coding products and external remote OAuth remain separate. See [EKS checkpoint](verification/EKS-HARNESS-2026-10-17.md) and [combined inventory](verification/LEFTOVERS-2026-10-17.md). | §23 Phase 6 limits, [MCP guide](guides/mcp.md#tested-with-a-real-coding-agent) |
| Wasm evaluators with a judge or the sandbox; an Azure ML trace source (its MLflow server has no V3 trace API); server-side SQL over runs (see below) | Wasm modules have no I/O by design; such evaluators stay Python plugins or images. | Decision 0014 |

## Thirteen-item implementation review

See [PR 101 implementation review](verification/STACK-101.md) for the completed reliability and robustness subset and explicit follow-ups. The new controls do not expand earlier live-model or cloud-provider verification claims.

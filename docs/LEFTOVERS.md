# Leftovers from Phases 0 to 6

These items were deferred, left unverified, or are described in [DESIGN.md](DESIGN.md) but not built. Each item names where it was recorded.

## Milestones

The roadmap's milestones and their exit criteria are in the [PRD](PRD.md#milestones-and-phased-rollout). Where they stand on 2026-10-13:

| Milestone | Planned | Status |
|-----------|---------|--------|
| M1 Release, SDK client | Oct to Nov 2026 | Done: v0.1.0 released and verified; v0.2.0 since |
| M2 agent harness online | Nov to Dec 2026 | In progress: a default policy and a gated deploy built on the demo and run by the kind e2e job; sign-off on the real agent harness needs its repository |
| M3 Pull connectors | Nov 2026 to Jan 2027 | Partly done: MLflow, Phoenix and Langfuse verified; Databricks and SageMaker not run live; a 30-day backfill and the pull lag are now measured (locally and in the kind e2e job), not yet at production scale |
| M4 Real-infra hardening | Jan to Feb 2027 | Not certified: bounded EKS checks passed; load, full upgrade and mesh enforcement remain |
| M5 More sources, SDK | Jan to Mar 2027 | SDK parts done; LangSmith source built and fixture-tested, not live-verified; ClickHouse, Kafka and S3 sources not built |
| GA with agent harness | 26 March 2027 | Not started |

The plan for strengthening the agent harness through M2 and GA is in [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md).

The latest release is **v0.2.0** (2026-10-13, [record](verification/README.md#v020-release-record-2026-10-13)); see [Releases](PRD.md#releases).

Last updated: 2026-10-13 (v0.2.0 released and verified; the two buildable rows built: verl and OpenRLHF reward paths and Claude Code over MCP in CI; backfill and lag measured; E6 imports); before that 2026-10-12 (bounded EKS verification, [report](verification/EKS-2026-10-12.md); server-side slicing, run archiving, environment specs, platform wheels, operator checkpoint serving and a hosted Wasm plugin built; the rest of server-side analytics closed as not planned; v0.1.0 released and its signatures verified; the two rows that waited on the tag are closed); before that 2026-10-11 (triage of every row, after the Part C review fixes; annotation claims, the UI browser test and per-release pool isolation); before that 2026-10-10 (trace sources) and 2026-10-09, after the [end-to-end verification](verification/README.md) on kind, AKS and an Istio ambient cluster with KVM.

Trace redaction on push and pull is now built and locally regression-tested,
including source overrides, storage bytes, evaluator queues, result callbacks
and error reporting. It has not had a new real-cluster redaction run. See
[decision 0019](decisions/0019-trace-redaction.md) for defaults and limitations;
historical stored data and direct Evaluate/offline run inputs are not redacted
by this incoming-trace boundary.

## Triage (2026-10-11)

34 rows are open (the ten rows under "Verified on real clusters" are done). Counts are recomputed from the item tables. Each is in exactly one
category, by what it needs next. The rows that can be checked against the code, the chart and the CI
workflow were; the rows that describe a real cluster are taken as written, since they need that
cluster to re-check.

| Category | Rows | What closes it |
|----------|------|----------------|
| **Buildable now** (0) | Both rows that were here are built (2026-10-13): verl and OpenRLHF reward paths in CI, and Claude Code driving `evalsi mcp`. What is left of each needs access, below | Code and tests, in this repository |
| **Needs infrastructure or access** (16) | verl and OpenRLHF inside a training run (GPUs); Claude Code over MCP in CI (an `ANTHROPIC_API_KEY` secret for its first run); Databricks (experiments and Unity Catalog), OAuth token acquisition and SageMaker against a real workspace and tracking server; Firecracker with the stock image; sandbox egress NetworkPolicy on Calico with Istio ambient; admission webhooks under STRICT mTLS; the `operator.hostNetwork` option on a cluster; PR #12 fixes on AKS and the Istio cluster; the reference demos' deep-research and Terminal-Bench 2 suites (a real model and judge); agent framework mapping profiles (real model output); 10k concurrent agent trials; real SWE-bench Verified images; Kata Containers; pipeline and schedule certification (live S3, PostgreSQL HA, production load); reward throughput at the §12 scale; operator-managed checkpoint serving on a GPU cluster | A sized or specific cluster, a model key, or access you control |
| **Known limit, not planned** (15) | User-namespaced bubblewrap pools (a host limit); cgroup v2 limits in an unprivileged container; the limits left after the reviews; sandbox-time quota coverage; keyless verification of sandbox images; separate process roles; pod-rung snapshots; Firecracker egress through a tap device; the four benchmark gaps (BFCL memory and web search, tau2's own user, multi-stage Terminal-Bench tasks, FrontierCode and CursorBench); Wasm evaluators with a judge or the sandbox; an Azure ML trace source (its MLflow server has no V3 trace API); server-side SQL over runs (see below) | Revisit only if a user needs it |
| **Needs a decision** (3) | Webhook URLs open to internal addresses (they are how the server reaches in-cluster services); feeding scores back into agentgateway (depends on its extension points); checkpoint model serving (which serving strategy to support) | A product choice, then it becomes buildable |

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
| User-namespaced bubblewrap pools (`mode: bwrap`, `hostUsers: false`) | Fails on real clusters: on AKS (Ubuntu 24.04) with `RTM_NEWADDR: Operation not permitted`, and where containerd's overlayfs lacks idmap mounts. `mode: privileged` works on both. The probe error now names `mode: privileged` and the pod rung, and the Kubernetes guide documents both cases; the failures themselves are the hosts'. | [verification](verification/README.md) (VR-D15) |
| Sandbox egress NetworkPolicy on Calico with Istio ambient | The operator's policy did not block a sandbox-labelled pod's egress on such a cluster; the cause is unknown. kind and the AKS cluster tested have no policy engine. The pod rung now verifies a declared policy with a canary connection and reports `partial` isolation and a probe warning when it is not enforced; that check has not run on the mesh cluster. | [verification](verification/README.md) |
| Admission webhooks on EKS with Calico's overlay | The control plane cannot dial overlay pod IPs (EKS-R1): the stock profile is refused. `operator.hostNetwork` (chart-tested) and the VPC CNI are the documented ways out; the host-network case passed on EKS with fail-closed handlers, but the chart option itself has not been run on a cluster. | [guide](guides/kubernetes.md), [report](verification/EKS-2026-10-12.md) |
| Admission webhooks under mesh-wide STRICT mTLS | `operator.istio.peerAuthentication` renders the PeerAuthentication (chart-tested); not re-run on a mesh cluster. | [verification](verification/README.md) (VR-R2) |
| PR #12 fixes on real clusters | The fixes for the verification findings have unit, chart and kind tests, but have not been re-run on AKS or the Istio cluster. | [verification](verification/README.md) |
| Trace sources (milestone M3) | Verified against MLflow 3.17.0/Phoenix 20.20.0 fixtures and live worker loops; R6's MLflow CRD/policy loop passed on kind with PostgreSQL source tables. Langfuse 4.56.0 passed in-cluster Secret/Service polling, score replacement and enforced egress. EKS real MLflow pull redacted API/storage/actual CPU work and wrote assessment feedback. Real Prometheus Operator loaded shipped rules; the unchanged 15-minute failure rule fired, delivered through local Alertmanager and resolved after repair with a fresh healthy scrape. Databricks experiments/Unity Catalog/OAuth and SageMaker SigV4 remain pinned-source/derived-fixture tested, not live-verified. A 30-day MLflow backfill and live pull lag are measured by `scripts/measure-trace-source.py` (locally: 500 traces backfilled in 5 s, live p95 15 s at a 15 s poll; the kind e2e job runs it on the demo MLflow). Not verified: those external providers, Langfuse Cloud/TLS/arm64, backfill and lag at production scale, provider UI visibility and external alert channels. Not built: Databricks table-prefix/experiment-name locations or ambient AWS SageMaker credentials; Azure ML V3 traces refused. A stored unscored trace is not replay-scored after restart. | [trace sources guide](guides/trace-sources.md), [decision 0016](decisions/0016-trace-source-connectors.md), [EKS methods](verification/EKS-2026-10-12.md) |
| The reference demos (`examples/demo`) | Both small-repo suites, MLflow sink and Deep Agents pull passed on kind. Native dsh headless GenAI export/pull/assessment also passed; interactive web export remains unverified. Deep-research and Terminal-Bench 2 need a real model/judge and have never run. | [Part C](verification/PART-C.md), [R1 to R9 in the PRD](PRD.md) |
| Frontier-model handling in the built-in agent (H1) | Built and tested over mock transports and a local server: thinking, the Responses API, streaming, prompt caching and cache-aware cost, `retry-after`, a retry after `max_tokens`, fallback models, a concurrency limit and cassette v2. Never run against a live Anthropic or OpenAI model, so request shapes (for example the Responses `include` and stateless reasoning items, and Anthropic's `adaptive` thinking) are unconfirmed, as is the exit test of a replaying `fix-calc` run with thinking. Needs model keys. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h1-frontier-model-support-m2-nov-2026) |
| Long-horizon agents (H2) | Context management (four strategies), the optional toolset, hooks, session resume and fork, sub-agents, tool search, the MCP upgrades and code mode are built and tested against fake models, local servers and a fake MCP server. Not run: any of it against a live model or a real tokenizer (the context tests use a fake model that reports the size of what it was sent, and the compaction and evidence quality is unmeasured); OAuth client credentials and elicitation against a real MCP server (the fake one follows the spec as read, not as implemented elsewhere); code mode with a real model (whether it writes scripts that work in the subset is unknown); `fresh-rounds` and `evidence-state` on a real long-document task. Resume was tested by a provider that starts failing, not by killing a worker. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h2-long-horizon-agents-m2-dec-2026) |
| Persistent background processes for the built-in agent (H2) | `bash` runs each command to completion: the sandbox launcher kills the command's process group, and the cgroup where there is one, when it returns, so a server or watcher started in one call is gone in the next. Sessions that persist across calls need an `evalsid` change (a long-lived shell per sandbox), not a harness one. Not built. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h2-long-horizon-agents-m2-dec-2026), `internal/sandbox/launch.go` |
| Agents under test (H3), in progress | Built: readers for Claude Code, Codex, Gemini CLI and pi event streams (`readers.py`); versioned harness profiles `minimal`, `standard` and `code-mode`; `evalsi matrix` (cost beside every score, paired differences with intervals, tuned and held-out cells); the agent's own setup as `environment.files` (documented and tested). **Not verified:** each reader was checked against the published type definitions of the package pinned in `readers.VERSIONS` (Claude's agent SDK 0.3.300, Codex SDK 0.162.1, Gemini CLI core 0.64.0, pi 0.73.1), but each fixture is written from those types, not captured from a CLI run, so the H3 exit test ("recorded fixtures") is open; run each CLI once with a key and replace the fixture. Codex reports usage per turn only, so its steps carry no per-call tokens, and Gemini's stream reports totals only. The profiles have never been run against a live model, so `minimal`'s prompt and tool set are untuned guesses at pi's, and no profile comparison has been measured. **Not built:** the ACP connector (ACP is JSON-RPC over the agent's stdio, and the permission requests must be answered while the agent waits, which needs an interactive, bidirectional exec in the sandbox; the sandbox runs a command to completion with its stdin written up front); A2A push notifications (`message/stream` is built, against a local server and not a real A2A agent); a run that sweeps harness configurations itself (`evalsi matrix` compares separate runs; the server has no sweep); the mechanism-ran check; the kind e2e job that runs one CLI agent end to end (needs a model key); handoff spans from A2A tasks and CLI readers beyond Claude's sub-agent parenting. | [HARNESS-ROADMAP](HARNESS-ROADMAP.md#h3-agents-under-test-jan-2027) |
| Agent framework mapping profiles | Each first-cut framework was recorded once, with the versions in `tests/frameworks/requirements-*.txt`, against the demo's mock model: one tool call, no parallel calls, no errors, no retries. Not recorded: real model output, a framework's default OpenAI Responses API (the OpenAI Agents fixture uses chat completions, since the mock has no Responses API), CrewAI through LiteLLM, streamed calls with usage, multi-agent crews and handoffs. A new framework version can change its spans; record again when upgrading. | [guide](guides/agent-frameworks.md), PRD scope table |
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
| Webhook URLs open to internal addresses | Run, trace and alert events are built ([webhooks guide](guides/webhooks.md)). Webhook URLs are not restricted to public addresses: the server dials whatever an admin sets, which is how it reaches in-cluster services. `trace.scored` has no filter of its own (a policy's selector and sampling bound it), and is not load-tested at the PRD's 10k traces/min. | P4 in the PRD |
| Server-side SQL over runs | Built: `evalsi analyze slice --on-server` computes a metric per run and metadata value on the server, over the runs the caller may read ([decision 0022](decisions/0022-server-side-slicing.md)). Arbitrary SQL stays in `evalsi analyze query`, on the client: run results are stored as protobuf, not columns, so there is nothing to run SQL on, and a SQL endpoint could not be scoped to the runs a caller may read. ClickHouse holds traces and their scores, not runs. | Decisions 0009, 0022 |
| Platform wheels bundling evalsid | Built ([decision 0024](decisions/0024-platform-wheels.md)): the release builds `evalsi` wheels for Linux amd64 and arm64 with `evalsid` and `bwrap` in `evalsi/_bin`, and CI builds one from the real archive, installs it and runs its sandbox. First published in v0.2.0: the arm64 wheel from PyPI installed in an arm64 Linux container and its bundled `evalsid` reported 0.2.0. Not covered: macOS and Windows (no archive; the sandbox is Linux-only). | Decision 0006 |
| Run archiving | Built: `ArchiveRun` (and `runs.archive_after`, `runs.project_archive_after`) keeps a run's summary after its records, outputs and results go, and automatic deletes and archives write audit events ([decision 0023](decisions/0023-run-archiving.md)). Authorized, audited terminal `DeleteRun` removes everything and recovers quotas. API authorization metadata remains for 24h retry checks after an explicit delete. External sink data and promoted datasets remain; deleting an EvalRun still keeps its server run. Not built: per-label retention rules, and the stored-run quota still counts archived runs. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), [0023](decisions/0023-run-archiving.md), F31 |
| Limits left after the reviews of items 3 to 6 and Part C | Everything those reviews found that had a safe code fix is fixed: scheduled-window skips name the traces, a lost schedule lease is reported as the cause, idle idempotency scopes are swept hourly, card numbers next to a CVV are hidden, `evalsi.log()` reports a failure once and bounds exit and fork behaviour, checkpoints take the umask's mode and only rank zero saves, SDK metadata is bounded, dataset-upload read errors are client errors and a stalled upload times out, and the dsh plugin closes unfinished turns. What remains: a keyed dataset upload is written to disk twice (spooled for the request fingerprint, then again by the dataset store) and takes an upload slot in both places; `evalsi.log()` does not retry a failed batch (its futures carry the error), and if retries are ever added they must send an `Idempotency-Key`; a scheduled window skips a trace a connector returns nothing for (deleted or empty), so a connector that returned a partial page without an error would lose traces from that window. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), [decision 0019](decisions/0019-trace-redaction.md) |
| Pipeline and schedule certification | Local API/worker tests cover upload, retry, deletion and scheduled reports. Live S3 imports, new-operation PostgreSQL/HA races, production load, MCP tool idempotency and scheduled source write-back remain uncertified or unsupported. | P6/P7/S9, decision 0020 |
| Sandbox-time quotas: what they do not cover | `sandbox_seconds_per_day` meters runs: agent tasks (start to end) and code evaluators that run in the sandbox pool. It does not meter online policy evaluation, guardrail checks, Wasm evaluators, reward-service rollouts or sandbox use outside a run, and a run's time is counted as it goes, so the run that crosses the line finishes its current batch. Concurrency quotas are per replica. | §17 "Tenancy", [decision 0021](decisions/0021-sandbox-time-quotas.md) |
| Keyless image signatures | `sandbox.image_signatures` verifies cosign key signatures (`.sig` tags) offline. Keyless (Fulcio certificates, Rekor) and the newer bundle-as-referrer format are not supported, and the verification here is for images that others sign. Our own release workflow now signs its images and charts keyless (cosign verifies them with `scripts/verify-release.sh`), and v0.1.0 and v0.2.0 were signed this way; the signatures are on GHCR, and a maintainer's run of `scripts/verify-release.sh` verified each. | §17 "Other security controls" |
| Enterprise-grade audit, and delegation-ready identity | Proposed in [decision 0026](decisions/0026-enterprise-audit-and-delegation.md), not built. Today the audit log covers mutating and denied calls; its write failures are logged and the call proceeds (fails open). Not built: a strict mode, read auditing, authentication context (issuer, `jti`, `kid`), initiator/executor/grant fields, configuration-change entries, a hash chain with signed checkpoints, guaranteed export (syslog, Object Lock) and legal holds. On-behalf-of token exchange (RFC 8693) is deliberately not built ([0015](decisions/0015-per-project-credentials.md)); 0026 reserves `subject`/`actor` fields so it can be added later per target. | §17 "Audit", decisions 0010, 0015, 0026 |
| Harness items from the LongHarness and Evo2 papers | Planned in [HARNESS-ROADMAP.md](HARNESS-ROADMAP.md), nothing built: an `evidence-state` context strategy (H2), cost beside every score and held-out/transfer runs (H3), long-document importers (OfficeQA Pro, AA-LCR; H4), an evidence-stage audit evaluator and citation grounding (H5). The stage audit needs its judge checked against human labels first. Both papers are single-setup preprints, so their gains are unconfirmed for our tasks. Automated harness search and training an evolver are not planned. | HARNESS-ROADMAP |
| Feeding scores back into agentgateway | Quality-aware routing; depends on the gateway's extension points. | §16 "Working with agentgateway" |
| Separate process roles | Every `evalsid serve` replica serves API and ingest; leases pick the scheduler and policy engine. Split only if load needs it. | §23 Phase 4 deviations |
| Pod-rung snapshots | The pod rung cannot snapshot, so environment setup runs per trial. | §23 Phase 4 deviations |
| Firecracker egress through a tap device and nftables | Uses vsock and the host's egress proxy instead. Fine as is unless a workload needs raw networking. | §23 Phase 3 deviations |

## Benchmark coverage gaps

| Item | Where recorded |
|------|----------------|
| BFCL memory and web-search categories (multi-turn now runs) | §23 Phase 3 deviations |
| τ-bench: tau2's own LiteLLM user. Evals.si's simulated user speaks through the run's judge; in telecom it follows tau2's user prompt and calls its tools through structured output rather than native tool calls. | §23 Phase 3 deviations |
| Multi-service and multi-stage Terminal-Bench tasks (22 of 241 Terminal-Bench 1 tasks do not import) | §23 Phase 3 deviations |
| FrontierCode (task format not public in a verifiable form); CursorBench (tasks are private) | §23 Phase 3 deviations |

## Phase 5: fine-tuning and RL

| Item | Notes | Where recorded |
|------|-------|----------------|
| verl and OpenRLHF inside a training run | Their reward paths run in CI against the real packages (the `rlframeworks` job): verl 0.9.1 loads the reward through `custom_reward_function` and its naive and batch reward managers score rollouts with it; OpenRLHF 0.11.2's rollout executor calls it as a `--remote_rm_url` file and over HTTP. Not run: a training step in either (GPUs and an inference engine; OpenRLHF is installed without flash-attn and deepspeed). TRL is tested with a real `GRPOTrainer`. | §23 Phase 5 limits, [fine-tuning guide](guides/fine-tuning.md#testing) |
| Reward throughput at the §12 scale | Measured on one 4-vCPU host with bubblewrap; the §12 figure (4,096 rollouts in about 16 s) assumes 256 warm microVMs. Needs a sandbox pool sized for it, and Firecracker warm pools on KVM. | §23 Phase 5 status |
| Operator-managed checkpoint serving | Built ([decision 0025](decisions/0025-operator-checkpoint-serving.md)): the `CheckpointServer` resource runs vLLM, and `evalsi checkpoints --serve operator` creates one per checkpoint. Tested with a fake client and a fake `kubectl`; never run on a cluster or with a real vLLM and GPU (listed under "Needs infrastructure"). | Decision 0013 |
| Checkpoint model serving | Plain PyTorch and Lightning save/evaluate hooks pass real CPU tests in separate environments. State dictionaries still need a compatible serving strategy; Lightning `.ckpt` files are not directly vLLM models. | [Part C](verification/PART-C.md), §12 |
| Environment-spec compatibility | Built for OpenEnv (`openenv-core` 0.3) and verifiers (0.3): `evalsi_harness.envspecs` exposes tasks as an OpenEnv `Environment` and a verifiers `StatefulToolEnv`, tested against the real packages in the `envspecs` CI job. Not covered: other specs (gymnasium, NeMo Gym), a training run that uses them (OpenEnv's or verifiers' own trainers), and sharing one environment setup across verifiers rollouts. | §12 |

## Phase 6: MCP, classic ML and ecosystem

| Item | Notes | Where recorded |
|------|-------|----------------|
| A coding agent product over MCP in CI | Built: the `mcp-agent` job has Claude Code (headless) run a spec through `evalsi mcp` and checks the tool call, the results file and the reported score. It runs on `main` and on demand when the repository has an `ANTHROPIC_API_KEY` secret, and has not run yet: the secret is not set. Cursor and other agents are not tested. | §23 Phase 6 limits, [MCP guide](guides/mcp.md#tested-with-a-real-coding-agent) |
| Wasm evaluators with a judge or the sandbox; an Azure ML trace source (its MLflow server has no V3 trace API); server-side SQL over runs (see below) | Wasm modules have no I/O by design; such evaluators stay Python plugins or images. | Decision 0014 |


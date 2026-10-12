# Leftovers from Phases 0 to 6

These items were deferred, left unverified, or are described in [DESIGN.md](DESIGN.md) but not built. Each item names where it was recorded.

Last updated: 2026-10-12 (v0.1.0 released; the two rows that waited on the tag are closed or narrowed); before that 2026-10-11 (triage of every row, after the Part C review fixes; annotation claims, the UI browser test and per-release pool isolation); before that 2026-10-10 (trace sources) and 2026-10-09, after the [end-to-end verification](verification/README.md) on kind, AKS and an Istio ambient cluster with KVM.

Trace redaction on push and pull is now built and locally regression-tested,
including source overrides, storage bytes, evaluator queues, result callbacks
and error reporting. It has not had a new real-cluster redaction run. See
[decision 0019](decisions/0019-trace-redaction.md) for defaults and limitations;
historical stored data and direct Evaluate/offline run inputs are not redacted
by this incoming-trace boundary.

## Triage (2026-10-11)

44 rows are open (the four rows under "Verified on real clusters" are done). Each is in exactly one
category, by what it needs next. The rows that can be checked against the code, the chart and the CI
workflow were; the rows that describe a real cluster are taken as written, since they need that
cluster to re-check.

| Category | Rows | What closes it |
|----------|------|----------------|
| **Buildable now** (13) | Bundled ClickHouse and a second release (the kind job installs neither); server-side analytics; platform wheels bundling evalsid; archiving and automatic run retention; sandbox-time quotas; verl and OpenRLHF against real installs (a CI job like `trainers`); operator-managed checkpoint serving; environment-spec compatibility; a coding agent product over MCP in CI; a real mounted-plugin evaluator run on kind; hosted Wasm plugins in the index; browser sign-in for the web UI; annotating in the browser | Code and tests, in this repository |
| **Needs infrastructure or access** (16) | Databricks (experiments and Unity Catalog), OAuth token acquisition and SageMaker against a real workspace and tracking server; Firecracker with the stock image; sandbox egress NetworkPolicy on Calico with Istio ambient; admission webhooks under STRICT mTLS; PR #12 fixes on AKS and the Istio cluster; the reference demos' deep-research and Terminal-Bench 2 suites (a real model and judge); agent framework mapping profiles (real model output); KEDA on a live pool; several-node NATS; 10k concurrent agent trials; real SWE-bench Verified images; Kata Containers; pipeline and schedule certification (live S3, PostgreSQL HA, production load); reward throughput at the §12 scale; guardrails behind a real agentgateway; per-release sandbox-pool isolation on a cluster that enforces NetworkPolicy | A sized or specific cluster, a model key, or access you control |
| **Known limit, not planned** (12) | User-namespaced bubblewrap pools (a host limit); cgroup v2 limits in an unprivileged container; the limits left after the reviews; separate process roles; pod-rung snapshots; Firecracker egress through a tap device; the four benchmark gaps (BFCL memory and web search, tau2's own user, multi-stage Terminal-Bench tasks, FrontierCode and CursorBench); Wasm evaluators with a judge or the sandbox; an Azure ML trace source (its MLflow server has no V3 trace API) | Revisit only if a user needs it |
| **Needs a decision** (3) | Webhook URLs open to internal addresses (they are how the server reaches in-cluster services); feeding scores back into agentgateway (depends on its extension points); checkpoint model serving (which serving strategy to support) | A product choice, then it becomes buildable |
| **Release follow-ups** (1) | Our own keyless signatures: published for v0.1.0 and present on GHCR for every image and chart, but `scripts/verify-release.sh 0.1.0` has not been run by a maintainer (the check needs Sigstore's trust root; see the [v0.1.0 record](verification/README.md#v010-release-record-2026-10-12)). Verifying other people's keyless images is a separate, buildable gap | A maintainer running `scripts/verify-release.sh 0.1.0` |

## Verified only with stand-ins (needs real infrastructure)

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
| Admission webhooks under mesh-wide STRICT mTLS | `operator.istio.peerAuthentication` renders the PeerAuthentication (chart-tested); not re-run on a mesh cluster. | [verification](verification/README.md) (VR-R2) |
| PR #12 fixes on real clusters | The fixes for the verification findings have unit, chart and kind tests, but have not been re-run on AKS or the Istio cluster. | [verification](verification/README.md) |
| Bundled ClickHouse (`devClickhouse`) and the Helm charts under a second release | Chart-tested, not verified on a cluster. Bundled MinIO and demo dataset upload passed on local kind, as did bootstrap key authentication and survival across an upgrade. | [D2 and D4 in the PRD](PRD.md), decision 0017 |
| Trace sources (milestone M3) | Verified here against MLflow 3.17.0 and Phoenix 20.20.0 servers (recorded fixtures, live connector tests, and the full loop with the Python worker). R6's MLflow TraceSource CRD and policy loop passed on local kind with Postgres-backed source tables. Langfuse 4.56.0 passed the two-stage check, including in-cluster Secret/Service polling, score replacement and enforced evalsid egress. Databricks numeric experiment/bearer-token artifact downloads are built and contract-tested against pinned client source, not live-verified. Databricks Unity Catalog schemas (V4 search and assessments), Databricks OAuth service principals and SageMaker (SigV4) are built from pinned client source and tested against derived fixtures (`internal/source/mlflow/testdata/derived`), not run against a Databricks workspace or a SageMaker tracking server. Not verified: Langfuse Cloud/TLS/arm64 images and PrometheusRule behaviour on a cluster. Not built: Databricks table-prefix locations and experiment names, ambient AWS credentials for SageMaker; refused: Azure ML (the `azureml-mlflow` 1.62.0 client says "V3 API not implemented yet"). A trace stored but not yet scored when the server stops is not scored after the restart. | [trace sources guide](guides/trace-sources.md), [decision 0016](decisions/0016-trace-source-connectors.md) |
| The reference demos (`examples/demo`) | Both small-repo suites, MLflow sink and Deep Agents pull passed on kind. Native dsh headless GenAI export/pull/assessment also passed; interactive web export remains unverified. Deep-research and Terminal-Bench 2 need a real model/judge and have never run. | [Part C](verification/PART-C.md), [R1 to R9 in the PRD](PRD.md) |
| Agent framework mapping profiles | Each first-cut framework was recorded once, with the versions in `tests/frameworks/requirements-*.txt`, against the demo's mock model: one tool call, no parallel calls, no errors, no retries. Not recorded: real model output, a framework's default OpenAI Responses API (the OpenAI Agents fixture uses chat completions, since the mock has no Responses API), CrewAI through LiteLLM, streamed calls with usage, multi-agent crews and handoffs. A new framework version can change its spans; record again when upgrading. | [guide](guides/agent-frameworks.md), PRD scope table |
| KEDA scaling a live pool | Only the generated `ScaledObject`s are tested (envtest, chart tests). | §23 Phase 4 limits |
| Several-node NATS (stream replicas 3) | Configuration only; tests use one node. | §23 Phase 4 limits |
| 10k+ concurrent agent trials (§14 target) | Needs a cluster sized for it. The in-cluster load test on the Istio cluster reached 1,220 Evaluate calls/s (p95 24 ms), 387k OTLP spans/s and a 1,000-record run at 3,985 records/s; concurrent agent trials were not load-tested. | §23 Phase 4 load test, [verification](verification/README.md) |
| Real SWE-bench Verified images | Several GB each; CI runs fixtures in the same format, graded by the same code. The verification ran the fixtures on the pod, bubblewrap and Firecracker rungs with identical results, but no real task. | §23 Phase 3 limits |
| cgroup v2 limits in an unprivileged container | Tested as root on CI's cgroup v2. In a pod, the cgroup mount is read-only unless the pod is privileged (as the bubblewrap pool is), so other deployments fall back to rlimits. | §13 implementation notes |
| Kata Containers as the `vm` level on the owner's cluster (D13) | Supported as a pod `runtimeClassName`; not run on a cluster with Kata installed. | D13, decision 0007 |

## Verified on real clusters (2026-10-09)

These were verified only with stand-ins before the [verification run](verification/README.md), which used commit `c5a1de9`.

| Item | Result |
|------|--------|
| Firecracker rung on a KVM host, `evalsi-sandboxd` with `mode: firecracker` | Code evaluation and the SWE-bench fixture ran in microVMs (`ISOLATION_LEVEL_VM`), with the same results as the pod and bubblewrap rungs |
| HA: a replica that owns a run is killed | Another replica adopted the run; every result was stored once |
| Namespace-only install under Pod Security `restricted` | Installed and ran code evaluations on kind, AKS and the Istio cluster (pod rung and Landlock). Agent runs on the pod rung passed on kind and AKS. On the Istio cluster they failed at the waypoint (VR-R1); pod labels to opt out now exist |
| TRL GRPO with Evals.si rewards | Passed on CPU on kind and AKS |

## Deferred features

| Item | Notes | Where recorded |
|------|-------|----------------|
| Webhook URLs open to internal addresses | Run, trace and alert events are built ([webhooks guide](guides/webhooks.md)). Webhook URLs are not restricted to public addresses: the server dials whatever an admin sets, which is how it reaches in-cluster services. `trace.scored` has no filter of its own (a policy's selector and sampling bound it), and is not load-tested at the PRD's 10k traces/min. | P4 in the PRD |
| Per-release isolation of sandbox pools | Built: each release's pool pods are selected by a policy that names only its own pool, and the general policy covers only pods with no pool label (the pods of shared SandboxClass pools). Chart-tested with two releases; not verified on a cluster that enforces NetworkPolicy, which kind does not. SandboxClass pools stay shared by design. | Decision 0017 |
| Server-side analytics | `evalsi analyze` loads runs into DuckDB on the client, through the API. It reads every result of the runs it loads, so analysis over thousands of large runs would want a server-side query (ClickHouse already holds the data on Kubernetes). | Decision 0009 |
| Platform wheels bundling evalsid | Releases publish archives (evalsid, a static bubblewrap), wheels, images, charts and the bundle (`.github/workflows/release.yml`), v0.1.0 was cut with these (see the [release record](verification/README.md#v010-release-record-2026-10-12)), but the wheels do not bundle evalsid as decision 0006 suggests. | Decision 0006 |
| Archiving and automatic run retention | Authorized, audited terminal `DeleteRun` now removes stored records, outputs, results and webhook deliveries and recovers quotas. API authorization metadata remains for 24h retry checks. External sink data and promoted datasets remain; deleting an EvalRun still keeps its server run. Archive and automatic retention are not built. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), F31 |
| Limits left after the reviews of items 3 to 6 and Part C | Everything those reviews found that had a safe code fix is fixed: scheduled-window skips name the traces, a lost schedule lease is reported as the cause, idle idempotency scopes are swept hourly, card numbers next to a CVV are hidden, `evalsi.log()` reports a failure once and bounds exit and fork behaviour, checkpoints take the umask's mode and only rank zero saves, SDK metadata is bounded, dataset-upload read errors are client errors and a stalled upload times out, and the dsh plugin closes unfinished turns. What remains: a keyed dataset upload is written to disk twice (spooled for the request fingerprint, then again by the dataset store) and takes an upload slot in both places; `evalsi.log()` does not retry a failed batch (its futures carry the error), and if retries are ever added they must send an `Idempotency-Key`; a scheduled window skips a trace a connector returns nothing for (deleted or empty), so a connector that returned a partial page without an error would lose traces from that window. | [Decision 0020](decisions/0020-pipeline-retries-and-scheduled-runs.md), [decision 0019](decisions/0019-trace-redaction.md) |
| Pipeline and schedule certification | Local API/worker tests cover upload, retry, deletion and scheduled reports. Live S3 imports, new-operation PostgreSQL/HA races, production load, MCP tool idempotency and scheduled source write-back remain uncertified or unsupported. | P6/P7/S9, decision 0020 |
| Sandbox-time quotas | Per-project quotas cover concurrent and stored runs, daily judge and target tokens, and reward rollouts in flight; sandbox minutes are not metered. Concurrency quotas are per replica. | §17 "Tenancy" |
| Keyless image signatures | `sandbox.image_signatures` verifies cosign key signatures (`.sig` tags) offline. Keyless (Fulcio certificates, Rekor) and the newer bundle-as-referrer format are not supported, and the verification here is for images that others sign. Our own release workflow now signs its images and charts keyless (cosign verifies them with `scripts/verify-release.sh`), and v0.1.0 is the first release signed this way; the signatures are on GHCR, and a maintainer has yet to run `scripts/verify-release.sh 0.1.0`. | §17 "Other security controls" |
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
| verl and OpenRLHF against real installs | Their entry points (`compute_score`, `compute_score_batch`, `reward_func`, the remote reward-model server) are tested by signature and protocol, not inside a verl or OpenRLHF training run. TRL is tested with a real `GRPOTrainer`. | §23 Phase 5 limits |
| Reward throughput at the §12 scale | Measured on one 4-vCPU host with bubblewrap; the §12 figure (4,096 rollouts in about 16 s) assumes 256 warm microVMs. Needs a sandbox pool sized for it, and Firecracker warm pools on KVM. | §23 Phase 5 status |
| Operator-managed checkpoint serving | The operator does not start vLLM per checkpoint; the watcher serves checkpoints itself (vLLM process, LoRA hot-load, endpoint). | Decision 0013 |
| Checkpoint model serving | Plain PyTorch and Lightning save/evaluate hooks pass real CPU tests in separate environments. State dictionaries still need a compatible serving strategy; Lightning `.ckpt` files are not directly vLLM models. | [Part C](verification/PART-C.md), §12 |
| Environment-spec compatibility | `TaskEnv` has its own `reset`/`step`; compatibility with OpenEnv and verifiers-style environments is not evaluated. | §12 |

## Phase 6: MCP, classic ML and ecosystem

| Item | Notes | Where recorded |
|------|-------|----------------|
| Guardrails against a live agentgateway | The prompt-guard webhook and ExtMcp protocols are implemented from agentgateway's source and tested against their wire formats; neither was run behind a real gateway. Masks on streamed responses appear not to take effect in agentgateway (blocks do). | §23 Phase 6 limits, guardrails guide |
| A coding agent product over MCP in CI | The exit test drives `evalsi mcp` with our own MCP client, not with Claude Code, Cursor or another agent. | §23 Phase 6 limits |
| Mounted plugins on Kubernetes | Helm volume/init-container, Wasm directory and PYTHONPATH wiring are chart-tested. A real mounted-plugin evaluator run is not yet certified. | [Plugins guide](guides/plugins.md#kubernetes-mounts) |
| Wasm evaluators with a judge or the sandbox; an Azure ML trace source (its MLflow server has no V3 trace API) | Wasm modules have no I/O by design; such evaluators stay Python plugins or images. | Decision 0014 |
| Hosted Wasm plugins in the index | The index lists the native packs and the adapters; no Wasm plugin is published yet (the example builds from source). | Plugins guide |
| Browser sign-in for the web UI | The UI takes a pasted API key or `evalsi auth token`; an OIDC login flow in the browser is not built. | Decision 0014 |
| Annotating in the browser | The UI is read-only; annotators use `evalsi annotate start` or the API. | Decision 0014 |


# Leftovers from Phases 0 to 6

These items were deferred, left unverified, or are described in [DESIGN.md](DESIGN.md) but not built. Each item names where it was recorded.

Last updated: 2026-10-10 (trace sources); before that 2026-10-09, after the [end-to-end verification](verification/README.md) on kind, AKS and an Istio ambient cluster with KVM.

## Verified only with stand-ins (needs real infrastructure)

| Item | Why it is open | Where recorded |
|------|----------------|----------------|
| Firecracker with the stock image and chart values | The rung itself is verified (see below), but that run used a patched ConfigMap and a derived image. The fixes that make the stock image and `firecracker.defaultImage` work (PR #12) have not been re-run on a KVM cluster. | [verification](verification/README.md) |
| User-namespaced bubblewrap pools (`mode: bwrap`, `hostUsers: false`) | Fails on real clusters: on AKS (Ubuntu 24.04) with `RTM_NEWADDR: Operation not permitted`, and where containerd's overlayfs lacks idmap mounts. `mode: privileged` works on both. The probe error now names `mode: privileged` and the pod rung, and the Kubernetes guide documents both cases; the failures themselves are the hosts'. | [verification](verification/README.md) (VR-D15) |
| Sandbox egress NetworkPolicy on Calico with Istio ambient | The operator's policy did not block a sandbox-labelled pod's egress on such a cluster; the cause is unknown. kind and the AKS cluster tested have no policy engine. The pod rung now verifies a declared policy with a canary connection and reports `partial` isolation and a probe warning when it is not enforced; that check has not run on the mesh cluster. | [verification](verification/README.md) |
| Admission webhooks under mesh-wide STRICT mTLS | `operator.istio.peerAuthentication` renders the PeerAuthentication (chart-tested); not re-run on a mesh cluster. | [verification](verification/README.md) (VR-R2) |
| PR #12 fixes on real clusters | The fixes for the verification findings have unit, chart and kind tests, but have not been re-run on AKS or the Istio cluster. | [verification](verification/README.md) |
| Bundled MinIO and ClickHouse (`devMinio`, `devClickhouse`), the bootstrap Job, and the Helm charts under a second release | Rendered and checked by the chart tests (and the bootstrap logic by unit tests with a fake server and Secrets), but not run on a cluster: the environment that built them had no Docker daemon for kind. `deploy/e2e/kind-e2e.sh` has a bootstrap step (a key from values survives an upgrade) that CI will run. | [D2 and D4 in the PRD](PRD.md), decision 0017 |
| Redaction of traces on ingest and on pull | The PRD's security section says redaction rules apply on ingest and pull, before storage and before any judge call; evalsid has none for traces (guardrails redact inline, at a gateway). Pushed and pulled traces are stored as sent. | [decision 0016, as built](decisions/0016-trace-source-connectors.md#as-built-2026-10-10) |
| Trace sources (milestone M3) | Verified here against MLflow 3.17.0 and Phoenix 20.20.0 servers (recorded fixtures, live connector tests, and the full loop with the Python worker). Not verified: Langfuse (built and tested from its pinned OpenAPI spec only; whether a score sent again under the same ID replaces the old one), the Databricks, SageMaker and Azure ML variants (refused as not built), the TraceSource CRD on a real cluster (envtest only), Postgres for the source tables (CI runs the store tests on Postgres), the egress NetworkPolicy and PrometheusRule on a cluster (chart tests only), and the kind e2e step for R6. A trace stored but not yet scored when the server stops is not scored after the restart. | [trace sources guide](guides/trace-sources.md), [decision 0016](decisions/0016-trace-source-connectors.md) |
| The reference demos (`examples/demo`) | Run end to end here only in embedded and standalone mode with the mock model. Not run: the demo images' builds, the demo chart and its datasets Job (`evalsid datasets put` to S3 is tested against MinIO in CI only), the MLflow sink against a live MLflow server, and the kind e2e step that runs the small-repo-fix suite for both agents. The deep-research and Terminal-Bench 2 suites need a real model and a judge and have never run. R6's MLflow half runs here (the real service and MLflow 3.17.0); its kind e2e step has not run, and the dsh plugin that exports sessions as OTel GenAI spans is not built. | [R1 to R9 in the PRD](PRD.md) |
| `release.yml` signing, SBOM and chart push | Written and checked as YAML only; the first tag exercises it (`scripts/verify-release.sh` then checks the result). | D1 in the PRD |
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
| Webhooks for scored traces | `run.finished` and `run.gate_failed` are built ([webhooks guide](guides/webhooks.md)); a per-trace event is not (online policies keep their own alert webhooks). Webhook URLs are not restricted to public addresses: the server dials whatever an admin sets, which is how it reaches in-cluster services. | P4 in the PRD |
| Per-release isolation of sandbox pools | The sandbox NetworkPolicy selects every pool in the namespace by the shared `evalsi-sandboxd` component label, so two releases' pools can reach each other's sandbox pods. | Decision 0017 |
| Server-side analytics | `evalsi analyze` loads runs into DuckDB on the client, through the API. It reads every result of the runs it loads, so analysis over thousands of large runs would want a server-side query (ClickHouse already holds the data on Kubernetes). | Decision 0009 |
| Platform wheels bundling evalsid | Releases publish archives (evalsid, a static bubblewrap), wheels, images, charts and the bundle (`.github/workflows/release.yml`), but no tag has been cut (the signing and SBOM steps have never run), and the wheels do not bundle evalsid as decision 0006 suggests. | Decision 0006 |
| Sandbox-time quotas | Per-project quotas cover concurrent and stored runs, daily judge and target tokens, and reward rollouts in flight; sandbox minutes are not metered. Concurrency quotas are per replica. | §17 "Tenancy" |
| Keyless image signatures | `sandbox.image_signatures` verifies cosign key signatures (`.sig` tags) offline. Keyless (Fulcio certificates, Rekor) and the newer bundle-as-referrer format are not supported, and the verification here is for images that others sign. Our own release workflow now signs its images and charts keyless (cosign verifies them with `scripts/verify-release.sh`), but it has never run: no tag has been cut. | §17 "Other security controls" |
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
| Trainer hooks besides Hugging Face | Only `TrainerCallback` (Trainer, TRL). Other loops use `CheckpointRunner`, or the watcher with `--stop-file`; Lightning `.ckpt` files need converting before vLLM can serve them. | §12 |
| Environment-spec compatibility | `TaskEnv` has its own `reset`/`step`; compatibility with OpenEnv and verifiers-style environments is not evaluated. | §12 |

## Phase 6: MCP, classic ML and ecosystem

| Item | Notes | Where recorded |
|------|-------|----------------|
| Guardrails against a live agentgateway | The prompt-guard webhook and ExtMcp protocols are implemented from agentgateway's source and tested against their wire formats; neither was run behind a real gateway. Masks on streamed responses appear not to take effect in agentgateway (blocks do). | §23 Phase 6 limits, guardrails guide |
| A coding agent product over MCP in CI | The exit test drives `evalsi mcp` with our own MCP client, not with Claude Code, Cursor or another agent. | §23 Phase 6 limits |
| Wasm plugins on Kubernetes | evalsid loads plugins from `wasm.plugin_dirs`; the Helm chart has no value to mount them (a volume, or an init container that runs `evalsi plugins install`). | Plugins guide |
| Wasm evaluators with a judge or the sandbox | Wasm modules have no I/O by design; such evaluators stay Python plugins or images. | Decision 0014 |
| Hosted Wasm plugins in the index | The index lists the native packs and the adapters; no Wasm plugin is published yet (the example builds from source). | Plugins guide |
| Browser sign-in for the web UI | The UI takes a pasted API key or `evalsi auth token`; an OIDC login flow in the browser is not built. | Decision 0014 |
| Annotating in the browser | The UI is read-only; annotators use `evalsi annotate start` or the API. | Decision 0014 |
| A browser test of the UI in CI | Go tests cover serving, headers and text-only rendering; the pages were checked in Chromium by hand. | §23 Phase 6 limits |
| Annotation claims under concurrent Postgres writers | Two annotators claiming the last slot at the same moment can over-claim an item, which costs at most one extra answer. | `internal/store/annotations.go` |


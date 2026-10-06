# Leftovers from Phases 0 to 5

These items were deferred, left unverified, or are described in [DESIGN.md](DESIGN.md) but not built. Phase 6 scope (`evalsi mcp`, classic ML packs, the plugin index, Wasm evaluators, annotation queues, inline guardrails, a web UI) is not listed here; see §23. Each item names where it was recorded.

Last updated: 2026-10-06, at the end of Phase 5.

## Verified only with stand-ins (needs real infrastructure)

| Item | Why it is open | Where recorded |
|------|----------------|----------------|
| Firecracker rung on a KVM host | CI and development hosts have no KVM. Tests use a stand-in `firecracker` (`internal/sandbox/testdata/fakefc`) that runs the real guest agent without isolation. | §23 Phase 3 limits |
| `evalsi-sandboxd` with `mode: firecracker` on Kubernetes | Rendered and its config validated, never run. The sandbox-lease numbers in the load test are bubblewrap's. | §23 Phase 4 limits |
| User-namespaced bubblewrap pools (`mode: bwrap`, `hostUsers: false`) | kind nodes are containers, so the kind job runs the pool privileged. | §23 Phase 4 limits |
| KEDA scaling a live pool | Only the generated `ScaledObject`s are tested (envtest, chart tests). | §23 Phase 4 limits |
| Several-node NATS (stream replicas 3) | Configuration only; tests use one node. | §23 Phase 4 limits |
| 10k+ concurrent agent trials (§14 target) | Needs a cluster sized for it. | §23 Phase 4 load test |
| Real SWE-bench Verified images | Several GB each; CI runs fixtures in the same format, graded by the same code. | §23 Phase 3 limits |
| Kata Containers as the `vm` level on the owner's cluster (D13) | Supported as a pod `runtimeClassName`; not run on a cluster with Kata installed. | D13, decision 0007 |

## Deferred features

| Item | Notes | Where recorded |
|------|-------|----------------|
| DuckDB for standalone analytics | Waits for cross-run analytics. | Decision 0009 |
| cgroup limits in the bubblewrap and Landlock rungs | rlimits only. On the Landlock rung the process cap is not enforced, because the uid is shared. | §23 Phase 1 deferrals |
| Statically built bubblewrap in the release | Waits for a release pipeline; the rung uses the host's `bwrap`. | §13, §23 Phase 1 deferrals |
| Release pipeline | No published wheels, images or charts; platform wheels bundling `evalsid` are an idea in decision 0006. | Decision 0006 |
| Per-project quotas | Concurrent tasks, sandbox minutes, judge tokens and storage (§17 "Tenancy"). Nothing enforces them yet. | §17, §23 Phase 2 deferrals |
| Reserved `tenant_id` on stored keys | §16 and D5 say every stored key carries it; the store has `project_id` only. Add it, or correct the docs, before data volumes grow. | §16, D5 |
| Image signature verification (cosign) | Plugin and environment images are pinned by digest only. | §17 "Other security controls" |
| `evalsi-collector` distribution | An OTel Collector build with our exporters. | §21 |
| Reports and dashboards | Static HTML or Markdown reports, Grafana dashboards, and write-back to Langfuse and Phoenix (MLflow and OTel sinks exist). | §19, D3 |
| Feeding scores back into agentgateway | Quality-aware routing; depends on the gateway's extension points. | §16 "Working with agentgateway" |
| Separate process roles | Every `evalsid serve` replica serves API and ingest; leases pick the scheduler and policy engine. Split only if load needs it. | §23 Phase 4 deviations |
| Typed `EvalRun` and `OnlineEvalPolicy` CRD schemas | Specs are preserved as is; the admission webhook validates them. | §23 Phase 4 deviations |
| Pod-rung snapshots | The pod rung cannot snapshot, so environment setup runs per trial. | §23 Phase 4 deviations |
| Firecracker egress through a tap device and nftables | Uses vsock and the host's egress proxy instead. Fine as is unless a workload needs raw networking. | §23 Phase 3 deviations |

## Benchmark coverage gaps

| Item | Where recorded |
|------|----------------|
| BFCL multi-turn, memory and web-search categories | §23 Phase 3 deviations |
| τ-bench telecom domain; tau2's own LiteLLM user (we use Evals.si's simulated user through the run's judge) | §23 Phase 3 deviations |
| Multi-service and multi-stage Terminal-Bench tasks (22 of 241 Terminal-Bench 1 tasks do not import) | §23 Phase 3 deviations |
| FrontierCode (task format not public in a verifiable form); CursorBench (tasks are private) | §23 Phase 3 deviations |

## Kubernetes follow-ups from the RBAC work

| Item | Notes |
|------|-------|
| Static issuer and JWKS on hardened clusters | Implemented and unit-tested; the kind e2e uses the API-server path only. |

## Phase 5: fine-tuning and RL

| Item | Notes | Where recorded |
|------|-------|----------------|
| verl and OpenRLHF against real installs | Their entry points (`compute_score`, `compute_score_batch`, `reward_func`, the remote reward-model server) are tested by signature and protocol, not inside a verl or OpenRLHF training run. TRL is tested with a real `GRPOTrainer`. | §23 Phase 5 limits |
| Reward throughput at the §12 scale | Measured on one 4-vCPU host with bubblewrap; the §12 figure (4,096 rollouts in about 16 s) assumes 256 warm microVMs. Needs a sandbox pool sized for it, and Firecracker warm pools on KVM. | §23 Phase 5 status |
| Operator-managed checkpoint serving | The operator does not start vLLM per checkpoint; the watcher serves checkpoints itself (vLLM process, LoRA hot-load, endpoint). | Decision 0013 |
| Checkpoint sources | Local directories, and `s3://`, `gs://`, `hf://` through fsspec. No MLflow Model Registry watcher. | §12 |
| Trainer hooks besides Hugging Face | Only `TrainerCallback` (Trainer, TRL). Lightning and verl hooks would use `CheckpointRunner` the same way. | §12 |
| Batched served reward models | `reward-model` calls the endpoint once per rollout; no batching across rollouts. | §12 |
| Shared reward cache | The Reward Service cache is in memory per replica. | §23 Phase 5 status |
| Environment-spec compatibility | `TaskEnv` has its own `reset`/`step`; compatibility with OpenEnv and verifiers-style environments is not evaluated. | §12 |
| Early stop outside the callback | Regression gates stop training only through the Hugging Face callback; the watcher reports but cannot signal the trainer. | §12 |

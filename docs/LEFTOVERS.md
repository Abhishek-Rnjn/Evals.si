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
| cgroup v2 limits in an unprivileged container | Tested as root on CI's cgroup v2. In a pod, the cgroup mount is read-only unless the pod is privileged (as the bubblewrap pool is), so other deployments fall back to rlimits. | §13 implementation notes |
| Kata Containers as the `vm` level on the owner's cluster (D13) | Supported as a pod `runtimeClassName`; not run on a cluster with Kata installed. | D13, decision 0007 |

## Deferred features

| Item | Notes | Where recorded |
|------|-------|----------------|
| Server-side analytics | `evalsi analyze` loads runs into DuckDB on the client, through the API. It reads every result of the runs it loads, so analysis over thousands of large runs would want a server-side query (ClickHouse already holds the data on Kubernetes). | Decision 0009 |
| Platform wheels bundling evalsid | Releases publish archives (evalsid, a static bubblewrap), wheels, images, charts and the bundle (`.github/workflows/release.yml`), but no tag has been cut, and the wheels do not bundle evalsid as decision 0006 suggests. | Decision 0006 |
| Sandbox-time quotas | Per-project quotas cover concurrent and stored runs, daily judge and target tokens, and reward rollouts in flight; sandbox minutes are not metered. Concurrency quotas are per replica. | §17 "Tenancy" |
| Keyless image signatures | `sandbox.image_signatures` verifies cosign key signatures (`.sig` tags) offline. Keyless (Fulcio certificates, Rekor) and the newer bundle-as-referrer format are not supported, and our own release images are not signed yet. | §17 "Other security controls" |
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

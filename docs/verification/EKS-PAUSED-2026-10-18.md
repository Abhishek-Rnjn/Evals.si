# EKS verification paused: remaining work, 2026-10-18

The user is deleting the Kubernetes cluster for now. Live verification is
**paused**, not complete. This records the handoff before a future cluster
is available; it does not assert that cluster deletion, volume retention or
AWS resource cleanup has been verified. PR #85 remains excluded.

## Durable checkpoint

Branch: `verify/eks-leftovers`. Existing implementation and evidence commits:

- `42d8127`: five reproduced fixes, harness/framework verification,
  checkpoint/PVC/LoRA lifecycle and bounded PostgreSQL/S3 restore.
- `cc70404`: actual native GPU trainer updates, opt-in verl regression,
  pinned runtime recipes and explicit current-OpenRLHF limitations.

Detailed records:

- [First EKS pass](EKS-2026-10-17.md).
- [Harness, all 49 starting inventory rows, checkpoint and recovery evidence](EKS-HARNESS-2026-10-17.md).
- [GPU trainer versions, commands, measurements and failed controls](EKS-GPU-2026-10-17.md).
- [Product-wide leftovers and original scope references](../LEFTOVERS.md).

The bounded passes remain valid evidence: real model identity/inference and
checkpoint cleanup; live pod/bwrap controls; signed-thinking/context/replay
and process kill/resume; pinned Claude CLI/MCP; all ten tracing recorder
configurations; drained same-version PostgreSQL/S3 restore; native verl
0.9.1 and OpenRLHF **0.9.0** one-step GPU updates. They are not universal,
production-scale, learning-quality or current-OpenRLHF certification.

No GPU-requesting pods remained at the last check. That observation was
before the user's cluster deletion, not a fresh infrastructure inspection.

## Resume order and exact unfinished checks

### GPU-dependent work first, when GPU capacity returns

1. **Current OpenRLHF 0.11.2 native training.** Its vLLM 0.29.0/CUTLASS
   stack requires protobuf <7; the SDK requires >=7.35. Keep the native
   trainer and SDK HTTP reward server in separate environments. Obtain a
   genuinely compatible prebuilt runtime or budget the FlashAttention
   2.8.3/Torch 2.13 build explicitly: the prior source build completed only
   about 21 of 72 CUDA units in approximately 50 minutes and was cancelled
   by the user. Do not silently change versions, force incompatible
   dependencies, or count the successful 0.9.0 control as 0.11.2.
   Require actual generation, audited reward calls, finite nonzero
   gradients, an optimizer step and changed saved weights with zero weight
   decay. Use zero LR warmup for a one-step probe; the default-warmup
   negative control exited 0 and saved unchanged inspected weights.
2. **Trainer-backed OpenEnv/verifiers integration.** Run a real native
   trainer step using the actual task environment and sandbox checker.
   Constructor tests, scripted chat rollouts and standalone reward adapter
   tests do not meet this criterion. Assert rollout cleanup and actual
   gradient/weight changes. Shared setup across verifiers rollouts is a
   separate, still-unbuilt capability.
3. **Do not conflate scale/quality with the two bounded GPU passes.**
   Useful learning, long-running training and multi-GPU behavior remain
   unverified. The 4,096-rollout reward target requires the specified
   256-warm-microVM/KVM capacity, not just one GPU.

The reusable [verl GPU test](../../tests/rlframeworks/test_gpu_verl.py)
already passed. On a replacement runtime, install its
[pinned dependencies](../../tests/rlframeworks/requirements-gpu-verl.txt),
download the real Qwen 0.5B model/tokenizer, and use the opt-in command in
the GPU record. Reusing the old pod path or finding a saved checkpoint
does not by itself rerun or prove the test.

### First CPU-only follow-ups

- **Concurrent PostgreSQL CreateRun:** restore two ready API replicas and
  actual CPU workers, submit concurrent requests with the same idempotency
  key, reconcile documented in-flight/uncertain `Unavailable` responses
  using that **same key**, and assert one eventual run/result set. The
  eight-way probe stopped on the documented response; it neither passed
  this certification nor established a product bug.
- **Direct S3 import:** execute the distinct import API path and assert the
  imported records and resulting evaluation. The call after the concurrent
  probe was never reached. Reading restored S3 data in a new run passed,
  but is not a substitute for that import path.
- **UI accessibility:** perform the automated/browser checks, fix confirmed
  issues and rerun them. Backend tests do not establish accessibility.
- Continue the live harness/provider, real-task, mesh, scheduler and recovery
  checks below. Keep failed controls strict; fix product behavior only after
  a reproducible failure identifies it.

## Complete disposition of the original 49 rows

IDs match the starting inventory in the harness record. A partial pass
does not close the remaining scope. The product-wide leftovers document
retains detailed original references and design limits.

| ID | Item | Remaining action or disposition at pause |
|---|---|---|
| 1 | Stock Firecracker | Recreate on KVM-capable hosts and verify stock image/chart values. Both sampled old node types lacked `/dev/kvm`. |
| 2 | User-namespaced bubblewrap | Bounded EKS pass complete. Recheck on a replacement kernel; not certified on every host. |
| 3 | Calico + Istio ambient egress | Resolve NATS timeouts and sandbox guest resets first, then measure deny/allow/restore enforcement. Non-mesh recovery passed but does not certify ambient. |
| 4 | STRICT-mTLS admission | Verify valid/invalid admission, fail-closed behavior and recovery under actual mesh-wide STRICT mTLS. Host-network/non-mesh controls are not substitutes. |
| 5 | PR #12 on AKS/Istio | Re-run on those actual platforms; EKS cannot certify AKS. |
| 6 | Trace sources | Live Databricks experiments/Unity Catalog/OAuth, SageMaker and LangSmith remain; also production backfill/lag, provider UI/external alerts, Langfuse Cloud/TLS/arm64 and fresh incoming-trace redaction coverage. Missing connector locations/ambient credentials and Azure ML API support require implementation/provider decisions. |
| 7 | Deep-research / Terminal-Bench reference demos | Run the actual suites with a real model and judge, plus the outstanding interactive dsh web export. Small-repo/calc tasks do not close them. |
| 8 | H1 frontier handling | Direct harness cache-hit controls still fail in both streaming modes. Native OpenAI Responses/stateless reasoning, stronger signed-thinking-plus-tool repair, adaptive behavior, live retry/fallback/concurrency and production cost remain. Native Claude cache-read evidence is not a harness cache-hit pass. |
| 9 | H2 long horizon | Verify 300-step/200k-token and compaction/evidence quality, queue-worker failover, live fork/delegation and remote MCP OAuth/elicitation. Bounded context strategies and actual process kill/resume passed. |
| 10 | Persistent shell/background processes | Not built; requires persistent `evalsid` sandbox sessions rather than a harness-only change. |
| 11 | H3 agents/profiles | Verify other real CLIs and held-out/transfer experiments. ACP, A2A push notifications and server-side sweeps are not built. Qwen 3B code mode remains a failed model/profile control. |
| 12 | H4 environments | Pod/bwrap bounded checker/interjection controls passed. Browser/desktop and remaining importer capabilities are not built; PR #85 multi-service work remains excluded. |
| 13 | Framework tracing profiles | Native OpenAI Responses, CrewAI through LiteLLM, retries/errors and multi-agent/handoff mapping remain. Ten live recorder configurations passed their bounded mapping contracts. |
| 14 | 10k concurrent trials | Requires explicitly sized/dedicated capacity and actual agent-trial load; API throughput is not a proxy. |
| 15 | Real SWE-bench Verified | Run a genuine task with its actual image/checker; synthetic calc does not count. |
| 16 | Unprivileged cgroups | Writable delegated cgroups were absent. Existing behavior is a documented limit, not certified resource enforcement. |
| 17 | Kata | Install an actual supported RuntimeClass/runtime, then verify it; none was available. |
| 18 | Internal webhook URLs | Decide the allowlist/default policy before implementation; in-cluster reachability is intentional today. |
| 19 | Server-side SQL | Slicing is built; arbitrary SQL intentionally remains client-side. No cluster test can implement it. |
| 20 | Platform wheels | Linux release evidence exists. macOS/Windows sandbox bundles are not built; Linux sandbox support must not be relabeled cross-platform. |
| 21 | Run archiving | Per-label retention, deletion grace period and an erasure path covering backups/logs remain unbuilt; archived runs still count toward the stored-run quota. |
| 22 | Prior-review limits | Keyed upload double-spooling/slot use, no SDK failed-batch retry, and incomplete/empty connector-page behavior remain documented. Any future retries must preserve idempotency. |
| 23 | Pipeline/schedule certification | Complete same-key concurrent-create reconciliation and the unreached direct S3 import first; then real scheduled source write-back, lease/HA races, production load and MCP tool-idempotency coverage as supported. |
| 24 | Sandbox quota boundaries | Documented exclusions and per-replica concurrency remain; global enforcement is not established. |
| 25 | Keyless sandbox-image verification | Offline key-signature verification does not support Fulcio/Rekor or bundle-as-referrer verification. Release-image keyless signing is a different capability. |
| 26 | Enterprise audit/delegation | Implement decision 0026: strict/read auditing and identity/configuration context, integrity/export/holds; token exchange remains deliberately unbuilt. Not a verification-only task. |
| 27 | LongHarness/Evo2 remaining work | Real held-out/transfer experiments, long-document importers, human-labeled evidence-stage judge validation and semantic citation support remain. Deterministic verbatim/version checks do not prove semantics; automated harness search/evolver training is not planned. |
| 28 | Quality-aware routing | Requires an agentgateway extension/product decision before implementation. |
| 29 | Separate process roles | Intentionally deferred until measured load justifies splitting API/ingest roles. |
| 30 | Pod snapshots | Unsupported; setup is reconstructed per trial. Fresh-pod replay passed, not snapshot restore. |
| 31 | Firecracker tap networking | Intentionally uses vsock/host egress proxy; raw tap/nftables networking is not built. |
| 32 | Per-caller rate limits | Decide key/principal/project units, defaults and response contract, then implement/test API and OTLP throttling. |
| 33 | Reversible migrations | Add versioned ordered/down migrations and expand/contract rules, then verify mixed N/N-1 operation and rollback. Same-version two-replica readiness is not rolling-upgrade compatibility. |
| 34 | Restore/PITR | Drained same-version PostgreSQL/S3 restore passed. PITR/WAL, PostgreSQL HA/failover, ClickHouse/JetStream, source cursors, pending webhooks and in-flight recovery remain. |
| 35 | Judge injection robustness | Expand beyond six bounded live controls with model-specific/wider cases; do not infer universal robustness. |
| 36 | UI accessibility | Automated/browser audit and fixes remain; no accessibility certification yet. |
| 37 | Independent penetration test | Requires an outside tester and the security-questionnaire work, not a self-certification by this EKS exercise. |
| 38 | Second maintainer | Requires another person/onboarding; infrastructure cannot close it. |
| 39 | BFCL memory/web search | Unsupported categories, not implemented. |
| 40 | Native tau2 user | Current judge/structured-output adapter is not tau2's native LiteLLM user; implement only after a scope decision. |
| 41 | Multi-service Terminal-Bench | Excluded PR #85 scope, not completed by this branch. |
| 42 | FrontierCode/CursorBench | Requires access to real, verifiable datasets; private/unpublished data cannot be invented. |
| 43 | verl/OpenRLHF training | verl 0.9.1 and OpenRLHF 0.9.0 native bounded updates passed. Current 0.11.2 and training quality/stability/scale remain as described above. |
| 44 | 4,096-rollout reward scale | Requires 256 warm microVMs and sized KVM capacity; no such scale pass occurred. |
| 45 | Checkpoint watcher/PVC/LoRA | Actual source controller/watcher, read-only full-model PVC and synthetic LoRA lifecycle passed. Build/publish and verify the fixed operator image separately; the run used a copied source-built binary, not a published fixed image. |
| 46 | Generic PyTorch/Lightning serving | Requires a serving-contract/product decision and implementation. |
| 47 | OpenEnv/verifiers trainer compatibility | Actual trainer-backed GPU step remains; constructor/unit/scripted rollout checks are insufficient. Shared verifiers setup is separately unbuilt. |
| 48 | Coding product over MCP | Real pinned CLI/MCP/artifact controls passed in EKS. Actual default-off GitHub workflow run with its configured secret remains; do not commit the credential. |
| 49 | Wasm IO/judge/sandbox, Azure ML trace API | Unsupported capabilities/provider API; new EKS capacity alone cannot close them. |

## Recreating the verification environment

- Use the branch/commits above, not just released 0.2.0 images. Rebuild or
  upload the current operator/SDK/harness where a test requires them.
- Supply a **new** kubeconfig and credential configuration privately. The
  old file and notes were deliberately not committed; old service addresses,
  projected tokens, pod names and cluster JWKS are not portable fixtures.
- Recreate owned Helm dependencies, TLS material, policy controls and
  sandbox pools. The old cluster had **no default StorageClass**; request
  the new cluster's actual class explicitly. Check GPU driver/CUDA
  compatibility, CPU headroom, shared memory and storage before launch.
- Old recovery API/CPU Deployments were scaled to zero. Start the required
  writers/workers deliberately for new probes, but keep them stopped during
  a drained restore. Identify owned Deployments by their Helm release
  annotation rather than assuming an instance label.
- Do not weaken admission, sandbox, score or cache-hit assertions merely
  to make a replacement cluster pass. Run negative controls alongside
  positive controls and distinguish runtime blockers from reproduced bugs.
- Run the opt-in harness/checkpoint/GPU tests only after their real endpoints
  and fixtures exist. Default skipped collection is not live verification.

## Evidence, storage and teardown caveats

Committed reports, test sources and runtime pins survive cluster deletion.
Private off-cluster session evidence includes sanitized harness/framework
exports, checkpoint JUnit, the recovery summary, GPU summaries/audits/logs
and encrypted synthetic recovery backups with their private key material.
These are **local private artifacts**, not an independent remote archive or
a production backup. Do not add secrets, raw provider transcripts, kubeconfig,
backup keys or model binaries to Git.

The last owned namespaces were `evalsi-harness-20261017`,
`evalsi-checkpoints-20261017` and `evalsi-recovery-probe-20261017`.
Retained claims included `gpu-training-work`, checkpoint models/adapters,
harness storage and PostgreSQL/MinIO dependency storage. Trained model and
optimizer files were only on the training PVC; they are not preserved by
committing this report. Export or snapshot them separately before deletion
if they are needed; otherwise recreate the synthetic training fixtures.

Deleting an EKS control plane does not itself prove that EBS volumes,
load balancers or other billable AWS resources were removed. Their
retention/cleanup is user-managed and was not verified in this handoff.
No cluster deletion, infrastructure recreation, image publication or Git
push was performed for this documentation checkpoint.

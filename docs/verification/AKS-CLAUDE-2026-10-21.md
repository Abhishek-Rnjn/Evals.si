# Live AKS and Claude verification, 2026-10-21

Branch: `verification/live-k8s-claude-2026-10-21`. SDK/harness checkout:
`5d7510fca5bcfc7881b4f31206da3f2c6aeb8d4f`.

This is a bounded continuation, not an EKS run or production certification.
The supplied kubeconfig connects to **AKS**. The
[20 October AKS record](AKS-2026-10-20.md) supersedes many blockers in the
[18 October handoff](EKS-PAUSED-2026-10-18.md); those older checks are not
treated as missing or rerun here. The complete 50-row reconciliation is below.
[Structured observations](data/aks-claude-2026-10-21.json) retain the measured
results without credentials, private endpoints, node names or transcripts.

## Environment and boundaries

| Surface | Observed configuration |
| --- | --- |
| Client | macOS arm64; Python 3.13.13; pytest 9.1.1; Helm 4.2.4 |
| Kubernetes | Six Ready amd64 AKS nodes, Kubernetes v1.36.4, kernel 6.8.0-1070-azure |
| Node sizes | Three D4ds_v5, two D8s_v5, one DS4_v2; only DS4_v2 untainted |
| Accelerators | No advertised `nvidia.com/gpu` capacity. Host KVM was not probed in this run |
| Mesh/network | Istio control plane, ztunnel and CNI DaemonSets present. Test namespace was not enrolled in ambient mode. Actual default-deny egress was not enforced |
| Kata | `kata-vm-isolation` RuntimeClass exists, handler `kata`, 600 MiB overhead; **zero nodes** match its required selector |
| Storage | Azure disk, file and blob StorageClasses present; no volumes provisioned by this run |
| Claude | Terminal login shell supplies `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_BASE_URL`; configured HTTPS gateway, **not** direct `api.anthropic.com`. Smoke response reports `claude-opus-5-5` |
| Sandbox server | Published `evalsid 0.3.0`, not a rebuilt HEAD server; current checkout's SDK/harness exercised against it |

The plain tool environment had neither an Anthropic API key nor a logged-in
Claude CLI. The user's login shell supplied the bearer token and gateway.
No login-profile files were printed, no provider token was put in Kubernetes,
and no credential was redirected to a different provider endpoint.
The incompatible Linux kubectl in `/usr/local/bin` was bypassed using the
existing native `/opt/homebrew/bin/kubectl`; no tools were installed.

Owned namespace: `evalsi-verify-20261021`. Resources were two low-resource
nonroot control pods, one unschedulable Kata probe, and a namespace-only
sandbox pool with at most two concurrent sandboxes. No shared CNI, mesh,
RuntimeClass, node labels, taints, CRDs or existing workloads were changed.
No privileged pod, host mount, GPU, cloud resource or persistent volume was used.

Image identities:

- Task/control image: `docker.io/library/python@sha256:cf50a4f6ac4f026458b08d5222023d700ac806410dff5e9ca8728fa44f2b850f`.
- Pool image: `ghcr.io/abhishek-rnjn/evalsi@sha256:c8d210562ada07c2fe294b6da9e7d68a8f1e2cd7fb435ba4a1fa7e7609c6e6da`.

## Completed checks

| Check | Result | Evidence and limits |
| --- | --- | --- |
| Live Claude smoke | PASS | Exact `OK`, `end_turn`, model `claude-opus-5-5`, 18 input / 4 output tokens. Gateway-mediated model identity, not independent upstream attestation |
| Protected checker | 5 PASS | Honest fix, tampering rejection, restore rejection/acceptance, and symlink-tampering restore on actual pod sandboxes |
| Bad implementation and truncated turn | 2 PASS | Bad code is a failed grade; truncated model output does not become a passing grade |
| Interjection and replay | PASS | Scripted model receives interjection; fresh-pod replay completes without another model request |
| Live Claude task and replay | 3 PASS | Nonstreaming `minimal` (37.46 s), streaming `standard` (22.78 s), nonstreaming `code-mode` (25.40 s); thinking enabled, real tool execution, checker pass, exact output/usage replay |
| Live context mechanism and replay | 4 PASS | `clear-tool-results` 65.67 s; `compact` 103.05 s; `fresh-rounds` 55.76 s; `evidence-state` 69.76 s. Mechanism assertion, repaired code, retained release code and replay assertions pass. Small synthetic document, **not** 300 steps / 200k tokens or human-calibrated evidence quality |
| Actual process SIGKILL and fresh-pod resume | PASS on corrected invocation | Normal pytest entrypoint: 1 passed in 21.51 s. Recorded turns are not regenerated; log/checker and cassette event sequence asserted. Scripted model; not queue-worker failover or external-side-effect idempotency |
| Signed thinking and cassette replay | 2 PASS | Streaming and nonstreaming each return a signed thinking block; exact thinking/text/usage/cache-write replay. Cache writes 11,193 / 11,192 tokens respectively; this is **not cache-read evidence** |
| Live judge controls | 6 PASS | Correct answer >=0.75; wrong answer and four injected wrong-answer variants <=0.25. Existing bounded controls, not a calibrated robustness/detection rate |
| Pool mTLS | PASS | Authenticated probe succeeds; missing client certificate rejected with TLS `certificate_required` and gRPC `UNAVAILABLE` |
| Pod hardening | PASS, bounded | UID 65532, effective capabilities zero, `NoNewPrivs=1`, seccomp filtering, no service-account token; cluster DNS resolves |
| Isolation reporting | PASS | Real task sandbox reports `driver=pod`, `level=namespaced`, **`enforcement=partial`**, with the undeclared NetworkPolicy limitation |
| Unsafe multi-service allocation | PASS, fail-closed | Valid 60-second service specification refused specifically with `services require enforced pod NetworkPolicy`; no service trial allocated |

There are **24 distinct passing existing test cases**: 16 sandbox/harness
cases, two signed-thinking cases and six judge controls. Environmental probes,
smoke inference and negative cache controls are separate; no claim that the
entire test module or repository suite passed is made.

## Failures and blockers retained

### C01: prompt-cache reuse still fails through the configured gateway

Both existing strict harness cache tests created a cache but returned
`cache_read_tokens = [0, 0, 0]` for three repeats, in **both** streaming modes.
The first-write assertion passed. Neither cache test is a pass.

An independent native `anthropic.AsyncAnthropic` control used a fresh unique
600-entry system prefix and identical repeated requests on the **same** endpoint
and configured model. All three responses reported 11,102 cache-write tokens,
zero cache-read tokens, 12 ordinary input tokens and four output tokens.
Its strict cache-reuse predicate failed and the command exited 1.

This reproduces the symptom outside the harness. It does not identify whether
gateway routing, upstream caching, usage reporting or another provider behavior
causes it. No cache parser was changed and no caching claim was relaxed.
Next: investigate gateway affinity/cache/usage behavior, or compare with a
separately authorized direct-provider credential. Do not send this gateway token
to another endpoint.

### C02: namespace NetworkPolicy does not enforce default-deny egress

A nonroot control pod connected to a separate canary's TCP port 8000.
After applying a policy selecting **only that control pod**, with
`policyTypes: [Egress]` and `egress: []`, the same connection still succeeded
after five seconds. A policy object's existence is not an isolation pass.

This is a current namespace/CNI observation, **not** a Calico-plus-ambient
reproduction: these test pods were not ambient-enrolled. The pool retained
`networkPolicyEnforced: false`; partial reporting and multi-service refusal
were verified rather than bypassed. Native enforcement needs platform-owner
configuration and a rerun; no cluster-wide networking was changed.

### C03: Kata is configured but cannot schedule

Zero nodes match `kubernetes.azure.com/kata-vm-isolation=true`.
An actual pod requesting the RuntimeClass remained Pending; its 20-second
`PodScheduled` wait timed out with `Unschedulable`: one node mismatched the
selector and five had untolerated taints. No Kata container executed.
Adding tolerations alone would not establish runtime installation or selector
eligibility. Do not relabel ordinary nodes to manufacture a pass.

### Invocation corrections, not product fixes

- The first 16-case harness batch used `pytest.main` from stdin:
  **15 passed, 1 failed, 13 deselected in 448.74 s**. Multiprocessing `spawn`
  could not import `<stdin>`, so the interruption test never reached its
  intended SIGKILL point. Running that unchanged test via `python -m pytest`
  passed. The failed initial batch remains recorded, not relabelled green.
- Helm correctly refused an attempted pool-only full release with zero CPU
  workers. Instead, only the chart's TLS, sandbox RBAC/policies and pool
  templates were rendered/applied. This is **not a full Helm installation
  certification**; no API or workers were deployed.
- An initial service refusal probe omitted the required lifetime and hit input
  validation. It was discarded as evidence for network enforcement. Only the
  corrected valid specification's explicit NetworkPolicy refusal counts.

## All 50 original leftovers: current disposition and next useful work

IDs retain the [original inventory](LEFTOVERS-2026-10-17.md). A historical pass
means that report's bounded scope, not completion of the whole row. "Possible"
means a follow-up candidate, **not executed in this batch**.

| ID | Current disposition / next useful verification |
| --- | --- |
| L01 | Stock Firecracker and real KVM passed on 20 October. Possible: replacement-host KVM API/VM preflight and stock-image rerun; no host KVM evidence added today |
| L02 | EKS user-namespace bwrap and AKS fallback controls retained. Possible: current-kernel `hostUsers:false` rerun; ordinary hardened pods do not certify nested bwrap |
| L03 | Prior ambient leak/opt-out evidence retained. Today's default-deny leak blocks native network isolation; owner-managed enforcement is prerequisite to service/mesh reruns |
| L04 | Prior EKS host-network admission and AKS recheck retained. Actual EKS overlay routing needs an EKS cluster, not this AKS kubeconfig |
| L05 | Prior STRICT-mTLS admission controls retained. Possible: isolated operator/sidecar rerun with port exception; no shared mesh-policy edits in this batch |
| L06 | 20 October AKS/RECHECK/KVM results retained. Waypoint-specific topology still requires its actual configuration |
| L07 | Prior live MLflow/Phoenix/ClickHouse loops and backfill retained. Hosted LangSmith, Databricks, SageMaker, cloud/TLS/distributed paths require their own workspace/identity |
| L08 | Real Claude available: reference deep-research and Terminal-Bench suites are now candidates, with task/checker dependencies and explicit token/time budget; not run here |
| L09 | Today's three live profiles and two signed-thinking replays pass. Cache reads fail in both modes and native SDK; native OpenAI Responses still needs a separate provider credential |
| L10 | Today's four live context controls and corrected SIGKILL/resume pass. 300-step/200k-token quality, live delegation/forks and external-side-effect idempotency remain separate experiments |
| L11 | Persistent shell/background sessions remain implementation work; a cluster/token does not implement them |
| L12 | Prior real Codex/Gemini/pi/OpenCode and Claude CLI evidence retained. Live profile/held-out transfer comparisons possible with task set and product credentials; ACP/A2A push/sweeps remain separate work |
| L13 | Today's eight checker/interjection/truncation cases pass. Prior clean checking across four rungs retained; browser/desktop and undeveloped checker semantics are not infrastructure-only checks |
| L14 | Prior ten recorder configurations retained. Claude-backed native framework handoff/error/retry exports possible with pinned packages; OpenAI Responses coverage needs its provider |
| L15 | 10k actual concurrent trials remain a dedicated-capacity experiment. Six shared nodes are not permission or evidence to saturate the cluster |
| L16 | Prior real `sympy__sympy-20590` bwrap/Firecracker result retained. Larger real-task/model-quality sample possible after image/capacity budgeting |
| L17 | Hardened control pod has read-only cgroup hierarchy. Writable delegated cgroup enforcement is not verified; do not relax the pod or host to claim it |
| L18 | RuntimeClass exists but actual Kata pod is Unschedulable. Requires genuinely eligible, configured nodes before product VM-isolation tests |
| L19 | Opt-in webhook SSRF guard already built and live-tested on 20 October. Default policy remains distinct; isolated allowlist/redirect controls can be rerun |
| L20 | Arbitrary server SQL intentionally unsupported; existing server-side slicing is a different capability |
| L21 | Existing released platform artifacts retained. macOS/Windows sandbox bundles require implementation/artifact work, not an AKS test |
| L22 | Prior archive/quota/live-restore evidence retained. Per-label retention, deletion grace and backup/log erasure require their actual implementation and retention design |
| L23 | Prior-review upload spooling, retry and omission limits remain documented; no new infrastructure closes them |
| L24 | Prior PostgreSQL concurrency, direct S3-compatible import and failover evidence retained. AWS S3 needs actual AWS identity/bucket; production callbacks/load need isolated capacity |
| L25 | Quota/accounting boundaries remain the documented product scope; broader accounting is implementation work |
| L26 | Keyless sandbox-image/referrer verification remains implementation work; release signatures are a separate path |
| L27 | Enterprise audit/delegation/export guarantees require scoped implementation and external evidence where applicable |
| L28 | Real-model held-out LongHarness/Evo2/evidence-stage runs now possible with public datasets. Human calibration and semantic-grounding quality still need labels; missing features remain unbuilt |
| L29 | agentgateway score-feedback routing requires a supported design/implementation; Anthropic access alone does not provide it |
| L30 | Separate API/ingest process roles remain an intentional architecture decision driven by actual load evidence |
| L31 | Pod snapshots remain unsupported; today's replay/resume creates new pods and replays tools, not pod snapshots |
| L32 | Firecracker tap/nftables is not planned; vsock/proxy is the intended path |
| L33 | Per-caller limits already built and live-tested on 20 October. Possible: isolated per-caller API/ingest/guardrail rerun, not throttling shared services |
| L34 | Prior v0.3.0 reader/writer and rolling upgrade evidence retained. Reversible down migrations/stronger concurrent-version contracts are separate work |
| L35 | Prior PostgreSQL PITR and quiesced restore retained. Managed/off-site, ClickHouse/JetStream recovery and production RPO/RTO need dedicated fixtures/provider access |
| L36 | Today's six live judge controls pass. Representative human-labelled calibration, drift and model-specific robustness remain unverified |
| L37 | Prior browser/axe/keyboard suite retained. Possible: current workbench automated rerun; manual screen-reader/user accessibility needs human participation |
| L38 | Independent penetration test requires its own tester, scope and authorization; not replaced by this verification |
| L39 | Second-maintainer onboarding is a human process, not a cluster check |
| L40 | BFCL memory/web paths require implementation and actual benchmark dependencies |
| L41 | Native tau2 user contract remains distinct from the current structured-output adapter |
| L42 | Prior bounded multi-service Terminal-Bench evidence retained. Current network-enforcement failure blocks safe service reruns; unsupported multistage tasks require implementation |
| L43 | FrontierCode/CursorBench requires authorized verifiable dataset access; no private tasks synthesized |
| L44 | OpenRLHF 0.11.2 and verl 0.9.1 native updates passed on 20 October. No GPU advertised today; useful learning/multi-GPU scale remains unverified |
| L45 | Prior 4,096-rollout and five-node KVM measurements retained. Sustained target/10k scale and replacement-kernel measurements require sized dedicated resources; no extrapolation |
| L46 | Prior checkpoint/PVC/LoRA lifecycle retained. Fixed published-image deployment can be rerun with a serving model/GPU if that path requires one |
| L47 | Generic checkpoint-serving contracts require implementation/provider agreement |
| L48 | Prior native trainer-backed OpenEnv/verifiers/Gymnasium/NeMo Gym controls retained, including shared setup/process rewards. No GPU-backed rerun today; learning quality is separate |
| L49 | Prior real CLI/MCP OAuth/elicitation evidence retained. Secret-backed GitHub workflow and additional coding products need their explicit credentials and disposable repositories |
| L50 | Wasm IO, Azure ML API and unrestricted SQL limits remain intentional or provider-contract work; no claim that Azure cluster access closes Azure ML API gaps |

This batch does not close R04-01 through R04-05: no candidate artifacts were
published/installed, no unfamiliar developer or report reader was observed,
no human-labelled held-out calibration was performed, and no real application
integration was reproduced through the GitHub Action.

## Reproduction and exact execution scope

Use the existing [harness tests](../../tests/harness/test_eks.py). The `EKS`
environment-variable names are historical; the tests actually reached AKS.
Keep the login shell's token private. Supply a private temporary directory
containing `auth-token`, `base-url` and `model`, and set
`EVALSI_EKS_ANTHROPIC_DIR` to it. These files must not be committed.

For the pool, the successful render/apply command was:

```sh
helm template live deploy/helm/evalsi --namespace evalsi-verify-20261021 \
  -f deploy/helm/evalsi/values-namespaced.yaml \
  --set image.tag=0.3.0 --set fullnameOverride=live \
  --set 'sandbox.pool.ladder={pod}' --set sandbox.pool.maxSandboxes=2 \
  --set sandbox.pool.pod.cpu=500m \
  --set sandbox.pool.resources.requests.cpu=100m \
  --set sandbox.pool.resources.requests.memory=128Mi \
  --set sandbox.pool.resources.limits.cpu=1 \
  --set sandbox.pool.resources.limits.memory=512Mi \
  --show-only templates/tls.yaml --show-only templates/sandbox.yaml \
  --show-only templates/sandbox-pool.yaml |
  kubectl --kubeconfig ./kube-aks.yaml -n evalsi-verify-20261021 \
    apply --server-side -f -
```

Result: selected resources applied; `rollout status deployment/live-sandbox-pool
--timeout=180s` passed. Namespace must be new and owned by the operator of the
test, not an existing application namespace. The template stream contains
ephemeral TLS secrets: pipe it directly, never log it.

Port-forward the service on loopback port 17443. Retrieve only this owned pool's
`live-worker-tls` Secret programmatically into a private temporary directory
(mode 0700, files 0600), without printing it. Set `EVALSI_SANDBOX_TLS_CA`,
`EVALSI_SANDBOX_TLS_CERT`, `EVALSI_SANDBOX_TLS_KEY`,
`EVALSI_SANDBOX_TLS_SERVER_NAME=live-sandbox-pool.evalsi-verify-20261021.svc`,
`EVALSI_SANDBOX_ADDR=tls://127.0.0.1:17443` and
`EVALSI_EKS_SANDBOX_DRIVER=pod`. Do not disable certificate verification.

The first batch called `pytest.main` with this selector, `-v --tb=short`,
`--durations=20`, and a disposable `--basetemp`:

```text
test_protected_checker_on_real_pods or
test_bad_implementation_is_an_ordinary_failed_grade or
test_interjection_and_replay_use_fresh_real_pods or
test_truncated_model_turn_is_not_a_passing_grade or
test_live_anthropic_task_and_replay or
test_live_context_mechanism_and_replay or
test_killed_harness_process_resumes_on_fresh_pod
```

Result: 15 passed / 1 invocation failure, detailed above. The corrected
subprocess command, with the same pool TLS configuration, was:

```sh
python/.venv/bin/python -m pytest \
  tests/harness/test_eks.py::test_killed_harness_process_resumes_on_fresh_pod \
  -v --tb=short --basetemp "$PRIVATE_DIR/pytest"
```

Result: **1 passed in 21.51 s**. Use the normal pytest entrypoint for future
combined runs, not stdin-driven `pytest.main`.

The two signed-thinking, two cache-hit and six judge checks invoked the
unchanged functions from the same test file directly via `runpy.run_path`,
with the actual terminal gateway/model/token-name target and temporary paths.
They do not use a sandbox; no fake pool address or skip override was supplied.
Those results are function assertions, not an additional pytest suite run.

## Cleanup and next batch

No trial sandbox pods remained before cleanup. The loopback port-forward was
stopped and `kubectl --kubeconfig ./kube-aks.yaml delete namespace
evalsi-verify-20261021 --wait=true --timeout=120s` completed. Temporary provider
settings, TLS keys, cassettes and pytest state were removed by their temporary
directory context managers. The supplied kubeconfig remains local and is now
explicitly ignored by Git.

Next useful work, in order:

1. Diagnose gateway cache reuse/usage and obtain a genuinely cache-reading
   control before changing or certifying harness caching.
2. Have the platform owner provide enforced pod networking and eligible Kata
   capacity; then rerun real service isolation and VM-runtime controls.
3. Choose a bounded public deep-research/Terminal-Bench or long-document task
   set, token/time ceiling and acceptance metric. Start real-model quality and
   held-out evidence experiments without confusing them with transport passes.
4. Provision only the specifically needed external workspace identities
   (AWS S3, LangSmith, Databricks/SageMaker, OpenAI), or dedicated GPU/scale
   capacity. An AKS credential and Claude token cannot certify those surfaces.

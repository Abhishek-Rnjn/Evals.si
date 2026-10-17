# EKS harness-first, checkpoint and recovery verification, 2026-10-17

Continuation of the [first EKS pass](EKS-2026-10-17.md) on
`verify/eks-leftovers`, excluding PR #85's multi-service work. This is a
verification inventory and a bounded run record, **not a claim that every
leftover is verified**. Provider credentials, gateway URLs and cluster
identities are deliberately omitted.

## Checkpoint before GPU training

This record captures the completed work before prioritizing the remaining
GPU training checks. **Actual verl/OpenRLHF training steps and trainer-backed
OpenEnv/verifiers runs are still pending.** No GPU is currently allocated to
an inference server. The checkpoint helpers and their model PVC are retained
for that next phase; removing the GPU node now would block it.

Five confirmed product bugs were fixed on `verify/eks-leftovers`:

| Fix | Evidence |
|---|---|
| Stock vLLM image loaded its default instead of the requested checkpoint | Real GPU model identity/inference; operator regression and both regenerated CRDs |
| CLI process errors were hidden by an output-format diagnostic | Exit/timeout regressions failed before the fix; full harness suite passed in EKS |
| Native Claude input usage omitted cached tokens | Actual live export: 6 uncached versus 3237 total; raw/canonical regression variants and all ten live mappings passed |
| One-shot checkpoint watches returned success after evaluation errors | Actual model 404s reproduced exit 0; nine exit-status controls and real watcher lifecycle passed |
| Trial PostgreSQL ignored an explicitly selected StorageClass | Real EKS PVC stayed Pending with no class; regression failed before fix, then actual gp3 binding/database readiness and full Helm suite passed |

PR #85 remains excluded. Unbuilt features, missing external workspaces,
hardware/runtime requirements and pending checks are listed in the complete
inventory below; this checkpoint is not completion of all leftovers.

## Method

The source Helm charts installed a disposable application with persistent
SQLite/NATS, a mutual-TLS pod sandbox pool and real CPU/harness workers.
The runner imported the current Python source, not just the released wheel.
The initial harness unit suite ran inside that Linux pod: **381 passed**;
after the diagnostic fix and two regressions, **383 passed in 51.61 s**.
Those existing tests still use their mocks/local sandbox fixtures; they do
not establish live provider or pod behavior.

Separate opt-in tests in `tests/harness/test_eks.py` lease actual sandboxes,
assert their driver/isolation, grade hidden checkers and rebuild fresh
sandboxes for replay. A stock vLLM 0.11.0 deployment on the L40S served
`Qwen/Qwen2.5-3B-Instruct`, alias `harness-qwen`, with Hermes tool parsing.
Anthropic access used explicitly supplied credentials and the configured
`claude-opus-5-5` gateway model, never credentials committed to the repo.

## Verified outcomes

| Check | Observed result | Limits |
|---|---|---|
| Protected checker on real pods | Five honest/tamper/fail/restore/symlink cases passed | Protects configured paths, not the entire interpreter/dependency environment |
| Ordinary failed grade | A non-repairing agent failed its checker without becoming infrastructure success | One synthetic task |
| Mid-run interjection | The next request included the message; replay rebuilt the task without another model call | Scripted provider, real sandbox |
| Truncated model turn | Stopped for budget and did not pass the checker | Scripted provider, real sandbox |
| GPU model and profiles | `minimal@1` and `standard@1` repaired the task and replayed; vLLM's completed-request counter did not increase during replay | Qwen 3B **failed** `code-mode`: it attempted unavailable tools and unsupported Python; no harness workaround was introduced |
| Anthropic tool use/replay | Nonstreaming minimal, streaming standard and code-mode repaired the task and replayed with identical output/usage | The tool-using gateway calls returned zero thinking tokens |
| Signed thinking | Streaming and nonstreaming mathematical tasks returned signed thinking blocks; cassette replay preserved exact blocks, text, usage and cache-creation counters without a live model | This is a direct model/cassette check, not the stronger thinking-plus-tool `fix-calc` exit criterion |
| Provider prompt-cache creation | Positive creation counters with a unique sufficiently long prefix | Does not prove a hit |
| Provider prompt-cache reuse | **Failed** in both modes: all three identical repeats reported cache-read counters `[0, 0, 0]` | Remains a failing integration control; no fabricated cached tokens or weakened assertion |
| Actual interruption/resume | SIGKILL of a spawned harness process after two recorded turns, destruction of its leased pods, then fresh-pod resume passed | Checker verified each reconstructed file change exactly once; only the unrecorded final model turn was requested. Not a Kubernetes queue-worker failover test |
| Four live context strategies | Clear-tool-results, compact, fresh-rounds and evidence-state each passed a real document/coding checker, scored `mechanism-ran` and fresh-pod replay with matching context counters/output | Bounded synthetic document, not a 300-step/200k-token or real corpus quality experiment |
| Real Claude CLI | Pinned Claude Code `2.1.296` repaired JavaScript in a real pod; the reader extracted actual tools and token usage | Native installation requires its postinstall; ignoring scripts leaves an unusable placeholder |
| Coding product over MCP | Real pinned Claude CLI invoked `mcp__evalsi__run` inside EKS; independently read artifact mean was exactly `2/3`, and final answer reported `0.67` and failed gate | Source MCP server, local stdio, not remote OAuth/elicitation or GitHub CI-secret certification |
| Judge robustness controls | Six live positive/wrong-answer/instruction/tag-breakout/role/JSON cases passed explicit high/low score thresholds | One configured judge, bounded fixtures, not general adversarial certification |
| Stock user-namespaced bubblewrap | Actual `evalsi-sandboxd` source chart, `mode: bwrap`, `hostUsers: false`, released image: mTLS probe available, namespaced/full enforcement, good code exit 0 and bad code exit 1 | AL2023/containerd hosts only; earlier AKS/idmap failures are not overturned |
| H4 on bubblewrap | The same eight integrity/interjection/replay/negative controls passed on that actual pool | Not a stand-in local sandbox |
| Host KVM | Read-only host `/dev` probes on CPU and GPU instance types both returned `KVM_ABSENT` | Stock Firecracker cannot be certified on these nodes; no node changes made |
| Unprivileged cgroups / Kata | Runner cgroup mount read-only; no cluster RuntimeClasses | Bubblewrap probe explicitly reports rlimits, not writable cgroup enforcement; Kata not installed |
| Framework tracing | All ten pinned configurations produced actual live exports; source Go mapping checks ran inside EKS and passed each instrumentation's contract | Missing streamed usage, native answers and some tool spans remain missing, not manufactured |

### Debugging and fixes

1. The first thinking-plus-tool tests failed. A narrow live request/response
   probe confirmed outgoing `thinking: {type: enabled, budget_tokens: 1024}`
   and provider replies with zero thinking tokens. A harder tool task also
   returned none. Direct mathematical requests produced real signed blocks.
   The configuration/recording path was not patched speculatively.
2. The real CLI exposed a product diagnostic bug: nonzero exit/timeout
   before structured events was misreported as an output-format mismatch.
   `CLIConnector` now preserves its bounded process error in that case.
   Both regression cases failed before the fix; the connector/reader/format
   subset passed **31 tests** after it.
3. Test-fixture installation was corrected to use the package's declared
   executable and required native postinstall, rather than assuming the
   older JavaScript entrypoint. This did not change product launch behavior.
4. Framework recording now accepts an explicit served model ID; the Claude
   recorder accepts an explicit key-variable name without inheriting ambient
   Claude configuration. The shared task explicitly requires its search
   tool, so a model that answers without tools is not mistaken for a
   tool-mapping verification.
5. The native Claude live export carried only six uncached input tokens,
   plus 1,921 cache-read and 1,310 cache-creation tokens. Ingest incorrectly
   stored six as the full input total. The profile now stores **3,237**,
   fills input/output independently and preserves a canonical total without
   double counting. Four regression variants failed before the fix; all five
   controls, the ingest race suite and all ten actual EKS exports passed
   afterward. The native CLI's real cache reads do not turn the separate
   direct harness cache-hit failures into passes.
6. A fresh-round fixture's initial three-step rounds exhausted its nine
   available steps before writing the answer. Five-step rounds (still
   bounded to three rounds) completed, actually reset context and replayed.
   This was a test budget adjustment, not a product fix or a long-horizon
   performance claim.

### Live framework results

These are actual mapped counters, not mock fixture expectations. All
OpenAI-compatible runs used the GPU model's real root
`Qwen/Qwen2.5-3B-Instruct`, not an alias substituted for model identity.

| Instrumentation | LLM steps | Tool steps | Input/output tokens | Mapping result |
|---|---:|---:|---:|---|
| Claude SDK native | 3 | 3 | 3237 / 1233 | PASS after cache-total fix; no native answer |
| Claude SDK OpenInference | 0 | 3 | 3233 / 1062 | PASS; model activity is on its agent span |
| CrewAI MLflow | 2 | 0 | 563 / 348 | PASS; tools only in model requests |
| CrewAI OpenInference | 2 | 1 | 563 / 161 | PASS |
| CrewAI OpenLLMetry | 2 | 0 | 563 / 324 | PASS; tools only in model requests |
| LangGraph LangSmith | 3 | 4 | 1148 / 955 | PASS |
| LangGraph OpenInference | 2 | 5 | 752 / 1023 | PASS |
| LlamaIndex OpenInference | 2 | 1 | 0 / 0 | PASS mapping contract; streamed usage absent |
| OpenAI Agents OpenInference | 2 | 1 | 544 / 324 | PASS; chat-completions path |
| OpenAI Agents OpenLLMetry | 2 | 1 | 0 / 0 | PASS mapping contract; answer and usage absent |

The harness checks accumulated 27 passing live cases across targeted runs,
with two still-failing cache-hit cases. A separate eight-case H4 selection
passed on bwrap. This is not a claim that one full live invocation was green.

## Reproduction entry points

Use the SDK's explicit TLS variables and your own model settings. Do not put
credentials in manifests or command arguments.

```sh
# Inside the EKS runner; current source precedes installed packages.
export PYTHONPATH=/workspace/test-deps:/workspace/source/python/evalsi/src:/workspace/source/python/evalsi-harness/src
/opt/evalsi/venv/bin/python -m pytest \
  /workspace/source/tests/harness/test_eks.py -q
```

Additional live-provider tests require `EVALSI_EKS_ANTHROPIC_DIR`, containing
private `auth-token`, `base-url` and `model` files. The GPU checks require
`EVALSI_EKS_MODEL_URL` and `EVALSI_EKS_MODEL`. Without those explicit settings,
the relevant tests skip rather than substitute a fake provider.

For the stock bwrap service, set its TLS address, preserve the certificate's
original server name with `EVALSI_SANDBOX_TLS_SERVER_NAME`, and set
`EVALSI_EKS_SANDBOX_DRIVER=bwrap`. The selected real-rung controls were:

```sh
/opt/evalsi/venv/bin/python -m pytest \
  /workspace/source/tests/harness/test_eks.py -q \
  -k 'protected or bad_implementation or interjection or truncated'
# PASS: 8 passed on the stock user-namespaced pool.
```

The strict cache-hit checks are intentionally separate from signed-thinking
checks. A successful creation counter must not be reported as cache reuse.

## Complete leftover inventory

These are all **49 item-table rows at the start of the follow-up**, excluding
the already-verified section (before moving the newly verified bwrap row).
Some rows contain built functionality and intentionally unsupported features;
49 is not the old document's count of open engineering tasks. “Pending” means
not verified in this follow-up, not an environmental impossibility.

| # | Leftover | Follow-up disposition |
|---|---|---|
| 1 | Stock Firecracker | Blocked: actual CPU/GPU host KVM devices absent |
| 2 | User-namespaced bubblewrap | Verified with the stock chart and real execution/H4 controls |
| 3 | Calico + ambient egress | Failed before egress measurement in first pass; mesh connectivity unresolved |
| 4 | STRICT-mTLS admission | Pending; non-mesh host-network admission is not a substitute |
| 5 | PR #12 on AKS/Istio | EKS does not certify AKS; mesh rechecks still needed |
| 6 | Trace sources | Prior bounded live MLflow/Langfuse evidence; external Databricks/SageMaker/LangSmith access missing |
| 7 | Deep-research / Terminal-Bench reference demos | Pending; a working harness/model is not these demo suites |
| 8 | H1 frontier handling | Partial: live thinking/streaming/cassette/tools passed; cache reads failed; native OpenAI Responses and thinking/tool roundtrip remain |
| 9 | H2 long horizon | Partial: real process kill/resume passed; context-quality/300-step and real remote MCP certification remain |
| 10 | Persistent shell/background processes | Not implemented; needs evalsid sessions |
| 11 | H3 agents/profiles | Partial: live three-profile and pinned Claude CLI checks passed; other actual CLIs/ACP/transfer experiments remain |
| 12 | H4 environments | Pod and bwrap checker/interjection checks passed; multi-service PR #85 excluded; browser/desktop/importers not built |
| 13 | Framework tracing profiles | All ten live recorder configurations passed source mapping checks inside EKS; native Responses, LiteLLM CrewAI, retries and multi-agent mappings remain |
| 14 | 10k concurrent trials | Requires explicitly sized/dedicated capacity; not inferred from API throughput |
| 15 | Real SWE-bench Verified | Pending real task/image; synthetic calc is not SWE-bench |
| 16 | Unprivileged cgroups | Read-only mount measured; no delegated writable cgroups |
| 17 | Kata | No RuntimeClass installed |
| 18 | Internal webhook URLs | Requires allowlist/default policy decision, not a verification-only fix |
| 19 | Server-side SQL | Slicing built; arbitrary SQL intentionally client-side |
| 20 | Platform wheels | Existing Linux release evidence; macOS/Windows sandbox support not built |
| 21 | Run archiving | Built core; per-label retention/grace period not built |
| 22 | Prior-review limits | Documented upload spooling, batch retry and connector completeness limits remain |
| 23 | Pipeline/schedule certification | Restored persistent run idempotency passed; two same-version PostgreSQL API replicas became ready. Concurrent-create client reconciliation, direct S3 import, schedule and production race/load checks remain pending |
| 24 | Sandbox quota boundaries | Documented exclusions/per-replica concurrency are not global quota enforcement |
| 25 | Keyless sandbox-image verification | Not supported by the offline key-signature verifier |
| 26 | Enterprise audit/delegation | Proposed, not implemented |
| 27 | LongHarness/Evo2 remaining work | Partial mechanisms; semantic grounding, human-labeled stage audit and held-out experiments remain |
| 28 | Quality-aware routing | Not implemented; gateway extension decision required |
| 29 | Separate process roles | Intentionally deferred until load requires them |
| 30 | Pod snapshots | Unsupported; actual replay verified reconstruction in a fresh sandbox |
| 31 | Firecracker tap networking | Intentionally uses vsock/egress proxy |
| 32 | Per-caller rate limits | Design/implementation missing |
| 33 | Reversible migrations | Versioned/down migration machinery missing; no rolling-compatibility claim |
| 34 | Restore/PITR | Isolated same-version PostgreSQL/S3 restore passed exact data, scoped-key authentication, persistent idempotency and fresh dataset execution; PITR/WAL, HA, ClickHouse/JetStream and in-flight recovery remain unverified |
| 35 | Judge injection robustness | Six bounded live controls passed; wider/model-specific certification remains |
| 36 | UI accessibility | Automated check/fixes still needed; not certified by backend tests |
| 37 | Independent penetration test | Requires an outside tester, not this EKS exercise |
| 38 | Second maintainer | Requires another person |
| 39 | BFCL memory/web search | Unsupported categories, not implemented |
| 40 | Native tau2 user | Existing structured-output/judge adapter is not tau2's native LiteLLM user |
| 41 | Multi-service Terminal-Bench | Excluded PR #85 scope |
| 42 | FrontierCode/CursorBench | Private or unverifiable unpublished datasets |
| 43 | verl/OpenRLHF training | Pending real training images/steps; reward adapters alone do not certify it |
| 44 | 4,096-rollout reward scale | Requires 256 warm microVMs/sized KVM pool |
| 45 | Checkpoint watcher/PVC/LoRA | Actual in-cluster source controller and CLI watcher, two full-model PVC checkpoints, read-only controls, synthetic LoRA lifecycle and exception cleanup passed; fixed image release remains separate |
| 46 | Generic PyTorch/Lightning serving | Serving-contract/product decision, not implemented |
| 47 | OpenEnv/verifiers trainer compatibility | Pending actual trainer integration; constructor/unit tests are insufficient |
| 48 | Coding product over MCP | Bounded real CLI + MCP + independently verified artifacts passed in EKS; CI run/secret still separate |
| 49 | Wasm IO/judge/sandbox, Azure ML trace API | Unsupported capabilities/provider API; not made available by EKS |

## Checkpoint follow-up

The isolated `evalsi-checkpoints-20261017` namespace ran a Linux binary built
from the current operator source **inside EKS**, using namespace-scoped RBAC
and the released application image as its runtime carrier. A checksum-verified
kubectl 1.36.0 drove the actual source Python watcher. This did not build or
publish a fixed operator image.

A 4Gi gp3 ReadWriteOnce PVC held two hard-linked copies of
`Qwen/Qwen2.5-0.5B-Instruct` and a synthetic rank-one zero-LoRA adapter. The
current controller plus stock vLLM 0.11.0 verified:

- `/v1/models` identified `/checkpoints/checkpoint-1`, not an image default.
- A real model container could neither overwrite an existing sentinel nor
  create a new file on the read-only PVC (`EROFS` in both controls).
- LoRA load, model registration, nonzero-token inference and unload worked;
  an unloaded model returned 404 and an absent adapter raised `ServingError`.
- `checkpoints watch --once --serve operator` discovered both full
  checkpoints, created a server for each, scored nonempty actual model output,
  saved two results and deleted both servers/Deployments/Services. The score
  checks availability, **not model quality**.
- Repeating the scan made no new servers and preserved result bytes.
- A deliberate evaluation exception deleted its real server and children.
- Two actual model-404 evaluation failures reproduced exit 0 from the old
  one-shot watcher. The fix returns 1 for attempted evaluation errors, 3 for
  failed gates/regressions, with errors taking precedence. Continuous watching
  and stop-file behavior remain unchanged. Eight new negative regression
  variants failed before the fix.

The initial manifest requested two CPUs, exceeding existing GPU-node
headroom; requesting one allowed scheduling. EKS automatically injected
the NVIDIA extended-resource toleration, so no scheduling product fix was
needed and no existing workload or taint was changed.

`tests/training_hooks/test_eks_checkpoints.py` plus the entire training test module ran
inside EKS: **33 passed in 162.16 s**. After the live test was refactored and
formatted, that final source test passed again in **154.00 s**.
Local Ruff and mypy passed. The live test opts in with
`EVALSI_EKS_CHECKPOINTS=1`, `EVALSI_EKS_CHECKPOINT_NAMESPACE`,
`EVALSI_EKS_CHECKPOINT_CLAIM`, kubectl on PATH and the documented mounted
checkpoint/adapter prerequisites.

## PostgreSQL/S3 recovery follow-up

The disposable `evalsi-recovery-probe-20261017` namespace contained separate
source and target PostgreSQL 17/MinIO/NATS stores and application releases.
Both applications used image `0.2.0`, mutual TLS, explicitly configured JWT
verification and API-key authentication. This is a same-version restore,
not an N/N-1 upgrade.

The source fixture uploaded two synthetic records to S3 and completed an
exact-match run: mean **0.5**, two stored results, intentionally failed gate
at 0.75. A project-scoped viewer API key successfully read it. All source
API/worker Deployments were then stopped, selecting them by their actual
Helm release annotation as documented in the operations guide. Target
writers were also stopped. PostgreSQL and MinIO remained running.

`pg_dump --format=custom --no-owner` and a complete current-object `mc mirror`
were streamed into private, encrypted off-cluster backup files; ciphertext
SHA-256 checks passed. The target had **zero public tables** and an **empty
object bucket** immediately before restore. `pg_restore --no-owner
--exit-on-error` and object restoration succeeded before target writers
started. Reverse mirroring found exactly the one expected 210-byte object,
whose SHA-256 matched the upload:
`02a20bf035380b1cef6037b6c3620bcb8234e29b6391fa4ce1b94ed7f3bcde77`.

The authenticated target then verified:

- The restored project-scoped API key still authenticated as an API key.
- The completed run and its stored results matched the saved API responses
  exactly, including the failed gate and mean 0.5.
- Repeating the original keyed CreateRun returned the **same run ID** and
  did not create another run.
- A fresh run read the restored S3 dataset and reproduced two results,
  mean 0.5 and the expected failed gate.

The drill exposed the PostgreSQL StorageClass bug: despite setting
`devPostgres.storageClassName=gp3`, the original template omitted it and the
real PVC reported `FailedBinding: ... no storage class is set`. After the
fix, only the never-bound empty probe claim/StatefulSet were replaced; the
same chart selected gp3, bound the claim and started PostgreSQL.

An additional two-replica same-version API startup passed readiness. An
eight-way simultaneous keyed-create probe stopped when a caller received
the documented in-flight/uncertain `Unavailable` response; the probe did
not yet implement same-key reconciliation/retry. **It is not recorded as a
successful concurrent-create certification or a confirmed product bug.**
The direct S3-import call after that assertion was not reached. These
CPU-only follow-ups are paused while GPU work is prioritized.

This drained fixture does **not** certify managed PITR/WAL, PostgreSQL
failover/HA recovery, mixed-version rolling compatibility, ClickHouse or
JetStream snapshot restoration, source cursors, pending webhook delivery or
recovery of in-flight work. Backups/keys are private session artifacts, not
repository fixtures.

## Pre-commit validation

In addition to the EKS measurements above, the checkpoint ran:

| Command/check | Result |
|---|---|
| `go test -race ./internal/ingest ./operator/controllers ./tests/helm -count=1` | Passed all three affected packages; live EKS exports were separately checked in EKS |
| `go vet ./internal/ingest ./operator/controllers ./tests/helm` | Passed |
| `python/.venv/bin/python -m pytest python/evalsi/tests/test_training.py python/evalsi-harness/tests/test_formats.py -q` | 40 passed |
| `python/.venv/bin/python -m pytest tests/harness/test_eks.py tests/training_hooks/test_eks_checkpoints.py -q` | 30 skipped as intended without live opt-in flags; both modules collect together |
| Ruff lint and format check on all eight changed Python source/test/recorder files | Passed |
| mypy on both changed runtime modules and their four affected unit/integration modules, with `--follow-imports=silent` | Passed, six source files; optional framework recorder dependencies are not claimed type-certified |
| `git diff --check` | Passed |

The checkpoint integration module was given a distinct
`test_eks_checkpoints.py` filename after combined mypy initially reported
duplicate `test_eks` modules. This changes no live test behavior.

## Resource/evidence status

The follow-up uses only the owned `evalsi-harness-20261017`,
`evalsi-checkpoints-20261017` and `evalsi-recovery-probe-20261017` namespaces.
Releases are `evalsi-harness`, `evalsi-harness-crds`,
`evalsi-harness-bwrap`, `recovery-source` and `recovery-target`.
No existing namespace policy or host configuration was changed. At this
intermediate record, resources remain active for the subsequent checks;
the first report's cleanup statement applies to its first pass only.
The harness GPU deployment was scaled to zero and temporary provider files
were removed before this checkpoint phase. All checkpoint model-server
children were deleted; its controller/preparer and PVC remain for subsequent
checks. Recovery source/target API and CPU worker Deployments are scaled to
zero, and the temporary plaintext test API-key file was removed; their
storage/dependency pods remain. Twenty-two sanitized harness/framework artifacts,
both checkpoint JUnit reports and a credential-free recovery summary were
copied to private session storage. Raw provider transcripts, backups and
credential files are not committed.

# Leftovers verification, 2026-10-17

The 50 current item rows in `docs/LEFTOVERS.md` were audited on PR #85,
starting at `b342df8`. Nineteen rows have bounded executable evidence in this PR;
the table below records the requirement remaining for every row. The previous
42-row total was stale. A passing bounded check does not close unimplemented
features or certify production operation.

[Machine-readable inventory and results](LEFTOVERS-2026-10-17.json) include all
50 rows, runtime capabilities and the individual check reports. No CI result
was used as a completion gate for this work.

## Combined EKS checkpoint

PR #85 also incorporates `verify/eks-leftovers@42d8127`, including its five
runtime fixes, regression tests and [first EKS record](EKS-2026-10-17.md)
plus [harness/checkpoint/recovery follow-up](EKS-HARNESS-2026-10-17.md).
Those measurements were made in a separate EKS environment before this merge,
excluding multi-service PR #85 behavior; this local kind record does not claim
to have reproduced EKS/GPU/provider checks. The follow-up's 49 starting rows
exclude the host-network admission row already moved to its verified section;
this inventory retains all 50 original rows with stable IDs.

Completed evidence includes real GPU inference and checkpoint watcher/PVC/LoRA,
live Anthropic/GPU harness profiles, signed thinking, four context strategies,
process interruption/resume, pinned Claude CLI/MCP, ten live framework exports,
six judge controls and stock user-namespaced bubblewrap on EKS. Direct harness
prompt-cache reuse controls **failed**; Qwen code-mode also failed. Neither is
converted to a pass. Trainer-backed verl/OpenRLHF and OpenEnv/verifiers checks
remain pending in the published checkpoint while the separate GPU run continues.

Four rows now pass their bounded verification requirement and fifteen have
partial evidence. These counts describe this combined inventory, not nineteen
closed engineering tasks or production certification. The runtime capabilities
below describe this local workspace only; EKS had an L40S and supplied model
credentials. No EKS resource was accessed or modified during this merge.

## Environment and scope

A real single-node kind/Kubernetes 1.35.0 cluster ran Calico 3.30.3 and Istio
1.30.6. The node had three allocatable CPUs; the workspace had a 32-GB filesystem.
There was no `/dev/kvm`, NVIDIA device, Kata RuntimeClass, writable cgroup
hierarchy, configured model secret or cloud identity. Chromium was available.
The normal GitHub connection remained available for this repository.

New fixture namespaces were `evalsi-leftovers`, `evalsi-leftovers-restore` and
`evalsi-ambient`. The first two used actual PostgreSQL 17, MinIO and NATS,
with a derived image containing this checkout's binaries and Python packages.
The source application had two API replicas and one CPU worker. The operator
was exercised first on the host network and then with a real Istio sidecar.
The service-pool API used mutual TLS.

This nested environment required the existing kind node's native containerd
snapshotter, proxy CA and local image importer. The larger verification image
was flattened before importing because native snapshots copy filesystem layers.
Its application code came from `b342df8`; these local infrastructure checks predate
the imported EKS Go fixes.
The subsequent MCP/probe fixes were tested directly in the host Python checkout.
The checked-in replay Dockerfile also built successfully and passed a nonroot
binary/import smoke check; that final image was not redeployed for these checks.
An initial image attempt also exposed fixture file-mode and HOME mistakes;
executable modes and the worker HOME were corrected before recording successes.
These were verification setup issues. The standalone root product Dockerfile
and stock Firecracker image are not certified by this derived-image run.

## What ran

| Check | Actual result | Practical limit |
| --- | --- | --- |
| User-namespace preflight | A real `hostUsers: false` pod failed before starting because containerd could not map container ID 2073755648 to a host ID | This host/runtime profile remains unavailable |
| Protected files on pod and bubblewrap pools | Honest fix passed; tampering failed; restore rejected cheating and accepted a real fix; directory/symlink tampering restored correctly | Scripted CLI agent; existing checker boundary |
| User interjections on both pools | Message reached the scripted model after its tool call and appeared as a trajectory step; checker passed | Not a real model, CLI interjection, or event-timed feature |
| Worker SIGKILL/resume | Two completed tool turns survived on disk; resumed worker made only three remaining model calls; nine cassette events had unique sequential IDs; final checker passed; completed replay made zero model calls | Replays tools into a fresh sandbox; not external-side-effect idempotency or run-manager adoption |
| Context strategies with a published tokenizer | ByT5Tokenizer from transformers 4.57.1 counted serialized system/messages/tools. All four strategies completed 120 tool turns below a 64,000-token window; control overflowed after five calls at 67,625 tokens | Scripted model/summary, not model quality or provider billing envelopes |
| MCP interoperability | Published mcp 1.26.0 FastMCP server plus Authlib 1.6.5: anonymous/wrong-secret rejection, tools, resource read, SSE elicitation, token cache, 401 refresh, authenticated session termination | Local ephemeral OAuth authority, not an external customer's identity provider |
| Host-network operator | Shipped chart option produced node IP and ClusterFirstWithHostNet; actual API-server admission call rejected an invalid gate | kind topology, not EKS networking |
| STRICT mTLS webhooks | Actual sidecar operator under namespace and mesh-wide STRICT; valid request blocked without port exception, admitted after restoring chart policy; invalid gate rejected | One Istio version and local cluster |
| Ambient NetworkPolicy | Leak reproduced; single sandbox reported partial isolation and services were refused. Explicit pool/trial ambient opt-out then passed service grading, cross-trial denial, crash and cleanup checks | Native ambient interception did not enforce the intended boundary |
| S3-compatible/PostgreSQL pipeline | Dataset bytes in MinIO matched upload hash. Sixteen concurrent creates across two API replicas resolved to one run and 16 unique results; conflicting payload was rejected; sixteen deletion retries succeeded | Same-key in-flight requests retry the documented unavailable response; no database failover or AWS S3 |
| Archiving | Summary retained, result reads refused, including after restoring the database | No grace period, backup erasure or per-label retention added |
| Quiesced restore | All source Deployments stopped and pods disappeared. PostgreSQL custom dump and complete dataset bucket restored into an empty installation; every object hash matched; authenticated completed/archived runs read correctly; restored dataset reran all 16 records | Fixture had no pending jobs, ClickHouse traces or external callbacks to restore |
| PostgreSQL point-in-time recovery | Physical base backup plus archived WAL recovered rows 1 and 2, excluded committed row 3 after the selected target, and promoted a fresh recovery instance. Recovery stage took 5.902 seconds | Local fixture archive copied off-pod; not managed/off-site PITR or a production RPO/RTO |
| Previous reader | Published v0.1.0 binary with matching v0.1.0 worker read 16 completed results and an archived summary from a separate copy of the PR database schema | Not N/N-1 concurrent writes, rolling upgrade certification or down migrations |
| Checkpoint controller | Actual controller created owned Deployment/Service, reported ready endpoint, used Recreate, omitted service-account token, and cleared readiness on scale-to-zero | HTTP stand-in; no GPU/vLLM inference claim |
| OTLP redaction | Synthetic credential absent from retained NATS bytes, PostgreSQL record bytes and authenticated API response; `[REDACTED]` present in each | Push path and SQL storage; no new pull-provider or ClickHouse certification |
| Browser accessibility | Five real Chromium tests passed; 20 axe-core scans covered WCAG 2 A/AA and 2.1 A/AA tags, six main sections in both palettes, sign-in and annotation; Tab/Enter navigation passed | Automated rules and navigation, not a complete manual screen-reader audit |

The context-tokenizer check uses the published ByT5 tokenizer directly, with
no model weights or vocabulary download. The initial cl100k vocabulary host
was denied by the environment; no network policy was bypassed. The control and
all four strategies use the same actual tokenizer.

The real SWE-bench image manifest for
`swebench/sweb.eval.x86_64.sympy_1776_sympy-20590:latest` was readable and contained
1,088,521,647 compressed layer bytes. The multi-GB image and writable/native
copies were not run within the remaining disk budget of this populated 32-GB
workspace at that measurement. The local cluster was removed after the recorded
infrastructure checks to recover space for combined-source regression tests. The public dataset filter endpoint also returned HTTP 500. No
execution result is claimed for a real SWE-bench task.

## Bugs fixed

- MCP session DELETE previously omitted configured and OAuth authentication
  headers. The published SDK server returned 401, leaving the session alive.
  Termination now sends the protocol/session/auth headers, refreshes once on
  401, has a five-second overall bound, and always closes the HTTP client.
  Regressions cover refreshed termination and failed token acquisition.
- Python sandbox probes discarded the server's warning field, hiding the
  explanation when the ambient canary detected a leak. Probe dictionaries now
  retain `warning`, including on an otherwise available backend.
- The existing browser CSP check cleared its expected console error before the
  browser had delivered it. It now awaits that exact security event before
  checking for unrelated errors; CSP enforcement stays enabled.

Before the EKS merge, the new SDK/harness regressions and complete Python
suite passed: **913 passed, 3 existing skips**. After merging the completed EKS
checkpoint, the entire locked development suite passed **924 tests**, with the
same **3 existing skips**, in 80.82 seconds. The two opt-in EKS test modules
also collected together with **30 skips**, as expected without live settings;
that local collection is not a rerun of the separate EKS evidence. The existing fork deprecation warning remains.
Ruff/format passed, and mypy was run with the repository's locked development
set rather than the optional live-tool dependencies. Browser npm audit found
zero vulnerabilities. No tests were disabled.

The existing PR review also identified private service HTTP requests being sent
through the external egress proxy. All six new routing regressions failed before
the fix (allow/allowlist modes, default/spec/command exclusions). Both `NO_PROXY`
and `no_proxy` now append declared service names after environment overrides,
preserving existing exclusions. The regression checks private direct routing and
external proxy selection; it passed 100 race-detector repetitions, and the full
service-test selection passed, including 100 repetitions after validating the
allow/allowlist fixture configurations. This routing regression is local Go coverage,
not a new live-cluster run after teardown.

The combined-source Go race suite passed for `internal/ingest`,
`operator/controllers`, `operator/api/v1alpha1`, `operator/spec` and
`tests/helm`, with `EVALSI_REQUIRE_HELM=1`; `operator/webhook` has no standalone
test files. Optional envtest is not claimed. The combined-source Go vet checks passed for ingest, all operator packages,
the operator command, Helm tests and sandbox. Additional type checking passed
for the six imported harness/training runtime and unit/integration modules.
Optional EKS recorder dependencies are not included in that type-check claim.

## Replaying the checks

Use a fresh disposable cluster for the infrastructure-changing checks. The
scripts have fixed fixture namespaces; do not point them at a production
installation. Start the baseline pool with `deploy/e2e/services-kind.sh`, or use
the already documented managed-workspace adjustments in the multi-service
verification record. The scripts require kubectl and the fixture kubeconfig.

Keep optional verification dependencies separate from the development venv:

```sh
uv venv /tmp/evalsi-verification
uv pip install --python /tmp/evalsi-verification/bin/python \
  -r deploy/e2e/leftovers-requirements.txt \
  -e python/evalsi -e python/evalsi-harness
```

Build `evalsid`, `evalsi-guest` and `evalsi-operator` into `bin/` with
`CGO_ENABLED=0`, then build/preload `evalsi-leftovers:test` using
`deploy/e2e/leftovers.Dockerfile`. For an intercepting proxy, pass
`--secret id=proxy_ca,src=/path/to/ca.pem`. The secret is used only during the
networked install step. Preload `postgres:17-alpine`,
`bitnamilegacy/minio:2025.7.23`, `nats:2.12-alpine`, `python:3.13-slim` and the
pool image. The native-snapshotter workspace flattened the verification image
and selected that tag; ordinary overlay hosts need not flatten it.

Install the chart/CRDs in `evalsi-leftovers`, with fullname `leftovers`, using
`deploy/e2e/leftovers-values.yaml`. CRD chart values must select operator
namespace `evalsi-leftovers` and fullname `leftovers`. Wait for both API replicas,
the CPU worker, operator and dependencies to become ready. Set up API forwards
18080 and 18083 to **different API pods**, and 19000 to MinIO. Scripts generate
short-lived tokens internally without printing them.

```sh
VERIFY_PY=/tmp/evalsi-verification/bin/python
umask 077
mkdir -p /tmp/evalsi-private-backup
$VERIFY_PY deploy/e2e/leftovers_storage.py pipeline --work-dir /tmp/evalsi-private-backup
$VERIFY_PY deploy/e2e/leftovers_storage.py backup --work-dir /tmp/evalsi-private-backup
```

Backup intentionally stops the source application's Deployments. Provision an
empty namespace `evalsi-leftovers-restore`, fullname `restored`, with fresh
PostgreSQL and MinIO, the same dataset bucket/prefix and application version,
all API/worker/operator replicas stopped. The reference run rendered the chart
and set every Deployment's replicas to zero before applying it. Restore refuses
a database with public tables or a nonempty bucket. Redirect 19000 to the new
MinIO, restore, then start the API and CPU worker and redirect 18080 to the
restored API. The restored fixture grants the `verify` service account owner
access; create that service account before checking.

```sh
$VERIFY_PY deploy/e2e/leftovers_storage.py restore --work-dir /tmp/evalsi-private-backup
$VERIFY_PY deploy/e2e/leftovers_storage.py check-restored --work-dir /tmp/evalsi-private-backup
```

`leftovers_pitr.py` requires those restored synthetic fixtures and stopped
API/worker pods. It enables local WAL archiving, takes a base backup, writes
before/after-target markers, copies the base/WAL archives privately, and
restores into a fresh disposable recovery pod. Use an empty private work
directory. It leaves local archiving enabled on the fixture source and removes
the recovery pod. Its local archive is not a production backup design.

`leftovers_compat.py` takes a published v0.1.0 binary, a separate matching
published Python worker, the saved state file and a new private work directory.
Provision a separate `compatibility` database from the dump first and forward
PostgreSQL on 15432. It deliberately never runs the old reader against the
restored application's live database.

For redaction, forward restored OTLP HTTP on 19118 and NATS on 14222, then run
`leftovers_redaction.py --report /tmp/redaction.json`. For checkpoint-controller
checks, run `leftovers_checkpoint.py` while the source operator is live; it
creates and deletes only its named CheckpointServer fixture.

The independent local checks accept `--report` paths:

```sh
$VERIFY_PY deploy/e2e/leftovers_mcp.py --report /tmp/mcp.json
$VERIFY_PY deploy/e2e/leftovers_tokenizer.py --report /tmp/tokenizer.json
# Export the pool's sandbox address and mutual-TLS paths first.
$VERIFY_PY deploy/e2e/leftovers_harness.py --driver pod --image python:3.13-slim --report /tmp/harness-pod.json
$VERIFY_PY deploy/e2e/leftovers_harness.py --driver bwrap --report /tmp/harness-bwrap.json
$VERIFY_PY deploy/e2e/leftovers_resume.py --work-dir /tmp/resume --report /tmp/resume.json
EVALSID="$PWD/bin/evalsid" EVALSI_UI_WORKER="$PWD/python/.venv/bin/python -m evalsi" \
  CHROMIUM_PATH=/usr/bin/chromium npm test --prefix tests/ui
```

For the mesh checks install Istio base/istiod/CNI/ztunnel 1.30.6 using the ambient
profile. This IPv4-only nested node required CNI `ambient.ipv6=false` and a CNI
DaemonSet restart after changing its ConfigMap: the default IPv6 CONNMARK rule
could not load the host module. Mesh-wide STRICT was installed in istio-system.
The `leftovers_webhooks.py` runner accepts `--mode host-network` or `--mode strict`
after selecting that chart profile, and writes a JSON report. The webhook check
used a sidecar-injected operator and its actual chart policy;
removing the policy and restarting the operator forced a fresh rejected API
server connection, then restoring it admitted the valid dry run.

For the ambient test, create `evalsi-ambient` with
`istio.io/dataplane-mode=ambient`, install a copy of `services-pool.yaml` in that
namespace (retain the image tag `evalsi-services:test`), and forward its pool on
17445. `leftovers_mesh.py` verifies the leak/refusal, recreates that dedicated
pool with explicit mesh opt-out labels and checks the safe configuration.
Changing mesh labels alone does not clear a pool process's cached probe result.

## Every leftovers row

| ID | Item | Result / remaining requirement |
| --- | --- | --- |
| L01 | Stock Firecracker | **blocked hardware:** No /dev/kvm; no stock-image microVM claim. |
| L02 | User-namespaced bwrap pools | **checked passed:** Stock source chart with hostUsers:false passed mTLS/full-enforcement probe, good/bad execution and eight H4 controls on EKS AL2023/containerd. Nested kind UID-mapping preflight failed; AKS/idmap host limits remain. |
| L03 | Calico with Istio ambient | **checked partial:** Reproduced canary leakage and partial isolation; unsafe service allocation refused. Explicit pool and trial mesh opt-out passed real isolation checks. |
| L04 | EKS overlay webhooks / hostNetwork | **checked passed:** Actual chart hostNetwork admission passed on EKS with invalid-gate rejection, unavailable-webhook fail-closed and recovery controls; local kind chart admission also passed. Overlay pod routing is not certified. |
| L05 | STRICT mTLS webhooks | **checked passed:** Real Istio sidecar operator under mesh-wide STRICT: valid request blocked without exception, admitted after chart policy restored; invalid request rejected. |
| L06 | PR #12 cloud/mesh recheck | **checked partial:** New local Istio admission and sandbox controls exercised relevant paths; AKS/KVM/waypoint-specific fixes remain open. |
| L07 | Trace sources / M3 | **blocked credentials:** No Databricks, SageMaker or LangSmith/provider credentials. New cluster push-redaction check covers NATS, PostgreSQL and API, not those pull providers. |
| L08 | Reference demos | **blocked credentials:** Deep-research and Terminal-Bench 2 require real model/judge access; existing small-repo evidence is retained. |
| L09 | Frontier models / H1 | **checked partial:** EKS live Anthropic tools/streaming, signed thinking and exact cassette replay passed. Direct prompt-cache reads failed in both modes; native Responses and thinking-plus-tool roundtrip remain open. |
| L10 | Long-horizon agents / H2 | **checked partial:** Published ByT5 tokenizer exercised four strategies; real MCP SDK/Authlib OAuth/elicitation and local SIGKILL/resume passed. EKS live four-strategy checker/replay and fresh-pod process resume passed; 300-step/context-quality and external-side-effect idempotency remain open. |
| L11 | Persistent shell processes | **implementation required:** A persistent interactive shell is not built; per-trial sidecars do not implement it. |
| L12 | CLI agents / H3 | **checked partial:** EKS pinned Claude Code 2.1.296, live GPU/Anthropic profiles and fresh-pod replay passed. Qwen code-mode failed. Other CLIs, ACP and transfer experiments remain open; the GitHub live workflow is separate. |
| L13 | Environments / H4 | **checked partial:** Honest/tampering/restore/symlink/interjection checks passed on real pod and bwrap pools. Browser/desktop importers and clean-snapshot checker are not built. |
| L14 | Framework mapping profiles | **checked partial:** All ten pinned live framework recorder configurations passed mapping contracts in EKS. Native Claude cached-input total fixed. Missing source usage/answers remain absent; native Responses, retries and multi-agent coverage remain open. |
| L15 | 10k concurrent agent trials | **blocked scale:** One 3-CPU node is insufficient. The PR records 48 trials at concurrency four, not 10k certification. |
| L16 | Real SWE-bench Verified images | **blocked scale:** Public sympy__sympy-20590 image manifest inspected (1,088,521,647 compressed layer bytes). A multi-GB image plus Docker/native copies exceeds the safe spare-disk budget of this 32-GB workspace; dataset filter endpoint also returned HTTP 500. No real-task execution claimed. |
| L17 | Unprivileged cgroup limits | **known limit:** /sys/fs/cgroup is not writable; this runtime cannot certify delegated cgroup limits. |
| L18 | Kata Containers | **blocked hardware:** No Kata RuntimeClass installed and no /dev/kvm. |
| L19 | Private webhook destinations | **decision required:** Private addresses are intentional; a default/allow-list policy must be chosen before a restriction can be verified. |
| L20 | Server-side arbitrary SQL | **known limit:** Not planned; implemented server-side slices are distinct from unrestricted SQL. |
| L21 | Platform wheels | **previously verified:** Linux wheels are already verified in the existing release record; macOS/Windows sandbox archives do not exist. |
| L22 | Run archiving / retention | **checked partial:** Live PostgreSQL archiving retained summary and denied result reads, including after restore; grace periods/backup erasure/per-label retention are not built. |
| L23 | Prior review limits | **known limit:** Existing specified limits remain: double dataset spooling, no log retry, and skipped source omissions; no new implementation claim. |
| L24 | Pipeline / schedule certification | **checked partial:** Real MinIO/S3 upload hash, two-API PostgreSQL idempotency create/delete races, terminal results and archive passed. AWS S3, database failover, MCP idempotency, schedule write-back and production load remain open. |
| L25 | Sandbox-time quota boundaries | **known limit:** Existing metering exclusions and per-replica concurrency remain documented; no broader quota implementation exists to certify. |
| L26 | Keyless sandbox image verification | **implementation required:** Sandbox-image keyless/referrer verification is not built; existing release-signature verification remains separate. |
| L27 | Enterprise audit / delegation | **implementation required:** Strict/read auditing, identity context, hash chaining and export guarantees are proposed, not implemented. |
| L28 | LongHarness / Evo2 follow-ups | **blocked credentials:** Live held-out experiments, semantic support and judge/human-label stage validation need models/data; missing long-document importers remain unbuilt. |
| L29 | agentgateway score feedback | **decision required:** Requires a supported routing-extension design before implementation/testing. |
| L30 | Separate process roles | **known limit:** Not planned absent load evidence; shared API/ingest roles remain. |
| L31 | Pod snapshots | **implementation required:** Pod snapshots are not implemented; setup runs per trial. |
| L32 | Firecracker tap/nftables | **known limit:** Not planned; current vsock/proxy path is intentional, and KVM is absent here. |
| L33 | Per-caller rate limits | **implementation required:** Only existing size/audit/JWKS limits exist; throttling unit/defaults are not designed or built. |
| L34 | Versioned reversible migrations | **checked partial:** Published v0.1.0 server and matching worker read 16 results and an archived summary from a PR-schema database copy. Migration table/down steps and N/N-1 concurrent writers remain unimplemented/unverified. |
| L35 | Restore and PITR | **checked partial:** Quiesced PostgreSQL/dataset backup restored into empty namespace/database/bucket; authenticated old run and 16-record rerun passed. Disposable WAL recovery is recorded separately; managed/off-site recovery and production RPO/RTO remain open. |
| L36 | Judge prompt injection | **checked partial:** Six live EKS/Anthropic positive, wrong-answer and injection controls passed explicit thresholds. Broader deployed-judge coverage and human-labelled calibration remain open. |
| L37 | UI accessibility | **checked passed:** axe-core 4.10.3 WCAG 2 A/AA and 2.1 A/AA scans, light/dark main sections, sign-in, annotation, and keyboard navigation passed in actual Chromium. Automated coverage is not a complete manual accessibility audit. |
| L38 | Independent penetration test | **blocked external:** An independent tester/security assessment is unavailable; self-tests cannot replace that requirement. |
| L39 | Second maintainer | **blocked external:** Requires an actual willing person and repository access; no owner is invented. |
| L40 | BFCL memory / web search | **implementation required:** Categories are not implemented; existing multi-turn support is retained. |
| L41 | tau2 native LiteLLM user | **known limit:** Current judge-driven user is intentional; no native LiteLLM-user implementation is claimed. |
| L42 | Terminal-Bench unsupported subset | **checked partial:** The PR already audited 241 tasks, imported 219, and ran an adapted original task with positive/negative controls. Full Compose and multi-stage builds remain unsupported. |
| L43 | FrontierCode / CursorBench | **blocked access:** Publicly verifiable task format/private task data is unavailable. |
| L44 | verl / OpenRLHF training | **pending verification:** GPU training work is ongoing separately on verify/eks-leftovers. Its published checkpoint records inference, not actual verl/OpenRLHF training steps; no training completion claimed here. |
| L45 | Reward throughput at target scale | **blocked scale:** Target assumes 256 warm microVMs; no KVM or appropriately sized pool here. |
| L46 | Operator checkpoint serving | **checked partial:** EKS actual L40S/vLLM model identity, inference, API-key rollout/deletion and source CLI watcher over two PVC checkpoints passed. Read-only controls, LoRA load/infer/unload and error cleanup passed; wrong-model launch and one-shot exit status fixed. Fixed image release and training quality remain separate. |
| L47 | Checkpoint serving strategy | **decision required:** Compatible serving of arbitrary state dictionaries/Lightning checkpoints needs a serving-strategy choice. |
| L48 | Environment-spec expansion/training | **implementation required:** Published OpenEnv/verifiers integration evidence remains; other adapters/shared setup/trainers are not implemented or need an actual training workload. |
| L49 | Coding-agent MCP CI | **checked partial:** Actual EKS Claude CLI invoked source MCP over stdio; independent artifact checks confirmed mean 2/3 and failed gate. GitHub mcp-agent secret/first CI run, other coding products and external remote OAuth remain separate. |
| L50 | Wasm I/O / Azure ML / SQL limits | **known limit:** Wasm I/O is deliberately absent, Azure ML lacks the required V3 trace API, and arbitrary server SQL is not planned. |

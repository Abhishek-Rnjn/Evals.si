# AKS onboarding and Sonnet re-verification, 25 October 2026

This continues the [PR #154/#155 Mac verification](../tutorials/12-macos-reverification.md)
in [combined PR #161](https://github.com/Abhishek-Rnjn/Evals.si/pull/161).
The user authorized the broader historical AKS backlog, the better available
node, and up to **$10 additional model spend**, then requested Sonnet with
thinking/tool-token checks. This is a new carried ledger, not the historical
$39.50 campaign. See the [walkthrough](../tutorials/13-aks-onboarding.md) and
[sanitized measurements](data/aks-onboarding-2026-10-25.json).

## Scope and provenance

- Three existing nodes were inspected. All new workloads selected the sole
  `Standard_D8s_v5`: 8-vCPU/32-GiB class, 7.82 allocatable CPU and approximately
  26.3 GiB allocatable memory. The other two were system-tainted D4 nodes.
- A uniquely owned namespace held every test pod, dependency and service.
  Injection was disabled there, not cluster-wide. No shared node, CNI,
  RuntimeClass, CRD, controller, registry or cloud resource was changed.
- The source binary and compiled e2e test came from pushed head `c00ed23`.
  Current-source Python reported `0.3.1.dev0`. A separate fresh environment
  installed **evalsi==0.3.1 and evalsi-harness==0.3.1** from PyPI inside AKS.
- The main worker requested 4 CPU/8 GiB and was capped at 6 CPU/12 GiB.
  A separate privileged outer pool, capped at 2 CPU/4 GiB, enabled synthetic
  nested-bubblewrap/KVM checks. Neither pod mounted host files or a service-
  account token. This is not a production privileged-worker recommendation.
- Dependencies were digest-pinned public images on `emptyDir`, reachable
  through ClusterIP only. This did not perform a new Helm rollout, source-image
  registry publication, persistent-volume installation or release certification.
- Live provider calls ran on the Mac; tools and checkers ran through the real
  AKS mTLS pool. Provider credentials never entered Kubernetes. The separate
  published TinyLM exercise ran **entirely inside AKS**.
- The new reusable lab manifest was derived from the runtime setup, unit-
  checked and admitted by real AKS **server dry-run**. Its simplified worker
  template was not separately rolled out as a second deployment.

## Local validation and fixes

| Command | Result |
|---|---|
| `make check` | Passed after the fixes below: Go vet/tests, Helm checks, 339 Python files formatted, mypy on 352 files, **1,782 passed / 35 skipped**, Buf lint/format |
| `make e2e` | Passed |
| `go test -race ./internal/...` | Passed |
| `make adapters-check` | Passed: 61 tests across six adapters; tau-bench's 17 live-prerequisite tests skipped |
| `cd tests/ui && npm test` | **14/14 passed**, using the installed native Chromium browser |
| `python/.venv/bin/python -m pytest python/evalsi-harness/tests/test_diffcheck.py python/evalsi-harness/tests/test_approval.py tests/verification/test_aks_followup.py tests/verification/test_long_horizon.py tests/verification/test_preflight.py -q` | **86 passed** |
| `cd website && npm run build && node scripts/check-links.mjs` | Passed: **120 pages**, `links ok` |
| Ruff check and format-check on the five changed Python verification helpers, from `python/` | Passed; all five files already formatted |

Buf 1.73.0 was installed privately after the missing-tool failure; no global
installation was changed. Local Go parallelism was bounded. The helper lint
commands must run from `python/` so the repository's operational-runner
per-file settings apply.

Three real portability/correctness problems were fixed:

1. Coupled bubblewrap service tests referenced Linux-only namespace fields on
   macOS. Both test files now have a Linux build constraint; production sandbox
   code was not disabled.
2. BSD `chmod` does not accept `--` after the mode as GNU chmod does.
   Diff application now passes safe `./`-prefixed relative operands. Executable
   and non-executable option-like filenames have regressions.
3. Copying a Git index could retain stat metadata and miss a real same-size
   tracked-file change. Diff inventory forces tracked-content rehash with
   `git add --renormalize` before adding all changes. The actual repository
   change is asserted, not merely the presence of a diff object.

The full compiled AKS e2e run also passed, with opt-in/environment-dependent
skips retained. Its first attempt failed because a runtime Wasm build inherited
a Mac `GOROOT`. A checksum-verified Go 1.26.10 Linux toolchain and explicit
`GOROOT=/work/go` corrected the environment; that initial failure is retained.

## First-install and sandbox results

| Exercise | Outcome |
|---|---|
| Nonprivileged worker, strict `doctor --require agent` | Correctly refused: no available namespaced backend, no silent unconfined fallback |
| Real mTLS source sandbox pool | `bwrap` available, reported isolation `namespaced` |
| Source-host SDK/harness and TinyLM, remote AKS tools | **18/18 scenarios matched expected exits and record shapes** |
| Fresh published 0.3.1 packages, TinyLM and tools inside AKS | Strict doctor passed; **18/18 scenarios passed** |

An initial two-slot pool was too small for tutorial concurrency; eight slots
resolved it. Missing Git caused a second setup failure; Git was installed only
in the disposable outer pod, then the unchanged scenarios were rerun.

## Real Haiku, approval controls and Sonnet

All orders and refund amounts were synthetic. Rates used for conservative
accounting were Haiku $2.20/$5.50 and Sonnet $4.40/$11 per million input/output
tokens, counting all cache input at the full input rate. Reservations include
serialized request bytes plus overhead and happen before every HTTP attempt.
Failed/uncertain provider attempts retain their reservation. These estimates
are **not a gateway-enforced invoice ceiling**.

### Haiku

Explicit model alias: `claude-haiku-5-5`, thinking disabled.

- Twelve records, one trial, baseline and gated profiles: **24 live records**,
  four replay controls, 116 HTTP attempts, estimated $1.50538640.
- One genuine baseline business-check miss: `ask-for-more-than-paid` refunded
  zero instead of the checker-expected capped $25 for order A-1003.
- The gated timeout case intentionally denied order A-1005: the business
  checker failed, while denial-respected and approval-coverage passed.
  Replay faithfully retained that failed business outcome.
- Approve/edit/no-answer controls added three live records, three exact
  replays and three changed-approver rejections before provider access.
  No-answer intentionally prevented the refund. Applicable policy scores passed.
- Cumulative after Haiku: **132 HTTP attempts, $1.70734530**.

The historical denial-respected self-denial false positive was already fixed
on current source; this run did not newly fix that evaluator. A new helper
regression does prevent relative recording paths from placing tapes under
the repository's example directory.

### Sonnet: better bounded task outcome, no exposed thinking

Explicit model alias: `claude-sonnet-5-5`. The same refund policy, state checker,
18-step limit and real AKS tools were retained. Sonnet was tested only on
`full-refund`, `ask-for-more-than-paid`, and `gateway-timeout-after-write`.
This is a model-plus-configuration comparison, not a thinking-only ablation.

| Attempt | Task outcome | Observable provider behavior |
|---|---|---|
| Adaptive, low effort, response cap 1,024 | First six live trajectories passed, including the Haiku miss. Third-trial setup and the first replay were blocked by Docker Hub's unauthenticated pull limit | 20 HTTP attempts; 14 `tool_use` blocks; **zero thinking blocks** |
| Adaptive, high effort, two-turn multiplication tool control | Tool used and exact result 57,403 returned | Two HTTP attempts; one tool call; no thinking blocks; both responses reported `output_tokens_details.thinking_tokens: 0` |
| Explicit thinking, budget 1,024, response cap 2,048; same image pinned through a verified public mirror | **9/9 live refund checks passed**, three trials per record; **3/3 exact checker replays**, matching HITL transcripts and zero replay provider calls | 30 HTTP attempts; **21 tool calls**, 15 text blocks; no thinking or signed-thinking blocks; all responses reported `thinking_tokens: 0` |

The pinned retry used exactly
`mirror.gcr.io/library/python@sha256:d879ed88565a7a3c614b980b369d4a5025a9d057b71f93d09fc6d0b2ca823a18`.
The public mirror returned the existing image digest. This avoided repeatedly
resolving a mutable Docker Hub tag; it did not change the checker or lower a gate.
The first failed attempt, including its setup errors, remains private evidence.

The explicit retry consumed **47,293 input tokens including cache** and
**3,372 output tokens**. No response reached `max_tokens`.
The transport inspected raw provider content before parsing and stored only
block types/counts, signature presence, usage fields and token details in
its accounting evidence, not reasoning text.

**Conclusion:** real tool calling worked and Sonnet passed these bounded
cases, including the particular Haiku miss. Thinking was explicitly requested
but **not exposed by the configured provider route**. A live signed-thinking
round trip could not be verified. There was no separately reported tool-token
counter, token-ID trace or logprob trace. Counting `tool_use` blocks is not
counting tool tokens; ordinary input/output totals remain the token evidence.
Do not infer hidden reasoning from a request flag or claim broad model quality
from three records repeated three times.

The existing 1,024-token ledger default remains unchanged. Explicit thinking
opts into a tested 2,048-token cap, with a matching pre-request reservation;
the $10 aggregate cutoff is unchanged. Every intermediate ledger was carried:

**Final new-campaign total: 184 HTTP attempts, $2.12206730 conservative estimate.**
This includes both failed experiments and the extra high-effort probe.
No paid process remained running after completion.

## Load: preserve four throughput failures

The unchanged gates were **stored** spans/sec >=20,000 and freshness p95 <=120s,
not HTTP-accepted spans/sec. Each replicate ingested 400,000 spans.

| Backend | Replicate | Stored spans/sec | Freshness p95 seconds | Throughput |
|---|---:|---:|---:|---|
| SQLite FULL | 1 | 33,645 | 32.9261 | Pass |
| SQLite FULL | 2 | 20,213 | 2.6721 | Pass |
| SQLite FULL | 3 | 28,467 | 21.1980 | Pass |
| SQLite NORMAL | 1 | 19,074 | 2.6887 | **Fail** |
| SQLite NORMAL | 2 | 24,870 | 13.5696 | Pass |
| SQLite NORMAL | 3 | 24,643 | 13.9569 | Pass |
| ClickHouse | 1 | 11,715 | 4.0208 | **Fail** |
| ClickHouse | 2 | 13,546 | 21.8839 | **Fail** |
| ClickHouse | 3 | 12,946 | 18.0905 | **Fail** |

All nine passed freshness; five passed throughput. The runner returned nonzero
and retained all nine reports. The stronger node did not close the overall
performance requirement. These are bounded single-worker measurements, not
production capacity or N-1 evidence.

## Recovery, filesystem and real virtualization

| Control | Measured outcome | Boundary |
|---|---|---|
| SQLite SIGKILL, three FULL and three NORMAL trials | Each recovered all 100 previously observed stored traces with unchanged content and `integrity_check=ok`; **220 acknowledged buffered traces lost** out of 320 accepted, plus 20 uncertain at crash | Process crash, not power loss; HTTP acceptance is not durable storage |
| Current-source PostgreSQL reopen/read/write | First run snapshot read back after daemon restart; second snapshot added; two exact snapshots passed | Not a fresh historical-release pair, rolling upgrade, HA or offsite backup certificate |
| ClickHouse dependency restore | 100 exact synthetic rows restored after source deletion; fingerprint matched | Owned local backup only |
| JetStream dependency restore | 50 exact messages, consumer ack floor and three pending acks restored | Last-activity timestamp was not claimed as durable sequence state |
| Real audit filesystem failure | UID 65534 startup refused unwritable directory; writable positive passed; real OS `EFBIG` produced HTTP **503 unavailable**, mutation absent, daemon alive and reads successful | `RLIMIT_FSIZE=0` with SIGXFSZ ignored, not a read-only remount or power loss |
| Real KVM | API 12 and successful VM creation on the selected node | No other node classes certified |
| Firecracker/jailer 1.17.0 | Sixteen requested concurrent sessions at boot limits 2/8/16 passed in 3.212/2.981/3.210s; real jailed guest restore/resnapshot and four source ownership/checksum invariants passed | Synthetic VM smoke evidence, not a production capacity limit |

The Firecracker release archive checksum was verified. The guest kernel's
observed SHA-256 was
`49ba99a5299444ac59dda2efc3569cc2d58a5d72ea6475a6bfc37aa0bf322e54`;
it was stable across controls, but no independent upstream kernel checksum
was available. Do not describe that observed hash as independent authentication.

Representative in-pod commands, with their observed results:

```text
/work/venv/bin/python /work/source/tests/verification/aks_followup_load.py
  FAILED overall: 4/9 unchanged stored-throughput gates failed
/work/venv/bin/python /work/source/tests/verification/aks_followup_storage.py sqlite --output /work/evidence/sqlite-crash
  completed six trials; committed-state invariants passed, buffered losses retained
/work/build/e2e.test -test.run '^TestAKSFollowupDependencyRestore$' -test.v -test.timeout=5m
  PASSED with explicit owned ClickHouse/NATS environment and recovery opt-in
/work/e2e.test -test.run '^TestAKSFollowupFirecracker$' -test.v -test.timeout=10m
  PASSED with real assets, KVM opt-in and source guest binary
/work/venv/bin/python /work/source/tests/verification/aks_followup_filesystem.py --output /work/audit-filesystem
  PASSED in the disposable privileged outer container
```

The filesystem probe required Linux root and permission to change another
UID's process limit; the ordinary worker correctly lacked that capability.
Earlier helper attempts corrected HOME inheritance, API key format and signal
handling. No new product audit-fail-closed claim is based only on mocked errors.

## Networking and Kata: still blocked

- Baseline network connectivity passed.
- Connectivity **still passed after default-deny NetworkPolicy**: the supplied
  cluster did not enforce this control.
- A Kata pod was admitted but remained `Pending` and unscheduled; zero selected-
  node matches advertised the required runtime capability.
- A privileged KVM probe on the selected D8 node ran and created a real VM.

Neither API admission nor a RuntimeClass object proves runtime availability.
Safe multi-tenant networking and Kata certification remain open. The probe
helper now verifies namespace ownership and pins all probes to the requested
node SKU without mounting service-account tokens.

## Workbench, native download and cleanup

Current-source daemon and a zero-cost synthetic HTTP agent ran inside the
ordinary worker. The browser exercised three reviewed synthetic records,
two trials each, baseline, bad candidate and corrected candidate:

- `aks-onboarding-block`: **BLOCK**.
- `aks-onboarding-pass`: **PASS**.
- `aks-onboarding-insufficient`: **INSUFFICIENT**, because three independent
  records cannot satisfy a minimum of four.

The synthetic agent received exactly 18 requests. These were browser workflow
controls, not human-labelled calibration or paid provider evaluations.
The user confirmed the native `release-check.sh` download. Its actual on-disk
file was checked for baseline and policy pins and contained no plaintext
credentials. It was not run, changed or removed. The browser automation
download event had timed out; this is **user-confirmed download plus file
inspection**, not an automated-download pass.

The inspected download pinned:

| Field | Value |
|---|---|
| Baseline run | `run-138249d5694eb96f` |
| Baseline SHA-256 | `74ce10231939d5967a69d370d19212a7d27aeb47f1945e7f427ab9f2043fc21c` |
| Policy SHA-256 | `e937b09d3e75a0477e2c7656a173bf1c751365613a9074b76676d5663d4b9da5` |

The workbench stopped gracefully, its quiesced database was copied privately,
and the backup's SQLite integrity check passed. Worker evidence and selected
pool logs/results were copied before removing `emptyDir` data.

At the user's request, the ownership label was verified and **only the owned
namespace was deleted**. Namespace absence and absence of labelled pods were
verified. Both owned loopback ports, 17443 and 18081, were closed. The three
exact ephemeral CA/server/client private-key files were removed. The user's
download remained untouched. Its temporary localhost endpoint is no longer
available.

## What this does not close

The four throughput failures, acknowledged ingest-buffer loss, unenforced
NetworkPolicy, unschedulable Kata and unavailable exposed thinking remain
explicit findings. No new human-labelled calibration, native Claude CLI
permission-denial test, mutable external tag publication/signature revalidation,
published-release rolling upgrade, HA/N-1, power-loss or offsite recovery
certificate is claimed. The broad kind e2e script was not run because it
contains broad Docker-cache cleanup; prior isolated kind controls remain valid
within their documented scope.

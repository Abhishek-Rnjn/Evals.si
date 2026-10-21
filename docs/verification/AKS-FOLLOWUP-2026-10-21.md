# AKS matching-code follow-up, 2026-10-21

## 1. Audit before execution

Recorded current `main`: `5d7510fca5bcfc7881b4f31206da3f2c6aeb8d4f`.
Branch: `verification/aks-followup-2026-10-21`, based on `f86889068cb6c917b8956ea832b084bc575d8bad`
to retain the preceding verification fixtures and reports. Product code is
identical to the recorded main; subsequent edits are verification-only.
The audit was completed before executing any new tests.

Classification concerns the complete requested **AKS** check, not the presence
of unit coverage. Earlier bounded results remain valid within their original
scope. The 20 October report names `main@2e67fae` plus its five fixes; that does
not prove every independently merged change was executed on that cluster.
The 21 October harness/horizon runs used the published **0.3.0 Go server**,
not a HEAD server.

| # | Item | Audit classification | Evidence and decision |
|---|------|----------------------|-----------------------|
| 1 | Final OTLP fast path, batching, statement cap and queue | Tested on older code | [20 October, TestLoad](AKS-2026-10-20.md#testload-testse2eload_testgo-on-standard_d8s_v5-designmd-12-14), lines 127-154, explicitly says the final ingest revision could be rerun on AKS. Final changes are in `314660e` (#123); use the final current binary rather than the historical throughput claim. |
| 2 | SQLite FULL versus NORMAL, and crash durability | Tested on older code | [20 October](AKS-2026-10-20.md), lines 140-151, has local final-revision FULL/NORMAL throughput, not a three-replicate AKS comparison or kill/restart durability test. `314660e` added the option. |
| 3 | Concurrent ClickHouse freshness poller | Not tested | [20 October](AKS-2026-10-20.md), line 153, explicitly says ClickHouse was not rerun after the eight-worker correction. `736848b` (#129) introduced the corrected poller. |
| 4 | Real jailer copy ownership/checksums through snapshots | Not tested | [20 October](AKS-2026-10-20.md), lines 43-45 and 72-75, has the copy fix and real KVM/default snapshot tests, but no explicit real-jailer source-owner/checksum experiment. Unit/fake-jailer coverage is not this check. |
| 5 | Parallel boot settings 2, 8 and 16 | Not tested | [20 October](AKS-2026-10-20.md), lines 155-162, reports a warm/default pool, not the three requested settings. Added in `314660e`. |
| 6 | Digest reuse versus mutable-tag registry resolution | Not tested | `314660e` and [20 October](AKS-2026-10-20.md), line 45, name `TestImageCacheOnlyPinned`; retain that unit evidence. No registry-side digest-versus-changing-tag experiment is recorded. Do not create a registry/repository to obtain one. |
| 7 | Strict audit failure and besteffort contrast on AKS | Not tested | `b75a865` (#120) introduced strict audit and its fault-injection test. [21 October environment](AKS-CLAUDE-2026-10-21.md#environment-and-boundaries), line 26, names server 0.3.0. Existing source-level tests do not establish deployed HEAD audit behavior. |
| 8 | 0.3.0 to current migration and older-reader behavior | Tested on older code | [20 October N-1 row](AKS-2026-10-20.md#m4-zero-downtime-rolling-upgrade-ha-failover-n-1-compatibility--multi-service-task-environments) records a real bounded schema reader/writer pass. `b75a865` adds versioned migrations/minimum-reader behavior after the report's stated base; no matching-code execution is identified. Recheck schema compatibility, not Helm rolling upgrade. |
| 9 | Model-driven refund suite, at least three trials | Not tested | `4c3098f` (#125) added the suite. [Remaining work](../REMAINING-WORK.md), STRAT-1, explicitly retains the model-driven run; the [suite](../../examples/support-refund/README.md) uses stand-ins. |
| 10 | Live approval gate, evaluators and exact replay | Not tested | `5d7510f` (#132) adds scripted-model coverage. [H8](../HARNESS-ROADMAP.md#h8-oversight-topologies-and-conversations-proposed-after-h3) distinguishes the built scripted approver from unbuilt human/model approvers; no live refund-agent approval experiment is reported. |
| 11 | Claude judge calibration on labelled refund outputs | Not tested | `4581e36` (#126) merges calibration. [Calibration limits](JUDGE-CALIBRATION-2026-10-21.md#limits) explicitly says labels were synthetic and HTTP judging scripted. Assistant-created labels must not be called human labels. |
| 12 | Workbench against a current server on AKS | Not tested | `1a11397` (#124), `0e46db5` and `8947dd0` add/fix the workbench. [Workbench verification](RELEASE-WORKBENCH.md) is local Linux with a real local daemon and scripted HTTP agent, not a HEAD server on this cluster. Retain that local evidence. |
| 13 | Actual Claude CLI permission_denials field | Not tested | [21 October](AKS-CLAUDE-2026-10-21.md), lines 28-30, says the plain environment had no logged-in CLI. Earlier CLI/MCP passes do not document this field. Only attempt if the installed CLI can use the same gateway with metered requests. |
| 14 | Current KVM, NetworkPolicy and Kata availability | Tested on older code | [21 October blockers](AKS-CLAUDE-2026-10-21.md#failures-and-blockers-retained) records unenforced egress and unschedulable Kata; KVM was not probed that day. Re-probe cheaply before hardware-dependent work, without changing shared configuration. |

Read/reconciled the requested reports and inventories in order, and inspected
`git log --oneline 2e67fae..HEAD`. In particular, `b75a865` adds migration/audit
contracts; `314660e` records the older AKS pass and final ingest/sandbox fixes;
`a625dc6`/`0e46db5` add server workbench surfaces; `736848b` changes the load
poller; `5d7510f` adds approval behavior. No complete current-AKS execution of
the 14 specifically scoped checks is established by those reports.

Already-passing profiles/context mechanisms, the 300-step horizon, ordinary
SIGKILL resume, signed thinking, pool mTLS/hardening and local-kind service
lifecycle are not rerun. New side-effect replay, live approval behavior and
independent repeated-trial observations are distinct from those controls.

### Constraints and experiment plan

- Total authorization: **$40**, including the previous **$16.51619320**.
  Initially refuse any provider attempt crossing **$36 (90%)**. The user
  subsequently explicitly raised the cumulative cutoff to **$39.50** on
  21 October. The already-running refund campaign retains its stricter $36
  guard; subsequent campaigns use $39.50. Historical charges and uncertain
  reservations are retained; gateway invoice amounts remain unguaranteed.
- One new owned namespace, ClusterIP services, loopback forwarding and
  `emptyDir` only. No external resources, PVCs, shared policy/runtime/node/CRD
  edits, registry pushes or provider credentials in Kubernetes.
- Cross-built current binaries and installed matching wheels are not release
  image or Helm certification. Record node SKU/storage class of experiment
  without node names, addresses or account identifiers.
- Report failures without product fixes. Only verification helpers and reports
  may change. No publish or push is authorized.

## 2. Executed checks

### Checkpoint 1: load, capability and current-server controls

This is an **in-progress checkpoint**, not a completed campaign. Product code
remains unchanged. Completed load evidence was copied from the worker's
`emptyDir` to private local storage.
The user requested commits as substantial groups finish, or approximately
hourly during active execution. Each checkpoint records its own snapshot;
there is no unattended hourly commit timer.

The cross-built current server and tests ran in the owned AKS namespace.
The worker was scheduled on **Standard_DS4_v2**, with a 4 CPU / 8 GiB container
limit, Linux `6.8.0-1070-azure`, containerd `2.3.6-1`, and node-local `emptyDir`
storage. This is not a dedicated-node, persistent-disk or release-image result.

#### Stored ingest and concurrent freshness

Executed `python /work/load.py` in the worker. For every declared replicate,
the runner invokes:

```sh
/work/build/e2e.test -test.run '^TestAKSFollowupLoad$' \
  -test.v -test.timeout=12m
```

The runner sets `EVALSI_AKS_FOLLOWUP_LOAD=1`, the explicit SQLite mode,
`EVALSI_E2E_WORKER=/work/venv/bin/python -m evalsi`, and a unique JSON report.
ClickHouse replicates additionally select the owned ClusterIP service.
All nine processes returned **1**, because stored ingest missed the unchanged
**20,000 spans/s** threshold. Each ingested 400,000 spans. All concurrent
freshness p95 measurements passed the unchanged **120-second** threshold.

| Backend | Replicate | Stored spans/s | Freshness p50 s | Freshness p95 s | Poll p95 ms |
|---|---:|---:|---:|---:|---:|
| SQLite FULL | 1 | 15,548 | 2.376 | 2.689 | 4.035 |
| SQLite FULL | 2 | 18,098 | 3.396 | 7.827 | 14.669 |
| SQLite FULL | 3 | 15,803 | 2.401 | 2.733 | 4.031 |
| SQLite NORMAL | 1 | 15,751 | 2.413 | 2.693 | 3.466 |
| SQLite NORMAL | 2 | 15,495 | 2.382 | 2.699 | 3.222 |
| SQLite NORMAL | 3 | 15,689 | 2.402 | 2.695 | 3.119 |
| ClickHouse | 1 | 11,770 | 17.065 | 25.318 | 79.931 |
| ClickHouse | 2 | 10,396 | 9.774 | 13.891 | 120.259 |
| ClickHouse | 3 | 9,380 | 2.822 | 3.255 | 189.262 |

No clear NORMAL-mode throughput advantage was demonstrated. Pool initialization,
capability probes and the strict-audit test overlapped part of the campaign;
shared-node/cgroup contention is a possible confounder. These measurements do not
isolate a sole bottleneck or establish a production capacity limit. No replicate
was dropped or replaced, and no threshold was relaxed.

#### Audit and identity

Executed the actual current-source server test binary inside AKS:

```sh
/work/build/server.test -test.run '^TestStrictAuditRefusesAChangeItCannotRecord$' -test.v
/work/build/server.test -test.run '^(TestAnIdentityIsDecidedAlikeOnEverySurfaceAndRevokedEverywhere|TestAuditEntriesCarryAuthContextReadsConfigAndAChain)$' -test.v
```

Both commands **passed**. The strict/besteffort subtests took 2.51 seconds
together; chained audit took 1.35 seconds and identity/revocation 1.24 seconds.
These launch real in-process servers and exercise transport surfaces. Audit
fault injection drops only the audit table in a disposable test database:
strict mode refuses the change, besteffort permits it while counting the error,
and permission denial remains denial. This is **not** an unwritable-filesystem
drill against a separately managed daemon.

#### Capability probes

- Network canary connected before **and after** the namespace deny policy:
  NetworkPolicy enforcement remains a failure.
- Kata pod admission succeeded, but no node matched its existing RuntimeClass
  scheduling selector; the pod remained Pending/unscheduled.
- Privileged host-device probing was admitted. Five nodes successfully created
  KVM VMs: three Standard_D4ds_v5 and two Standard_D8s_v5. The Standard_DS4_v2
  probe remained Pending. KVM capability is not a Firecracker/jailer pass.
- Temporary probes were deleted; a subsequent namespace query found **zero**
  KVM probe pods. The campaign namespace and services remain active.

#### Live campaign progress

At this checkpoint, all **36/36 baseline refund trials** passed the unchanged
business checker. **19/36 approval trials** had completed: 18 business-check
passes and one intentionally denied refund that therefore failed the original
amount/recipient expectations. The initial checkpoint interpreted the
`denial-respected` failure as a blocked retry. **Raw-trace inspection later
disproved that interpretation:** the evaluator counted the original denied
tool event as a retry. See checkpoint 2 below.
These are interim observations; final denominators, replay and controls remain
pending. Private records/cassettes and provider settings are not committed.

#### Fixture validation

- `python/.venv/bin/python -m pytest tests/verification/test_long_horizon.py -q`:
  **24 passed**, including supported budget increases, reservation retention and
  refusal before the next over-limit attempt.
- `go test ./tests/e2e -run '^$'`: **passed compilation**, no tests executed.
  This does not claim that the unfinished dependency-recovery fixture ran.
- Ruff checks **passed** for `aks_followup_live.py`, `aks_followup_load.py`,
  `aks_followup_probes.py`, `long_horizon.py` and `test_long_horizon.py`.
- `python/.venv/bin/python -m mypy --follow-imports=silent tests/verification/aks_followup_live.py tests/verification/aks_followup_load.py tests/verification/aks_followup_probes.py tests/verification/long_horizon.py`:
  **passed**, four source files.
- Structured checkpoint JSON: nine replicate outcomes, unchanged threshold
  classifications and cumulative/incremental spending arithmetic **verified**.

### Checkpoint 2: completed refund campaign, restore and offline analysis

#### Completed live trials and replay

The metered `aks_followup_live.py` command completed with **exit 0**, preserving
72 live records and four fresh-sandbox replays:

| Profile | Live trials | Business checker | Approval coverage | Denial-respected |
|---|---:|---|---|---|
| Baseline | 36 | 36 passed | 36 not applicable | 36 not applicable |
| Approval | 36 | 33 passed; 3 intentionally denied refunds fail original amount/recipient expectations | 15 passed; 21 not applicable | 12 passed; 3 false-positive failures; 21 not applicable |

All three denied cases contain **one** attempted refund command, one denial
resolution and the corresponding original tool event with
`evalsi.tool.hook_denied=true`, followed by an allowed handoff. There are **zero
repeat refund commands** and **zero executed denied refunds**. The loop emits
hook steps before its original tool-result step; `denial_respected` scans later
matching tool events without excluding that original denied call. It therefore
flags a retry that never occurred. This is an evaluator defect, not observed
model disobedience. Product code remains unchanged. The offline repro is retained
as an explicit strict expected failure.

The baseline and approval versions of `full-refund` and
`gateway-timeout-after-write` each replayed with **zero provider HTTP attempts**
and **identical HITL sequences**. This establishes four representative exact
replays, not whole-campaign replay, changed-approver rejection, edit/no-answer
controls, fork or external-service exactly-once behavior.

The pool's `kubectl exec` stream subsequently ended with WebSocket close 1006 /
unexpected EOF. The worker had zero restarts; the original pool process remained
alive and its port accepted connections. No server restart or paid retry was
needed, and the campaign still completed without task errors.

#### ClickHouse and JetStream backup/delete/restore

Executed the final current-source fixture inside the owned worker:

```sh
EVALSI_AKS_FOLLOWUP_RECOVERY=1 \
EVALSI_TEST_CLICKHOUSE_URL=http://clickhouse:8123 \
EVALSI_TEST_NATS_URL=nats://nats:4222 \
/work/build/recovery.test -test.run '^TestAKSFollowupDependencyRestore$' \
  -test.v -test.timeout=5m
```

**Passed**, both subtests, 0.10 seconds total:

- ClickHouse used its supported `BACKUP ... TO Disk` and `RESTORE ... FROM Disk`
  SQL, deleting the source database between them. The restored 100 synthetic rows
  retained count **100**, ID sum **4950** and payload fingerprint
  **5867902235618448090**. Restore plus fingerprint read took **0.028 seconds**.
- JetStream used its stream snapshot/restore protocol, deleting the source
  stream between them. All **50 exact message payloads**, delivered/ack-floor
  sequence positions, **40 undelivered messages** and **3 pending acknowledgments**
  were restored. Restore plus checks took **0.020 seconds**. Snapshot SHA-256:
  `0cbe5b3d955261ce1ba9fa870e680ec41b9cb4651321359db7713e0fb7510f5b`.
- JetStream's ack-floor last-activity timestamp was available before backup and
  absent immediately after restoration. Do not describe this as byte-identical
  recovery of every runtime metadata field.

Two earlier JetStream fixture attempts failed and are retained:
first, `nats: no responders available for request` after sending restore chunks
without the required per-chunk request acknowledgment; second, `consumer state
changed` because the assertion compared the non-restored activity timestamp as
well as durable positions. The fixture now uses the supported acknowledged-chunk
protocol, asserts durable positions/payloads and explicitly reports the timestamp
limitation. No Evals.si product or NATS server code was changed. A subsequent
JetStream-only run and the final combined run passed.

These are tiny, quiesced, same-namespace, same-version dependency restores from
`emptyDir`, **not** off-cluster disaster recovery, production RPO/RTO, PostgreSQL
recovery or an application-wide consistent backup.

#### Repeated trials and heuristic validity

`aks_followup_analysis.py` analyzed the saved baseline records with no model
requests. Twelve cases with three trials each had pair agreement **1.0** and
observed within-case variance **0.0**. The repository's advice remains
conservative: worst-case variance bound **0.25**; **385 / 97 / 25** trials per
case for margins **0.05 / 0.10 / 0.20** at 95% confidence. Hoeffding gives **738**
for margin 0.05. Three identical scores do not establish deterministic outputs.

Review of all **49 distinct baseline tool inputs** found business CLI calls and
documentation/source reads, not hidden-checker access, benchmark-answer fetching
or test/guardrail edits. On those assistant-reviewed synthetic honest controls:
tampering **0/36 flags**, contamination **0/36 flags**, guardrail-edit **36/36
skipped** for missing harness-edit metadata. Skips are not clean edits.

Six separately labelled synthetic challenge records preserve three known false
positives: a legitimate existing-test correction, an ordinary
`solutions-architecture.md` URL and a meaning-preserving guardrail rewrite.
Each was flagged. The corresponding hidden-checker probe, benchmark-answer fetch
and actual guardrail weakening were also flagged. Do not pool these deliberately
hard examples with the 36 live controls or claim population false-positive rates.
All labels are **assistant-created/reviewed, not human adjudication**.

#### Installed-artifact quickstart

In the clean AKS venv, with no repository source checkout:

```sh
EVALSID=/work/build/evalsid /work/venv/bin/python -m evalsi doctor --require sdk --format json
/work/venv/bin/python -m evalsi init --template recorded /work/artifact-recorded
cd /work/artifact-recorded
EVALSID=/work/build/evalsid /work/venv/bin/python -m evalsi run -f run.yaml --output results.json
/work/venv/bin/python -m evalsi report results.json -o report.html
```

All commands **passed**. Both records scored exact-match **1.0**, both gates
passed, and HTML was exported and copied to private local evidence. Doctor
reported SDK/harness **0.3.0.dev0**, Python **3.13.16**, installed gRPC worker
dependencies and usable Landlock. It also correctly warned that the copied
server advertises **0.0.0-dev**, not the SDK version. Source provenance is known;
release-package version matching, published platform wheels, agent-template
execution and Helm rollout remain distinct unverified surfaces.

Fixture validation: focused Python tests **27 passed, 1 expected failure**
(the denial self-event defect); Ruff and mypy passed. The analysis runner's first
local invocation lacked the source import path and failed before evaluation;
rerunning with explicit SDK/harness `PYTHONPATH` succeeded.

### Checkpoint 3: approval edge controls and real KVM

#### Approve, edit, no-answer and changed-approver rejection

Executed `aks_followup_live.py --approval-controls` with a new private output
directory and checkpoint 2's complete `budget.json` as `--prior-budget`.
The process completed with **exit 0** under the **$39.50 cumulative cutoff**.

- Approve: the request was approved, the authorized 42.00 refund ran, and the
  unchanged business checker and approval coverage passed.
- Edit: the approver replaced the command with the same authorized refund using
  the explicit `approval-edited` idempotency key. The trace records `edit`, the
  rewritten call ran, and business/coverage checks passed.
- No-answer: no refund ran; the model handed off to a human. The original business
  checker correctly remains failed for the missing refund/recipient and unexpected
  handoff. The denial evaluator repeats its original-denied-event false positive.
- All three controls replayed with **zero provider requests** and **identical
  HITL sequences**.
- Changing each approver's configuration then refused replay with
  `CassetteError ... a replay needs the same hooks in the same order`, with
  **zero provider requests**, in all three cases.

These are three small scripted-approver/live-agent controls, not a human approver
or general shell-policy enforcement. Literal command patterns can be bypassed
by representations outside their matching rules; no general authorization
guarantee is claimed.

#### Real Firecracker parallel boots and jailer source integrity

A second owned pod, `kvm-worker`, was admitted in the same namespace on
**Standard_D8s_v5**, privileged, with only `/dev/kvm` as a host-device mount,
`emptyDir` storage, 2 CPU / 4 GiB requested and 4 CPU / 6 GiB limits.
No node labels/taints, RuntimeClasses, shared policy or host configuration changed.
The initial ext4-tool check failed; `e2fsprogs` was installed inside this pod only.

Firecracker/jailer **1.17.0** came from the upstream GitHub release and its
archive SHA-256 matched the upstream sidecar:
`06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558`.
The guest kernel came over HTTPS from upstream's existing public CI
`v1.10/x86_64/vmlinux-6.1.102`; observed SHA-256:
`49ba99a5299444ac59dda2efc3569cc2d58a5d72ea6475a6bfc37aa0bf322e54`.
No independent upstream kernel checksum was available: do not describe that
kernel hash as separately authenticated release provenance. The guest is the
current-source cross-built static binary and Alpine 3.22.6 is digest-pinned.

```sh
EVALSI_AKS_FOLLOWUP_KVM=1 EVALSI_FOLLOWUP_KVM_ASSETS=/work/assets \
/work/build/kvm.test -test.run '^TestAKSFollowupFirecracker$' \
  -test.v -test.timeout=18m
```

**Passed**, 8.90 seconds total. With the Firecracker-only ladder, 128 MiB VMs and
16 concurrently requested sandboxes per setting, every guest returned the exact
`original` file:

| Configured max parallel boots | Successful sandboxes | Batch seconds |
|---:|---:|---:|
| 2 | 16/16 | 3.050 |
| 8 | 16/16 | 1.714 |
| 16 | 16/16 | 1.728 |

These are small cached-image command batches, not a production scaling curve;
configured concurrency is not an independently instrumented instantaneous peak.

The real jailer ran with non-root UID/GID **4242**, booted a guest, snapshotted
and restored it. A stronger targeted run then snapshotted the restored guest and
verified unchanged **owners and SHA-256 contents of all four original sources**:
kernel, disk, VM state and memory. The original kernel remained **0:0**.

```sh
EVALSI_AKS_FOLLOWUP_KVM=1 EVALSI_FOLLOWUP_KVM_ASSETS=/work/assets \
/work/build/kvm.test -test.run '^TestAKSFollowupFirecracker$/^jailer_snapshot_source_integrity$' \
  -test.v -test.timeout=4m
```

**Passed**, 4.99 seconds, with no mock VM, fake jailer or weaker-driver fallback.
Both logs and asset hashes were copied to private local evidence. The owned
KVM pod remains alive for subsequent checks and must be removed at cleanup.

### Checkpoint 4: lifecycle, native Claude calibration and AKS browser

#### Delegation, fork/resume and actual external side effects

`aks_followup_lifecycle.py --output <new-private-directory> --prior-budget
<approval-controls-budget.json>` **passed**, using the current AKS sandbox pool
and explicit Sonnet through the authorized gateway. Client TLS material stayed
in private local temporary files; provider credentials never entered Kubernetes.

- One real subagent completed two subagent steps and the linked handoff checker
  passed. Inspection of the privately retained provider request bodies verified
  that the child's large raw background-evidence marker was absent from parent
  requests.
- A PostgreSQL-backed external write with an application idempotency key stayed
  at **[1, 1, 1]** through the runner's record/replay/resume/fork checks.
- The deliberately unkeyed negative control grew from **1 to 2** on replay.
  These are model-only cassettes with real tools reexecuted: the harness does
  **not** provide an exactly-once external-side-effect guarantee.
- Completed replay and resume made **zero provider requests**; the fork made
  one live continuation request. The original cassette SHA-256 was unchanged.

#### Two real native Claude calibration passes

Executed `aks_followup_calibration.py --source <live-refund-directory>
--output <new-private-directory> --prior-budget <lifecycle-budget.json>`.
Both rubric candidates received two fresh repeats over the same 60 observations:
**24 frozen real model outputs and 36 synthetic policy-violating mutations**.
All labels are assistant-created/reviewed, not independent human judgments.
Twelve task groups split into six tuning and six held-out groups, with
30 observations in each split and no group/input leakage.

The native Anthropic SDK uses `httpx2`, unlike the harness transport. Its
verification-only transport shares the existing cumulative ledger and reserves
each request before network access, including SDK retries. Explicit Sonnet,
the original approved gateway and the $39.50 cutoff were retained.

The first full pass completed, but the runner's HTML export failed because the
renderer returns bytes and the fixture initially used `write_text`. The complete
immutable first report and **$21.09669000** ledger were preserved. After correcting
only the fixture to `write_bytes`, execution resumed with
`--resume-before <saved-calibration-before.json>` and that latest ledger.
Dataset/protocol pins were checked before accepting the completed first pass;
none of its paid requests were repeated.

Both passes **completed**, with HTML and a fixed-output drift report:

| Held-out result, for each candidate in each pass | Value |
|---|---:|
| Independent task groups / observations | 6 / 30 |
| True positive / true negative | 12 / 18 |
| False positive / false negative | 0 / 0 |
| Accuracy / precision / recall / kappa | 1.0 / 1.0 / 1.0 / 1.0 |
| Repeat decision disagreement | 0.0 |
| Fixed-output accuracy delta between passes | 0.0 |

The tuning tie selected declared-first `current-correctness`, threshold **0.5**.
This does not show that rubric is superior. All 500 grouped bootstrap resamples
had no empirical variation: the report explicitly marks confidence intervals
**unavailable**, not population certainty. These small, deliberately contrasting
synthetic observations do not certify real-world judge validity or absence of
future drift.

#### Browser workbench on the current AKS daemon

Started the current cross-built daemon with `aks-followup-api.yaml` using
`aks_followup_workbench.py` inside the owned worker, and verified the UI over
the existing loopback-only forward. The fixture uses a **synthetic HTTP agent**,
not Claude, three approved dataset records, two trials, and pinned built-in
exact-match/tool-errors/run-completed evaluators.

Actual browser interactions selected the approved dataset, validated and
consented to destinations, started runs, inspected identities, configured the
lower-is-better tool-errors rule with maximum per-trial critical score **0**,
and saved immutable release decisions:

| Control | Required independent records | Actual records / trials | Outcome |
|---|---:|---:|---|
| Intentionally bad agent | 3 | 3 / 2 | **BLOCK** |
| Corrected good agent | 3 | 3 / 2 | **PASS** |
| Same good agent, insufficient record count | 4 | 3 / 2 | **INSUFFICIENT** |

The paired-case inspector showed the bad agent's `approval timeout` and
`awaiting_human` evidence. Switching candidate trial to **1** loaded the actual
`trial-1-lookup` evidence while baseline trial **0** remained unchanged.
Declared commits `a`/`b`/`c` repeated 40 times are synthetic identities, not
attestations of actual code revisions.

The integrated browser did not emit Playwright's native download event within
10 seconds. Rather than claim a completed native file download, verification
captured the actual export Blobs while still invoking the original object-URL
function. Candidate spec, policy and CLI export contents were persisted privately.
The SDK command `python -m evalsi release policy-hash -f <exported-policy.json>`
**passed**, yielding the exact CLI-export pin:
`e937b09d3e75a0477e2c7656a173bf1c751365613a9074b76676d5663d4b9da5`.
Dataset digest and two-trial pins matched. Running `python -m evalsi release gate`
with the exported spec/policy, baseline evidence/commit and policy SHA pins,
against the same AKS server **correctly returned exit 3 and
RELEASE_OUTCOME_BLOCK**. PASS/INSUFFICIENT screenshots and CLI output are saved
privately. No hosted Action dispatch or full browser/accessibility certification
is claimed.

Checkpoint fixture validation **passed**: Ruff, Ruff format check, mypy on all
three new Python runners, and focused pytest **30 passed, 1 strict expected
failure** for the retained original-denied-event defect.

### Checkpoint 5: crash durability, real source-version migration and packaging

#### SQLite FULL/NORMAL: actual daemon SIGKILL during ingest

Executed **three declared replicates per mode**, six total:

```sh
python /work/storage.py sqlite --output /work/storage-crash
```

`storage.py` is the copied `tests/verification/aks_followup_storage.py`.
Each trial launched a separate current `evalsid serve --no-auth` process on
loopback, stored and read back 100 known traces, then continued OTLP/HTTP ingest
on a separate thread. It sent SIGKILL to that exact daemon PID while ingestion
was active, tracked response-acknowledged IDs separately from the final uncertain
request, and restarted against the **same database**, not a new store.

| Mode | Replicate | HTTP-acknowledged traces | Observed stored before kill | Recovered | Acknowledged but lost | Uncertain in-flight IDs |
|---|---:|---:|---:|---:|---:|---:|
| FULL | 1 | 320 | 100 | 100 | 220 | 20 |
| FULL | 2 | 320 | 100 | 100 | 220 | 20 |
| FULL | 3 | 300 | 100 | 100 | 200 | 20 |
| NORMAL | 1 | 320 | 100 | 100 | 220 | 20 |
| NORMAL | 2 | 300 | 100 | 100 | 200 | 20 |
| NORMAL | 3 | 320 | 100 | 100 | 220 | 20 |

All six recovered every previously observed stored trace with unchanged
summary/record SHA-256, reported WAL mode and passed `PRAGMA integrity_check`.
The final request's connection reset/disconnect is retained as **uncertain**,
not counted as acknowledged. No previously observed stored trace was lost.

**HTTP acceptance is not a durable commit:** the process's two-second assembler
buffer lost 200-220 already-acknowledged traces per trial in **both** modes.
This is a retained ingest durability limitation, not evidence that FULL lost
committed SQLite rows or that NORMAL is power-loss safe. No host/power loss,
disk failure, production RPO or transaction-level fsync instrumentation was tested.

#### Actual tagged 0.3.0 and current PostgreSQL reader/writer sequence

Cross-built the existing local `v0.3.0` tag at
`203f23cbc6c6a4c5848a0d7703e2b6c38f0cc066` separately, without changing the worktree.
SHA-256 of source-built historical/current binaries:

- v0.3.0: `cf952971adeb3bef0fc51d732787a5a85524510609012bc5384416f7eab0a24f`.
- Current: `a640f9cb4a8a58bfc4d65e509d3ee945d4fd14306a4e5478b659ba6af815f438`.

Created `evalsi_followup_migration` inside the existing owned PostgreSQL 17 pod;
the original lifecycle database was untouched. Sequential executions of
`python /work/storage.py postgres --binary <historical-or-current-binary>
--output <new-phase-directory> --dsn <owned-disposable-database>
[--expected <prior-snapshots.json>] [--create <run-name>]` all **passed**:

1. Actual historical daemon initialized the database and completed a recorded run.
2. Current daemon adopted/migrated it, read that run unchanged and wrote another.
3. Actual historical daemon reopened the migrated schema, read both unchanged
   and wrote a third.
4. Current daemon reopened and read all three unchanged.

Each run had three synthetic recorded records, one trial, three nonempty
passing exact-match results. IDs, common run fields/specs/summaries and exact
stored results/records were compared, not merely row counts.
The new `evaluationContract` field is excluded from cross-version comparison
because the historical reader predates it. Generated agent outputs, newer
feature semantics, simultaneous rolling replicas, Helm rollback and a published
release-binary pair are **not** certified by this bounded source-tag test.

Two initial fixture attempts were correctly rejected with HTTP 400:
`spec.trials above 1 needs a target; without one every trial is identical`.
The corrected recorded-only fixture uses one trial. Their launch directories
and logs were preserved; no product change was made.

Current startup recorded all **five** schema steps, each minimum reader **0**.
After restore verification, a separate negative control temporarily marked the
owned database as requiring reader version **6**. Executing
`python /work/storage.py schema-guard ...` made the actual current daemon refuse
startup with `the database schema is newer than this evalsid can read`.
The marker was restored to 0 in `finally`. This verifies the current guard
against simulated future metadata, not an actual destructive contract migration
or retroactive guard support in v0.3.0.

#### PostgreSQL dump/delete/restore, including application reads

After all migration-phase daemons stopped and no sessions remained on that
isolated database:

```sh
pg_dump -U postgres -d evalsi_followup_migration --format=custom --no-owner
# Delete only this fixture's database; restore into a new empty owned database.
pg_restore -U postgres -d evalsi_followup_restored \
  --no-owner --exit-on-error --single-transaction
```

Both commands **passed** inside the existing PostgreSQL pod. The dump was
streamed to a private local mode-0600 file before source deletion. SHA-256:
`9b42d4d70bcbca93012920f8c354c5e81b3902aa771bf401738a370c24deb846`.
Every one of **41 public tables** had identical row counts and ordered canonical
row fingerprints after restore: three runs, nine admitted records, nine stored
results and five migration rows, including their identities. A current daemon
then read all three exact saved API snapshots from the restored database.

This is a tiny quiesced same-version dependency backup with a private local copy,
not encrypted off-site/managed PITR or a coordinated SQL/object-store/trace/queue
recovery epoch. The recorded-only runs had zero generated-output rows.

#### Unchanged installed agent starter: success, regression and honest failures

In the ordinary clean-wheel worker:
`python -m evalsi init --template agent /work/artifact-agent` **passed**.
The unmodified starter's doctor and execution initially exited **1**:
bubblewrap was missing; Landlock was below its required namespaced isolation.
Installing only that missing dependency, bubblewrap **0.12.0**, did not solve
the host restriction: retry exited **1**, `Failed to make / slave: Permission denied`.
These failures and outputs are retained; isolation/network requirements were
not weakened and no unconfined fallback was used.

Used the **already authorized, owned privileged KVM pod**, not a changed shared
node or newly privileged worker. After observed missing-venv/bubblewrap failures,
installed the same built wheels in a fresh venv and bubblewrap inside that pod.
`pip check` **passed**; no repository Python checkout was present.
The exact same generated starter then used real **bwrap namespaced isolation**
with the unchanged `network: deny` request:

| Command, with `EVALSID=/work/build/evalsid` | Result |
|---|---|
| `python -m evalsi run -f run.yaml --output results-privileged.json` | **exit 0**, task-success 1.0, zero evaluation errors, both gates pass |
| `python -m evalsi run -f regressed.yaml --output results-regressed-privileged.json` | **exit 3**, task-success 0.0, zero evaluation errors; expected business regression |
| `python -m evalsi report results-privileged.json -o report-privileged.html` | **exit 0**, HTML copied privately |
| `python -m evalsi doctor --require agent --format json` | **exit 1**: sandbox check passes (`bwrap`), but server advertises 0.0.0-dev versus SDK/harness 0.3.0.dev0 |

The remaining doctor failure is a source-build metadata mismatch, not a passing
release-package compatibility check. The CLI agent is synthetic and offline:
**zero Claude requests**. Namespaced process networking does not repair or
certify the cluster's unenforced pod NetworkPolicy.

#### Real public-registry image cache, with deliberately offline controls

Cross-built `internal/sandbox/aks_image_cache_test.go` and executed:

```sh
EVALSI_AKS_FOLLOWUP_IMAGE_CACHE=1 \
EVALSI_FOLLOWUP_IMAGE_REPORT=/work/image-cache-report.json \
/work/build/image-cache.test -test.run '^TestAKSFollowupImageCache$' \
  -test.v -test.timeout=5m
```

**Passed**, 1.59 seconds. Used upstream Alpine 3.22.6 and the already recorded
digest `sha256:3e9b4b680bfc9fb5269227cffbd6d42be39fbf7c0b908123913864aa4447e764`,
a new test-owned image directory and actual registry responses. A counting
transport then deliberately denied outbound registry requests for negative
controls; no successful registry response was faked.

- First digest fetch: **7** registry HTTP requests; root filesystem extracted.
- Same-store warm pinned reuse: **0** requests, succeeds with transport blocked.
- Real tag resolution: **4** requests, same digest as the pinned reference.
- Reusing the tag with transport blocked: **1** attempted request, correctly fails.
- A new image-store instance with the disk cache present: **1** attempted request,
  correctly fails offline; the zero-request fast path is **in-memory**, not a
  cross-process offline-cache guarantee.
- Moving the cached root aside: **1** attempted request, correctly fails offline.

No registry/repository was created, no tag was published/mutated and no signature
verifier was configured. Mutable-tag movement and signature revalidation are not
claimed from this bounded live lookup/reuse experiment.

### Final disposition of the audited checks

Execution and cleanup are complete, with **no product fixes and no push**.
Product-source paths remain identical to recorded main
`5d7510fca5bcfc7881b4f31206da3f2c6aeb8d4f`; added Go files are verification tests.
Checkpoint commits preserve intermediate failures/corrections:
`4855edf`, `e2e4c2f`, `ba2356d`, `19969ea`, `267904d`, followed by this
final evidence/cleanup documentation checkpoint.

| Audit item | Final bounded disposition |
|---|---|
| 1. Current stored-ingest throughput | **Failed** unchanged target in all nine declared replicates |
| 2. SQLite FULL/NORMAL and crash durability | Six real process-crash trials; stored rows preserved, acknowledged assembler-buffer loss in both modes; no power-loss claim |
| 3. Corrected ClickHouse freshness polling | All three ClickHouse freshness checks pass; throughput remains failed |
| 4. Real jailer source integrity | **Passed** real restore/resnapshot, four source owners/checksums unchanged |
| 5. Parallel boot settings 2/8/16 | **Passed** 16/16 guests at each setting; not a production scaling curve |
| 6. Digest reuse versus tags | Real lookup/offline controls pass; actual tag mutation and signature revalidation **not run** |
| 7. Strict audit and besteffort | Current real-server fault-injection tests pass inside AKS; separate unwritable-daemon filesystem drill **not run** |
| 8. Historical/current migration and readers | Real source-tag pair common recorded runs and current simulated-future guard pass; not published-release rolling certification |
| 9. Model-driven refund suite | **36/36** baseline business passes, three trials per synthetic task |
| 10. Live approval, evaluators and replay | Live controls/replay verified; denied-call self-event evaluator false positive retained |
| 11. Native Claude calibration | Both repeated passes complete; assistant labels only, unavailable empirical uncertainty, no human/population certification |
| 12. Current AKS browser workbench | BLOCK/PASS/INSUFFICIENT, paired trials and pinned CLI verified; native download event not observed |
| 13. Claude CLI `permission_denials` | **Not run**: mandatory per-request conservative reservation was not established |
| 14. Current KVM, NetworkPolicy and Kata | Real KVM passes; NetworkPolicy remains unenforced; Kata has no matching nodes |

## 3. Spend

Prior conservative aggregate: **$16.51619320**. At checkpoint 1, the live ledger
recorded **$18.59430660 cumulative**, an increase of **$2.07811340**.
This is a conservative token-price estimate, not a gateway invoice.
The active campaign retains its initial $36 cutoff. Subsequent campaigns carry
the latest complete ledger into the explicitly authorized $39.50 cutoff.
At checkpoint 1, per-check final accounting was pending while the campaign ran.

Checkpoint 2's completed live campaign records **$19.31146260 cumulative**:
baseline **$1.41902420**, approval **$1.37624520**, four replays **$0**, in addition
to the preserved prior **$16.51619320**. There were **285 new HTTP attempts**.
Recovery, offline analysis and artifact quickstart made no provider requests.
The next paid campaign must carry this complete ledger into the $39.50 cutoff.

Checkpoint 3: **$19.42178600 cumulative**. The approval controls added
**$0.11032340** over 13 HTTP attempts: approve **$0.03271840**, edit
**$0.03271840**, no-answer **$0.04488660**. Three positive replays and three
changed-approver rejections added **$0**. Real KVM checks added **$0**.
Subsequent paid work must carry the approval-controls ledger, not checkpoint 2's.

Checkpoint 4: **$22.76245520 cumulative**, **$6.24626200** above the original
$16.51619320 aggregate. Lifecycle added **$0.05078700** across nine attempts:
delegation **$0.03538920**, keyed record plus fork **$0.00951720**, unkeyed record
**$0.00588060**; replay/resume added **$0**.
Native calibration added **$3.28988220**: smoke **$0.00048180**, first pass
**$1.62363520**, second pass **$1.66576520**.
There were **482 calibration attempts**: 481 responses (one smoke plus
480 ratings), and one attempt without an HTTP response, whose **$0.04030400**
reservation remains charged conservatively. Do not relabel that unknown
transport failure as a confirmed HTTP rate limit or a free request.
All historical reservations and the earlier smoke allowance remain included.
Follow-up campaigns have now added **789 HTTP attempts**, 1,584 cumulatively.
Browser/CLI synthetic-agent checks added **$0** provider cost. No paid job remains
running. Subsequent paid work must carry
`live-calibration-resume/budget.json`, not an older checkpoint's ledger.

Checkpoint 5 remains **$22.76245520**. All storage, migration, restore, installed
agent and public-registry controls above added **$0** provider spend.

Final checkpoint 6 is unchanged: **$22.76245520**, including every carried
historical/unknown-response reservation and the earlier smoke allowance.
There are **1,584 cumulative HTTP attempts**, 789 added during this follow-up.
The $39.50 pre-request cutoff was never reached; unused estimated headroom
below that cutoff is **$16.73754480**. No additional model requests are planned
or running. This remains a conservative provider-price estimate, not an invoice.

## 4. Failures, causes and proposed fixes

All nine stored-throughput failures are retained above. Poll p95 below 0.2 seconds
versus multi-second freshness suggests that polling latency alone does not explain
the observed freshness, but does not identify the dominant stage. A subsequent
engineering investigation should isolate queueing, batching, database work and
resource contention before selecting a fix.

The crash trials additionally demonstrate an acknowledgment boundary: successful
OTLP acceptance can precede durable storage. Document that boundary and evaluate
durable buffering/acknowledgment or retained upstream delivery before promising
crash-safe ingest. Both SQLite modes preserved every trace observed committed;
changing SQLite synchronous mode alone does not persist the in-memory assembler.

NetworkPolicy and Kata remain blocked without prohibited shared infrastructure
changes. The denial self-event false positive is an evaluator finding, not a
model retry or an executed unauthorized refund. A proposed product fix is to
associate resolutions with their original tool-call identity and exclude that
event before detecting later attempts; verification should retain a genuine
second-attempt negative control. No product fixes have been applied.

## 5. Surprises and misleading passes

Historical current-Python tests against Go 0.3.0 are not HEAD Go certification.
Assistant-created labels are not independent human calibration.

## 6. Unverified scope

All approved follow-up executions are complete, with the failures and bounded
claims retained above. The seven original groups are reconciled under the
approved audit-first scope, including the explicit exclusion of redundant
context/profile reexecution. Final resource cleanup is verified below.
The actual tag-movement experiment, signature revalidation, native browser
download event, release-package/version certification, simultaneous rolling
upgrade, power loss and production/off-site recovery remain unverified.
Strict audit was exercised with the current in-process real-server
fault-injection tests on AKS, not a separate daemon unwritable-filesystem drill.
Broader context/profile reexecution was excluded by the approved audit-first
scope because the earlier horizon campaign already covered those mechanisms.
Claude CLI field research remains conditional on per-request metering.
Publication, shared infrastructure changes and external-resource creation remain
prohibited regardless of available funds.

## 7. Cleanup

**Completed and verified.** Before deletion, the namespace's exact ownership
label was checked. Its 24 inventoried objects contained no PVCs; every service
was ClusterIP, pod volumes had no PVC references and pod specs had no Claude
credential/gateway environment entries. No node, RuntimeClass, CRD, shared CNI/
mesh policy, registry, external storage or other user's resources were changed.

The owned workbench script received SIGTERM at its exact discovered PID and
exited **0**; both its daemon and synthetic-agent ports closed. Its quiesced
database, logs, 24 synthetic-agent request entries, screenshots and exported
inputs were copied privately before deletion. The local database copy passed
`PRAGMA integrity_check`, with four terminal runs and four release decisions.
Observed database SHA-256:
`89bd0d08b2b44d542d24b729fbdd4e08287c98c57950abc64d26208fce3a0094`.

```sh
kubectl --kubeconfig ./kube-aks.yaml delete namespace \
  evalsi-verify-20261021-followup --wait=true --timeout=180s
```

**Passed.** The namespace is absent; querying its pods, services, Secrets,
ConfigMaps, NetworkPolicies, Roles, RoleBindings and PVCs returned zero objects.
This removed the worker, privileged KVM worker, pool process/sandboxes and all
owned dependency/TLS/RBAC resources. The known local forward was stopped and
both host ports **17445 and 18081** were independently verified closed.

Private pre/post-cleanup manifests, crash/migration/calibration/lifecycle evidence,
starter logs/HTML, registry results and the latest budget ledger persist locally.
The PostgreSQL dump's mode 0600 and SHA-256 were reverified; generated starter
files were byte-identical across the ordinary/privileged runs.
Only the specifically created historical-source extraction directory was removed
as build scratch; evidence and built binaries were retained. No unattended job
or hourly commit timer was installed.

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

## 4. Failures, causes and proposed fixes

All nine stored-throughput failures are retained above. Poll p95 below 0.2 seconds
versus multi-second freshness suggests that polling latency alone does not explain
the observed freshness, but does not identify the dominant stage. A subsequent
engineering investigation should isolate queueing, batching, database work and
resource contention before selecting a fix.

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

Delegation/forks/external side effects, agent-template quickstart, PostgreSQL
recovery and crash-durability drills, migration, digest-cache live controls, judge
calibration and the current-AKS browser workflow remain pending.
Claude CLI field research remains conditional on per-request metering.
Publication, shared infrastructure changes and external-resource creation remain
prohibited regardless of available funds.

## 7. Cleanup

Probe resources are removed, but the owned namespace, dependency pods and active
loopback forwarding remain necessary for the running campaign. Final namespace
deletion, forwarding shutdown and absence checks are **not complete**.

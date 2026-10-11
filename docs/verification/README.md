# Verification

End-to-end checks of Evals.si on real clusters, and where each finding stands now.

| File | What it is |
|------|------------|
| [VERIFICATION-REPORT.md](VERIFICATION-REPORT.md) | The full run: setup, every exit criterion with its evidence, product bugs F1–F6, doc differences D1–D15 and environment problems R1–R2 |
| [FINAL-RESULTS.md](FINAL-RESULTS.md) | One table of every exit criterion across the three clusters |
| [PART-C.md](PART-C.md) | M4/M5 requested subset, native dsh integration, review fixes and final-phase verification limits |

The run used commit `c5a1de9` on 2026-10-09, on three clusters:

| Cluster | Shape |
|---------|-------|
| kind | Local, arm64, kindnet (no NetworkPolicy enforcement), no KVM |
| AWC | Istio ambient with a forced waypoint and mesh-wide STRICT mTLS, Calico, KVM on every node |
| AKS | Ubuntu 24.04, every node tainted, no NetworkPolicy engine |

The report's finding IDs clash with the PRD's own D1–D11, so other documents refer to them as VR-F1…, VR-D1… and VR-R1….

## Finding status

[PR #12](https://github.com/Abhishek-Rnjn/Evals.si/pull/12) fixed most findings after the run, in commits `033b11c`, `967fdd9` and `8cf4c81`. Those fixes have unit, chart and kind tests. They have not been re-run on AKS or AWC.

| Finding | Status |
|---------|--------|
| VR-F1 judge `projects` crashes workers | Fixed (`033b11c`) |
| VR-F2 audit misses escalation refusals | Fixed (`033b11c`) |
| VR-F3 sandboxd can't set the firecracker default image | Fixed (`967fdd9`): `firecracker.defaultImage`, `diskMB` |
| VR-F4 image lacks `mkfs.ext4` | Fixed (`967fdd9`): the image ships firecracker and e2fsprogs |
| VR-F5 `global.imageRegistry` doubles a path | Fixed (`967fdd9`) |
| VR-F6 no tolerations on operator, NATS, dev Postgres | Fixed (`967fdd9`) |
| VR-D1 no macOS build | Fixed (`8cf4c81`), checked in CI |
| VR-D2 file datasets need S3 on Kubernetes | Documented (`8cf4c81`); still how it works |
| VR-D3 Helm 4 needs `--force-conflicts` after a ConfigMap edit | Documented (`8cf4c81`) |
| VR-D4 an all-errors run succeeds | Fixed (`8cf4c81`) |
| VR-D5 `env_from` is a map | Documented (`8cf4c81`) |
| VR-D6 `wasm run --params` position | Fixed (`8cf4c81`) |
| VR-D7 kind-e2e diagnostics used `id` | Fixed (`8cf4c81`) |
| VR-D8 pod rung with a non-root image | Fixed (`967fdd9`): the error names the fix (`runAsUser`) |
| VR-D9 admission refusals not counted | Counted per replica, now covered by a test (`8cf4c81`) |
| VR-D10 firecracker mode has no binary | Fixed (`967fdd9`) |
| VR-D11 e2e SandboxClass claims enforcement | Fixed (`8cf4c81`) |
| VR-D12 annotation interval off scale | Fixed (`8cf4c81`) |
| VR-D13 TLS EOF log per probe | Fixed (`8cf4c81`) |
| VR-D14 OTLP drops look like success | Fixed (`8cf4c81`): partial success |
| VR-D15 `mode: bwrap` fails on Ubuntu 24.04 (`RTM_NEWADDR`) | Mitigated: the error names `mode: privileged` and the pod rung, and the [Kubernetes guide](../guides/kubernetes.md#when-bubblewrap-pools-fail-and-when-networkpolicy-is-not-enforced) documents it. The failure itself is the host's |
| VR-R1 Istio waypoint breaks NATS and pod-rung calls | Chart values and docs (`967fdd9`): `serviceLabels`, pod `labels` |
| VR-R2 STRICT mTLS blocks admission webhooks | Fixed: `operator.istio.peerAuthentication` renders it (off by default); chart-tested, not re-run on a mesh cluster |
| bwrap fails where overlayfs lacks idmap mounts (AWC) | Mitigated: same hint and guide section as VR-D15 |
| Sandbox egress NetworkPolicy not enforced on Calico + Istio ambient (AWC) | Cause still unknown (not our selectors: the policy selects the label the pods carry). Fixed so it is never claimed falsely: the probe runs a canary connection from a sandbox pod and warns, and isolation reports `partial`, when a declared policy is not enforced. Needs a re-run on the mesh cluster ([RECHECK.md](RECHECK.md)) |

## Bug findings of 2026-10-10

[BUG-FINDINGS-2026-10-10.md](BUG-FINDINGS-2026-10-10.md) is a review and a kind run of `64de43d`; its IDs are F01–F31 (F06 retracted). Every fix below has a regression test that fails without it.

| Finding | Status |
|---------|--------|
| F01 reward params weaken `min_isolation` | Fixed (`72f2369`): the stronger level wins, in Go and Python; unknown levels are refused |
| F02 same-project policy replacement skips the stored policy | Fixed (`e317b99`) |
| F03 omitted judge sidesteps rules on the default judge | Fixed (`9224980`): rules see the effective judge (Evaluate, EvaluateStream, rewards, runs, policies); ListRuns and ListPolicies filter with it too |
| F04 stored runs ignore revoked grants | Fixed (`0d6d29f`): checked when a run is prepared and at every batch |
| F05 dataset symlink aliases cross projects | Fixed (`7bb6c49`): a path resolving into another project's promotions is refused |
| F07 failed write-backs never retry | Fixed (`0019eb1`): retried with backoff, honouring Retry-After. Pending scores still live in memory only |
| F08 write-back scores of two policies collide | Fixed (`47589b6`): the policy is part of a write's identity |
| F09 service-scoped `traces.read` cannot drive trace datasets | Fixed (`106899c`): like a trace listing, a trace dataset needs `traces.read` in the project, and each trace (service, labels) is checked as it loads, so service- and label-scoped grants both work. A query matching no readable trace has no records (InvalidArgument) |
| F10 TRL component functions reuse stale rewards | Fixed (`91e973e`) |
| F11 unfinished checkpoint evaluations pass | Fixed (`0586b29`) |
| F12 relative `datasets_dir` under a symlinked working directory | Fixed (`7bb6c49`) |
| F13 `go test ./...` does not build on macOS | Fixed (`7b83bd7`): the guest is Linux-only; sandbox tests accept the platform refusal off Linux |
| F14 worker test sockets too long on macOS | Fixed (`83a995c`) |
| F15 GNU `sed -i` in demo and harness fixtures | Fixed (`267a4d6`) |
| F16 RBAC changes stay on one replica | Fixed (`4895816`): broadcast through NATS, and every replica reloads every 10s. Not re-run with two real replicas |
| F17 operator-made workers crash on a read-only home | Fixed (`33e3e0d`): a writable home. The worker still has no readiness signal of its own |
| F18 operator-made workers lack the release's S3 and sandbox TLS Secrets | Fixed (`33e3e0d`) for Evaluators on the release's worker ConfigMap; envtest and chart tests |
| F19 label-only edits never reach the API | Fixed (`f312d51`) for policies and trace sources; a project-label change moves the object without cleaning up the old project's |
| F20 bundled MinIO cannot write its PID file | Fixed (`67d0ff9`); checked with the pinned image as uid/gid 1001 (bucket, write, read, restart) |
| F21 bootstrap keys refused by default auth | Fixed (`7ad75ab`) |
| F22 sympy grading evaluates answers as Python | Fixed (`aeb5066`): answers are vetted against an allowlist before evaluation. Not run in a separate, resource-limited process |
| F23 shadow replays bypass the stored-run quota | Fixed (`6eec747`); replicas admitting at the same moment can still each take the last slot |
| F24 promotions from several runs duplicate record IDs | Fixed (`866b310`): promoted IDs are `<run>/<record>`, appends skip IDs already present |
| F25–F27 analytics metric names, double counting, missing model | Fixed (`f97f208`) |
| F28 embedded all-error runs pass | Fixed (`db11aee`) |
| F29 reports show another trial's answer | Fixed (`f97f208`): embedded results files record each output's trial |
| F30 reports hide a numeric 1 | Fixed (`f97f208`): only passes are hidden |
| F31 no way to delete stored runs | Fixed locally (2026-10-11): authorized/audited terminal deletion removes owned records/results/outputs/webhook deliveries and recovers quotas; active/leased runs are refused. Real-server worker pipeline regression passes; external sinks and automatic retention remain separate |

## Never verified on a real cluster

KEDA scaling a live pool, several-node NATS, a real SWE-bench Verified task, vLLM checkpoint curves, and agentgateway in front of Evals.si. See [LEFTOVERS.md](../LEFTOVERS.md).

## Pipeline and scheduled pull checks (2026-10-11)

Local regressions cover unary and HTTP/2 streaming replay/conflicts, API-key
metadata-only retries without a stored plaintext secret, durable claim expiry
and uncertain outcomes, JSONL/real Parquet validation, row errors, input limits,
filesystem confinement and S3 prefix refusals. Run store tests cover all states,
live-lease refusal, quota recovery and owned webhook cleanup. The real evalsid
Python fixture uploads a dataset, executes it, reuses the same CreateRun key and
deletes the finished run. The source package's real Python worker test creates
and persists a scheduled batch report; window boundary, deadline, overlap and
restart reconciliation tests also pass.
Final review added simultaneous cache entry/byte capacity regressions on both
SQLite and disposable PostgreSQL 17.11, with race detection. PostgreSQL run
lifecycle/deletion tests also pass. Its initial 256 MiB test-container limit
killed a backend while loading the 64 MiB fixture; rerunning with a 1 GiB limit
passed. Scheduled-source manual backfill is refused without changing its cursor.

Final local gates passed: `make check` (458 Python tests, three existing skips),
`go test -race ./...`, and `make e2e` (44.004 s for the e2e package and 5.150 s
for sources, including the real-worker scheduled report). The final corrections
also pass `go test -race ./internal/store ./internal/server ./internal/source`.
Authenticated HTTP upload replay/conflict and project-admin deletion retry tests
pass. This does not certify live
S3 import, cloud connector credentials, production load or new-operation HA
failover. Initial kind attempts failed with probe timeouts and API connection
refusals. After stopping `asf-dev` with user authorization and removing stale
test fixtures, `CLUSTER=evalsi-items36-clean SKIP_LANDLOCK=1
deploy/e2e/kind-e2e.sh` exited successfully. Airgap installation, parity,
webhooks, bootstrap, bubblewrap, pod sandboxes, both reference demos, MLflow
sink and pull/write-back, restricted namespace operation and static issuer/key
checks passed. The issuer-discovery denial intentionally times out its rollout
before verifying fail-closed behavior and recovery. Landlock was not verified
on Docker Desktop's kernel; CI run 119 later passed the default amd64 kind
job with Landlock (see the recheck below).
See [the consolidated changes and fixes](CHANGES-2026-10-11.md) for items 1-6,
the final verification evidence and remaining limits.

## Trace redaction checks (2026-10-11)

The server now shares startup-compiled `traces.redaction` rules across direct
OTLP ingestion and the policy engine's push/pull storage boundary. Source
overrides are sanitized after normalization. Connector error logs and stored
error status apply the configured text rules without changing retry errors.
See [decision 0019](../decisions/0019-trace-redaction.md).

Local tests passed with raw-value sentinels checked against:

- the pre-NATS forwarding callback;
- SQLite database/WAL bytes and retrieved trace summaries and records;
- evaluator queues and result/sink callbacks in both push and pull modes;
- storage-failure logs, connector-error logs and stored source error status;
- nested JSON/metadata, resource attributes, labels, events, links, status,
  inspected byte attributes, unknown fields, disabled mode and repeat passes.

The full gates passed: `make check` (456 Python tests, three existing skips),
`go test -race ./...`, and `make e2e` with the real Python worker (43.739 s).
Commands used `TMPDIR=/tmp` for macOS worker sockets and the local buf tool
directory on `PATH` for `make check`.

This is local executable evidence, not a new Kubernetes or cloud redaction
certification. It does not prove redaction of historical storage, upstream
trace stores, arbitrary external error text, direct Evaluate requests or
offline run inputs.

## Platform-aware bundle recheck (2026-10-10)

On this laptop, Docker 28.5.1 and arm64 kind (Kubernetes 1.35.0) rejected the
original bundle: its multi-platform NATS index referenced an absent amd64
manifest. Platform-scoped exports fixed the import. A focused real arm64
bundle build, kind import and Helm install passed.

The subsequent full `deploy/e2e/kind-e2e.sh` run passed the image build, bundle,
cluster, bundle install (including sandboxd), bundle-only image assertion,
embedded/Kubernetes parity, admission webhooks, online policy sync, bootstrap
key authentication and survival across an upgrade, bubblewrap code evaluation,
and pod-rung agent evaluation and cleanup. Both demo image builds and loads
passed. An early run failed during demo installation with readiness and API/DNS
timeouts. Later runs, described below, supersede that partial result.

`make check` and `go test -race ./...` passed with `TMPDIR=/tmp` and CI's buf
1.73.0. The latest run reported 456 Python tests passed and 3 existing skips;
no test was weakened.

### Docker Desktop Landlock deferral

Later local runs passed both agents' small-repo suites (task-success 1 across
33 trials each), the demo dataset upload and MLflow sink, R6's MLflow pull and
assessment write-back (one trace, two scores), and restricted-namespace
pod-rung code and agent evaluations. The dsh nested sandbox needed the explicit
outer-sandbox setting in [decision 0018](../decisions/0018-dsh-outer-sandbox.md).
The mock model reuses bundled Python instead of pulling it under another alias.

That historical run stopped at Landlock: the Docker Desktop kernel lacks its
support. `SKIP_LANDLOCK=1` explicitly omits only the Landlock block and retains
pod-rung isolation for downstream denied issuer-discovery and static-key
recovery checks. CI and default local runs still require Landlock. The default
amd64 kind job passed with Landlock in CI run 119 on 2026-10-11; a new AKS
recheck is still pending. No complete Kubernetes pass is claimed for an
opted-out Mac run.

The subsequent command
`CLUSTER=evalsi-e2e-desktop SKIP_LANDLOCK=1 deploy/e2e/kind-e2e.sh` exited 0 on
Docker Desktop 28.5.1, arm64 kind/Kubernetes 1.35.0. Every non-Landlock step
passed, including the bubblewrap and pod-rung runs, both demo suites, MLflow
sink and R6 trace-source write-back, restricted-namespace code and agent runs,
denied issuer discovery (a restarted pod reported 403 and failed closed), and
static issuer/JWKS recovery followed by code evaluation with mean 0.5.
The job ended with `passed with Landlock deferred (not a complete Kubernetes
verification)`. The five previously paused unrelated cluster containers were
restarted afterward. This run does not verify Landlock or the default CI job.

## Mac and CI recheck (2026-10-11)

Both added commits (`60b31a9`, `ba2ceff`) were reviewed before changes. At
`ba2ceff`, this arm64 Mac with Docker Desktop client/server 28.5.1 passed:

- `PATH=/tmp/evalsi-verify-tools:$PATH TMPDIR=/tmp make check`, using CI's buf
    1.73.0: Go formatting, vet and tests; Ruff checks and format, mypy, 458 Python
    tests with three existing skips; buf lint and format. The first attempt
    stopped at missing buf; the complete rerun passed without weakening tests.
- `TMPDIR=/tmp go test -race ./...`.
- `TMPDIR=/tmp make e2e`: real Python and SWE-bench workers; e2e 32.866 s,
    source package 4.454 s.
- `CLUSTER=evalsi-verify119 SKIP_LANDLOCK=1 TMPDIR=/tmp
    deploy/e2e/kind-e2e.sh` from a new two-node kind/Kubernetes 1.35.0 cluster.
    Image build, bundle export/import and installation (including sandboxd),
    bundle-only image assertion, embedded/Kubernetes parity, admission webhooks,
    online policy, bootstrap key authentication across upgrade, bubblewrap code,
    pod-rung agent cleanup, both reference demo suites, demo MLflow scores,
    MLflow TraceSource pull/scoring/write-back, EvalRun deletion, restricted
    namespace install and pod-rung code/agent runs, denied issuer discovery and
    static issuer/key recovery all passed. The discovery denial intentionally
    times out its rollout before recovery. Docker 28.5.1 lists all three platform
    flags, exercising the scoped inspect/save/load path. Landlock was explicitly
    omitted and was not attempted on macOS.

A disposable PostgreSQL 17 database with a verified 1 GiB limit passed all
idempotency store tests with race detection. Temporary Go overlays also ran
the server's failed-call retry and completed-outcome replay tests against that
real database: the quota failure released its key, retry executed, and the
third call replayed without executing. A separate capacity probe seeded 1024
uncompleted claims older than expiry plus six days; a new claim succeeded and
subsequent pruning removed all stale claims. The existing retention regression
also confirmed unresolved claims remain blocked before that boundary. These
probes changed no branch code; the container reported `OOMKilled=false`.

The older-Docker path also passed with real Docker 27.5.1 client and daemon,
not mocks. A disposable `docker:27-dind` daemon used the classic `overlay2`
store (`containerd-snapshotter=false`). An arm64 evalsi image was loaded into
that store; `bundle.sh --tag verify119 --platform linux/arm64` exported all six
images, and the resulting `install.sh --kind evalsi-verify119-legacy --registry
airgap.invalid --with-sandboxd --sandboxd-mode privileged --sandbox-classes`
loaded, retagged and exported them for native kind, then installed the CRDs,
server with PostgreSQL, and sandboxd. Docker 27 help lists no platform flag for
inspect/save/load; recorded calls used the plain commands for all three.

An earlier probe used Docker 27's CLI against Docker Desktop 28's
multi-platform containerd store. Its unscoped archive failed kind import with
an absent manifest digest, and a later load stalled. That combination is not
verified: the successful fallback result applies to a single-platform classic
store on the matching arm64 host. Temporary CLI transport bugs were corrected
outside the repository before the successful run. No production scripts were
changed or tests weakened.

[CI run 119](https://github.com/Abhishek-Rnjn/Evals.si/actions/runs/38135174954)
completed successfully at `ba2ceffe3907dc54df35b98321723fa9136b8fce`.
Its `kubernetes` job ran the default `deploy/e2e/kind-e2e.sh` on
`ubuntu-latest` (amd64), without `SKIP_LANDLOCK`: kind setup and Kubernetes
end-to-end steps passed, including Landlock. This is CI's Linux evidence,
not Mac Landlock certification or a new AKS run. No tags were pushed.

### Release-readiness checks

The full Helm test package passed separately with
`EVALSI_REQUIRE_HELM=1 EVALSI_TEST_KUBE_VERSION=1.34.0`, `1.36.0` and `1.37.0`.
The same version loop is now required by CI. These are Helm capability and
rendered-configuration checks, not live clusters at those three versions.
`make check` and `go test -race ./...` passed again after these edits.
`make e2e` passed with the real Python and SWE-bench workers.

[RELEASING.md](../RELEASING.md) describes the required settings audit,
maintainer-authorized dry run, tag publication and exact-identity signature
verification. Live GitHub settings and PyPI trusted publishers have not been
audited in this recheck: the local CLI has no github.com login. No release
workflow was dispatched, no tag was pushed and no release was published.

## Langfuse live recheck (2026-10-10)

Both stages passed against Langfuse `v4.56.0` (release dated 2026-10-09).
The fixture uses amd64 web/worker images, emulated on this arm64 laptop and
kind cluster. The locally pulled arm64 worker had zero-byte entrypoint and
package files; this is a local observation, not certification of that image.
Stage 2 deployed these exact amd64 manifests:

| Image | Digest |
|-------|--------|
| `docker.langfuse.com/langfuse/langfuse` | `sha256:6ecbe177e900c46cb6f070c5d49e15781b6895fa0f04cd867d15e56ddd7f57bb` |
| `docker.langfuse.com/langfuse/langfuse-worker` | `sha256:589e328edcacf473734ed0b853e154313b0b243a24a8700a6885d70140b403d5` |

### Stage 1: Compose

The six services in [langfuse-compose.yaml](../../deploy/e2e/langfuse-compose.yaml)
became healthy. Runtime-generated credentials are required; do not enable shell
tracing or print Compose's resolved configuration. The successful command sequence,
with the same credential-generation method, is:

```sh
export LANGFUSE_TEST_SECRET="$(openssl rand -hex 24)"
export LANGFUSE_TEST_ENCRYPTION_KEY="$(openssl rand -hex 32)"
export LANGFUSE_TEST_PUBLIC_KEY="pk-lf-$(openssl rand -hex 16)"
export LANGFUSE_TEST_SECRET_KEY="sk-lf-$(openssl rand -hex 16)"
docker compose -f deploy/e2e/langfuse-compose.yaml up -d --wait --wait-timeout 600
EVALSI_TEST_LANGFUSE_URL=http://127.0.0.1:13001 \
EVALSI_TEST_LANGFUSE_CREDENTIAL="$LANGFUSE_TEST_PUBLIC_KEY:$LANGFUSE_TEST_SECRET_KEY" \
go test ./internal/source/langfuse -run '^TestLiveLangfuse$' -count=1 -v
```

The live test passed in 6.71s: OTLP protobuf ingestion with the v4 ingestion
header, Basic-auth observations listing, a three-span pull, numeric score
creation and replacement under the same ID. The connector's v2 observations
and POST scores paths worked unchanged. The probe's legacy score GET returned
404 in events-only mode; GET `/api/public/v3/scores?id=...` worked instead.

### Stage 2: In-cluster TraceSource

Prerequisites: the healthy stage-1 Compose fixture, the repository's Python
environment, and a disposable kind release with the demo project, CPU workers
and operator. The base release was installed by:

```sh
CLUSTER=evalsi-e2e-desktop SKIP_LANDLOCK=1 deploy/e2e/kind-e2e.sh
KUBE_CONTEXT=kind-evalsi-e2e-desktop bash deploy/e2e/langfuse-kind.sh
```

[langfuse-kind.sh](../../deploy/e2e/langfuse-kind.sh) is opt-in, not part of
the per-push kind job. It changes the existing release's source grants and egress
values and leaves the fixture for inspection. It uses ephemeral Kubernetes
storage, not production persistence. Never run it against a production release.

All six Langfuse Deployments became available. Evalsid ran in-cluster and the
`langfuse-live` TraceSource reached `Synced` and `Tailing`. Its first successful
run reported `pulled: 2`, `scored: 4` and `lastWriteBack`. Independent v3
read-back found both `tool-errors` and `loop-detection` scores with the policy
metadata. The final hardened rerun additionally required the newly injected
trace's two deterministic score IDs, avoiding success from previous runs.
The live connector test in that final run passed in 5.37s.

The Secret file, Service DNS, operator admission, source polling, policy
evaluation and score write-back passed. The network control ran from an actual
evalsid pod: allowed connection succeeded, removing only the Langfuse exception
blocked a new connection, and restoring it recovered access. This cluster uses
Kubernetes 1.35.0 and `kindest/kindnetd:v20251212-v0.29.0-alpha-105-g20ccfc88`;
its observed enforcement supersedes the assumption that all kindnet versions
lack NetworkPolicy support. Earlier cluster records above remain historical.

Stage 2 required the existing `env: file:<secret>/<key>` grant format and an
explicit host-scoped `allow_http` exception for this HTTP fixture. Admission
refused the credential without that exception. Egress also had to allow the
discovered API-server address on port 6443 for issuer discovery, not just the
Service port 443. These are stage-2 configuration differences, not connector
API differences. V3 `fields=details` omits `traceId`; read-back uses the supported
trace filter plus exact score IDs.

The earlier low-disk Docker failure was resolved after host storage cleanup.
NATS had recovered a stream behind its CPU consumer sequence; with approval,
only that test consumer was recreated. Streams and stored runs were retained.
The five previously paused unrelated cluster containers were restored with
`docker start`. No Langfuse Cloud, TLS deployment, pagination at scale, production
storage, arm64 Langfuse image or PrometheusRule behaviour is certified here.

## Databricks experiment transport (contract-tested only)

`variant: databricks` now supports numeric MLflow experiment IDs and supplied
workspace bearer credentials. V3 search, trace metadata and assessment writes
reuse the MLflow transport. Span fetch uses the signed artifact download API
defined by MLflow 3.17.0's official `databricks_artifacts.proto` and
`databricks_artifact_repo_resources.py`. The serialized spans follow its
`entities/span.py`, including JSON-encoded string attributes.

`go test ./internal/source/mlflow -count=1` passed fake-server checks for
workspace authentication, signed HTTPS download headers without forwarding the
workspace token, record and event conversion, assessment creation and redirect
refusal. The source's existing guarded client remains in use for both hosts.
No Databricks workspace was available: this is built and contract-tested, not
live-verified. Unity Catalog, SQL warehouses and automatic token acquisition
remain unsupported.

After these edits, `PATH="/tmp/evalsi-check-tools:$PATH" TMPDIR=/tmp make check`,
`TMPDIR=/tmp go test -race ./...` and `TMPDIR=/tmp make e2e` passed. Python
reported 456 passed and 3 existing skips; the real-worker e2e took 43.59s.

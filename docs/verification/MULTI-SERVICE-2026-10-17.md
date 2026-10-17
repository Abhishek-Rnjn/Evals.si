# Multi-service task environments, 2026-10-17

Bounded verification of decision [0027](../decisions/0027-multi-service-environments.md)
on the `codex/multi-service-environments` branch, based on `9718395`.
The follow-up includes fixes and live verification beyond initial PR head
`4ee52b8`. [Machine-readable results](MULTI-SERVICE-2026-10-17.json) record the
runtime, measured values, upstream revision, refusals and preserved file hashes.

## Live pod check

The new `deploy/e2e/services.py` completed against a real local kind 0.31.0 /
Kubernetes 1.35.0 cluster with Calico 3.30.3 enforcing NetworkPolicy. The pool
used the branch's compiled `evalsid`/`evalsi-guest`, with mutual TLS for the
sandbox API, and `python:3.13-slim` / `redis:7-alpine` service images.

Output:

```text
PASS: readiness, API/database grading, fresh state, trial isolation, crash errors
PASS: multi-service Terminal-Bench-format import and execution
PASS: destroy and failed-startup cleanup
```

The check established:

- Redis became ready before the HTTP API, and the agent received a ready
  environment with both services visible through the sandbox status API.
- The checker failed with an ordinary failed result before the fixture agent
  wrote the database value, then passed through the HTTP API after that write.
- A second simultaneously live trial retained its own empty database.
- From the first trial, TCP connections to the other trial's database and guest
  ports failed. Connections to its own services succeeded.
- Redis shutdown caused a subsequent checker call to report an infrastructure
  error; a service restart was not accepted as a fresh trial.
- The included adapted Terminal-Bench-format task imported, ran its oracle and
  passed its checker on the pod backend.
- Destroy and failed service startup removed their entire pods. A final label
  query returned no service pods; the pool itself remained for inspection.

This managed workspace uses nested containers. To boot the local kind node,
its missing `/dev/kmsg` was created, its containerd snapshotter was changed to
`native` because nested overlay mounts failed, and its trust store was given
the session proxy CA. Images were preloaded using containerd's local importer.
These were local environment adjustments, not product changes. The standalone
`services-kind.sh` runner is checked in for ordinary Docker/kind hosts; its full
bootstrap path has not been run unchanged in this workspace.

## Automated coverage

Go service tests cover validation, unsupported backends, dependency ordering,
resource/security manifests, no inherited agent secrets or volumes, readiness,
restart failure classification, failed-startup cleanup, bounded opt-in logs and
pool-UID-scoped reconciliation of previous process instances.

Python tests cover spec/record overrides, sandbox protocol serialization,
rejection of old servers, fresh per-trial setup without snapshots, cleanup on
setup cancellation, unsupported backends, setup network restrictions, the
restricted Compose importer and refusal of unsupported Compose options.

The generated Go/Python protocol files and EvalRun CRD schemas were regenerated.
Protobuf lint/format and shell syntax checks passed. The focused Go run included
sandbox services, run management, operator controllers and Helm chart tests;
the final Python suite passed 910 tests with 3 skipped. Ruff, formatting and
mypy also passed. `make e2e` passed both the real-worker checks and source checks,
with `NO_PROXY` extended only for the source tests' local fake hostnames.

The broad `go test -p 1 -race ./...` run hit two workspace-specific failures:
PID 1 retained bubblewrap zombies, exhausting the agent user's process slots,
and the source test's custom DNS resolver tried to resolve the session proxy.
The source package passed with its local mock-host proxy exceptions. The
current sandbox test binary passed in a separate privileged container using
bubblewrap; Landlock/cgroup/KVM checks remain subject to the host's capabilities.
The current focused service, operator and Helm tests passed with the race
detector, and `go vet -p 1 ./...` passed. No tests were disabled to obtain these
results. Final commands/results are also listed in the pull request.

## Follow-up: live faults and admission bug

The expanded `services.py` runner also completed real fault injection against the
updated pool, without waiting for CI:

- Cancelled an RPC while its service was unready: its pending pod disappeared
  and a replacement sandbox started successfully.
- Cancelled environment acquisition while setup was sleeping: the complete
  service pod disappeared.
- Set a 25-second service lifetime and ran `sleep 120`: kubelet reported
  `DeadlineExceeded`, every regular/init container terminated, and execution
  returned an infrastructure error after 27.2 seconds including creation.
- Killed an actual SDK worker with SIGKILL while its sandbox command was running:
  its 3-second idle lease and the normal 10-second reaper reclaimed the pod in
  10.0 seconds. This checks SDK-worker loss, not run-manager worker adoption.
- Killed the pool's actual container process with SIGKILL, preserving the pool
  pod UID. After its container restarted, the first service allocation deleted
  the previous process's pod; a new sandbox executed successfully and the old
  session ID returned `NOT_FOUND`.

The capacity experiment found a product bug: with `max_sandboxes: 4`, eight
simultaneous creations all succeeded and left eight service pods. Admission
previously counted registered sessions, excluding creations still starting.
The manager now reserves a slot under its mutex through startup and atomically
registers the result. Create and Restore share this path. Failed/cancelled opens
release their slots; cancelled backend results are closed. Shutdown rejects new
work, cancels and joins pending opens, and is safe to call repeatedly.

The new deterministic regressions failed against the previous manager (eight
opens admitted at a limit of two, pending creation surviving shutdown, repeated
Close panicking). They pass with the fix, including 100 repetitions under the
race detector. The corrected live burst accepted exactly four of 16 requests,
rejected the remaining 12 with the capacity error, and created only four pods.

## Bounded concurrent load

After the capacity burst, 48 API/Redis trials completed at concurrency four in
165.448 seconds. Each acquired fresh services, wrote a trial-specific value to
Redis, read it through the HTTP service, destroyed the sandbox, and waited for
its pod to disappear before freeing its client-side concurrency slot. The maximum
observed live service-pod count at acquisition was four. No service pods remained.

| Measurement | p50 | p95 |
| --- | ---: | ---: |
| Trial, including startup and pod deletion | 13.372 s | 16.299 s |
| Execution writing Redis and checking the API | 0.402 s | 0.891 s |

These measurements came from a shared, single-node local test cluster with
images preloaded. They establish bounded functional behavior, not production
throughput or latency guarantees.

## Upstream task coverage

`services_upstream.py` audits a clean checkout of
`laude-institute/terminal-bench` at
`d28711d0da2675d0bb1d56de45ae5df6082438a3`. All 241 task directories were
present, including files referenced by COPY. The importer accepts 219 and
refuses 22: all 12 original multi-service Compose tasks plus ten tasks with
unsupported/missing Dockerfiles or multi-stage/heredoc builds. Import acceptance
does not establish successful execution. The exact refusals are in the JSON
record beside this document.

The replay explicitly adapts `simple-web-scraper` to prebuilt client/server
images and the restricted Compose subset. It copies `task.yaml`, `solution.sh`
and `tests/test_outputs.py` byte-for-byte and records their SHA-256 hashes.
The server Python file is also copied unchanged. The adaptation replaces the
upstream framework's Compose builds, dynamic image/container names, log mounts,
custom network, published port and HTTP healthcheck with an explicit server
image/argv and TCP readiness on port 8000. It uses `python:3.13-slim` instead of
the framework client image, preinstalls the original requests/BeautifulSoup/pandas
versions, and replaces the network-installing test launcher with a preinstalled
venv containing the original pytest 8.4.1/pandas 2.3.2 checker dependencies.
Runtime network is denied except for private service communication.

The unchanged five upstream tests produced **0/5** before the solution,
**5/5** after running the unchanged reference solution, and **1/5** after
removing the output CSV. Both negative controls were ordinary failed grades,
with no infrastructure error. The sandbox was destroyed after replay.

To reproduce after starting the service pool and exporting its sandbox address
and mutual TLS paths:

```sh
git clone --filter=blob:none --sparse https://github.com/laude-institute/terminal-bench /tmp/terminal-bench
git -C /tmp/terminal-bench checkout d28711d0da2675d0bb1d56de45ae5df6082438a3
git -C /tmp/terminal-bench sparse-checkout set original-tasks
python/.venv/bin/python deploy/e2e/services_upstream.py \
  --upstream-root /tmp/terminal-bench --work-dir /tmp/upstream-services --prepare
docker build -t evalsi-upstream-client:test /tmp/upstream-services/client
docker build -t evalsi-upstream-server:test /tmp/upstream-services/server
kind load docker-image --name evalsi-services evalsi-upstream-client:test evalsi-upstream-server:test
python/.venv/bin/python deploy/e2e/services_upstream.py \
  --upstream-root /tmp/terminal-bench --work-dir /tmp/upstream-services
```

An intercepting HTTPS proxy may require passing its CA as a BuildKit secret:
`--secret id=proxy_ca,src=/path/to/ca.pem`. The secret is used only during package
installation. This workspace additionally used the node's local/native importer
as described above.

## Follow-up automated checks

- The complete current sandbox race-test binary passed in the separate
  privileged container; capability-dependent platform tests retained their
  existing skips. The new admission tests require no host sandbox and all ran.
- Sandbox RPC, run management, source and server/worker e2e packages passed with
  `go test -p 1 -race`; `go vet -p 1 ./...` passed.
- Python Ruff, formatting and mypy passed again. The suite rerun had 903 passes,
  three skips and seven fixture setup errors because this workspace resolves
  `go` to an unrelated `/usr/bin/go`. Selecting the real toolchain and freshly
  built server made the entire affected module pass (eight tests). Together,
  all 910 distinct tests passed; no tests were disabled. The initial branch
  verification above also recorded a clean full-suite run.
- The new e2e scripts passed Ruff/format using `python/pyproject.toml`; the shell
  runner passed `bash -n`. CI was not used as a completion gate for this follow-up.

## Limits

This is one local three-CPU node with scripted agents and no model calls. It
does not certify production scale, other CNIs/meshes, AKS/EKS, Kubernetes 1.29,
or execution of an untouched upstream Terminal-Bench suite. The adapted task
and the import audit are separate evidence. Bubblewrap/Firecracker multi-service
groups and service snapshots remain unsupported. Checker isolation remains the
existing same-sandbox behavior.

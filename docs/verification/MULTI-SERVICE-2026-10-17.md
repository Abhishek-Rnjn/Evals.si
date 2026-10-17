# Multi-service task environments, 2026-10-17

Bounded verification of decision [0027](../decisions/0027-multi-service-environments.md)
on the `codex/multi-service-environments` branch, based on `9718395`.

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

## Limits

This is one local node with scripted agents and no model calls. It does not
certify production scale, other CNIs/meshes, AKS/EKS, Kubernetes 1.29, or a real
upstream Terminal-Bench suite. The fixture proves the adapted format, not a
revised count of supported upstream tasks.

Pool process restart reconciliation and setup-cancellation cleanup have focused
automated tests, not a live process-kill experiment. The kubelet hard deadline
is rendered and unit-tested; expiry of a long-running trial was not timed in
this live check. Bubblewrap/Firecracker multi-service groups and service
snapshots are unsupported. Checker isolation remains the existing same-sandbox
behavior.

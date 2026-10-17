# 0027. Per-trial services on the pod sandbox backend

- **Status:** Accepted, implemented on this branch, 2026-10-17.

## Context

Agent tasks currently own one sandbox. An API, database or other long-lived
process cannot be started with `Exec` and expected to survive the call: the
launcher kills the command's process group when the call ends. This also means
Terminal-Bench tasks with multiple Compose services are rejected.

## Decision

`Environment.services` declares up to eight explicitly configured services.
Each has an image, argv, environment, TCP readiness port, dependencies, memory
limit and CPU limit. `service_lifetime` is mandatory (1 second to 24 hours).
The environment override on a record replaces the complete service list.

The first backend is Kubernetes pods, using native sidecar init containers
(`restartPolicy: Always`, Kubernetes 1.29+ with SidecarContainers enabled).
Services start in stable dependency order. Startup probes wait for each TCP
port before the next service starts; all services must also be ready before the
agent is given its sandbox. Each service must listen on the pod interface
(for example `0.0.0.0`), as kubelet probes connect to the pod IP.

One trial owns one pod. Services share its network and resolve service names to
loopback through `hostAliases`; ports must be distinct and 7070 is reserved for
the guest agent. Services do not mount the agent's workdir or guest binary, and
do not inherit its environment or credentials. They use the pool's pod security,
node placement, pull secrets and RuntimeClass; all Linux capabilities are
removed from service containers. Their writable image layers are private and
ephemeral. Persistent volumes, host mounts, published ports and custom networks
are not part of this API.

Multi-service creation requires `network_policy_enforced`, a successful existing
NetworkPolicy canary, and the pool's `POD_NAME`/`POD_UID`. The shipped chart and
operator supply that identity. Service pods must be in the pool pod's namespace
so Kubernetes can enforce the owner reference. `POD_NAMESPACE` (or the mounted
service-account namespace) identifies it. A declared policy whose canary leaks or cannot
run is insufficient. Other sandbox backends reject services. The Python client
also checks the sandbox protocol capability so an older sandbox server cannot
silently ignore a service declaration. Server, workers and CRDs must be upgraded
together for the new environment fields.

The existing per-sandbox policy applies at the pod boundary. Agent commands use
the guest's egress proxy when external access is allowed. Service images must
be ready to start without downloading packages; service-specific external egress
or proxy configuration is not added here. The NetworkPolicy is an external
cluster requirement, and the canary is a bounded check rather than certification
of every CNI or mesh configuration.

Every trial creates fresh services and reruns setup; no service memory, database
state or snapshot is reused. As on existing pod tasks, setup cannot request a
wider network than the agent. Use prebuilt images when setup needs downloads.
Services are available to setup, the agent and the checker.

Before and after each sandbox execution the backend inspects service readiness
and restart counts. A service that is unavailable or has restarted yields a
runner failure, which the harness reports as an infrastructure error. This is
an observation at command boundaries, not continuous health monitoring. A TCP
probe measures availability, not application correctness; the checker must
still validate application behavior.

Destroy and failed-startup cleanup delete the whole pod. Service pods have an
owner reference to the pool pod and a kubelet-enforced `activeDeadlineSeconds`.
After a pool process restart, its first service allocation reaps service pods
from previous process instances, scoped to that pool UID. If the pool never
restarts, the deadline stops service execution; Kubernetes owner garbage
collection removes the objects when the pool pod is deleted. An API outage can
delay deletion. Worker loss uses the existing idle-session reaper.

The pool's `max_sandboxes` counts both live sessions and creations still starting,
including snapshot restores. Admission reserves a slot atomically; cancellation
and failed startup release it. Manager shutdown rejects new work, cancels and
joins pending creation, then destroys registered sessions.

`Stats.services` returns readiness, restart counts and a reason. Logs are
explicitly requested with `include_service_logs`, capped at 100 lines and 16 KiB
per service; the pool requires `get pods/log`. They are not copied into scored
records automatically because application logs can contain input data.

The Terminal-Bench 1 importer accepts a documented Compose subset: a `client`
built from the task's Dockerfile, plus prebuilt services with explicit argv,
string environment values, list dependencies and an `x-evalsi` block for the
readiness port and resource limits. It refuses settings it cannot preserve.
This enables adapted tasks and the shipped fixture; it does not establish that
all previously rejected upstream tasks now work without adaptation.

## Consequences

- A task can evaluate stateful applications on the existing sandbox protocol.
- Trial isolation has one network and lifecycle boundary, without extra Services
  or per-trial RBAC objects.
- Sidecars run before the agent and consume resources throughout the trial;
  operators should set namespace resource quotas and choose appropriate limits.
- Single-sandbox tasks retain their existing manifests and execution behavior.
- Bubblewrap/Firecracker service groups, browser tooling, interactive shell
  sessions, full Compose support and clean-sandbox checker execution remain
  separate work.

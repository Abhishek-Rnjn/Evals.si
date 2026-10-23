# An API and database in each agent trial

`run.yaml` starts Redis and a tiny HTTP API beside the agent. The fixture agent
sets a database value, and the checker verifies the API returns it. A new trial
gets a new database. It uses no model and needs no provider key.

Run it against an Evals.si installation with a pod pool and enforced
NetworkPolicy:

```sh
evalsi run -f examples/multi-service/run.yaml --server "$EVALSI_SERVER"
```

Replace `spec.target.agent` to evaluate your own agent. The API is reachable at
`http://api:8080`, and Redis at `db:6379`, inside the trial. Ports are shared
within the pod and must be unique. Services listen on `0.0.0.0` for kubelet TCP
probes. Their images run the explicit `command` argv, with no implicit image
entrypoint. They inherit no agent environment or files.

Requirements: Kubernetes 1.29+ with native sidecars enabled, matching upgraded
server/workers/CRDs, the pod backend, `network_policy_enforced: true`, a passing
network canary, and pool `POD_NAME`/`POD_UID` supplied by the chart/operator.
The local, Landlock and Firecracker backends refuse this task; the bubblewrap rung runs it too (below). Pick `service_lifetime` to
cover image startup, setup, the agent and checker; the deadline terminates the
whole pod. A service crash or restart is an infrastructure error.

Inspect live services through the sandbox SDK:

```python
states = await sandbox.services()  # name, ready, restarts, reason
states_with_logs = await sandbox.services(include_logs=True)
```

Logs are opt-in and bounded to 100 lines / 16 KiB per service. They can contain
application data. The pool needs `get pods/log`, included in the chart's role.

`terminal-bench/answer` is the same task in Terminal-Bench 1 format, including
an oracle `solution.sh`. Import it with
`terminal-bench://examples/multi-service/terminal-bench?oracle=true&network=deny`.
It is a format fixture, not an upstream benchmark result. The supported
multi-service Compose subset is:

- The agent's container is `client`, or the service whose image is
  `${T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME}`. Its build can be in any context
  with any Dockerfile, build args and target; its `environment`, bind-mounted
  files of the task and `extra_hosts` are kept, and its command may only idle
  (or add host names with `echo '127.0.0.1 name' >> /etc/hosts`).
- Services: `image` or `build`, `command` and `entrypoint` by Docker's rules,
  `environment` and `env_file`, `depends_on` with `service_started`,
  `service_healthy` and `service_completed_successfully`, `healthcheck` (the
  readiness check), `ports`/`expose`, bind-mounted files of the task, named
  volumes (shared between services, and with the client's workdir),
  `working_dir`, `hostname`, `container_name`, `links`, `extra_hosts` to
  loopback, `deploy`/`mem_limit`/`cpus`, and `$VAR`, `${VAR:-x}` interpolation
  (an undefined variable is an error). A start command that installs packages
  first (`sh -c "pip install x && python app.py"`) has the installs moved into
  the service's setup (bubblewrap only), since a running service has no network.
- Refused, with the reason: `privileged`, `cap_add`, `devices`, `network_mode`,
  `pid`, `ipc`, `sysctls`; bind mounts of host paths outside the task;
  `extends`, `profiles`, `configs`, `secrets`; services on networks that share
  none; two services listening on one port (they share a loopback); a
  multi-stage Dockerfile for a service; binary files bound into a service.
  `restart`, `user`, `ulimits` and the like are dropped and listed in
  `metadata.terminal_bench.compose_ignored`.

## Services on the bubblewrap rung

A sandbox with `services` on the bubblewrap rung (no cluster needed) is a
process group on a private network: a holder process in new user and network
namespaces has only loopback, and every service and every command the agent
runs joins it through `nsenter` before its own bubblewrap sandbox starts.
Services reach each other and the agent by name (a generated `/etc/hosts`
maps names and aliases to loopback). Each service has a private copy of its
image's root, its own environment and none of the agent's files or
credentials; start order follows `depends_on`; a service is ready when its
`ready_command` exits 0, or its port accepts a connection, or, with neither,
when it has stayed up for a second; `oneshot` services must exit 0 first.
Logs are the last 16 KiB per service (`services(include_logs=True)`).
Memory, file size and processes are rlimits; `cpu_millis` is validated but not
enforced. A service that exits, or whose `service_lifetime` ends, is an
infrastructure error at the next command. Requirements: user namespaces,
`nsenter` (util-linux), `network: deny` (the private network has no route
out), and an image or none (the host's system directories, read-only). Not
supported: snapshots of a sandbox with services, `allowlist` or `allow`
networking, services that need to start as root (an image whose entrypoint
drops privileges, such as `postgres`, may fail), and several services bound to
the same port (e.g. three MinIO nodes on 9000).

To run the standalone Kubernetes lifecycle check locally:

```sh
deploy/e2e/services-kind.sh --load-trials 48 --report /tmp/service-results.json
```

It creates a named kind cluster with Calico, builds a small pool image, runs
positive and negative grading, checks fresh database state and denied
cross-trial connections, stops Redis, and checks startup-failure cleanup. It also
cancels startup/setup, times deadline expiry, kills an SDK worker and the pool
process, checks restart cleanup, bursts requests above the pool cap, and runs
concurrent API/Redis trials. Fault injection only targets its named local kind
cluster. Use `--cases basic`, `--cases lifecycle` or `--cases load` to select cases;
the default runs all three with 24 load trials at concurrency four. The cluster
remains available for inspection. CI runs it on main and manual dispatches.

The [verification record](../../docs/verification/MULTI-SERVICE-2026-10-17.md)
contains measured results, limitations and commands for replaying an adapted
upstream Terminal-Bench task with its original oracle and five tests.

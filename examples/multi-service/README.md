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
The local or bubblewrap backend refuses this task. Pick `service_lifetime` to
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

- `client`: only `build: .` and an optional `depends_on` list. The existing
  Dockerfile translator prepares the agent image and workdir.
- Services: `image`, explicit `entrypoint` argv, optional `command` argv,
  string-valued `environment`, list `depends_on`, optional `expose` matching
  the readiness port, and `x-evalsi: {port, memory_mb, cpu_millis}`.
- No mounts, persistent volumes, privilege flags, published ports, custom
  networks, sidecar builds, shell-form commands or Compose interpolation.

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

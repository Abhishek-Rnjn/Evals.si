# 0035. Service groups on bubblewrap, a broader Compose importer, and multi-stage builds

- **Status:** Accepted, implemented on this branch, 2026-10-23. Extends
  [0027](0027-multi-service-environments.md), which built services on the pod
  backend only.

## Context

0027 left three things open: services on the bubblewrap rung (the one CI and a laptop
have), an importer for Terminal-Bench 1's Compose files that was restricted on purpose
(explicit argv, no mounts, no interpolation, one client form), and multi-stage
Dockerfiles. The pinned audit of the upstream checkout imported 219 of 241 tasks and
refused 22, and importing a task that a later step cannot run is worth little.

## Decision

**Bubblewrap service groups.** A sandbox with services starts a *holder* process in new
user and network namespaces (`evalsid sandbox-netns`: loopback up, then wait on
stdin, which dies with the pool). Each service, and each command the agent runs, is
started as `nsenter --user --net --target <holder> -- bwrap --share-net ...`, so
bubblewrap keeps the group's network instead of making its own, and everything shares
one loopback with no route out. A generated `/etc/hosts` maps service names and
aliases to loopback. Each service gets a private copy of its image's root (or the host's
read-only system directories), an environment of its own, and none of the agent's
files, unless it asks for the agent's workdir (`workdir_mount`) or a named volume.
Readiness is the service's `ready_command` run in its own sandbox, else a TCP connect
from inside the network (`evalsid sandbox-probe`), else a second of staying up. A
one-shot service must exit 0 first. At each command boundary a dead service or an expired
`service_lifetime` is an infrastructure error, as on the pod backend. Services are killed
with the sandbox.

I chose `nsenter` over a supervisor that would run the agent's commands itself,
because the agent's commands then keep their existing launch path (limits, seccomp,
timeouts, cgroups). The cost is a dependency on util-linux's `nsenter`, which the rung
checks for and refuses without. Joining a namespace from Go needs a single-threaded
process, which the static, cgo-free `evalsid` is not.

Not kept on this rung, and why: `cpu_millis` (rlimits only; the pod backend enforces
it), `allowlist` and `allow` networking (the private network has no route out, and the
allowlist proxy would collide on its port between commands), snapshots (service state
is never kept, as on the pod), services that must start as root (a single-uid user
namespace cannot drop privileges, so `postgres` through its entrypoint script may
fail), and several services on one port (they share a loopback; the importer refuses
the ones it can see).

**Firecracker: not built.** The guest agent kills a command's process group when the
call ends, so a background service needs a new guest RPC and a guest binary change,
and nothing here can be run without `/dev/kvm`. The rung refuses services with that
reason instead. The pod rung keeps its own backend.

**Compose.** The importer is rewritten around Docker's rules instead of a whitelist:
the agent's service is found by name or by its image variable; `command` and
`entrypoint` combine as Docker combines them (an `image_entrypoint` flag keeps the
image's ENTRYPOINT, and the pod manifest uses `args` for it); `$VAR` interpolation
with Terminal-Bench's variables, where an undefined variable is an error rather than
a silent empty string; `healthcheck` becomes the readiness check; `depends_on`
conditions map to ordering, readiness and one-shot services; bind mounts of the task
become files (or files bound over a volume, as Docker does); named volumes are shared
between services and with the agent's workdir; limits come from `deploy`, `mem_limit`
and `cpus`. A start command that installs packages before a server is split into
setup (with the network) and command (without), because the pod's note that service
images "must be ready to start without downloading packages" is exactly what Compose
tasks violate. Refusals are for things a sandbox cannot give: privilege, host devices,
host paths, `network_mode`, `extends`, networks that share none, a client that starts
background work of its own, and the like. What is dropped (`restart`, `user`, `ulimits`)
is listed on the task.

Result on the pinned checkout (`d28711d0`): 240 of 241 import. The one refusal,
`catch-me-if-you-can`, starts a server in the agent's own container. Two earlier
"imports" were wrong: `broken-networking` and `catch-me-if-you-can` have a client
command or `extra_hosts` that the old importer discarded; the new one keeps the first
(in setup) and refuses the second.

**Multi-stage builds.** The last stage is the image. Each earlier stage that something
copies from (`COPY --from=name`, an index, or another image) becomes an entry in the
environment's `stages`, built in its own sandbox (its image, files, setup, and copies
from earlier stages), and the named files are read out through the sandbox API and
written into the target before its setup. A stage nothing copies from is never built.
The files pass through the pool, capped at 256 MiB, with execute bits restored by a
`chmod`. Globs in `--from` sources, `FROM scratch` and `RUN --mount` other than caches
are refused. A stage copied from an image alone (`COPY --from=uv /uv /bin/`) needs no
commands.

## Consequences

- A multi-service Terminal-Bench style task runs on bubblewrap in CI
  (`tests/test_real_services.py`), which the exit criterion asked for beside the pod.
- Importing a task does not mean it runs: tests that install packages at check time
  need them baked into the image, because a sandbox with services has no network.
  Copying a very large tree out of a stage (a conda install) exceeds the cap.
- The service API grew (`ports`, `ready_command`, `aliases`, `files`, `setup`,
  `setup_network`, `oneshot`, `workdir_mount`, `volumes`, `workdir`,
  `image_entrypoint`; and `volumes` on the sandbox and environment). The pod backend
  refuses the bubblewrap-only ones rather than ignoring them.

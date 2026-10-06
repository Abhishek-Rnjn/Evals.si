# 0005. Sandbox isolation ladder

- **Status:** Accepted, 2026-10-05 (design plan D6). Implemented: the standalone ladder in Phases 1 and 3, the Kubernetes ladder in Phase 4.

## Decision

Sandboxes use the strongest isolation available where they run, check it against the minimum level a task requires, and fail closed when nothing qualifies.

- **Standalone:** Firecracker (when `/dev/kvm` is usable) → static bubblewrap → Landlock → fail.
- **Kubernetes:** Firecracker via `sandboxd` on KVM nodes → bubblewrap in sandbox-pool pods → a hardened pod per sandbox → fail.

The process-confinement rungs adopt the design of the [deepseek-harness sandbox](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/sandbox/sandbox/README.md):

- fail closed;
- policy set per call;
- enforcement reported as `full` or `partial`;
- functional probes of each runner;
- separate dialects for denials and runner failures.

We harden its profile for untrusted model-generated code:

- an image root instead of the host root;
- read restrictions;
- a cleared environment and no secrets;
- network unshared by default;
- seccomp;
- resource limits.

## Consequences

- Every sandboxed record carries an `IsolationReport` (driver, level, enforcement) in its provenance. Gates can require a minimum.
- Sandbox unavailability and runner failures are infrastructure errors, never scored as model failures. Denials are the agent's own behavior and stay in its trajectory.
- bubblewrap ships as a separate statically linked binary under its own license (LGPL-2.0-or-later). Code derived from deepseek-harness keeps its MIT attribution.
- Details are in DESIGN.md §13.

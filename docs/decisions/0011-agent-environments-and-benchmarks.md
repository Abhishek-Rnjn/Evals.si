# 0011. Agent environments and benchmark grading

- **Status:** Accepted, 2026-10-05 (Phase 3)

## Decision

1. **Benchmarks are graded by their own code.**
   - SWE-bench uses `swebench`'s `get_eval_report` on the instance's eval script.
   - τ-bench uses tau2's `evaluate_simulation`.
   - Harbor uses the task's reward file; Terminal-Bench 1 uses its pytest rule.
   - BFCL uses its AST checker.

   Each adapter pins the upstream version it is contract-tested against. We do not reimplement scoring.
2. **Task formats are read directly, not run through the upstream harness.**
   - Importers turn tasks into records whose `metadata.environment` describes the sandbox.
   - Harbor and Terminal-Bench Dockerfiles are translated (`FROM`, `RUN`, `COPY`, `WORKDIR`, `ENV`, `ARG`, `SHELL`) rather than built, so no Docker daemon is needed. Tasks that need a build (multi-stage, `RUN --mount`) or several services are refused with the reason. A prebuilt image can always replace the translation.
3. **The Firecracker rung has no network device.**
   - The guest (`evalsi-guest`, the VM's init) talks to evalsid over vsock.
   - Egress, when a policy allows it, reaches the same logging egress proxy over vsock.
   - The rung needs no tap devices, nftables or root. The jailer is optional and needs root.
4. **The bubblewrap writable root is a private copy** of the image (`cp --reflink=auto`), not an overlay. The bubblewrap 0.9 found on common hosts has no overlay support.
5. **Whatever a spec makes the worker execute outside the sandbox** (stdio MCP servers, harness commands, Python harness classes, checker parsers) runs on a server only when the server config lists it. References in the `evalsi_*` packages are always allowed.
6. **Infrastructure failures are errors, never scores.** Failures of the image, setup, sandbox or checker are reported as errors on the task.

## Why

- Numbers that match the benchmark's own scoring are the point of running a benchmark. Our value is running it anywhere, with any agent, under a known isolation level, with the same scoring on every rung (the Phase 3 exit criterion).
- Requiring Docker would defeat the rootless standalone deployment and the Firecracker rung.
- Keeping the VM off the network makes egress policy one mechanism on every rung.

## Consequences

- **Upstream deviations are documented.**
  - τ-bench's user is the Evals.si simulator, through the run's judge, so scores are close to but not identical with tau2's leaderboard.
  - Terminal-Bench 1 failure names are split on the first `" - "`. This changes only test names, never verdicts.
  - Copied files exist before the first translated `RUN` step.
- **Coverage is partial.** About 9% of Terminal-Bench 1 tasks are refused, as are BFCL's multi-turn, memory and web-search categories and tau2's telecom domain.
- **Firecracker is tested in CI with a fake `firecracker`** that runs the real guest agent over unix sockets, because hosted CI has no KVM. Hosts with KVM should run `evalsid sandbox probe` before relying on the rung.
- Details are in the [agent runs guide](../guides/agent-runs.md) and DESIGN.md §23.

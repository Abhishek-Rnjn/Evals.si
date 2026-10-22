# 0037. Sandbox-time quota covers online evaluation, guardrails and reward rollouts

- **Status:** Accepted, implemented, 2026-10-22. Amends [0021](0021-sandbox-time-quotas.md).

## Context

[0021](0021-sandbox-time-quotas.md) metered what the run manager can see: agent
tasks and sandbox-pool evaluators of a run. It left out online policy
evaluation, guardrail checks, Wasm evaluators and reward-service rollouts, so a
project could occupy the sandbox pool, or burn evaluator time, outside any run.

## Decision

1. **One counter.** The same `quotas.*.sandbox_seconds_per_day` (and the same
   `quota_sandbox` table, UTC day) now also counts the wall-clock time of
   evaluators in the `sandbox` pool or running as Wasm (`wasm` pool), as their
   results report it, when they run for:
   - online policies (`path=policy`), charged to the policy's project;
   - guardrail checks (`path=guardrail`), to the guardrail's project;
   - reward-service rollouts (`path=reward`), to the request's project.
   Runs meter Wasm evaluators too (they were counted only in the sandbox pool).
2. **Where it is hooked.** All three paths call
   `evaluation.Service.RunRecordsFor(ctx, project, path, ...)`, which
   checks the project first and charges the evaluators' reported time after.
   Work that touches no sandbox or Wasm evaluator is not checked or counted, and
   a project without the quota is neither checked nor counted.
3. **Enforcement is a refusal of new work**, as for runs: a project at or over
   the day's quota gets `ResourceExhausted` from `RunRecordsFor`. What that
   means follows each path's own error handling: a reward call fails with that
   error; a guardrail check follows the guardrail's `failure_mode` (the gateway
   sees the same as for any evaluator error); an online policy's evaluation of
   that batch fails and is retried at the next opportunity, like any other
   evaluator error, so traces are scored once the quota resets or is raised
   rather than dropped. `evalsi_sandbox_quota_refused_total{path}` counts the
   refusals.
4. **The count is kept off the request path.** The meter holds charges in
   memory, adds them to the database every second, and reads a project's total
   at most every 2 seconds, so a refusal can lag the quota by about that long.
   If the database fails, work is allowed (not refused) and charges are kept for
   the next flush. Replicas count separately between writes, as the other
   quotas do.

## Consequences

- **Still not metered:** sandbox use outside a run by anything that is not one
  of these three paths (`evalsi sandbox run` by hand, the sandbox pool used by a
  worker started outside the server), and the in-process Wasm host's memory and
  CPU (only its wall-clock time counts).
- Evaluators that do not report a duration count as zero. The Wasm host and the
  sandbox-pool evaluators report one.
- Time is counted after an evaluator finishes, so a long evaluation can pass
  the quota by its own length before the next check refuses.

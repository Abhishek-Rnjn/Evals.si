# 0021. Sandbox time is a daily per-project quota, metered where a run knows its project

- **Status:** Accepted, 2026-10-12

## Context

Quotas cap concurrent and stored runs, daily judge and target tokens, and reward rollouts in flight. Sandboxed work (agent tasks and code evaluators, the most expensive thing a project can start besides model calls) was not metered, so one project could occupy a shared pool.

The sandbox itself cannot meter by project: `evalsid sandbox run` is a subprocess of the Python worker, and a sandbox pool serves many projects. The one place that knows the project, the run, and when the work started and ended is the run manager.

## Decision

1. **`quotas.default.sandbox_seconds_per_day`, with per-project overrides** like the token quotas (a zero override inherits the default). The day is the UTC day.
2. **What is metered, in wall-clock time:**
   - an agent task, from the start of the worker's `RunTask` to its end, whether it succeeds or not;
   - an evaluator whose manifest schedules it in the `sandbox` pool, for the duration its result reports.
3. **Parallel work adds up**, as core-seconds do: ten tasks of one second each are ten seconds.
4. **Enforcement is the token quotas':** a new run is refused with `ResourceExhausted` when the project is at or over the day's quota, and a running run is stopped with an error once its project's total passes it, at the next batch or task boundary. The count is in a `quota_sandbox` table beside `quota_usage`, so existing databases gain it without a column migration.
5. **A resumed run counts only the work it does after the resume**; nothing is stored per run.

## Consequences

- **Not metered:** online policy evaluation, guardrail checks, Wasm evaluators (they run in-process on wazero), reward-service rollouts, and sandbox use outside a run. These have their own limits or none.
- **Approximate by design:** a task's wall time includes the agent waiting on its model, which is what holds the sandbox. A run that crosses the line finishes its current batch or tasks first. Replicas count separately between writes, so two replicas can each admit the last run, as with the stored-run quota.
- **The unit is seconds**, not CPU-seconds or memory-time. Those would need the sandbox to report them.

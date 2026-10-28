# Lesson 5: check business state and onboard a real agent

The mock lab proves that requests and metrics flow. It cannot prove a refund
was written, written once, or written to the correct account. This lesson adds
a state checker, then describes the handoff to a real system.

## Source-only SQL lab

`environment.database` was added in 0.3.2, after the 0.3.1 release this course
pins. Do not run this spec against PyPI 0.3.1: install 0.3.2 or later, or follow
lesson 1's separate source install, activate it, and start the fake server with
that environment:

```bash
. python/.venv/bin/activate
cd examples/first-time-onboarding
python fake_model.py
```

In another terminal with the source environment active:

```bash
cd examples/first-time-onboarding
evalsi run -f database.yaml --output database-results.json
python verify.py --database
```

The direct database run should exit 0. The verifier adds both a passing SQL run
and a failing SQL run where the model skips the refund (exit 3).

Read [database.yaml](../../examples/first-time-onboarding/database.yaml). It:

1. Creates an in-memory SQLite `orders` table for each task/trial.
2. Seeds three orders as not refunded.
3. Exposes parameterized `lookup_order` and `refund` SQL tools.
4. Bounds the refund write to exactly one row with `min_rows` and `max_rows`.
5. Asserts that the final sum of refunded flags is exactly one.
6. Gates `task-success` at 1.0, alongside trajectory checks.

The fake model sends the same calls it used with mocks, but writes now affect
real isolated per-trial SQL state. A write matching zero rows is rolled back and
returned as an error. The checker queries state after the agent stops; the model
cannot alter the checker SQL through these tools.

This is a database tool environment in the harness process, not an OS sandbox
and not a connection to your production database. It deliberately exposes a
small SQL interface. The [database implementation](../../python/evalsi-harness/src/evalsi_harness/dbenv.py)
describes its authorizer and statement limits.

## Improve the checker before using it for a business decision

The course assertion verifies one refund flag in total. It does **not** establish
that the correct order was refunded. Tool-call accuracy supplies the expected
ID check in this tiny suite, but a production checker should directly assert:

- The intended order changed and all unrelated orders stayed unchanged.
- The customer owns the order and is eligible under the policy version.
- The amount and destination match the approved request.
- A repeated idempotency key did not issue another payment.
- An unsuccessful tool response left state unchanged.

Exercise: make a task-specific SQL assertion using each record's
`metadata.environment` override, then try refunding the wrong order. Keep the
negative control. For a richer existing example, use the
[support/refund starter suite](../guides/support-refund-suite.md), which includes
amounts, recipients, policy and duplicate handling with final-state grading.

## Exercise files and hidden code checkers on the released version

Return to the published environment for the `evalsi init --template agent`
exercise in lesson 1. That path executes a CLI in an actual namespaced Linux
sandbox and grades a file afterward. It is a different execution boundary from
the in-memory SQL lab. If strict doctor fails, provision the sandbox prerequisites
before trying it; lowering isolation changes the experiment.

## Replace the fake model in controlled stages

Copy `run.yaml` and change `target.model` and `target.base_url` to your approved
OpenAI-compatible endpoint. Configure authentication using the documented target
settings in [agent runs](../guides/agent-runs.md); keep secrets out of specs and
logs. Turn off the fake server's request logger when handling real content.
This course has not verified a hosted provider or real model inference.

Keep mock tools initially: this isolates protocol compatibility and model
behavior. Confirm that the model emits `tool_calls` with JSON argument strings
and consumes observations. Then replace the mocks with a controlled tool service
or a sandbox environment. A canned “refunded” response cannot validate a refund.
For an existing agent, use the documented HTTP, CLI, A2A, MCP or Responses
connector rather than rewriting its loop to fit the reference agent.

Real providers may require streaming, tool schema subsets, rate limits or
provider-specific settings. The fake endpoint implements only non-streaming chat
completions. Start with one case and inspect the entire trajectory before scaling.

## Build a representative dataset

Collect approved, sanitized examples from intended workflows. Include success,
ambiguous requests, permission denials, expired refunds, duplicate requests,
timeouts, partial writes and user corrections. Freeze a reviewed version and
record its digest. Keep a held-out set separate from prompt/harness tuning.

Use multiple independent tasks before adding trials; repeated calls on three
synthetic records do not create broad coverage. Report sample counts and
intervals. A deterministic fixture returning the same thing ten times is useful
for infrastructure checks, not model reliability estimation.

## Move the gate into CI

First make the local candidate and deliberately regressed candidate behave as
expected. Keep the spec, task data and checker under review. Pin package versions
and the action commit, configure bounded runtime, and use the
[maintained GitHub action](../guides/onboarding.md#maintained-github-action).

For this local lab CI can invoke `python verify.py` after installing the pinned
packages. That command verifies the tutorial infrastructure; it does not evaluate
your production agent. A real gate must run your candidate against approved tasks.

Before enabling enforcement, review all skipped records, evaluator errors and
gate directions. A strict mean threshold on three fixtures is a lab assertion;
select product thresholds with owners of the workflow and its failure costs.

## Handoff note template

Record the package versions and source SHA; endpoint/model snapshot; harness
profile and budgets; dataset/checker digests; trial count and independent task
count; baseline and candidate; metrics with intervals and coverage; failing
examples; and the checks that have not run. Keep raw artifacts in storage
appropriate for their data sensitivity. A colleague should be able to reproduce
the run without your terminal history.

Compare your notes with the [actual onboarding study](onboarding-study.md).

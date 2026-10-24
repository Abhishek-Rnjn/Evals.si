# First-time onboarding: from installation to an agent release gate

This is a hands-on course for someone installing **evalsi and evalsi-harness for
the first time**. You will run the real CLI and built-in harness against a local
fake model, inspect the conversations, break the agent deliberately, replay a
session, and graduate to checking business state.

The accompanying [lab](../../examples/first-time-onboarding/README.md) contains
complete runnable files. No provider key, model download or paid inference is
needed. Start with the published release; use a separate environment for the
source-only lesson. Commands assume Bash and a repository checkout.

| Lesson | Outcome | Prerequisites |
|---|---|---|
| [1. Install and diagnose](01-install.md) | Know which component works and why | Python 3.11+, network for package installation |
| [2. Score recorded runs](02-score.md) | Understand records, scores, skips and gates | SDK only |
| [3. Drive a fake model](03-tool-loop.md) | Observe real HTTP model calls and tool execution | SDK and harness |
| [4. Break, retry and replay](04-failures.md) | Diagnose wrong arguments, loops, faults and false success | Lesson 3 |
| [5. Check state and onboard a real system](05-state-and-handoff.md) | Separate text, trajectory and business correctness | Source checkout for SQL lesson; Linux sandbox for CLI starter |
| [Onboarding study](onboarding-study.md) | Review what was actually verified and what remains | No execution required |

Allow roughly 60–90 minutes for the course; that is a suggested learning schedule,
not a measured installation time. Every task and business rule is synthetic.
Three toy records validate the plumbing, not a model's production readiness.

## What you are installing

`evalsi` supplies records, evaluators, the run engine, gates, reports and the CLI.
`evalsi-harness` adds the reference agent loop: call a model, execute requested
tools, append observations, call the model again, then grade the task. It pins
`evalsi` to its own release. `evalsid` is the Go server and sandbox service;
published Linux platform wheels can bundle it, while a source install requires
a matching build for server/sandbox work.

In the mock-tool lessons, the harness runs in the Python process. There is no
sandbox environment or evalsid requirement for those lessons. The separate CLI
starter really executes code in a sandbox and has stricter prerequisites.

## The learning contract

Do not move on merely because a command printed a table. Confirm the expected
exit status, the number of scored records, zero evaluator errors, and the
negative control. A failure of the agent is evidence about behavior; an error in
the evaluator or model connection is evidence about setup. Keep those distinct.

The [existing guided setup](../guides/onboarding.md), [agent run reference](../guides/agent-runs.md)
and [built-in harness overview](../guides/built-in-harness.md) remain the detailed
references. This course supplies the first-time path through them.

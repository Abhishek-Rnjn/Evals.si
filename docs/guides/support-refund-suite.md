# A support and refund starter suite

This starter suite, added in 0.3.1, evaluates a tool-using support agent on order
refunds. It is a set of twelve editable tasks plus **state-based assertions**:
deterministic checks of the business system's final state, so a judge is never
the only evidence that a refund was correct. Run it with 0.3.1 or newer and
`evalsid` on `PATH` (see the [agent runs quick start](agent-runs.md#quick-start)):
published 0.3.0 does not include the `state-assertion` evaluator.

The files are in [`examples/support-refund`](../../examples/support-refund/).
Everything in them is synthetic.

## Why check state

An agent can answer "Refunded order 42!" after refunding the wrong amount,
refunding twice after a timeout, or refunding an order that belongs to someone
else. A judge reading the transcript can miss all three. Here the agent works
against a small order service, and the checker reads the service's database after
the agent finishes.

## Run it

```bash
cd examples/support-refund
evalsi run -f run.yaml                                    # exit 0
sed 's/agent.py, careful/agent.py, sloppy/' run.yaml > regressed.yaml
evalsi run -f regressed.yaml                              # exit 3
```

The first run uses a stand-in agent that follows the policy. The second uses the
same agent after a change that drops its checks. Each broken rule is its own
metric and its own gate, so the failure reads "refund-recipient fell to 0.8, and
refund-amount to 0.6", not just "task-success fell".

## What the tasks cover

| Category | Example task | What must be true afterwards |
|---|---|---|
| Happy path | Full refund of a delivered order | one refund of the order total, to the original method |
| Amount | A damaged item; a request for more than was paid | only the item; at most the order total |
| Recipient | A gift-card order | refunded to the gift card |
| Authorization | Outside the 30-day window; not delivered; another customer's order | no refund |
| Duplicate prevention | An order that was already refunded | no second refund |
| Retry and timeout | The gateway commits the refund, then times out | exactly one refund |
| Clarification | "I want my money back" with no order | a question, and no refund |
| Human handoff | A refund above the approval limit; a request for a person | a handoff, and no refund |

The policy is in the tool output the agent sees: a 30-day window after delivery,
refunds only to the order's owner, only for delivered orders, and refunds above
$150.00 need a human.

## How grading works

- **The service** (`shop.py`) is deliberately permissive, like an internal admin
  API. It does not enforce the policy; the agent's judgement is what is tested.
  Every write goes through a tool and into a hash-chained ledger.
- **The checker** (`check.py`) runs after the agent, in a fresh sandbox that
  receives only `shop.db` (`checker.isolation: clean`). Editing the service code
  or the interpreter cannot change the grade. It prints the harness's JSON
  format, with one entry per assertion in `tests`.
- **The expectation** for each task is in its checker command in `tasks.jsonl`,
  which the agent never sees. It lists the assertions that apply to that task.
- **`builtin/state-assertion`** turns one named entry of `tests` into a metric.
  Use it once per assertion, and give each use a `name`:

```yaml
evaluators:
  - ref: task-success
  - {ref: state-assertion, name: refund-amount, params: {assertion: refund-amount}}
  - {ref: state-assertion, name: handoff-correct, params: {assertion: handoff-correct}}
gates:
  - {metric: refund-amount, min: 1.0}
  - {metric: handoff-correct, min: 1.0}
```

A task whose checker reports the assertion as `skipped`, or not at all, is
skipped and left out of the mean. A checker that could not run is an error, not
a pass. The evaluator knows nothing about refunds: any checker that reports
named outcomes in `tests` works with it.

| Assertion | Applies to | Passes when |
|---|---|---|
| `refund-authorized` | every task | every refund was for the order the task asked about, and the policy allows it; a refund when none was due fails |
| `refund-amount` | tasks that expect a refund | the expected order was refunded exactly the expected amount |
| `refund-recipient` | tasks that expect a refund | the refund went to the original payment method |
| `no-duplicate-writes` | tasks with an order | no order was refunded twice (an already-refunded order counts), no second handoff |
| `handoff-correct` | tasks that decide on a handoff | a handoff exists exactly when expected |
| `clarification-asked` | ambiguous tasks | the agent asked, and refunded nothing |
| `ledger-intact` | every task | every refund and message went through the tools and still matches what they wrote |

## Make it yours

Edit `scenarios.py` for your policy and cases, run `python3 build.py`, and replace
`target.agent` in the spec template in `build.py` (`run.yaml` is regenerated) with your agent. The tasks carry a `category` and a `provenance`
field; keep a provenance for tasks you add, so it is clear who may reuse them.

## Limits

- Twelve synthetic tasks are a starting point. With so few, intervals are wide
  and one task moves a mean a long way; add your own cases before trusting a gate.
- `refund-authorized` and `ledger-intact` pass for an agent that does nothing.
  Gate on `task-success` and the assertions together.
- The checker takes who owns an order, its status and its total from the seed
  data, not from the database, and reports an edited order, a removed seed refund,
  a refund or message the tools did not write, or one edited after they wrote it,
  as `ledger-intact` failures. It cannot catch a
  forged ledger. Clean checking limits what an agent can change; it is not a
  defence against a determined adversary.
- The stand-in agents are rule-based. They show the gate working; they say
  nothing about how a model-driven agent will score.
- This is one workflow in one simulated service. A general database tool
  environment for other domains is not built.

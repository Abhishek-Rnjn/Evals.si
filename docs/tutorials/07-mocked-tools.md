# 7. Mocked business tools

Coding agents change files, so a checker can grade the end state. Many agents
instead call business APIs: look up an order, issue a refund, update a ticket. You
do not want those calls to reach a real system during an evaluation, and you want to
grade **which calls were made**, because the dangerous mistake is often a call that
should not have happened.

This tutorial evaluates a refund agent against mocked tools, with `tinylm-support`
as the careful agent and `tinylm-reckless` as the one that refunds first and asks
never. Keep TinyLM running from [tutorial 3](03-built-in-harness.md).

## The tasks and their expected calls

[`support.jsonl`](../../examples/tinylm/support.jsonl):

```json
{"id": "refund-paid",   "input": "Customer asks: please refund order 42.",       "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, {"name": "refund", "arguments": {"id": 42}}]}}
{"id": "refund-unpaid", "input": "Customer asks: please refund order 7.",        "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 7}}]}}
{"id": "refund-twice",  "input": "Customer asks: please refund order 13 again.", "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 13}}]}}
```

The reference lists the calls a correct agent makes. For the unpaid order and the
order already refunded, the correct behaviour is to look it up and **not** refund:
the absence of a `refund` call is part of the expectation.

## Mocked tools

[`support.yaml`](../../examples/tinylm/support.yaml) defines the tools in the spec.
Each one answers from a table keyed by its exact arguments, or with one fixed
response:

```yaml
  harness:
    builtin:
      max_steps: 6
      tools:
        mocks:
          - name: lookup_order
            description: Look up an order by id. Returns its payment status.
            input_schema: {type: object, properties: {id: {type: integer}}, required: [id]}
            responses:
              '{"id": 42}': "order 42: status paid, total $30.00"
              '{"id": 7}': "order 7: status not paid"
              '{"id": 13}': "order 13: status refunded on 2026-10-01"
          - name: refund
            description: Refund an order in full. Irreversible.
            input_schema: {type: object, properties: {id: {type: integer}}, required: [id]}
            response: "ok: refund issued"
  evaluators:
    - ref: tool-call-accuracy          # recall and precision against reference.tool_calls
    - ref: tool-errors
    - ref: run-completed
  gates:
    - {metric: tool-call-accuracy.precision, min: 1.0}   # no call the reference did not expect
    - {metric: tool-call-accuracy.recall, min: 1.0}
```

The model sees the name, description and schema exactly as it would for a real tool.
A call whose arguments are not in `responses` (and with no fallback `response`) gets
an error result, so an agent that invents an order id finds out.

There is no checker here: nothing in a sandbox changes. The environment block only
sets the sandbox policy, which still applies to the built-in tools.

## The careful agent

```bash
evalsi run -f support.yaml --output support.json; echo "exit $?"
```

```text
3 records

metric                        n  mean   95% CI          skipped  errors
tool-call-accuracy.recall     3  1.000  [1.000, 1.000]        0       0
tool-call-accuracy.precision  3  1.000  [1.000, 1.000]        0       0
tool-errors                   3  0.000  [0.000, 0.000]        0       0
run-completed                 3  1.000  [0.439, 1.000]        0       0

gates:
  [pass] tool-call-accuracy.precision mean min 1.0
  [pass] tool-call-accuracy.recall mean min 1.0
exit 0
```

```bash
python3 show.py support.json refund-paid
python3 show.py support.json refund-unpaid
```

```text
 1 model  'Let me look up order 42.' lookup_order({"id": 42})
 2 tool   lookup_order: order 42: status paid, total $30.00
 3 model  'Order 42 is paid, so it can be refunded.' refund({"id": 42})
 4 tool   refund: ok: refund issued
 5 model  'Refunded order 42.'

 1 model  'Let me look up order 7.' lookup_order({"id": 7})
 2 tool   lookup_order: order 7: status not paid
 3 model  'Order 7 cannot be refunded: order 7: status not paid.'

checker: none
scores:
  recall: 1.0  (matched 1 of 1 expected, 1 made)
  precision: 1.0  (matched 1 of 1 expected, 1 made)
```

## The reckless agent

```bash
sed 's/tinylm-support/tinylm-reckless/' support.yaml > reckless.yaml
evalsi run -f reckless.yaml --output reckless.json; echo "exit $?"
```

```text
3 records

metric                        n  mean   95% CI          skipped  errors
tool-call-accuracy.recall     3  0.167  [0.000, 0.884]        0       0
tool-call-accuracy.precision  3  0.333  [0.000, 1.000]        0       0
tool-errors                   3  0.000  [0.000, 0.000]        0       0
run-completed                 3  1.000  [0.439, 1.000]        0       0

gates:
  [FAIL] tool-call-accuracy.precision mean min 1.0 (mean 0.3333 is below 1)
  [FAIL] tool-call-accuracy.recall mean min 1.0 (mean 0.1667 is below 1)
exit 3
```

Per task:

| Task | Calls made | recall | precision | What went wrong |
|---|---|---|---|---|
| `refund-paid` | `refund(42)` | 0.5 | 1.0 | right outcome, skipped the check: missing `lookup_order` |
| `refund-unpaid` | `refund(7)` | 0.0 | 0.0 | refunded an unpaid order |
| `refund-twice` | `refund(13)` | 0.0 | 0.0 | refunded an order a second time |

```text
 1 model  'Refunding order 7.' refund({"id": 7})
 2 tool   refund: ok: refund issued
 3 model  'Refunded order 7.'

scores:
  recall: 0.0  (matched 0 of 1 expected, 1 made)
  precision: 0.0  (matched 0 of 1 expected, 1 made)
```

The two metrics fail differently, and you want both:

- **Recall** drops when an expected call is missing (`metadata.missing` names it).
  On `refund-paid` the customer got the right result, and recall still flags that
  the agent never checked.
- **Precision** drops when a call was made that the reference did not expect. That
  is the irreversible refund on `refund-unpaid` and `refund-twice`. For actions you
  cannot undo, a precision gate at 1.0 is the one that matters.

Notice what passes: `tool-errors` is 0 (every call succeeded, which is the
problem) and `run-completed` is 1 (it stopped on its own, confidently). Every
reckless answer reads as a success to the customer.

## Beyond exact matches

`tool-call-accuracy` compares names and arguments. When the order of calls matters,
or the reference is a whole trajectory, `trajectory-match` compares sequences. When
the tools are a real service you can stand up, put it in the environment and grade
its final state with a checker instead: the
[support and refund starter](../guides/support-refund-suite.md) does that with a
simulated shop, and checks business rules one assertion at a time with
`state-assertion`.

## What you learned

- `tools.mocks` gives an agent tools that answer from the spec: realistic to the
  model, harmless to the world.
- `reference.tool_calls` plus `tool-call-accuracy` grade what the agent did, not
  what it said.
- Gate precision for irreversible actions and recall for required checks.

Next: [8. The server, the UI and a regression loop](08-server-ui-and-regressions.md).

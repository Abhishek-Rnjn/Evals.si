# Lesson 4: break the agent, inject faults and replay a session

Keep the fake model running and use the example directory from lesson 3. Each
file changes one scenario while retaining the same dataset and evaluators.
Run negative controls separately: exit 3 is the intended outcome.

## A confident answer with an omitted action

```bash
evalsi run -f regressed.yaml --output regressed.json
```

The fake model uses `skip-refund`. It looks up each order and claims it refunded
it without making the refund call. Expected: exit 3, recall 0.5, precision 1.0,
contains 1.0. This is why a final-answer phrase check cannot serve as the only
release criterion. Look at the per-record tool steps before changing thresholds.

## Incorrect arguments

```bash
evalsi run -f bad-arguments.yaml --output bad-arguments.json
```

The model omits the order ID from the lookup arguments and then claims success.
Expected: exit 3, tool-call accuracy fails. Mock tools are canned fixtures; their
schema documents the intended interface but does not establish stateful business
validation. In particular, this lesson does not assume a mock rejects every
schema violation. The accuracy evaluator compares the emitted arguments with the
reference and catches the mismatch independently.

In a real service, validate parameters at the tool boundary as well. A schema
sent to a model is guidance; it is not an authorization boundary.

## A bounded loop

```bash
evalsi run -f loop.yaml --output loop.json
```

The model repeatedly requests `lookup_order`. Expected: exit 3, loop-detection
0.0, no refund call and no final text (the phrase evaluator skips). Inspect `metadata.agent.stop_reason`:
`max_steps` should explain why the harness stopped. The six-step cap bounds a
bad agent instead of letting it spend indefinitely. This fixture exercises a
step cap; real providers additionally need token, spend and wall-clock budgets.

## Tool failures and bounded retry

```bash
evalsi run -f fault.yaml --output fault.json
```

This spec injects an error into every `refund` call with `rate: 1.0`. The careful
model observes `ERROR:`, retries once, and then stops with a failure answer.
Expected: exit 3, nonzero tool-errors, contains 0.0. The evaluator has not crashed:
these are errors *made by tools inside the trajectory*, not evaluator errors.

Tool-call recall can remain 1.0 because the requested refund was attempted.
That says nothing about whether it succeeded. The explicit `tool-errors <= 0`
gate catches this scenario; final-state grading is stronger still.

To experiment, copy `fault.yaml` to another file and change the rate to 0.5.
The injected pattern is deterministic for a given task/trial configuration;
different trials can see different patterns. Do not keep rerunning until you
obtain a favorable score and call that reliability.

## Record, stop the model, then replay

```bash
evalsi run -f record.yaml --output recorded.json
```

Expect exit 0. The harness writes a tape per record and trial under `tapes/`.
Inspect one:

```bash
head -n 4 tapes/refund-42.0.jsonl
```

Return to terminal A and stop the fake model with Ctrl-C. Then:

```bash
evalsi run -f replay.yaml --output replayed.json
```

Expect exit 0 with the same behavioral metric means, without a live model.
The tape is an execution artifact, not a replacement for a versioned dataset.
Recordings bind harness configuration; changing tools, profiles or context
strategies can invalidate replay. Replay of shell/file tools can execute actions
again to rebuild an environment, so do not treat arbitrary replay as side-effect
free. This lab uses mocks only.

Delete `tapes/` when you want a fresh recording. Do not commit real conversations
or credentials. Generated artifacts from this lab are ignored by its `.gitignore`.

## Run the whole published-release matrix

This command starts its own server on a free loopback port, creates all results
and tapes in a temporary directory, and checks expected statuses and metric
invariants. Stop the manual server if you no longer need it.

```bash
python verify.py
```

Expected cases: baseline 0; omitted refund 3; loop 3; bad arguments 3; persistent
fault 3; recording 0; replay 0. Replay uses an unreachable model endpoint, proving
that model access is unnecessary for this scenario. Temporary results are removed
on completion. For inspectable artifacts use the manual commands above.

When debugging a new integration, change one thing at a time: connectivity,
model protocol, tool dispatch, task state, evaluator, then threshold. Continue to
[final-state grading and production handoff](05-state-and-handoff.md).

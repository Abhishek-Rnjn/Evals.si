_Draft._

# A regression that fails the build: gating an agent's recorded answers

A change to a prompt, a model or a tool can make an agent worse without any test going red. The usual reason is that nobody wrote a check that fails on the new behavior. This post shows the smallest version of that check: a fixed set of cases, a threshold, and a command that exits non-zero when the numbers drop.

Everything below uses the starter that `evalsi init` writes. The cases are synthetic: two capital-city questions, not a measurement of any real agent. They show how the gate behaves, nothing more.

## Create the starter

```bash
python -m venv venv
./venv/bin/python -m pip install evalsi
./venv/bin/evalsi init my-evals
```

```text
Created recorded starter in my-evals
  cd my-evals
  evalsi doctor
  evalsi run -f run.yaml --output results.json
  evalsi report results.json -o report.html
```

The directory holds `data.jsonl` (recorded answers), `run.yaml` (the spec) and `regressed.jsonl` plus `regressed.yaml`, a copy where one answer has gone wrong. No server, sandbox or model key is involved: the outputs are already recorded, and the evaluator is a literal string comparison.

The spec asks for every answer to match its reference, for no evaluator to error, and for no record to be skipped for lack of an output:

```yaml
  evaluators:
  - ref: exact-match
  gates:
  - metric: exact-match
    min: 1.0
  - metric: exact-match
    stat: GATE_STAT_ERROR_RATE
    max: 0.0
  - metric: exact-match
    stat: GATE_STAT_SKIPPED_RATE
    max: 0.0
```

## The passing run

```bash
evalsi run -f run.yaml
echo "exit code: $?"
```

```text
2 records

metric       n  mean   95% CI          skipped  errors
exact-match  2  1.000  [0.342, 1.000]        0       0

gates:
  [pass] exact-match mean min 1.0
  [pass] exact-match error_rate max 0.0
  [pass] exact-match skipped_rate max 0.0
exit code: 0
```

The mean is 1.000, and the interval beside it is `[0.342, 1.000]`. With two records, two passes still leave room for a true pass rate as low as about a third. That width is the honest answer to "is it fixed?", and it shrinks only when you add cases.

## The regression

The two files differ in one line. In `regressed.jsonl` the answer for Japan is `Kyoto` instead of `Tokyo`:

```bash
diff data.jsonl regressed.jsonl
```

```text
2c2
< {"id": "japan", "input": "Capital of Japan?", "output": "Tokyo", "reference": "Tokyo"}
---
> {"id": "japan", "input": "Capital of Japan?", "output": "Kyoto", "reference": "Tokyo"}
```

Running the regressed spec:

```bash
evalsi run -f regressed.yaml
echo "exit code: $?"
```

```text
2 records

metric       n  mean   95% CI          skipped  errors
exact-match  2  0.500  [0.095, 0.905]        0       0

gates:
  [FAIL] exact-match mean min 1.0 (mean 0.5 is below 1)
  [pass] exact-match error_rate max 0.0
  [pass] exact-match skipped_rate max 0.0
exit code: 3
```

The failing gate names the metric, the rule and the value that broke it. The exit code is 3. In `evalsi run`, 0 means the gates passed, 3 means a gate failed, and 1 means the run itself could not complete, such as a bad spec. A CI step that exits non-zero fails the job, so there is no glue code between the check and the red build.

## What this does not cover

Exact match compares text. It cannot tell that "Tokyo, Japan" is also a correct answer, and a real agent needs evaluators that fit what it does: tool-call checks, a hidden checker over the final state, or a judge you calibrate.

The repository also ships a larger starter for a refund-handling agent (`examples/support-refund`): twelve synthetic tasks, graded on a small order database after the agent finishes, with one gate for each business rule. It runs a stand-in agent inside a sandbox, so it needs a Linux host with bubblewrap, which the machine used for this draft did not have. Its commands and output are therefore not shown here; they are in the guide linked below.

## Try it

```bash
pip install evalsi
evalsi init my-evals
cd my-evals
evalsi run -f run.yaml
evalsi run -f regressed.yaml
```

Replace `data.jsonl` with approved records from your own application, add cases until the interval is narrow enough to trust, and set thresholds you can defend. The full walkthrough, including running a real agent in a sandbox and a GitHub Actions workflow, is in [Gate an agent in CI](../guides/gate-an-agent-in-ci.md). The refund suite is described in [A support and refund starter suite](../guides/support-refund-suite.md).

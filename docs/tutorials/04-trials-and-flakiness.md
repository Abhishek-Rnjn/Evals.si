# 4. Trials, flakiness and pass^k

Real agents are not deterministic. The same prompt passes on Monday and fails on
Tuesday, and one run per task tells you little about which kind of agent you have.
This tutorial uses `tinylm-flaky`, which botches one fix on every third attempt at
a task, to show what trials measure and how to gate on reliability.

Keep TinyLM running from [tutorial 3](03-built-in-harness.md) (`python3 tinylm.py --port 8765 -v 2> model.log &`).

## Run every task three times

[`flaky.yaml`](../../examples/tinylm/flaky.yaml) is the base run with two changes:

```yaml
  target:
    model: tinylm-flaky
  trials: 3
  gates:
    - {metric: task-success.pass^3, min: 0.8}            # 3 in a row, on 80% of tasks
    - {metric: task-success, stat: GATE_STAT_ERROR_RATE, max: 0.05}
```

```bash
evalsi run -f flaky.yaml --output flaky.json; echo "exit $?"
```

```text
3 records x 3 trials

metric                   n  mean   95% CI          skipped  errors
task-success             9  0.667  [0.667, 0.667]        0       0
task-success.pass@3      3  1.000  [0.439, 1.000]        0       0
task-success.pass^3      3  0.000  [0.000, 0.561]        0       0
task-success.task-score  0  -      -                     0       0
run-completed            9  1.000  [1.000, 1.000]        0       0
run-completed.pass@3     3  1.000  [0.439, 1.000]        0       0
run-completed.pass^3     3  1.000  [0.439, 1.000]        0       0

trials: run-completed would need about 97 per task for +/-0.1 on the noisiest task's mean (variance bound 0.250; you ran 3). A report only; trials is unchanged. See `evalsi determinism`.

gates:
  [FAIL] task-success.pass^3 mean min 0.8 (mean 0 is below 0.8)
  [pass] task-success error_rate max 0.05
exit 3
```

Three numbers describe the same nine attempts:

| Metric | Question it answers | Here |
|---|---|---|
| `task-success` | what share of attempts passed? | 6 of 9: 0.667 |
| `task-success.pass@3` | on how many tasks did **at least one** of 3 attempts pass? | all 3: 1.0 |
| `task-success.pass^3` | on how many tasks did **all** 3 attempts pass? | none: 0.0 |

`pass@k` is what you want when a person picks the best of k tries, or a test suite
can verify and retry. `pass^k` is what you want when every run must be right,
which is most agents in production. The same agent scores 1.0 on one and 0.0 on
the other, and a gate on the plain mean (0.667) would hide the problem behind an
average that looks like "mostly works".

Which trials failed:

```bash
python3 show.py flaky.json
```

```text
calc trial 0: stop=completed steps=5 checker=pass
money trial 0: stop=completed steps=5 checker=pass
shapes trial 0: stop=completed steps=5 checker=pass
calc trial 1: stop=completed steps=5 checker=pass
money trial 1: stop=completed steps=5 checker=pass
shapes trial 1: stop=completed steps=5 checker=pass
calc trial 2: stop=completed steps=5 checker=fail
money trial 2: stop=completed steps=5 checker=fail
shapes trial 2: stop=completed steps=5 checker=fail
```

And what the agent said when it failed:

```bash
python3 show.py flaky.json calc 2
```

```text
 5 model  'Found the bugs: add (- -> +). Writing the fix.' write_file({"path": "calc.py", ...
 6 tool   write_file: wrote 66 bytes to calc.py
 7 model  'Now a quick self-test.' bash({"command": "python3 -c \"import calc; print('add(2, 3) =',…)
 8 tool   bash: add(2, 3) = 5 mul(2, 3) = 5 [exit code 0]
 9 model  'Fixed calc.py. Self-test: add(2, 3) = 5; mul(2, 3) = 5.'

checker: fail Traceback (most recent call last): File "/workspace/test_hidden.py", line 3, in <module> assert mul…
```

Its own self-test printed `mul(2, 3) = 5`, and it reported success anyway. That
happens with real models more often than anyone would like, and it is why the
checker is hidden and the agent's word is not part of the grade.

## Read the intervals carefully

Two things in that table look odd and are not bugs:

- **`task-success` is 0.667 with the interval `[0.667, 0.667]`.** Trials of one task
  are not independent samples, so the interval is computed over tasks (clustered by
  record), and here every task has exactly the same mean, 2/3. Zero spread across
  three tasks gives a zero-width interval. It does not mean the estimate is certain;
  with three tasks nothing is.
- **`pass^3` is 0.0 with `[0.000, 0.561]`.** That is an exact binomial interval for
  0 of 3. Three tasks cannot rule out a true rate of 50%.

More tasks narrow both. More trials make `pass^k` stricter, not narrower.

## How many trials?

The `trials:` line under the table is an advisory, and so is `evalsi determinism`,
which reports how stable each metric was across trials and estimates how many
trials would pin each task's mean down to a margin:

```bash
evalsi determinism flaky.json
```

```json
{
  "metrics": [
    {"metric": "run-completed", "records": 3, "observed_constant_records": 3, "mean_pair_agreement": 1.0, ...},
    {"metric": "task-success",  "records": 3, "observed_constant_records": 0, "mean_pair_agreement": 0.3333333333333333, ...}
  ],
  "variance_aware_recommendation": {
    "metrics": [
      {"metric": "task-success", "trials_for_margin": {"0.05": 385, "0.1": 97, "0.2": 25}, ...}
    ]
  },
  "trial_recommendation": {"method": "hoeffding", "trials": 738, "margin": 0.05, "confidence": 0.95, ...},
  "note": "Observed score stability does not prove deterministic outputs. Errors and skipped results are not scores."
}
```

`mean_pair_agreement` is the share of pairs of trials of the same task that agree:
`run-completed` always agreed, `task-success` agreed on a third of pairs. The
recommendations are deliberately conservative: with three trials a pass/fail
metric's variance can only be bounded by the worst case (0.25), so they say "97
trials for ±0.1" for any pass/fail metric, varying or not. Read them as "three
trials cannot tell you much about one task's pass rate", not as a target.

Two rough edges, both reported in the [onboarding study](00-onboarding-study.md):

- The advisory line names `run-completed`, which never varied, as the metric that
  needs the most trials, because it ties with `task-success` on the worst-case bound.
- When the run includes `agent-efficiency`, the advisory applies the same absolute
  ±0.1 margin to the token count, whose values are in the thousands, and recommends
  millions of trials (`agent-efficiency.agent-tokens would need about 3994242 per
  task for +/-0.1`). Ignore it for count metrics.

## Reports

```bash
evalsi report flaky.json -o flaky.html
evalsi report flaky.json -o flaky.md
```

Both list the metrics and gates, then the lowest-scoring records with their input,
output and the checker's output, which is usually where you start debugging:

```text
## Lowest task-success

### calc (trial 2): fail
**input**

    The functions in calc.py return wrong results. Fix them so each function does what its name says. Do not add new files.

**output**

    Fixed calc.py. Self-test: add(2, 3) = 5; mul(2, 3) = 5.

> Traceback (most recent call last):
  File "/workspace/test_hidden.py", line 3, in <module>
    assert mul(2, 3) == 6, mul(2, 3)
           ^^^^^^^^^^^^^^
AssertionError: 5
```

The HTML report is one self-contained file (about 18 KB here), so it can be
attached to a CI run or a pull request as is.

## What you learned

- `trials: k` gives `pass@k` and `pass^k` for every pass/fail metric, for free.
- Gate on `pass^k` when every run must work. The mean hides flakiness.
- Intervals are over tasks. To narrow them, add tasks.
- An agent's own test output is evidence about the agent, not about the task.

Next: [5. Record, replay and compare](05-record-replay-and-compare.md).

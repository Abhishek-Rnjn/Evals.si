_Draft. Uses features on main that are not in a published release yet; publish after the next release._

# Why an eval gate can pass when 75% of the records were skipped

A gate that says `exact-match >= 0.9` reads like a statement about your data. It is a statement about the records that were scored. If most of your records have nothing to score, the gate can pass on what is left. This post builds that case on four synthetic records, shows the gate pass, and then adds the two gates that catch it.

The records and the question are made up for the demonstration. The behavior is real: every command and output below was run with a development build of the CLI from `main`.

## Four records, three without an output

Imagine a pipeline that is supposed to write an answer for each question, and has quietly stopped doing so for most of them. The recorded file looks like this:

```jsonl
{"id":"r1","input":"Capital of France?","output":"Paris","reference":"Paris"}
{"id":"r2","input":"Capital of Japan?","reference":"Tokyo"}
{"id":"r3","input":"Capital of Italy?","reference":"Rome"}
{"id":"r4","input":"Capital of Spain?","reference":"Madrid"}
```

Records `r2` to `r4` have no `output` field. The spec gates the mean of `exact-match` at 0.9:

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: skipped-demo
spec:
  dataset:
    path: records.jsonl
  evaluators:
    - ref: exact-match
  gates:
    - {metric: exact-match, min: 0.9}
```

## The gate passes

```bash
evalsi run -f run.yaml
echo "exit code: $?"
```

```text
4 records

metric       n  mean   95% CI          skipped  errors
exact-match  1  1.000  [0.207, 1.000]        3       0

gates:
  [pass] exact-match mean min 0.9

warning: the gate on exact-match passed on 1 scored evaluation(s); 3 were skipped and are not in its mean. Gate them too: {metric: exact-match, stat: GATE_STAT_SKIPPED_RATE, max: 0} or {metric: exact-match, stat: GATE_STAT_COUNT, min: N}.
exit code: 0
```

The gate passed, and the exit code is 0. The table shows why: `n` is 1 and `skipped` is 3. The mean of 1.000 is the mean of one record.

The warning is new on `main`. Without it, the only hint is the `skipped` column, which is easy to miss in a CI log.

## Why the mean leaves them out

A skipped record is one an evaluator cannot judge: exact match needs an output to compare. Scoring it as 0 would claim the answer was wrong, when there was no answer to be wrong. Scoring it as 1 would be worse. So the record is left out of the mean and counted in `skipped`. That is the right choice for one missing field in a large dataset, and it is a trap for a gate that only looks at the mean. The arithmetic is consistent, and the verdict is still not what you meant.

The interval shows the same thing from another side. `[0.207, 1.000]` is wide because it rests on a single record.

## Gate the skips

Add a gate on the skipped rate, which is the fraction of evaluations that were skipped:

```yaml
  gates:
    - {metric: exact-match, min: 0.9}
    - {metric: exact-match, stat: GATE_STAT_SKIPPED_RATE, max: 0}
```

```text
4 records

metric       n  mean   95% CI          skipped  errors
exact-match  1  1.000  [0.207, 1.000]        3       0

gates:
  [pass] exact-match mean min 0.9
  [FAIL] exact-match skipped_rate max 0.0 (skipped_rate 0.75 is above 0)
exit code: 3
```

The skipped rate is 0.75, which is the 75% in the title. The run now exits 3. `max: 0` is strict: it fails if any record is skipped. If some skips are legitimate in your data, set a small ceiling instead.

## Gate the count

A different guard asks for a minimum number of scored evaluations, whatever the cause:

```yaml
  gates:
    - {metric: exact-match, min: 0.9}
    - {metric: exact-match, stat: GATE_STAT_COUNT, min: 4}
```

```text
4 records

metric       n  mean   95% CI          skipped  errors
exact-match  1  1.000  [0.207, 1.000]        3       0

gates:
  [pass] exact-match mean min 0.9
  [FAIL] exact-match count min 4.0 (count 1 is below 4)
exit code: 3
```

The count gate also catches the case where the dataset itself shrank, since it does not care whether the missing records were skipped or never there. Use it with a number you expect, such as the size of your fixed test set. Errors are a third category, handled by `GATE_STAT_ERROR_RATE`; the starter from `evalsi init` already includes that one.

## What to take from it

Read `n` and `skipped` next to every mean. If a gate is the thing that decides whether a build passes, gate the counts that make the mean meaningful, not only the mean.

## Try it

```bash
pip install evalsi
```

The skipped-rate and count gates, and the warning, are on `main` and are not in the published 0.3.2. Gate syntax and the other statistics are in [Gate an agent in CI](../guides/gate-an-agent-in-ci.md#4-make-the-gate-trustworthy).

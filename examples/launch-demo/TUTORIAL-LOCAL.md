# Tutorial 1: catch a regression locally

**Outcome:** generate a passing run, introduce a known wrong recorded answer,
observe a nonzero gate exit, and inspect the failed example in an HTML report.
Allow about 5–10 minutes for the walkthrough; installation time depends on your
machine/network. This is an estimate, not measured independent-user onboarding.

## 1. Install published packages

Use Python 3.11+; Python 3.12/Linux amd64 was exercised for the published demo.
The following Bash commands do not install Evals.si from a source checkout:

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install 'evalsi==0.3.1' 'evalsi-harness==0.3.1'
evalsi --version
```

Expect `evalsi 0.3.1`. On Windows, activate the venv with the platform's activation
command; this package has not independently verified that host. The recorded-output
path does not require a Linux sandbox. Live code/agent execution has different
platform requirements; see the main onboarding guide.

## 2. Create the official recorded starter

```bash
evalsi init evals-demo
cd evals-demo
```

This creates a new directory and refuses to overwrite an existing one. It includes
`data.jsonl`, `regressed.jsonl`, `run.yaml`, and `regressed.yaml`. The demo package
contains equivalent fixture records and gates with descriptive run names.

Each JSONL row has an ID, input, recorded output and expected reference:

```json
{"id":"japan","input":"Capital of Japan?","output":"Tokyo","reference":"Tokyo"}
```

There is no generation target in the spec. These outputs already exist; Evals.si
scores them without running an agent or calling a model.

## 3. Run the passing control

```bash
evalsi run -f run.yaml --output baseline.json
echo "exit code: $?"
evalsi report baseline.json -o baseline.html
```

Expected: 2 scored records, exact-match mean **1.000**, zero errors/skips, passing
gates and exit **0**. Open `baseline.html` in your browser.

The mean threshold requires every fixture to match. A separate error-rate gate
requires zero evaluator errors:

```yaml
gates:
  - {metric: exact-match, min: 1.0}
  - {metric: exact-match, stat: GATE_STAT_ERROR_RATE, max: 0.0}
```

This simple gate is not a complete coverage contract. Missing/skipped records,
unrepresentative data and a poorly chosen evaluator need their own review.

## 4. Run the deliberately regressed control

The negative fixture changes Japan's recorded answer from `Tokyo` to `Kyoto`.
In Bash, capture the intentional failure without hiding its exit status:

```bash
set +e
evalsi run -f regressed.yaml --output regressed.json
status=$?
set -e
echo "exit code: $status"
test "$status" -eq 3
evalsi report regressed.json -o regressed.html
evalsi report regressed.json -o regressed.md
```

Expected: exact-match mean **0.500**, zero evaluator errors/skips, a failed mean
gate and exit **3**. In `regressed.html`, the lowest-scoring example identifies
`japan`, output `Kyoto`, reference `Tokyo`. Exit 3 means a gate failed; an execution
or configuration error is different and must also fail your CI step.

The 95% intervals are deliberately wide: approximately `[0.342, 1.000]` for the
baseline and `[0.095, 0.905]` for the negative control. The demo proves the
mechanics on two known fixtures; it does not establish general performance.

## 5. Apply this to your own application

Export your application's recorded answers using stable case IDs, reviewed
references and a recorded application/model configuration. Use representative
cases and preserve tuning versus held-out boundaries. Choose evaluators that
measure the intended behavior: exact-match can be inappropriate for paraphrases,
and textual answers alone do not establish correct business-state changes.

Generate candidate outputs before running the CI check, rather than repeatedly
scoring these static geography fixtures. Keep datasets, references and evaluator
settings reviewed. Do not interpret a simple mean gate as a paired noninferiority
test. For explicit approved baseline/candidate evidence and release policies,
continue with [release decisions](../../docs/guides/release-decisions.md).

Next: [publish a CI result and reports](TUTORIAL-CI.md).

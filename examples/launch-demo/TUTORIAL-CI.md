# Tutorial 2: publish the result in GitHub Actions

**Outcome:** a real gate exit controls the job, while its JSON/Markdown/HTML report
is retained even when the gate fails. The example uses static recorded outputs;
it becomes application protection only after your app refreshes those outputs.

## 1. Add the evaluation files

From your repository root, after installing the pinned packages from tutorial 1:

```bash
evalsi init evals
git add evals/run.yaml evals/regressed.yaml evals/data.jsonl evals/regressed.jsonl
```

Review those files before committing. Then copy [the workflow template](github-actions.yml)
to `.github/workflows/evalsi-recorded.yml`. It expects the starter at `evals/`.
For an existing project, preserve its files and adapt the paths rather than
overwriting a directory.

## 2. Understand the gate

The job installs `evalsi==0.3.1` and `evalsi-harness==0.3.1`. On a PR it checks
`evals/run.yaml`. A manual dispatch can select the passing or regressed fixture.

The gate step runs `evalsi run` directly, with no `continue-on-error` and no
`|| true`. Selecting `regressed` should make the job fail with exit 3. Subsequent
report/artifact steps use `if: always()` so a failed gate still has evidence.

This differs from `scripts/run_demo.py`: that local verification harness checks
both expected outcomes and returns success when the negative control correctly
fails. Do not use the verification harness as your production gate.

## 3. Verify both CI outcomes

After reviewing and committing the workflow, dispatch it with `fixture=baseline`:
the gate should pass. Dispatch `fixture=regressed`: the gate should fail, while
the job summary and `evalsi-recorded-report` artifact remain available. Inspect
the Japan example in the report. Actual Actions runs require your repository's
permissions, enabled Actions and the relevant network access.

The supplied materials validate equivalent commands locally. They do not claim
that these two manual runs have already happened in your repository. Record their
run URLs when you execute them; do not use an example URL as verification evidence.

## 4. Connect your real application

Add a reviewed step that generates candidate outputs from the checked-out
application before the gate runs. Keep test cases and references independently
reviewed and stable. Keep the failing control as a separate workflow-verification
check. Scoring committed static fixtures on every PR does not evaluate a changed
prompt, model or agent by itself.

Only synthetic records are uploaded by this template. Reports can contain inputs,
outputs and references: review artifact visibility, retention and data permissions
before using private/customer cases. The seven-day retention below is an example,
not an erasure guarantee. Keep credentials in the repository's secure settings if
your own generation step needs them; this offline example requires none.

See [the companion report](REPORT.md) for the actual local results and limits.

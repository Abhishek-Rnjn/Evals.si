# Evaluator validation and judge calibration, 2026-10-21

Scope: an independent Python CLI/SDK workflow for validating evaluators against
frozen human pass/fail judgements on fixed recorded outputs. No target execution
or server schema/API changes. See the [guide](../guides/judge-calibration.md),
[decision 0031](../decisions/0031-evaluator-calibration.md) and
[offline example](../../examples/judge-calibration/README.md).

## Evidence

| Check | Result |
|---|---|
| Full Python workspace | 1422 passed, 19 skipped, 2 existing warnings in 112.20 seconds with the correct compiler/built daemon; no failures |
| Focused calibration tests | 55 passed, including CLI/SDK, annotation protobuf JSON, pins/replacements, concurrent publication, split leakage, missing labels, threshold selection, metrics and repeated-call controls |
| Real-client judge transport | OpenAI-compatible client against a local HTTP endpoint: four distinct prompts called twice, eight fresh requests, alternating verdicts, response model identities and prompt/response hashes retained, no local cache file created, environment key absent from report |
| Tuning isolation | Changing held-out labels cannot change threshold selection; a candidate that wins held-out results cannot replace the candidate selected on tuning; incomplete/one-class tuning refuses threshold sweeps |
| Missing evidence | Errors, skips, non-finite/missing scores and a failed repeat remain incomplete; no score imputation or successful-repeat cherry-picking. Human ties remain unlabelled with all votes retained |
| Statistics | Equal weight per complete labelled case; repeats do not increase cases/groups; whole-group bootstrap and paired deltas; undefined/unstable denominators, one group and uniform bootstrap samples leave intervals unavailable |
| Offline example | Exact/fuzzy comparisons, a near-match false positive, human dissent, rejected dataset-pin control and a stricter-threshold judge-drift comparison on identical outputs |
| Formatting and typing | Workspace Ruff lint/format and strict mypy pass; example script also checked |
| Server integration regression | All 12 existing `test_client_evaluate.py` tests pass against a freshly built daemon and this checkout's Python worker |
| Documentation | `npm run build` in `website`: 86 pages built, new guide and decision included |
| HTML browser review | Chromium/Playwright at 1440px and 390px: both candidate rows visible, unresolved human consensus retained, no document overflow, zero scripts, external requests or page errors; table scrolls within its region on mobile |

Reproduce the main checks:

```bash
cd python
uv run ruff check .
uv run ruff format --check .
uv run mypy
uv run pytest -q
uv run python ../examples/judge-calibration/run.py --out /tmp/evalsi-calibration-demo
cd ../website
npm run build
```

The full suite needs an actual Go compiler on `PATH` or an explicitly built
`EVALSI_E2E_EVALSID` for the existing server integration fixtures. This workspace
also has an unrelated board-game executable named `go`; its initial fixture
build failed with `Go: Unknown option: build`. Rebuilding with the compiler at
`/workspace/toolchain/go/bin/go` and supplying the new daemon resolves it.
The two existing warnings concern a fork from a multithreaded logging test and
an asyncio subprocess transport being finalized after its harness loop closed.

## Limits

All labels here are synthetic fixtures. The HTTP judge is scripted. No hosted
model was billed, no real human-rated dataset was collected and no deployed
judge was certified. Existing real-human calibration requirements remain open.

This version handles binary human labels and record-scope numeric/pass-fail
evaluator outputs. Continuous scores, multiclass labels, pairwise preferences,
learned judge weights and a server/browser workspace remain open. The offline
HTML report is a review artifact, not a new server workflow.

Groups and task independence are declared by the caller. Exact input overlap
checks do not prove semantic independence. Bootstrap intervals are approximate;
small, uniform and class-sparse samples can lack an interval. Slice/comparison
results have no multiplicity adjustment. Human majority votes can be wrong.

Artifacts include records and comments and are private by default. Pins detect
changes, with trusted external pins needed to identify approved artifacts; they
do not establish label authorship, executed dependency bytes or model weights.
Provider-side caching/retries and remote model changes remain outside Evals.si's
cache control. Preserve the environment lockfile and third-party dependencies.

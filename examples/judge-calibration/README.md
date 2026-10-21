# Evaluator validation, offline

This compares exact and fuzzy matching against **synthetic fixture ratings** on
fixed recorded answers. It exercises the calibration workflow; it is not evidence
that either evaluator, or any LLM judge, is calibrated for real use.

From the repository root, with the Python workspace installed:

```bash
cd python
uv run python ../examples/judge-calibration/run.py --out /tmp/evalsi-calibration-demo
```

Open `/tmp/evalsi-calibration-demo/report.html`. The report includes thresholds
chosen only on tuning cases, held-out false positives and false negatives, slice
denominators, raw disagreements and an unresolved human tie. Fuzzy matching can
accept the incorrect near-match `101 degrees Celsius`; inspecting its false
positive matters as much as the aggregate agreement. Repeats of deterministic
evaluators have zero variation, and add no independent examples.

The script also verifies a rejected dataset-pin control and compares a stricter
threshold on the same outputs and labels in `judge-drift.json`. It writes a
digest-pinned dataset, replayable protocol and JSON report. Every output is new;
use a different directory to run again.

For real annotation exports and alternative LLM judge models/prompts, see the
[calibration guide](../../docs/guides/judge-calibration.md). The HTTP integration
test uses the real OpenAI-compatible client against a local scripted endpoint,
asserting fresh calls, differing verdicts and reported response model identities.

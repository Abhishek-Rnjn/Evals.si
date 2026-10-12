"""Run the custom evaluator on a dataset.

cd python/evalsi && uv run python ../../examples/custom-evaluator/run.py
"""

from pathlib import Path

from valid_citations import valid_citations

import evalsi

result = evalsi.evaluate(Path(__file__).with_name("data.jsonl"), [valid_citations])

for r in result.results:
    print(f"{r.record_id:<11} {r.outcome.name:<8} {[s.explanation or s.value for s in r.scores]}")
m = result.metric("valid-citations")
print(f"\nvalid-citations: {m.mean:.2f} over {m.n} scored, {m.skipped} skipped")

# Custom evaluator

A worked example of a Python evaluator you write yourself: `acme/valid-citations`
checks that a RAG answer's `[n]` citations each point at a retrieved passage.

| File | What it is |
|---|---|
| [`valid_citations.py`](valid_citations.py) | The evaluator: a decorated function |
| [`test_valid_citations.py`](test_valid_citations.py) | Unit tests and an end-to-end run |
| [`data.jsonl`](data.jsonl) | Four records: good, dangling citation, uncited, no context |
| [`run.py`](run.py) | Runs it and prints the results |
| [`pyproject.toml`](pyproject.toml) | Packages it and registers the pack by entry point |

```bash
cd python/evalsi
uv run pytest ../../examples/custom-evaluator              # the tests
PYTHONPATH=../../examples/custom-evaluator uv run python ../../examples/custom-evaluator/run.py
```

```
good        SCORED   ['2 valid citation(s)']
dangling    SCORED   ['cites [3], but only 1 passage(s) were retrieved']
uncited     SCORED   ['0 citation(s), expected at least 1']
no-context  SKIPPED  []

valid-citations: 0.33 over 3 scored, 1 skipped
```

## How it works

```python
@evaluator(
    name="acme/valid-citations",  # namespaced: <owner>/<name>
    version="1.0.0",
    requires=Requirements(output=True, context=True),
    outputs=[MetricSpec("valid-citations", ScoreType.PASSED, higher_is_better=True)],
)
def valid_citations(record: Record, *, min_citations: int = 1) -> Score: ...
```

- **Params** are the keyword-only arguments. `min_citations` defaults to 1;
  pass `valid_citations.bind({"min_citations": 2})`, or `params=` to `evaluate()`.
  Unknown params are rejected.
- **`requires`** declares what a record must have. A record with no `context`
  is **skipped**, not scored as zero, so it does not drag the mean down (the
  `no-context` row above).
- **`SkipRecord`** lets the function skip a record for other reasons. Any
  other exception is reported as an **error** and excluded from the metrics.
- **`Score`** holds exactly one of `number`, `passed`, `label` or `structured`,
  plus an `explanation` that appears in reports.
- The decorated object is callable and `.bind(params).run(...)` runs it the
  way the runner does, which is what the unit tests use.

## Using it elsewhere

- **Python**: pass the object to `evalsi.evaluate(data, [valid_citations])`.
- **By name** (`evalsi eval --evaluators acme/valid-citations`, servers,
  online policies): this example is already a package. `valid_citations.py`
  ends with a `Pack`, and `pyproject.toml` registers it under the
  `evalsi.packs` entry-point group. Install it next to evalsi:

  ```bash
  pip install ./examples/custom-evaluator
  evalsi catalog --pack acme
  evalsi eval --data examples/custom-evaluator/data.jsonl --evaluators acme/valid-citations \
    --param valid-citations.min_citations=2
  ```

- **Sandboxed and shareable**: write it as a Wasm plugin instead, see
  [`../wasm/text-checks`](../wasm/text-checks) and the
  [plugins guide](../../docs/guides/plugins.md). Wasm evaluators cannot call a judge.
- **LLM-judged metric**: add a `ctx` parameter to receive the judge client
  and set `judge=True` in `Requirements`.

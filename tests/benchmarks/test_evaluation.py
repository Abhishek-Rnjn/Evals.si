from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

spec = importlib.util.spec_from_file_location(
    "evaluation_benchmark", Path(__file__).with_name("evaluation.py")
)
benchmark = importlib.util.module_from_spec(spec)
spec.loader.exec_module(benchmark)


def test_normalization_is_detected_as_incorrect():
    rows = benchmark.corpus(12)
    normalized = [
        (r["id"], float(r["output"].strip().lower() == r["reference"].strip().lower()))
        for r in rows
    ]
    with pytest.raises(ValueError, match="incorrect"):
        benchmark.validate(rows, normalized)


@pytest.mark.parametrize("failure", ["missing", "duplicate", "nan", "extra"])
def test_invalid_results_cannot_become_fast_benchmark_samples(failure):
    rows = benchmark.corpus(12)
    scores = [(r["id"], float(r["output"] == r["reference"])) for r in rows]
    if failure == "missing":
        scores.pop()
    elif failure == "duplicate":
        scores.append(scores[0])
    elif failure == "nan":
        scores[0] = (scores[0][0], float("nan"))
    else:
        scores.append(("unknown", 1.0))
    with pytest.raises(ValueError):
        benchmark.validate(rows, scores)


def test_result_order_does_not_change_correctness_digest():
    rows = benchmark.corpus(12)
    scores = [(r["id"], float(r["output"] == r["reference"])) for r in rows]
    assert benchmark.validate(rows, scores) == benchmark.validate(
        rows, list(reversed(scores))
    )

"""Unit tests and an end-to-end check for the custom evaluator.

Run from python/evalsi:  uv run pytest ../../examples/custom-evaluator
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from valid_citations import valid_citations

import evalsi
from evalsi import Content, EvalContext, Record, Score

DATA = Path(__file__).with_name("data.jsonl")


def make(output: str, context: list[str] | None = None) -> Record:
    return Record(
        id="r1",
        output=Content.from_value(output),
        context=[Content.from_value(c) for c in context or []],
    )


def score(record: Record, **params: object) -> Score:
    bound = valid_citations.bind(params)
    return asyncio.run(bound.run(record, EvalContext()))[0]


def test_spec_is_declared() -> None:
    spec = valid_citations.spec
    assert spec.ref == "acme/valid-citations@1.0.0"
    assert spec.params == {"min_citations": 1}
    assert spec.requires.context


@pytest.mark.parametrize(
    ("output", "context", "params", "passed"),
    [
        ("Paris [1].", ["a"], {}, True),
        ("A [1] and B [2].", ["a", "b"], {}, True),
        ("A [1] and B [2].", ["a"], {}, False),  # [2] has no passage
        ("A [0].", ["a"], {}, False),  # markers are 1-based
        ("No citations.", ["a"], {}, False),
        ("No citations.", ["a"], {"min_citations": 0}, True),
        ("Only one [1].", ["a", "b"], {"min_citations": 2}, False),
    ],
)
def test_scoring(output: str, context: list[str], params: dict[str, int], passed: bool) -> None:
    result = score(make(output, context), **params)
    assert result.passed is passed
    assert result.name == "valid-citations"
    assert result.explanation


def test_dangling_citation_is_named_in_the_explanation() -> None:
    assert "[3]" in score(make("X [3].", ["a"])).explanation


def test_unknown_param_is_rejected() -> None:
    with pytest.raises(evalsi.EvaluatorConfigError):
        valid_citations.bind({"min_citation": 2})


def test_records_without_context_are_skipped_not_scored_zero() -> None:
    result = evalsi.evaluate(DATA, [valid_citations])
    by_id = {r.record_id: r for r in result.results}
    assert by_id["no-context"].outcome is evalsi.Outcome.SKIPPED
    summary = result.metric("valid-citations")
    assert (summary.n, summary.skipped, summary.errors) == (3, 1, 0)
    assert summary.mean == pytest.approx(1 / 3)  # only "good" passes

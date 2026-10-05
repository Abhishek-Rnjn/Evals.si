from __future__ import annotations

import asyncio

import pytest

from conftest import make_record
from evalsi import EvalContext, Record, Score, SkipRecord, Usage
from evalsi.evaluator import EvaluatorConfigError, EvaluatorDef
from evalsi.packs import core


def run(ref_def: EvaluatorDef, record: Record, **params: object) -> list[Score]:
    return asyncio.run(ref_def.bind(params).run(record, EvalContext()))


def one(ref_def: EvaluatorDef, record: Record, **params: object) -> Score:
    scores = run(ref_def, record, **params)
    assert len(scores) == 1
    return scores[0]


@pytest.mark.parametrize(
    ("output", "reference", "params", "expected"),
    [
        ("Paris", "Paris", {}, True),
        ("  paris ", "Paris", {}, True),
        ("paris", "Paris", {"normalize": False}, False),
        ("Paris.", "Paris", {}, False),
        ("Paris.", "Paris", {"ignore_punctuation": True}, True),
        ("canberra", ["Canberra", "Canberra, ACT"], {}, True),
        ("Sydney", ["Canberra", "Canberra, ACT"], {}, False),
    ],
)
def test_exact_match(
    output: str, reference: object, params: dict[str, bool], expected: bool
) -> None:
    score = one(core.exact_match, make_record(output, reference), **params)
    assert score.passed is expected
    assert score.name == "exact-match"


def test_contains_uses_reference_or_param() -> None:
    assert one(core.contains, make_record("The answer is PARIS", "paris")).passed
    assert one(core.contains, make_record("hello world", None), substring="WORLD").passed
    assert not one(
        core.contains, make_record("hello", None), substring="x", ignore_case=False
    ).passed
    with pytest.raises(SkipRecord):
        one(core.contains, make_record("hello", None))


def test_regex_match_requires_pattern() -> None:
    with pytest.raises(EvaluatorConfigError, match="requires params"):
        core.regex_match.bind({})
    assert one(core.regex_match, make_record("order #123"), pattern=r"#\d+").passed
    assert not one(
        core.regex_match, make_record("order #123"), pattern=r"#\d+", full_match=True
    ).passed


def test_fuzzy_match_takes_best_reference() -> None:
    score = one(core.fuzzy_match, make_record("colour", ["color", "colour"]))
    assert score.number == 1.0
    assert 0 < one(core.fuzzy_match, make_record("colr", "color")).number < 1  # type: ignore[operator]


@pytest.mark.parametrize(
    ("output", "reference", "expected"),
    [
        ("3 x 12 = 36, minus 4 leaves 32 apples.", "32", True),
        ("That comes to $14,000 per year.", "14400", False),
        ("The total is $1,250.", "#### 1250", True),
        ("about 0.5", "0.50", True),
        ("-3 degrees", "-3", True),
    ],
)
def test_numeric_match(output: str, reference: str, expected: bool) -> None:
    assert one(core.numeric_match, make_record(output, reference)).passed is expected


def test_numeric_match_skips_and_fails_cleanly() -> None:
    with pytest.raises(SkipRecord):
        one(core.numeric_match, make_record("42", "Paris"))
    score = one(core.numeric_match, make_record("I don't know", "42"))
    assert score.passed is False
    assert "no number" in score.explanation


def test_json_valid() -> None:
    assert one(core.json_valid, make_record('{"a": 1}')).passed
    assert not one(core.json_valid, make_record("{a: 1}")).passed


def test_json_schema() -> None:
    schema = {"type": "object", "properties": {"a": {"type": "integer"}}, "required": ["a"]}
    assert one(core.json_schema, make_record('{"a": 1}'), schema=schema).passed
    bad = one(core.json_schema, make_record('{"a": "x"}'), schema=schema)
    assert bad.passed is False
    assert bad.explanation.startswith("a:")
    assert not one(core.json_schema, make_record("nope"), schema=schema).passed


def test_length_units() -> None:
    record = make_record("one two three")
    assert one(core.length, record).number == 3
    assert one(core.length, record, unit="chars").number == 13
    with pytest.raises(ValueError, match="unit"):
        one(core.length, record, unit="tokens")


def test_usage_evaluators() -> None:
    record = make_record(
        usage=Usage(input_tokens=10, output_tokens=5, latency_ms=120, cost_usd=0.01)
    )
    assert one(core.latency, record).number == 120
    assert one(core.cost, record).number == 0.01
    tokens = {s.name: s.number for s in run(core.token_usage, record)}
    assert tokens == {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
    with pytest.raises(SkipRecord):
        one(core.latency, make_record())
    with pytest.raises(SkipRecord):
        run(core.token_usage, make_record(usage=Usage(latency_ms=3)))

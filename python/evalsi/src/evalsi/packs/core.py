"""The ``core`` pack: cheap, deterministic, dependency-free evaluators. On by default."""

from __future__ import annotations

import difflib
import json
import math
import re
import string
from typing import Any

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.registry import Pack
from evalsi.types import Content, Record, Score

PASSED = ScoreType.PASSED
NUMBER = ScoreType.NUMBER
REFERENCE = Requirements(reference=True)

_WHITESPACE = re.compile(r"\s+")
_PUNCTUATION = str.maketrans("", "", string.punctuation)
_NUMBER = re.compile(r"-?\d[\d,]*(?:\.\d+)?(?:[eE][-+]?\d+)?|-?\.\d+")


def _normalize(text: str, *, ignore_case: bool, ignore_punctuation: bool) -> str:
    if ignore_case:
        text = text.casefold()
    if ignore_punctuation:
        text = text.translate(_PUNCTUATION)
    return _WHITESPACE.sub(" ", text).strip()


def _references(content: Content) -> list[str]:
    """Acceptable answers: a JSON list means any of its items."""
    if content.is_json and isinstance(content.json, list):
        return [Content.from_value(item).as_text() for item in content.json]
    return [content.as_text()]


def _output_text(record: Record) -> str:
    assert record.output is not None  # guaranteed by Requirements(output=True)
    return record.output.as_text()


@evaluator(
    name="builtin/exact-match",
    version="1.0.0",
    description="Output equals the reference (any of them, if the reference is a list).",
    requires=REFERENCE,
    outputs=[MetricSpec("exact-match", PASSED, higher_is_better=True)],
)
def exact_match(
    record: Record,
    *,
    normalize: bool = True,
    ignore_punctuation: bool = False,
) -> Score:
    assert record.reference is not None
    output = _output_text(record)
    refs = _references(record.reference)
    if normalize:
        output = _normalize(output, ignore_case=True, ignore_punctuation=ignore_punctuation)
        refs = [
            _normalize(r, ignore_case=True, ignore_punctuation=ignore_punctuation) for r in refs
        ]
    return Score(passed=output in refs)


@evaluator(
    name="builtin/contains",
    version="1.0.0",
    description="Output contains the given substring, or the reference when none is given.",
    outputs=[MetricSpec("contains", PASSED, higher_is_better=True)],
)
def contains(record: Record, *, substring: str | None = None, ignore_case: bool = True) -> Score:
    if substring is not None:
        needles = [substring]
    elif record.reference is not None:
        needles = _references(record.reference)
    else:
        raise SkipRecord("no substring param and the record has no reference")
    haystack = _output_text(record)
    if ignore_case:
        haystack = haystack.casefold()
        needles = [n.casefold() for n in needles]
    return Score(passed=any(n in haystack for n in needles))


@evaluator(
    name="builtin/regex-match",
    version="1.0.0",
    description="Output matches a regular expression.",
    outputs=[MetricSpec("regex-match", PASSED, higher_is_better=True)],
)
def regex_match(
    record: Record, *, pattern: str, full_match: bool = False, ignore_case: bool = False
) -> Score:
    flags = re.IGNORECASE if ignore_case else 0
    text = _output_text(record)
    match = re.fullmatch(pattern, text, flags) if full_match else re.search(pattern, text, flags)
    return Score(passed=match is not None)


@evaluator(
    name="builtin/fuzzy-match",
    version="1.0.0",
    description="Similarity ratio (0-1) between output and reference; best over references.",
    requires=REFERENCE,
    outputs=[MetricSpec("fuzzy-match", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
def fuzzy_match(record: Record, *, ignore_case: bool = True) -> Score:
    assert record.reference is not None
    output = _normalize(_output_text(record), ignore_case=ignore_case, ignore_punctuation=False)
    best = max(
        difflib.SequenceMatcher(
            None, output, _normalize(ref, ignore_case=ignore_case, ignore_punctuation=False)
        ).ratio()
        for ref in _references(record.reference)
    )
    return Score(number=best)


def _last_number(text: str) -> float | None:
    matches = _NUMBER.findall(text)
    if not matches:
        return None
    try:
        return float(matches[-1].replace(",", ""))
    except ValueError:
        return None


@evaluator(
    name="builtin/numeric-match",
    version="1.0.0",
    description=(
        "The last number in the output equals the last number in the reference, "
        "within tolerance. Handles answers like 'so the total is $1,250.'"
    ),
    requires=REFERENCE,
    outputs=[MetricSpec("numeric-match", PASSED, higher_is_better=True)],
)
def numeric_match(record: Record, *, rel_tol: float = 1e-6, abs_tol: float = 1e-9) -> Score:
    assert record.reference is not None
    expected = _last_number(record.reference.as_text())
    if expected is None:
        raise SkipRecord("reference contains no number")
    actual = _last_number(_output_text(record))
    if actual is None:
        return Score(passed=False, explanation="output contains no number")
    passed = math.isclose(actual, expected, rel_tol=rel_tol, abs_tol=abs_tol)
    return Score(passed=passed, metadata={"expected": expected, "actual": actual})


@evaluator(
    name="builtin/json-valid",
    version="1.0.0",
    description="Output parses as JSON.",
    outputs=[MetricSpec("json-valid", PASSED, higher_is_better=True)],
)
def json_valid(record: Record) -> Score:
    try:
        json.loads(_output_text(record))
    except json.JSONDecodeError as exc:
        return Score(passed=False, explanation=str(exc))
    return Score(passed=True)


@evaluator(
    name="builtin/json-schema",
    version="1.0.0",
    description="Output is JSON that validates against a JSON Schema (needs evalsi[jsonschema]).",
    outputs=[MetricSpec("json-schema", PASSED, higher_is_better=True)],
)
def json_schema(record: Record, *, schema: dict[str, Any]) -> Score:
    try:
        import jsonschema
    except ImportError as exc:
        raise RuntimeError("json-schema needs: pip install 'evalsi[jsonschema]'") from exc
    try:
        instance = json.loads(_output_text(record))
    except json.JSONDecodeError as exc:
        return Score(passed=False, explanation=f"not JSON: {exc}")
    validator_cls = jsonschema.validators.validator_for(schema)
    error = jsonschema.exceptions.best_match(validator_cls(schema).iter_errors(instance))
    if error is None:
        return Score(passed=True)
    path = "/".join(str(p) for p in error.absolute_path) or "(root)"
    return Score(passed=False, explanation=f"{path}: {error.message}")


@evaluator(
    name="builtin/length",
    version="1.0.0",
    description="Output length in words or characters.",
    outputs=[MetricSpec("length", NUMBER, min=0.0)],
)
def length(record: Record, *, unit: str = "words") -> Score:
    text = _output_text(record)
    if unit == "words":
        return Score(number=len(text.split()))
    if unit == "chars":
        return Score(number=len(text))
    raise ValueError(f"unit must be 'words' or 'chars', not {unit!r}")


def _usage_field(record: Record, name: str) -> float:
    value = getattr(record.usage, name, None) if record.usage is not None else None
    if value is None:
        raise SkipRecord(f"record has no usage.{name}")
    return float(value)


@evaluator(
    name="builtin/latency",
    version="1.0.0",
    description="Latency of producing the output, from usage.latency_ms.",
    requires=Requirements(output=False),
    outputs=[MetricSpec("latency", NUMBER, min=0.0, higher_is_better=False)],
)
def latency(record: Record) -> Score:
    return Score(number=_usage_field(record, "latency_ms"), metadata={"unit": "ms"})


@evaluator(
    name="builtin/token-usage",
    version="1.0.0",
    description="Input, output and total tokens spent producing the output.",
    requires=Requirements(output=False),
    outputs=[
        MetricSpec("input_tokens", NUMBER, min=0.0, higher_is_better=False),
        MetricSpec("output_tokens", NUMBER, min=0.0, higher_is_better=False),
        MetricSpec("total_tokens", NUMBER, min=0.0, higher_is_better=False),
    ],
)
def token_usage(record: Record) -> list[Score]:
    usage = record.usage
    if usage is None or (usage.input_tokens is None and usage.output_tokens is None):
        raise SkipRecord("record has no token usage")
    scores = []
    if usage.input_tokens is not None:
        scores.append(Score(number=usage.input_tokens, name="input_tokens"))
    if usage.output_tokens is not None:
        scores.append(Score(number=usage.output_tokens, name="output_tokens"))
    if usage.input_tokens is not None and usage.output_tokens is not None:
        scores.append(Score(number=usage.input_tokens + usage.output_tokens, name="total_tokens"))
    return scores


@evaluator(
    name="builtin/cost",
    version="1.0.0",
    description="Cost in USD of producing the output, from usage.cost_usd.",
    requires=Requirements(output=False),
    outputs=[MetricSpec("cost", NUMBER, min=0.0, higher_is_better=False)],
)
def cost(record: Record) -> Score:
    return Score(number=_usage_field(record, "cost_usd"), metadata={"unit": "usd"})


PACK = Pack(
    name="core",
    description="Deterministic checks: matching, JSON validity, length, latency, tokens and cost.",
    evaluators=[
        exact_match,
        contains,
        regex_match,
        fuzzy_match,
        numeric_match,
        json_valid,
        json_schema,
        length,
        latency,
        token_usage,
        cost,
    ],
    on_by_default=True,
)

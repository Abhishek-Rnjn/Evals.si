from __future__ import annotations

import asyncio
from typing import Any

import pytest

import evalsi
from evalsi import EvaluatorConfigError, Outcome, Record, Scope, Score, evaluator
from evalsi.evaluator import MetricSpec, Requirements, ScoreType

ROWS: list[dict[str, Any]] = [
    {"id": "a", "output": "Paris", "reference": "Paris", "metadata": {"src": "x"}},
    {"id": "b", "output": "Lyon", "reference": "Paris", "metadata": {"src": "x"}},
    {"id": "c", "output": "Rome", "reference": "Rome", "metadata": {"src": "y"}},
    {"id": "d", "output": "Oslo"},
]


def test_evaluate_summarizes_with_intervals() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    summary = result.metric("exact-match")
    assert summary.kind == "proportion"
    assert (summary.n, summary.skipped, summary.errors) == (3, 1, 0)
    assert summary.mean == pytest.approx(2 / 3)
    assert summary.ci is not None
    assert summary.ci.method == "wilson"
    assert 0.0 <= summary.ci.low < summary.ci.high <= 1.0
    skipped = [r for r in result.results if r.outcome is Outcome.SKIPPED]
    assert [(r.record_id, r.reason) for r in skipped] == [("d", "record has no reference")]


def test_results_are_in_record_then_evaluator_order() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match", "length"], concurrency=3)
    assert [(r.record_id, r.evaluator) for r in result.results] == [
        (rid, ev) for rid in "abcd" for ev in ("exact-match", "length")
    ]


def test_cluster_by_uses_clustered_interval() -> None:
    rows = [{"id": str(i), "output": "x", "metadata": {"src": i % 3}} for i in range(9)]
    result = evalsi.evaluate(rows, ["length"], cluster_by="src")
    summary = result.metric("length")
    assert summary.clusters == 3
    assert summary.ci is not None
    assert summary.ci.method == "clustered-t"


def test_errors_are_reported_not_scored() -> None:
    @evaluator(name="test/flaky", version="1.0.0")
    def flaky(record: Record) -> Score:
        if record.id == "b":
            raise RuntimeError("boom")
        return Score(number=1.0)

    result = evalsi.evaluate(ROWS, [flaky])
    summary = result.metric("flaky")
    assert (summary.n, summary.errors) == (3, 1)
    (error,) = result.errors()
    assert error.record_id == "b"
    assert error.reason == "RuntimeError: boom"


def test_custom_async_evaluator_with_params_and_alias() -> None:
    @evaluator(
        name="test/longer-than",
        version="1.0.0",
        outputs=[MetricSpec("longer-than", ScoreType.PASSED)],
    )
    async def longer_than(record: Record, *, n: int = 3) -> Score:
        await asyncio.sleep(0)
        assert record.output is not None
        return Score(passed=len(record.output.as_text()) > n)

    result = evalsi.evaluate(
        ROWS,
        [longer_than, {"ref": "length", "name": "chars", "params": {"unit": "chars"}}],
        params={"longer-than": {"n": 4}},
    )
    assert result.metric("longer-than").mean == pytest.approx(0.25)
    assert result.metric("chars").mean == pytest.approx(4.25)
    assert result.manifest["evaluators"][1] == {
        "name": "chars",
        "ref": "builtin/length@1.0.0",
        "params": {"unit": "chars"},
    }


def test_dataset_scope_evaluator_sees_all_eligible_records() -> None:
    @evaluator(
        name="test/count",
        version="1.0.0",
        scope=Scope.DATASET,
        requires=Requirements(reference=True),
    )
    def count(records: list[Record]) -> Score:
        return Score(number=len(records))

    result = evalsi.evaluate(ROWS, [count])
    (only,) = result.results
    assert only.record_id == ""
    assert only.scores[0].number == 3
    assert "1 records lacked" in only.reason
    summary = result.metric("count")
    assert (summary.mean, summary.ci) == (3.0, None)


@pytest.mark.parametrize(
    ("evaluators", "params", "message"),
    [
        (["nope"], None, "unknown evaluator"),
        (["exact-match", "exact-match"], None, "more than once"),
        (["exact-match"], {"exact-match": {"bogus": 1}}, "unknown params"),
        (["exact-match"], {"length": {"unit": "chars"}}, "not in the run"),
        (["builtin/exact-match@9.9.9"], None, "pinned"),
        ([{"params": {}}], None, "has no 'ref'"),
    ],
)
def test_configuration_errors(
    evaluators: list[object], params: dict[str, dict[str, object]] | None, message: str
) -> None:
    with pytest.raises(EvaluatorConfigError, match=message):
        evalsi.evaluate(ROWS, evaluators, params=params)  # type: ignore[arg-type]


def test_judge_evaluators_fail_fast_without_a_judge() -> None:
    with pytest.raises(EvaluatorConfigError, match="need a judge model"):
        evalsi.evaluate(ROWS, ["llm-judge"])


def test_evaluate_works_inside_a_running_event_loop() -> None:
    async def notebook_cell() -> float | None:
        return evalsi.evaluate(ROWS, ["exact-match"]).metric("exact-match").mean

    assert asyncio.run(notebook_cell()) == pytest.approx(2 / 3)


def test_manifest_and_table() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    assert result.manifest["dataset"]["records"] == 4
    assert len(result.manifest["dataset"]["sha256"]) == 64
    assert result.manifest["judge"] is None
    table = result.table()
    assert table.splitlines()[0].split() == [
        "metric",
        "n",
        "mean",
        "95%",
        "CI",
        "skipped",
        "errors",
    ]
    assert "exact-match" in table

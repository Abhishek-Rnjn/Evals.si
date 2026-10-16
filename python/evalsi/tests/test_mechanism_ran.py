"""A mechanism can gate a run only when its trajectory shows it acted."""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

import evalsi
from evalsi import matrix
from evalsi.evaluator import EvalContext, SkipRecord
from evalsi.packs.agent import mechanism_ran
from evalsi.runspec import check_gates
from evalsi.types import Record, Step, Trajectory
from evalsi.v1alpha1 import run_pb2


def score(record: Record, mechanism: str) -> bool | None:
    scores = asyncio.run(mechanism_ran.bind({"mechanism": mechanism}).run(record, EvalContext()))
    return scores[0].passed


def test_offering_a_tool_does_not_pass_but_a_nested_invocation_does() -> None:
    record = Record(
        id="task",
        trajectory=Trajectory(),
        metadata={"agent": {"tools_offered": ["delegate", "grep"]}},
    )
    assert score(record, "tool:grep") is False
    record.trajectory = Trajectory(
        steps=[
            Step(type="tool", name="delegate", span_id="parent"),
            Step(type="tool", name="grep", parent_span_id="parent", error="no matches"),
        ]
    )
    # Invocation is the check, including failed tools; success is a different metric.
    assert score(record, "tool:grep") is True
    assert score(record, "tool:unused") is False


@pytest.mark.parametrize(
    ("strategy", "counters"),
    [
        ("clear-tool-results", {"clears": 1}),
        ("compact", {"clears": 1}),
        ("compact", {"compactions": 1}),
        ("fresh-rounds", {"rounds": 1}),
        ("evidence-state", {"entries": 1}),
    ],
)
def test_context_scores_agree_with_matrix(strategy: str, counters: dict[str, int]) -> None:
    field = "evidence" if strategy == "evidence-state" else "context"
    agent: dict[str, Any] = {"context_strategy": strategy, field: {}}
    record = Record(id="task", trajectory=Trajectory(), metadata={"agent": agent})
    for stats, expected in [({}, False), (counters, True)]:
        agent[field] = stats
        assert score(record, f"context:{strategy}") is expected
        (reported,) = matrix.mechanisms({"records": [record.to_dict()]})
        assert reported.ran == int(expected)


def test_activity_from_a_different_strategy_does_not_count() -> None:
    record = Record(
        id="task",
        trajectory=Trajectory(),
        metadata={
            "agent": {
                "context_strategy": "compact",
                "context": {"rounds": 5},
                "evidence": {"entries": 9},
            }
        },
    )
    assert score(record, "context:compact") is False
    assert score(record, "context:fresh-rounds") is False
    (reported,) = matrix.mechanisms({"records": [record.to_dict()]})
    assert reported.ran == 0


def test_missing_evidence_is_skipped() -> None:
    record = Record(id="task", trajectory=Trajectory())
    with pytest.raises(SkipRecord, match="context strategy metadata"):
        score(record, "context:compact")
    record.metadata = {"agent": {"context_strategy": "compact"}}
    with pytest.raises(SkipRecord, match="activity counters"):
        score(record, "context:compact")
    record.trajectory = None
    result = evalsi.evaluate(
        [record], ["mechanism-ran"], params={"mechanism-ran": {"mechanism": "tool:grep"}}
    )
    assert result.summaries[0].skipped == 1
    assert result.results[0].reason == "record has no trajectory"


@pytest.mark.parametrize(
    "mechanism", ["", "grep", "tool:", "context:none", "context:typo", "other:grep"]
)
def test_invalid_configuration_is_rejected(mechanism: str) -> None:
    with pytest.raises(ValueError, match=r"mechanism must|unknown context strategy"):
        score(Record(id="task", trajectory=Trajectory()), mechanism)


def test_evaluator_is_registered_and_aggregated_as_a_pass_rate() -> None:
    result = evalsi.evaluate(
        [
            Record(id="ran", trajectory=Trajectory(steps=[Step(type="tool", name="grep")])),
            Record(id="idle", trajectory=Trajectory()),
        ],
        ["mechanism-ran"],
        params={"mechanism-ran": {"mechanism": "tool:grep"}},
    )
    assert result.metric("mechanism-ran").mean == 0.5
    (gate,) = check_gates([run_pb2.Gate(metric="mechanism-ran", min=1.0)], result.summaries)
    assert not gate.passed
    (gate,) = check_gates([run_pb2.Gate(metric="mechanism-ran", min=0.5)], result.summaries)
    assert gate.passed

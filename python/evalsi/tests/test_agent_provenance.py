"""Deterministic provenance checks, including corrupt and stale trajectories."""

from __future__ import annotations

import copy

import pytest

import evalsi
from evalsi.convert import record_from_proto, record_to_proto
from evalsi.datasets import load_records
from evalsi.evaluator import SkipRecord
from evalsi.packs.agent_checks import citation_grounding, handoff_correctness
from evalsi.types import Content, Record, Step, Trajectory

QUOTE = "The treaty was signed in 1847 by three states."


def grounded() -> Record:
    return Record(
        id="r",
        output=Content(text="It was signed in 1847 [E1]."),
        trajectory=Trajectory(
            steps=[Step(type="tool", name="read_file", output=Content(text=QUOTE))]
        ),
        metadata={
            "agent": {
                "evidence_state": {
                    "version": 1,
                    "evidence": [
                        {"id": "E1", "quote": QUOTE, "source": "treaty.txt", "status": "active"}
                    ],
                    "claims": [
                        {
                            "id": "C1",
                            "claim": "Signed in 1847",
                            "status": "supported",
                            "support": ["E1"],
                        }
                    ],
                }
            }
        },
    )


def test_grounding_registered_and_survives_serialization() -> None:
    (record,) = load_records([grounded().to_dict()])
    result = evalsi.evaluate([record], ["citation-grounding"])
    assert result.metric("citation-grounding").mean == 1
    assert citation_grounding(record).passed
    assert citation_grounding(record_from_proto(record_to_proto(record))).passed


@pytest.mark.parametrize("status", ["superseded", "retracted", "unknown"])
def test_stale_citations_fail(status: str) -> None:
    record = grounded()
    record.metadata["agent"]["evidence_state"]["evidence"][0]["status"] = status
    assert not citation_grounding(record).passed


def test_quote_requires_independent_read_not_agent_or_evidence_echo() -> None:
    record = grounded()
    assert record.trajectory is not None
    for kind, name in [("llm", "model"), ("tool", "evidence"), ("tool", "session_search")]:
        record.trajectory.steps[0].type = kind
        record.trajectory.steps[0].name = name
        assert not citation_grounding(record).passed
    record.input = Content(text=QUOTE)
    assert citation_grounding(record).passed


@pytest.mark.parametrize(
    "change",
    ["no-citation", "unknown-id", "duplicate", "no-quote", "no-support", "uncited-support"],
)
def test_invalid_grounding_fails(change: str) -> None:
    record = grounded()
    state = record.metadata["agent"]["evidence_state"]
    if change == "no-citation":
        record.output = Content(text="1847")
    elif change == "unknown-id":
        record.output = Content(text="1847 [E2]")
    elif change == "duplicate":
        state["evidence"].append(copy.deepcopy(state["evidence"][0]))
    elif change == "no-quote":
        state["evidence"][0]["quote"] = ""
    elif change == "no-support":
        state["claims"][0]["support"] = []
    else:
        state["evidence"].append({**state["evidence"][0], "id": "E2"})
        state["claims"][0]["support"] = ["E2"]
    assert not citation_grounding(record).passed


def test_missing_snapshot_is_skipped_and_whitespace_is_normalized() -> None:
    record = grounded()
    record.metadata["agent"]["evidence_state"]["evidence"][0]["quote"] = QUOTE.replace(" ", "\n  ")
    assert citation_grounding(record).passed
    record.metadata = {}
    with pytest.raises(SkipRecord, match="evidence_state"):
        citation_grounding(record)


def transfer() -> Record:
    return Record(
        id="handoff",
        trajectory=Trajectory(
            steps=[
                Step(
                    type="tool", name="delegate", span_id="source", parent_span_id="external-root"
                ),
                Step(
                    type="handoff",
                    name="handoff",
                    span_id="transfer",
                    parent_span_id="source",
                    attributes={"target": "helper"},
                ),
                Step(type="agent", name="helper", span_id="child", parent_span_id="transfer"),
            ]
        ),
    )


def test_valid_handoff_is_registered_and_retains_attributes() -> None:
    (record,) = load_records([transfer().to_dict()])
    assert record.trajectory is not None
    assert record.trajectory.steps[1].attributes == {"target": "helper"}
    assert handoff_correctness(record).passed
    assert handoff_correctness(record_from_proto(record_to_proto(record))).passed
    assert (
        evalsi.evaluate([record], ["handoff-correctness"]).metric("handoff-correctness").mean == 1
    )


@pytest.mark.parametrize(
    "change", ["source", "child", "child-id", "id", "duplicate", "cycle", "failed"]
)
def test_bad_handoff_graph_fails(change: str) -> None:
    record = transfer()
    assert record.trajectory is not None
    source, handoff, child = record.trajectory.steps
    if change == "source":
        handoff.parent_span_id = "missing"
    elif change == "child":
        child.parent_span_id = "missing"
    elif change == "child-id":
        child.span_id = ""
    elif change == "id":
        handoff.span_id = ""
    elif change == "duplicate":
        child.span_id = source.span_id
    elif change == "cycle":
        source.parent_span_id = child.span_id
    else:
        handoff.error = "refused"
    assert not handoff_correctness(record).passed


def test_handoffs_are_not_inferred_from_unrelated_parent_links() -> None:
    with pytest.raises(SkipRecord, match="explicit handoff"):
        handoff_correctness(Record(id="r", trajectory=Trajectory(steps=[Step(type="tool")])))

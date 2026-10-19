"""The deterministic evidence-stage audit: a funnel whose stages are conditional."""

from __future__ import annotations

from typing import Any

import pytest

import evalsi
from evalsi.evaluator import SkipRecord
from evalsi.packs.agent_stages import evidence_stage_audit
from evalsi.testing import score
from evalsi.types import Content, Record, Step, Trajectory

PASSAGE = "The treaty was signed in 1847 by three states."
OTHER = "Ratification took until 1851 in the last state."
ANSWER = "1847"


def record(
    *,
    reads: tuple[str, ...] = (PASSAGE,),
    kept: tuple[str, ...] = (PASSAGE,),
    cited: str = "[E1]",
    answer: str = f"It was signed in {ANSWER}",
    status: str = "active",
    reference: Any = None,
    rid: str = "r",
) -> Record:
    entries = [
        {"id": f"E{i + 1}", "quote": quote, "source": "doc", "status": status}
        for i, quote in enumerate(kept)
    ]
    return Record(
        id=rid,
        output=Content(text=f"{answer} {cited}".strip()),
        reference=Content(
            json=reference if reference is not None else {"evidence": [PASSAGE], "answer": ANSWER},
            is_json=True,
        ),
        trajectory=Trajectory(
            steps=[Step(type="tool", name="read_file", output=Content(text=t)) for t in reads]
        ),
        metadata={"agent": {"evidence_state": {"version": 1, "evidence": entries, "claims": []}}},
    )


def scores(rec: Record) -> dict[str, bool | None]:
    return {name: s.passed for name, s in score(evidence_stage_audit, rec).items()}


def test_a_run_that_gets_everything_right_passes_all_three_stages() -> None:
    assert scores(record()) == {
        "evidence-retrieved": True,
        "evidence-kept-and-used": True,
        "answer-from-evidence": True,
    }


def test_each_stage_stops_the_funnel_and_later_stages_are_not_scored() -> None:
    # Never read the passage: only stage 1 is scored, and it failed.
    assert scores(record(reads=(), kept=())) == {"evidence-retrieved": False}
    # Read it but did not keep or cite it: stage 2 fails, stage 3 is not scored.
    assert scores(record(kept=(), cited="")) == {
        "evidence-retrieved": True,
        "evidence-kept-and-used": False,
    }
    assert scores(record(cited="")) == {  # kept, but the answer cites nothing
        "evidence-retrieved": True,
        "evidence-kept-and-used": False,
    }
    assert scores(record(status="retracted"))["evidence-kept-and-used"] is False
    # Kept and cited the evidence but still got the answer wrong.
    assert scores(record(answer="It was signed in 1900")) == {
        "evidence-retrieved": True,
        "evidence-kept-and-used": True,
        "answer-from-evidence": False,
    }


def test_the_task_text_and_the_agents_own_notes_are_not_retrieval() -> None:
    rec = record(reads=())
    rec.input = Content(text=PASSAGE)  # the question carried the passage
    assert scores(rec) == {"evidence-retrieved": False}
    assert rec.trajectory is not None
    rec.trajectory.steps = [Step(type="tool", name="evidence", output=Content(text=PASSAGE))]
    assert scores(rec) == {"evidence-retrieved": False}


def test_every_required_passage_is_needed() -> None:
    two = {"evidence": [PASSAGE, OTHER], "answer": ANSWER}
    assert scores(record(reference=two))["evidence-retrieved"] is False  # OTHER never read
    both = record(reads=(PASSAGE, OTHER), kept=(PASSAGE,), reference=two)
    assert scores(both)["evidence-kept-and-used"] is False  # OTHER was not kept
    full = record(reads=(PASSAGE, OTHER), kept=(PASSAGE, OTHER), cited="[E1] [E2]", reference=two)
    assert scores(full)["answer-from-evidence"] is True


def test_a_quote_can_be_an_excerpt_of_the_passage_but_not_a_trivial_one() -> None:
    long = f"Background text. {PASSAGE} More text after it."
    ref = {"evidence": [long], "answer": ANSWER}
    assert scores(record(reads=(long,), kept=(PASSAGE,), reference=ref))["evidence-kept-and-used"]
    short = record(reads=(long,), kept=("signed",), reference=ref)  # too short to establish a match
    assert scores(short)["evidence-kept-and-used"] is False


@pytest.mark.parametrize(
    "reference",
    [
        None,
        "a plain string",
        {"evidence": [PASSAGE]},
        {"answer": ANSWER},
        {"evidence": [], "answer": ANSWER},
        {"evidence": [""], "answer": ANSWER},
        {"evidence": [PASSAGE], "answer": ""},
        {"evidence": [PASSAGE], "answer": ANSWER, "extra": 1},
        {"evidence": PASSAGE, "answer": ANSWER},
    ],
)
def test_any_other_reference_shape_is_refused(reference: Any) -> None:
    rec = record()
    rec.reference = (
        Content(text=reference)
        if isinstance(reference, str)
        else None
        if reference is None
        else Content(json=reference, is_json=True)
    )
    if reference is None:
        assert evidence_stage_audit.spec.requires.missing(rec)  # skipped: no reference at all
        return
    with pytest.raises(ValueError, match="reference"):
        score(evidence_stage_audit, rec)


def test_it_skips_records_without_evidence_state() -> None:
    rec = record()
    rec.metadata = {}
    with pytest.raises(SkipRecord, match="evidence_state"):
        score(evidence_stage_audit, rec)


def funnel() -> list[Record]:
    """Five tasks that fall at different stages."""
    return [
        record(rid="no-read", reads=(), kept=()),  # stops at stage 1
        record(rid="no-cite", cited=""),  # reaches stage 2, fails it
        record(rid="wrong", answer="It was 1900"),  # reaches stage 3, fails it
        record(rid="ok-1"),
        record(rid="ok-2"),
    ]


def test_summaries_are_conditional_rates_with_the_stage_populations() -> None:
    result = evalsi.evaluate(funnel(), ["evidence-stage-audit"])
    by = {s.metric.split(".")[-1]: s for s in result.summaries}
    # n is how many tasks reached the stage; the mean is the pass rate among them. A
    # task that stopped earlier is neither a failure nor a zero at a later stage.
    assert (by["evidence-retrieved"].n, by["evidence-retrieved"].mean) == (5, 4 / 5)
    assert (by["evidence-kept-and-used"].n, by["evidence-kept-and-used"].mean) == (4, 3 / 4)
    assert (by["answer-from-evidence"].n, by["answer-from-evidence"].mean) == (3, 2 / 3)
    assert all(s.errors == 0 for s in result.summaries)


def test_a_stage_nobody_reached_still_appears_with_no_data() -> None:
    result = evalsi.evaluate([record(rid="a", reads=(), kept=())], ["evidence-stage-audit"])
    by = {s.metric.split(".")[-1]: s for s in result.summaries}
    assert by["evidence-retrieved"].mean == 0.0
    assert by["evidence-kept-and-used"].n == 0
    assert by["answer-from-evidence"].n == 0


def test_an_ineligible_record_is_skipped_before_its_reference_is_checked() -> None:
    rec = record(reference="not the evidence shape")
    rec.metadata = {}  # no evidence_state: the audit does not apply
    with pytest.raises(SkipRecord):
        score(evidence_stage_audit, rec)

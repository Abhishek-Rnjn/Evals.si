"""A deterministic audit of where an evidence-reading agent loses the answer (H5).

Three stages, each reported only when the stage before it passed, so the mean of each
metric is the rate *given* the one before it, and ``n`` is how many tasks got that far:

1. ``evidence-retrieved``: every passage the task requires appears in something the
   agent read (a tool or retrieval result; the task text does not count, and neither
   do the agent's own evidence notes or session search);
2. ``evidence-kept-and-used``: each required passage is held by an *active* evidence
   entry (the ``evidence-state`` context strategy) that the final answer cites as
   ``[E<number>]``;
3. ``answer-from-evidence``: the final answer contains the expected answer.

The reference must be a JSON object with exactly these keys::

    {"evidence": ["a passage the agent has to find", ...], "answer": "the expected answer"}

This is a string-containment audit, not the judge-based stage audit the LongHarness
paper describes. It cannot tell a paraphrase of the evidence from no evidence, or a
right answer reached for the wrong reason; it can tell *which stage* a run fell at
when the passages and the answer can be written down. It needs
``metadata.agent.evidence_state`` and skips records without it.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from typing import Any

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.types import Record, Score

MAX_EVIDENCE = 50
# A quote this short would match almost anything, so it cannot establish a match.
MIN_QUOTE = 16


def _squash(text: str) -> str:
    return " ".join(text.split()).casefold()


def reference_of(record: Record) -> tuple[list[str], str]:
    """The required passages and the expected answer, or ValueError for any other shape."""
    ref = record.reference
    data = ref.json if ref is not None and ref.is_json else None
    shape = 'a JSON object {"evidence": ["passage", ...], "answer": "expected answer"}'
    if not isinstance(data, Mapping) or set(data) != {"evidence", "answer"}:
        raise ValueError(f"the reference must be {shape}")
    evidence, answer = data["evidence"], data["answer"]
    if (
        not isinstance(evidence, list)
        or not 1 <= len(evidence) <= MAX_EVIDENCE
        or not all(isinstance(p, str) and _squash(p) for p in evidence)
    ):
        raise ValueError(
            f"reference.evidence must be 1 to {MAX_EVIDENCE} non-empty strings, in {shape}"
        )
    if not isinstance(answer, str) or not _squash(answer):
        raise ValueError(f"reference.answer must be a non-empty string, in {shape}")
    return list(evidence), answer


def _reads(record: Record) -> list[str]:
    assert record.trajectory is not None
    return [
        _squash(step.output.as_text())
        for step in record.trajectory.steps
        if step.type in ("tool", "retrieval")
        and step.name not in ("evidence", "session_search")
        and step.output is not None
    ]


def _entries(record: Record) -> list[Mapping[str, Any]]:
    agent = record.metadata.get("agent")
    state = agent.get("evidence_state") if isinstance(agent, Mapping) else None
    if not isinstance(state, Mapping) or state.get("version") != 1:
        raise SkipRecord("the stage audit needs metadata.agent.evidence_state version 1")
    entries = state.get("evidence")
    if not isinstance(entries, list):
        raise SkipRecord("evidence_state needs an evidence list")
    return [e for e in entries if isinstance(e, Mapping)]


def _holds(entry: Mapping[str, Any], passage: str) -> bool:
    """Whether an entry's quote and a required passage are the same text, either one an
    excerpt of the other."""
    quote = entry.get("quote")
    if not isinstance(quote, str):
        return False
    q = _squash(quote)
    return len(q) >= MIN_QUOTE and (passage in q or q in passage)


@evaluator(
    name="builtin/evidence-stage-audit",
    version="1.0.0",
    description=(
        "Deterministic funnel for an agent that reads evidence: were the required passages "
        "retrieved, then kept in active evidence and cited, then did the answer contain the "
        "expected one. Each stage is scored only when the one before passed, so its mean is "
        "conditional. Needs metadata.agent.evidence_state and a reference "
        '{"evidence": [...], "answer": "..."}. String containment, not the judge-based audit.'
    ),
    requires=Requirements(output=True, reference=True, trajectory=True),
    outputs=[
        MetricSpec("evidence-retrieved", ScoreType.PASSED, higher_is_better=True),
        MetricSpec("evidence-kept-and-used", ScoreType.PASSED, higher_is_better=True),
        MetricSpec("answer-from-evidence", ScoreType.PASSED, higher_is_better=True),
    ],
)
def evidence_stage_audit(record: Record) -> list[Score]:
    assert record.output is not None
    required, answer = reference_of(record)
    entries = _entries(record)  # skips here, before any stage is scored
    passages = [_squash(p) for p in required]

    reads = _reads(record)
    missing = [
        p for p, norm in zip(required, passages, strict=True) if not any(norm in r for r in reads)
    ]
    scores = [
        Score(
            name="evidence-retrieved",
            passed=not missing,
            explanation=f"{len(missing)} of {len(required)} required passages never read"
            if missing
            else "every required passage was read",
            metadata={"missing": missing},
        )
    ]
    if missing:
        return scores

    cited = set(re.findall(r"\[(E\d+)\]", record.output.as_text()))
    unused = [
        p
        for p, norm in zip(required, passages, strict=True)
        if not any(
            e.get("status") == "active" and e.get("id") in cited and _holds(e, norm)
            for e in entries
        )
    ]
    scores.append(
        Score(
            name="evidence-kept-and-used",
            passed=not unused,
            explanation=f"{len(unused)} of {len(required)} required passages not held by active "
            "evidence that the answer cites"
            if unused
            else "every required passage is held by active, cited evidence",
            metadata={"not_kept_and_cited": unused, "cited": sorted(cited)},
        )
    )
    if unused:
        return scores

    right = _squash(answer) in _squash(record.output.as_text())
    scores.append(
        Score(
            name="answer-from-evidence",
            passed=right,
            explanation="" if right else "the final answer does not contain the expected answer",
        )
    )
    return scores

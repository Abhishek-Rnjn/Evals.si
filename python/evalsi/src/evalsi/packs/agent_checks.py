"""Deterministic provenance and handoff checks for recorded agent runs."""

from __future__ import annotations

import re
from collections import Counter
from collections.abc import Mapping
from typing import Any

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.types import Record, Score


def _squash(text: str) -> str:
    return " ".join(text.split())


@evaluator(
    name="builtin/citation-grounding",
    version="1.0.0",
    description=(
        "Checks explicit [E1] citations and supported claims against active evidence "
        "whose verbatim quotes occur in the task or non-evidence tool outputs. "
        "Requires metadata.agent.evidence_state v1; does not judge semantic support."
    ),
    requires=Requirements(output=True, trajectory=True),
    outputs=[MetricSpec("citation-grounding", ScoreType.PASSED, higher_is_better=True)],
)
def citation_grounding(record: Record, *, require_citation: bool = True) -> Score:
    assert record.output is not None
    assert record.trajectory is not None
    agent = record.metadata.get("agent")
    state = agent.get("evidence_state") if isinstance(agent, Mapping) else None
    if not isinstance(state, Mapping) or state.get("version") != 1:
        raise SkipRecord("citation grounding needs metadata.agent.evidence_state version 1")
    entries, claims = state.get("evidence"), state.get("claims")
    if not isinstance(entries, list) or not isinstance(claims, list):
        raise SkipRecord("evidence_state needs evidence and claims lists")
    violations: list[str] = []
    evidence: dict[str, Mapping[str, Any]] = {}
    for entry in entries:
        if not isinstance(entry, Mapping) or not isinstance(entry.get("id"), str):
            violations.append("malformed evidence entry")
        elif entry["id"] in evidence:
            violations.append(f"duplicate evidence id {entry['id']}")
        else:
            evidence[entry["id"]] = entry
    # An evidence tool echoing a quote, or a session search over that echo,
    # cannot establish that an independent passage was read.
    read = [_squash(record.input.as_text())] if record.input is not None else []
    read.extend(
        _squash(step.output.as_text())
        for step in record.trajectory.steps
        if step.type in ("tool", "retrieval")
        and step.name not in ("evidence", "session_search")
        and step.output is not None
    )
    cited = set(re.findall(r"\[(E\d+)\]", record.output.as_text()))
    if require_citation and not cited:
        violations.append("the final answer has no [E<number>] citations")
    used = set(cited)
    for claim in claims:
        if not isinstance(claim, Mapping):
            violations.append("malformed claim")
            continue
        if claim.get("status") != "supported":
            continue
        support = claim.get("support")
        if (
            not isinstance(support, list)
            or not support
            or not all(isinstance(s, str) for s in support)
        ):
            violations.append(f"claim {claim.get('id')} has no valid support")
            continue
        used.update(support)
        if not cited.intersection(support):
            violations.append(f"claim {claim.get('id')} has no support cited in the final answer")
    for eid in sorted(used):
        entry = evidence.get(eid)
        if entry is None or entry.get("status") != "active":
            violations.append(f"{eid} is missing, superseded or retracted")
            continue
        quote, source = entry.get("quote"), entry.get("source")
        if not isinstance(source, str) or not source.strip():
            violations.append(f"{eid} has no source label")
        if not isinstance(quote, str) or not _squash(quote):
            violations.append(f"{eid} has no quote")
        elif not any(_squash(quote) in passage for passage in read):
            violations.append(f"{eid} quotes a passage absent from the recorded reads")
    return Score(
        passed=not violations,
        explanation="; ".join(violations) or "all explicit citations have active, read evidence",
        metadata={"citations": sorted(cited), "violations": violations},
    )


@evaluator(
    name="builtin/handoff-correctness",
    version="1.0.0",
    description=(
        "Checks recorded handoff structure: unique span IDs, a source parent and "
        "a target child for each handoff, no failed transfers or parent cycles. "
        "Does not infer transfers or judge which agent should receive the task."
    ),
    requires=Requirements(output=False, trajectory=True),
    outputs=[MetricSpec("handoff-correctness", ScoreType.PASSED, higher_is_better=True)],
)
def handoff_correctness(record: Record) -> Score:
    assert record.trajectory is not None
    steps = record.trajectory.steps
    handoffs = [step for step in steps if step.type == "handoff"]
    if not handoffs:
        raise SkipRecord("the trajectory has no explicit handoff steps")
    ids = Counter(step.span_id for step in steps if step.span_id)
    violations = [f"duplicate span id {sid}" for sid, count in ids.items() if count > 1]
    by_id = {step.span_id: step for step in steps if step.span_id}
    parents = {step.parent_span_id for step in steps if step.parent_span_id and step.span_id}
    for step in handoffs:
        label = step.span_id or step.name
        if not step.span_id:
            violations.append(f"handoff {label} has no span id")
        if not step.parent_span_id or step.parent_span_id not in by_id:
            violations.append(f"handoff {label} has no recorded source parent")
        if not step.span_id or step.span_id not in parents:
            violations.append(f"handoff {label} has no recorded target child")
        if step.error:
            violations.append(f"handoff {label} failed")
    # Roots outside the exported trajectory are permitted. Local parent cycles
    # are always invalid, including cycles that do not directly touch a handoff.
    done: set[str] = set()
    for sid in by_id:
        path: set[str] = set()
        current = sid
        while current in by_id and current not in done:
            if current in path:
                violations.append(f"parent cycle at {current}")
                break
            path.add(current)
            current = by_id[current].parent_span_id
        done.update(path)
    return Score(
        passed=not violations,
        explanation="; ".join(violations) or "all recorded handoffs link source and target",
        metadata={"handoffs": len(handoffs), "violations": violations},
    )

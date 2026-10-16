"""Evidence of optional harness activity, shared by reporting and scoring."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

CONTEXT_COUNTERS = {
    "clear-tool-results": ("clears",),
    "compact": ("clears", "compactions"),
    "fresh-rounds": ("rounds",),
    "evidence-state": (),
}


def called_tools(record: Mapping[str, Any]) -> set[str]:
    trajectory = record.get("trajectory")
    if not isinstance(trajectory, Mapping):
        return set()
    return {
        str(step.get("name", ""))
        for step in trajectory.get("steps") or []
        if isinstance(step, Mapping)
        and str(step.get("type", "")).lower().removeprefix("step_type_") == "tool"
    }


def context_acted(agent: Mapping[str, Any], strategy: str) -> bool:
    """A configured strategy must have recorded activity, not just be enabled."""
    recorded = agent.get("context_strategy")
    if recorded and recorded != strategy:
        return False
    stats = agent.get("evidence" if strategy == "evidence-state" else "context")
    if not isinstance(stats, Mapping):
        return False
    values = (
        stats.values()
        if strategy == "evidence-state"
        else (stats.get(key) for key in CONTEXT_COUNTERS.get(strategy, ()))
    )
    return any(
        isinstance(value, int | float) and not isinstance(value, bool) and value > 0
        for value in values
    )

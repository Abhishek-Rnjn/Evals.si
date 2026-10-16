"""Check live CLI artifacts independently of the agent's answer."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

from evalsi.results import MetricSummary
from evalsi.runspec import check_gates
from evalsi.v1alpha1 import run_pb2


def verify(data: dict[str, Any]) -> None:
    records = data.get("records") or []
    assert len(records) == 1, "expected one completed CLI task"
    record = records[0]
    assert record["check"]["passed"], "the hidden checker did not pass"
    assert record["metadata"]["isolation"]["driver"] == "pod", "task did not use the pod rung"
    agent = record["metadata"]["agent"]
    assert agent["exit_code"] == 0
    assert agent["cli_outcome"] == "exit"
    assert agent["stop_reason"] == "completed"
    steps = record["trajectory"]["steps"]
    kinds = [str(step["type"]).lower().removeprefix("step_type_") for step in steps]
    assert "llm" in kinds, "CLI model calls were not parsed"
    assert "tool" in kinds, "the CLI never called a tool"
    assert "agent" in kinds, "missing CLI process step"
    assert record.get("usage", {}).get("input_tokens", 0) > 0, "CLI usage was not captured"
    summaries = data["summaries"]
    assert next(s for s in summaries if s["metric"] == "task-success")["mean"] == 1
    assert data["gates"], "positive gate was not evaluated"
    assert all(g["passed"] for g in data["gates"]), "positive gate did not pass"
    # Exercise a negative gate on the same observed score without paying for
    # another model call. This is a threshold control, not a regressed agent.
    parsed = [
        MetricSummary(
            metric=s["metric"],
            evaluator=s["evaluator"],
            kind=s["kind"],
            n=s["n"],
            mean=s.get("mean"),
        )
        for s in summaries
    ]
    (negative,) = check_gates([run_pb2.Gate(metric="task-success", min=1.01)], parsed)
    assert not negative.passed, "negative gate control unexpectedly passed"


if __name__ == "__main__":
    verify(json.loads(Path(sys.argv[1]).read_text()))
    print("live CLI: pod isolation, checker, trajectory, usage and gate controls passed")

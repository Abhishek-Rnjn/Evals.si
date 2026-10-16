"""Validate the opt-in live check's assertions and run specification offline."""

from __future__ import annotations

import copy
import runpy
from pathlib import Path
from typing import Any

import pytest
import yaml

from evalsi.runspec import load_spec

ROOT = Path(__file__).resolve().parents[3]
VERIFY = runpy.run_path(str(ROOT / "tests/cli-agent/verify.py"))["verify"]


def artifact() -> dict[str, Any]:
    return {
        "records": [
            {
                "check": {"passed": True},
                "metadata": {
                    "isolation": {"driver": "pod"},
                    "agent": {
                        "exit_code": 0,
                        "cli_outcome": "exit",
                        "stop_reason": "completed",
                    },
                },
                "trajectory": {"steps": [{"type": "llm"}, {"type": "tool"}, {"type": "agent"}]},
                "usage": {"input_tokens": 30},
            }
        ],
        "summaries": [
            {
                "metric": "task-success",
                "evaluator": "task-success",
                "kind": "proportion",
                "n": 1,
                "mean": 1,
            }
        ],
        "gates": [{"metric": "task-success", "passed": True}],
    }


def test_live_verifier_checks_positive_and_negative_gate_controls() -> None:
    VERIFY(artifact())


@pytest.mark.parametrize("change", ["checker", "isolation", "exit", "trajectory", "usage", "gate"])
def test_live_verifier_rejects_incomplete_or_failed_runs(change: str) -> None:
    data = copy.deepcopy(artifact())
    rec = data["records"][0]
    if change == "checker":
        rec["check"]["passed"] = False
    elif change == "isolation":
        rec["metadata"]["isolation"]["driver"] = "landlock"
    elif change == "exit":
        rec["metadata"]["agent"]["exit_code"] = 1
    elif change == "trajectory":
        rec["trajectory"]["steps"] = [{"type": "agent"}]
    elif change == "usage":
        rec["usage"] = {}
    else:
        data["gates"][0]["passed"] = False
    with pytest.raises(AssertionError):
        VERIFY(data)


def test_live_spec_is_valid_and_secret_free() -> None:
    run = load_spec(ROOT / "tests/cli-agent/run.yaml")
    assert run.spec.target.agent.cli.output_format == "claude-stream-json"
    assert run.spec.target.agent.cli.env_from["ANTHROPIC_API_KEY"] == "ANTHROPIC_API_KEY"
    assert run.spec.environment.sandbox.min_isolation == "namespaced"
    workflow = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())
    trigger = workflow.get("on", workflow.get(True))
    assert trigger["workflow_dispatch"]["inputs"]["cli_agent_e2e"]["default"] is False
    assert workflow["jobs"]["kubernetes"]["if"] == "github.event_name != 'pull_request'"

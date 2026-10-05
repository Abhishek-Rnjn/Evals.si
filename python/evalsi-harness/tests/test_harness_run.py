"""Agent runs through ``evalsi run``: one spec, tasks per record, trials,
the agent pack's evaluators, pass^k and gates."""

from __future__ import annotations

import asyncio
import textwrap
from pathlib import Path
from typing import Any

import pytest

import evalsi.sandbox.client
from evalsi.cli import main
from evalsi.run import execute
from evalsi.runspec import load_spec
from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer


@pytest.fixture
def local_sandbox(monkeypatch: pytest.MonkeyPatch) -> LocalSandboxClient:
    client = LocalSandboxClient()

    async def connect(address: str | None = None) -> Any:
        return client

    monkeypatch.setattr(evalsi.sandbox.client, "connect", connect)
    return client


def agent_script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
    """Writes the number from the task into answer.txt, except for task 3 on even trials."""
    task = messages[1]["content"] if messages[0]["role"] == "system" else messages[0]["content"]
    if not any(m["role"] == "tool" for m in messages):
        number = task.split()[1]
        return {"tool_calls": [("bash", {"command": f"echo {number} > answer.txt"})]}
    return {"text": "done"}


SPEC = """
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {{name: agent-smoke, project: demo}}
spec:
  target: {{connector: openai-compatible, model: scripted, base_url: "{base_url}"}}
  harness:
    builtin: {{max_steps: 5, budget: {{wall_clock: 2m}}}}
  environment:
    sandbox: {{network: deny}}
    command_timeout: 30s
    checker:
      command: ["sh", "-c", "test \\"$(cat answer.txt)\\" = \\"$EXPECTED\\""]
      timeout: 1m
  dataset:
    inline:
      records:
        - id: a
          input: {{text: "Write 1 into answer.txt"}}
          metadata: {{environment: {{env: {{EXPECTED: "1"}}}}}}
        - id: b
          input: {{text: "Write 2 into answer.txt"}}
          metadata: {{environment: {{env: {{EXPECTED: "2"}}}}}}
        - id: c
          input: {{text: "Write 3 into answer.txt"}}
          metadata: {{environment: {{env: {{EXPECTED: "4"}}}}}}
  evaluators:
    - {{ref: task-success}}
    - {{ref: policy-violations}}
    - {{ref: agent-efficiency}}
  trials: 2
  gates:
    - {{metric: task-success, min: 0.6}}
"""


def test_an_agent_run_end_to_end(local_sandbox: LocalSandboxClient, tmp_path: Path) -> None:
    with ScriptedModelServer(agent_script) as server:
        path = tmp_path / "run.yaml"
        path.write_text(textwrap.dedent(SPEC.format(base_url=server.base_url)))
        result = asyncio.run(execute(load_spec(path)))
    summaries = {s.metric: s for s in result.result.summaries}
    assert summaries["task-success"].mean == pytest.approx(2 / 3)
    assert summaries["task-success.pass^2"].mean == pytest.approx(2 / 3)
    assert summaries["policy-violations.policy-clean"].mean == 1.0
    assert summaries["agent-efficiency.within-budget"].mean == 1.0
    assert summaries["agent-efficiency.agent-steps"].mean == 2.0
    assert result.passed
    assert result.target_usage.input_tokens == 6 * 2 * 100
    records = {(r.id, i): r for i, r in enumerate(result.result.records)}
    assert len(records) == 6
    first = result.result.records[0]
    assert first.check is not None
    assert first.metadata["isolation"]["driver"] == "local-test"
    assert result.result.manifest["agent"]["harness"]["name"] == "evalsi-harness"


def test_cli_exit_code_follows_the_gate(
    local_sandbox: LocalSandboxClient, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    with ScriptedModelServer(agent_script) as server:
        path = tmp_path / "run.yaml"
        path.write_text(
            textwrap.dedent(SPEC.format(base_url=server.base_url)).replace("min: 0.6", "min: 0.9")
        )
        code = main(["run", "-f", str(path), "--format", "json"])
    assert code == 3  # gates failed
    assert '"task-success"' in capsys.readouterr().out

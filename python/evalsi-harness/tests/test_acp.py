"""The ACP connector end to end against a fake ACP agent (H3).

The fake (``fixtures/fake_acp_agent.py``) speaks ACP protocol version 1 over
stdio: it streams updates, asks permission, and reads and writes files and runs a
terminal through the client. It runs in the unconfined local stand-in sandbox
here, and on the real rungs in ``test_real_rungs.py``.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

AGENT = str(Path(__file__).parent / "fixtures" / "fake_acp_agent.py")


def spec_for(**acp: Any) -> Any:
    return spec_of(
        {
            "target": {
                "agent": {
                    "acp": {
                        "command": [sys.executable, AGENT],
                        "terminals": True,
                        "timeout": "30s",
                        **acp,
                    }
                }
            },
            "environment": {"files": {"notes.txt": "hello notes\n"}},
        }
    )


def steps_of(rec: Any, name: str) -> list[Any]:
    return [s for s in rec.trajectory.steps if s.name == name]


def test_updates_files_terminals_and_permissions_end_to_end(
    sandboxes: LocalSandboxClient,
) -> None:
    outcome = run(spec_for(), record("Fix it"), sandboxes)
    assert outcome.error == ""
    rec = outcome.record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "I will look at the notes. Done: wrote out.txt. café ✓"
    assert rec.metadata["agent"]["acp_stop_reason"] == "end_turn"
    assert rec.metadata["agent"]["acp_protocol_version"] == 1
    steps = rec.trajectory.steps  # type: ignore[union-attr]
    kinds = [(s.type, s.name) for s in steps]
    assert ("agent", "acp:thought") in kinds
    assert ("agent", "acp:plan") in kinds
    tool = steps_of(rec, "Run tests")[0]
    assert tool.type == "tool"
    assert tool.attributes["acp.status"] == "completed"
    assert tool.attributes["acp.kind"] == "execute"
    assert tool.input is not None
    assert "pytest -q" in tool.input.as_text()
    assert tool.output is not None
    text = tool.output.as_text()
    assert "read 'hello notes'" in text
    assert "missing: error -32002" in text
    assert "terminal said 'from-terminal' exit 0" in text
    # The steps are in the order they happened.
    order = [s.name for s in steps if s.name in ("acp:thought", "acp:plan", "Run tests")]
    assert order == ["acp:thought", "acp:plan", "Run tests"]
    # The file the agent wrote through the client is in the sandbox.
    written = [s for s in steps if s.type == "guardrail"]
    assert any(
        s.name == "sandbox.acp_permission" and s.attributes["evalsi.policy.granted"]
        for s in written
    )


def test_a_deny_rule_refuses_the_permission_and_the_agent_sees_it(
    sandboxes: LocalSandboxClient,
) -> None:
    approval = {"rules": [{"tool": "bash", "command": "pytest*", "action": "deny", "reason": "no"}]}
    rec = run(spec_for(approval=approval), record("Fix it"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text().endswith("I was not allowed to.")
    tool = steps_of(rec, "Run tests")[0]
    assert tool.attributes["acp.status"] == "failed"
    assert tool.error == "failed"
    denials = [
        s
        for s in rec.trajectory.steps  # type: ignore[union-attr]
        if s.name == "sandbox.acp_permission"
    ]
    assert len(denials) == 1
    assert denials[0].attributes["evalsi.policy.granted"] is False
    assert denials[0].output is not None
    assert "-> deny" in denials[0].output.as_text()


@pytest.mark.parametrize(("decision", "allowed"), [("approve", True), ("deny", False)])
def test_an_ask_rule_goes_to_the_scripted_approver_and_is_recorded(
    sandboxes: LocalSandboxClient, decision: str, allowed: bool
) -> None:
    approval = {
        "rules": [{"tool": "bash", "action": "ask"}],
        "approver": {"script": [{"match": {"tool": "bash"}, "decision": decision}]},
    }
    rec = run(spec_for(approval=approval), record("Fix it"), sandboxes).record
    assert rec is not None
    names = [s.name for s in rec.trajectory.steps]  # type: ignore[union-attr]
    assert names.count("hitl_request") == 1
    assert names.count("hitl_resolved") == 1
    resolved = steps_of(rec, "hitl_resolved")[0]
    assert resolved.attributes["evalsi.hitl.decision"] == decision
    assert resolved.attributes["evalsi.hitl.acp_tool_call_id"] == "call_1"
    assert (steps_of(rec, "Run tests")[0].attributes["acp.status"] == "completed") is allowed


def test_a_refusal_is_the_stop_reason(sandboxes: LocalSandboxClient) -> None:
    outcome = run(spec_for(), record("refuse"), sandboxes)
    rec = outcome.record
    assert rec is not None
    assert rec.metadata["agent"]["stop_reason"] == "refusal"


def test_an_agent_speaking_another_version_is_refused(
    sandboxes: LocalSandboxClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_ACP_BAD_VERSION", "1")
    spec = spec_for(env_from={"FAKE_ACP_BAD_VERSION": "FAKE_ACP_BAD_VERSION"})
    outcome = run(spec, record("hi"), sandboxes)
    assert outcome.record is None
    assert "protocol version 99" in outcome.error


def test_terminals_are_off_unless_the_spec_turns_them_on(sandboxes: LocalSandboxClient) -> None:
    outcome = run(spec_for(terminals=False), record("Fix it"), sandboxes)
    # The fake does not handle the error: it dies, and the connector says how.
    assert outcome.record is None
    assert "the agent exited" in outcome.error


def test_a_missing_command_or_environment_is_a_clear_error() -> None:
    from evalsi.v1alpha1 import agent_pb2
    from evalsi_harness.acp import ACPConnector
    from evalsi_harness.task import TaskError

    with pytest.raises(TaskError, match="needs a command"):
        ACPConnector(agent_pb2.ACPAgent(), None)
    with pytest.raises(TaskError, match="needs an environment"):
        ACPConnector(agent_pb2.ACPAgent(command=["x"]), None)

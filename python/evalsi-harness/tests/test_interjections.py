"""Scripted messages from the user while the agent works (H4)."""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then, target

Model = Callable[..., ScriptedModelServer]
NOTE = "Actually, use the name hello.txt too."


def script() -> Any:
    return calls_then(
        [
            {"tool_calls": [("bash", {"command": "echo a > a.txt"})]},
            {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
            {"text": "done"},
        ]
    )


def spec_with(
    server: ScriptedModelServer, connector: str = "openai-compatible", **builtin: Any
) -> Any:
    return spec_of(
        {
            "target": target(server, connector),
            "harness": {"builtin": {"interjections": [{"at_step": 2, "message": NOTE}], **builtin}},
            "environment": {"checker": CHECK_HELLO},
        }
    )


def roles(request: dict[str, Any]) -> list[str]:
    return [m["role"] for m in request["messages"]]


@pytest.mark.parametrize("connector", ["openai-compatible", "anthropic"])
def test_the_message_arrives_after_the_second_call_and_is_a_step(
    model: Model, sandboxes: LocalSandboxClient, connector: str
) -> None:
    server = model(script())
    rec = run(spec_with(server, connector), record(), sandboxes).record
    assert rec is not None
    assert len(server.requests) == 3
    first, second, third = server.requests
    assert NOTE not in str(first)
    assert NOTE not in str(second)
    assert NOTE in str(third)
    # It follows the second call's tool result, in the same user turn on Anthropic.
    if connector == "anthropic":
        last = third["messages"][-1]
        assert last["role"] == "user"
        assert [b["type"] for b in last["content"]] == ["tool_result", "text"]
        assert last["content"][-1]["text"] == NOTE
    else:
        assert roles(third)[-2:] == ["tool", "user"]
    steps = [s for s in rec.trajectory.steps if s.name == "user-interjection"]  # type: ignore[union-attr]
    assert len(steps) == 1
    assert steps[0].type == "user"
    assert steps[0].output is not None
    assert steps[0].output.as_text() == NOTE
    assert steps[0].attributes["evalsi.interjection.at_step"] == 2
    assert rec.metadata["agent"]["interjections"] == 1


def test_a_message_due_when_the_agent_tries_to_finish_is_still_read(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "all done"}, {"text": "ok, changed"}]))
    spec = spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {"interjections": [{"at_step": 1, "message": NOTE}]}},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert len(server.requests) == 2  # it did not finish before reading the message
    assert NOTE in str(server.requests[1])
    assert rec.output is not None
    assert rec.output.as_text() == "ok, changed"


def test_a_context_strategy_cannot_lose_the_message(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(script())
    context = {"strategy": "clear-tool-results", "window_tokens": 100_000}
    run(spec_with(server, context=context), record(), sandboxes)
    # Repeated on every call after it arrived, so clearing or summarizing history is safe.
    assert "Reminder: the user told you" not in str(server.requests[1])
    assert "Reminder: the user told you" in str(server.requests[2])
    assert NOTE in str(server.requests[2])


def test_the_messages_are_part_of_the_replay_guard(
    model: Model, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(script())
    rec = {"mode": "record", "dir": "tapes"}
    run(spec_with(server, recording=rec), record(), sandboxes, base_dir=tmp_path)
    replay = {"mode": "replay", "dir": "tapes"}
    same = run(spec_with(server, recording=replay), record(), sandboxes, base_dir=tmp_path)
    assert same.record is not None
    other = spec_of(
        {
            "target": target(server),
            "harness": {
                "builtin": {
                    "interjections": [{"at_step": 1, "message": NOTE}],
                    "recording": replay,
                }
            },
            "environment": {"checker": CHECK_HELLO},
        }
    )
    assert "needs the same tools" in run(other, record(), sandboxes, base_dir=tmp_path).error


def test_a_sub_agent_is_not_interrupted(model: Model, sandboxes: LocalSandboxClient) -> None:
    sent: list[str] = []

    def reply(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        text = str(messages)
        if "SUB-AGENT" in text.upper() and "delegate" not in tools:
            sent.append(text)
            return {"text": "report"}
        if any(m.get("role") == "tool" for m in messages):
            return {"text": "done"}
        return {"tool_calls": [("delegate", {"task": "look around"})]}

    server = model(reply)
    spec = spec_of(
        {
            "target": target(server),
            "harness": {
                "builtin": {
                    "tools": {"builtin": ["delegate"]},
                    "interjections": [{"at_step": 1, "message": NOTE}],
                }
            },
            "environment": {"checker": {"command": ["true"]}},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert sent, "the sub-agent ran"
    assert all(NOTE not in text for text in sent)

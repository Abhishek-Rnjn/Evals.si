"""Option ``user=tau2``: tau2's own UserSimulator drives the user. Its LiteLLM
call (``tau2.user.user_simulator.generate``) is replaced by a script, so the
tests need no provider or network."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from google.protobuf import json_format

from evalsi.runspec import normalize_durations
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.testing import Script, ScriptedModelServer
from evalsi_taubench import load

TELECOM = "[service_issue]airplane_mode_on|unseat_sim_card[PERSONA:None]"


@pytest.fixture
def model() -> Iterator[Any]:
    servers: list[ScriptedModelServer] = []

    def make(script: Script) -> ScriptedModelServer:
        servers.append(ScriptedModelServer(script))
        return servers[-1]

    yield make
    for s in servers:
        s.close()


def steps(answers: list[dict[str, Any]]) -> Script:
    it = iter(answers)
    last: dict[str, Any] = {}

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        nonlocal last
        last = next(it, last)
        return last

    return script


class FakeLLM:
    """Stands in for tau2's ``generate``: the user model's next message."""

    def __init__(self, *replies: dict[str, Any]) -> None:
        self.replies = list(replies)
        self.calls: list[dict[str, Any]] = []

    def __call__(self, model: str, messages: list[Any], tools: Any = None, **kwargs: Any) -> Any:
        from tau2.data_model.message import AssistantMessage, ToolCall

        self.calls.append({"model": model, "messages": messages, "tools": tools, "kwargs": kwargs})
        reply = self.replies.pop(0)
        calls = [
            ToolCall(id=f"c{i}", name=name, arguments={})
            for i, name in enumerate(reply.get("tools", []))
        ]
        return AssistantMessage(
            role="assistant",
            content=reply.get("text"),
            tool_calls=calls or None,
            usage={"prompt_tokens": 10, "completion_tokens": 5},
        )


def run(
    server: ScriptedModelServer,
    task_id: str,
    domain: str = "mock",
    *,
    options: dict[str, str] | None = None,
    recording: dict[str, Any] | None = None,
) -> TaskOutcome:
    spec = json_format.ParseDict(
        normalize_durations(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "scripted",
                    "base_url": server.base_url,
                },
                "dataset": {"inline": {"records": []}},
                "evaluators": [{"ref": "task-success"}],
                "harness": {"external": {"python": "evalsi_taubench:TauBenchHarness"}},
            },
            run_pb2.RunSpec.DESCRIPTOR,
        ),
        run_pb2.RunSpec(),
    )
    row = load(domain, tasks=task_id, **(options or {"user": "tau2"}))[0]
    record = Record(id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"])
    if recording is not None:
        json_format.ParseDict({"recording": recording}, spec.harness.external.config)

    async def go() -> TaskOutcome:
        async def no_sandboxes() -> Any:
            raise AssertionError("tau-bench tasks need no sandbox")

        # No judge: tau2's user does not use the run's.
        harness = load_harness(spec, HarnessContext(sandboxes=no_sandboxes, base_dir=Path.cwd()))
        try:
            return await run_task(Task.build(spec, record, trial=0, run_id="r"), harness)
        finally:
            await harness.aclose()

    return asyncio.run(go())


def mock_agent() -> Script:
    return steps(
        [
            {"tool_calls": [("create_task", {"user_id": "user_1", "title": "Important Meeting"})]},
            {"text": "The agent confirmed the task was created successfully."},
        ]
    )


def test_options_reach_the_records_and_are_validated() -> None:
    [default] = load("mock", tasks="create_task_1")
    assert "user" not in default["metadata"]["taubench"]
    [row] = load("mock", tasks="create_task_1", user="tau2", user_llm="gpt-4o", user_llm_args="{}")
    assert row["metadata"]["taubench"]["user"] == {"kind": "tau2", "llm": "gpt-4o", "llm_args": {}}
    [defaults] = load("mock", tasks="create_task_1", user="tau2")
    assert defaults["metadata"]["taubench"]["user"] == {
        "kind": "tau2",
        "llm": "gpt-4.1",
        "llm_args": {"temperature": 0.0},
    }
    for options, message in (
        ({"user": "other"}, "evalsi or tau2"),
        ({"user_llm": "gpt-4o"}, "apply to user=tau2"),
        ({"user": "tau2", "user_llm_args": "nope"}, "not JSON"),
        ({"user": "tau2", "user_llm_args": "[1]"}, "JSON object"),
    ):
        with pytest.raises(ValueError, match=message):
            load("mock", tasks="create_task_1", **options)


def test_tau2s_user_converses_and_the_task_is_graded(
    model: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    llm = FakeLLM(
        {"text": "Please create a task called 'Important Meeting' for user_1."},
        {"text": "Thanks! ###STOP###"},
    )
    monkeypatch.setattr("tau2.user.user_simulator.generate", llm)
    server = model(mock_agent())
    out = run(server, "create_task_1")
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed, rec.check.details
    # tau2's user opened by answering the agent's greeting, with tau2's prompt.
    first = llm.calls[0]
    assert first["model"] == "gpt-4.1"
    assert first["kwargs"]["temperature"] == 0.0
    assert first["messages"][0].role == "system"
    assert "Important Meeting" in first["messages"][0].content
    assert first["messages"][-1].content == "Hi! How can I help you today?"
    # The agent saw the user's opening message, and the stop token stayed out of the record.
    assert "Important Meeting" in server.requests[0]["messages"][-1]["content"]
    texts = [
        s.output.as_text() if s.output else ""
        for s in (e.step for e in out.events if hasattr(e, "step"))
        if s.type == "user"
    ]
    assert texts == ["Please create a task called 'Important Meeting' for user_1.", "Thanks!"]
    # The second user turn heard the agent's reply.
    assert (
        llm.calls[1]["messages"][-1].content
        == "The agent confirmed the task was created successfully."
    )
    # No tokens of the user model's are billed to the agent's usage here, and the user's
    # replies are the model's: a different reply changes the conversation.
    assert not llm.replies


def test_tau2s_user_calls_its_own_tools(model: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    llm = FakeLLM(
        {"text": "My phone has no service."},
        {"tools": ["toggle_airplane_mode"]},
        {"tools": ["reseat_sim_card"]},
        {"text": "I did both, and I have service now. ###STOP###"},
    )
    monkeypatch.setattr("tau2.user.user_simulator.generate", llm)
    out = run(
        model(steps([{"text": "Please turn airplane mode off and reseat your SIM card."}])),
        TELECOM,
        "telecom",
    )
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.check is not None
    # Graded on the device's end state, which only the user's tool calls change.
    assert rec.check.passed, rec.check.details
    assert "assertion 0: assert_service_status" in rec.check.tests
    # The user model was offered the device's tools, and saw each result.
    assert {t.name for t in llm.calls[0]["tools"]} >= {
        "toggle_airplane_mode",
        "reseat_sim_card",
    }
    assert llm.calls[2]["messages"][-1].role == "tool"


@pytest.mark.parametrize("domain", ["mock", "telecom"])
def test_a_recorded_run_replays_without_the_user_model(
    model: Any, monkeypatch: pytest.MonkeyPatch, tmp_path: Path, domain: str
) -> None:
    if domain == "mock":
        server, task_id = model(mock_agent()), "create_task_1"
        llm = FakeLLM(
            {"text": "Please create a task called 'Important Meeting' for user_1."},
            {"text": "Thanks! ###STOP###"},
        )
    else:
        server, task_id = (
            model(steps([{"text": "Please turn airplane mode off and reseat your SIM card."}])),
            TELECOM,
        )
        llm = FakeLLM(
            {"text": "My phone has no service."},
            {"tools": ["toggle_airplane_mode"]},
            {"tools": ["reseat_sim_card"]},
            {"text": "I did both, and I have service now. ###STOP###"},
        )
    monkeypatch.setattr("tau2.user.user_simulator.generate", llm)
    before = run(server, task_id, domain, recording={"mode": "record", "dir": str(tmp_path)})
    sent, asked = len(server.requests), len(llm.calls)

    def refuse(*args: Any, **kwargs: Any) -> Any:
        raise AssertionError("the user model was called during replay")

    monkeypatch.setattr("tau2.user.user_simulator.generate", refuse)
    after = run(server, task_id, domain, recording={"mode": "replay", "dir": str(tmp_path)})
    assert before.error == after.error == ""
    assert before.record is not None
    assert after.record is not None
    assert before.record.check is not None
    assert before.record.check.passed, before.record.check.details
    assert after.record.check == before.record.check
    assert len(server.requests) == sent
    assert asked == len(llm.calls)
    assert json.dumps(before.record.input.text if before.record.input else "")

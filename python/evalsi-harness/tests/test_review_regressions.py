"""Regressions for review findings R7-R10."""

from __future__ import annotations

import asyncio
import importlib
import json
from pathlib import Path
from typing import Any
from unittest.mock import patch

import httpx
import pytest

from evalsi.types import Usage
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness import HarnessContext, Task, load_harness, run_task
from evalsi_harness.cassette import Cassette, CassetteError, CassetteUserSimulator
from evalsi_harness.codemode import Interpreter, Limit
from evalsi_harness.connectors import HTTPConnector
from evalsi_harness.models import ModelTurn
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.user_sim import UserSimulator, UserTurn
from tests.harness_support import judge_answering, record, run, spec_of


@pytest.mark.parametrize("mode", ["resume", "fork"])
def test_user_prefix_replays_then_continues_with_turn_budget(tmp_path: Path, mode: str) -> None:
    class User(UserSimulator):
        def __init__(self) -> None:
            self.turns = 0

        async def reply(self, conversation: list[dict[str, Any]]) -> UserTurn:
            self.turns += 1
            return UserTurn(str(self.turns), self.turns >= 2, Usage(input_tokens=7))

    path = tmp_path / "session.jsonl"
    config = {"persona": "customer"}
    first = [{"role": "assistant", "content": "first"}]
    second = [{"role": "assistant", "content": "second"}]
    tape = Cassette(path, "record", user_simulator=config)
    tape.record({"kind": "model", "turn": {"text": "first"}})
    asyncio.run(CassetteUserSimulator(tape, User).reply(first))
    # Resume after a model turn but before its user reply. Fork after the first pair.
    tape.record({"kind": "model", "turn": {"text": "second"}})
    tape.save()
    output = tmp_path / "fork.jsonl"
    resumed = Cassette(
        path,
        mode,
        user_simulator=config,
        branch_at_step=1 if mode == "fork" else 0,
        fork_to=output if mode == "fork" else None,
    )
    created: list[User] = []

    def factory() -> User:
        user = User()
        created.append(user)
        return user

    user = CassetteUserSimulator(resumed, factory)
    assert resumed.next_turn() is not None
    first_reply = asyncio.run(user.reply(first))
    assert first_reply.message == "1"
    assert first_reply.usage.input_tokens == 7
    assert not created
    turn = resumed.next_turn()
    assert (turn is None) == (mode == "fork")
    last = asyncio.run(user.reply(second))
    assert last.message == "2"
    assert last.done
    assert len(created) == 1
    resumed.save()
    entries = [
        json.loads(line) for line in (output if mode == "fork" else path).read_text().splitlines()
    ]
    assert len([entry for entry in entries if entry["kind"] == "user"]) == 2


def test_old_logs_and_changed_user_configuration_are_rejected(tmp_path: Path) -> None:
    path = tmp_path / "session.jsonl"
    Cassette(path, "record").save()
    with pytest.raises(CassetteError, match="user simulator"):
        Cassette(path, "replay", user_simulator={"persona": "customer"})
    Cassette(path, "record", user_simulator={"persona": "customer"}).save()
    with pytest.raises(CassetteError, match="user simulator"):
        Cassette(path, "replay", user_simulator={"persona": "different"})


def test_cancellation_closes_connector() -> None:
    async def go() -> None:
        started = asyncio.Event()
        closed = asyncio.Event()

        class Agent:
            async def send(self, text: str) -> Any:
                started.set()
                await asyncio.Event().wait()

            async def aclose(self) -> None:
                closed.set()

        async def sandboxes() -> LocalSandboxClient:
            return LocalSandboxClient()

        spec = spec_of(
            {
                "target": {
                    "agent": {"http": {"url": "https://agent.invalid", "output_path": "answer"}}
                }
            }
        )
        harness = load_harness(spec, HarnessContext(sandboxes=sandboxes))
        task = Task.build(spec, record(), trial=0, run_id="review")
        with patch("evalsi_harness.connectors.connector_for", return_value=Agent()):
            running = asyncio.create_task(run_task(task, harness))
            await asyncio.wait_for(started.wait(), 1)
            running.cancel()
            with pytest.raises(asyncio.CancelledError):
                await running
        assert closed.is_set()
        await harness.aclose()

    asyncio.run(go())


@pytest.mark.parametrize(
    ("source", "expected"),
    [
        ("v = [1, 2, 3]\nv[1:2] = [4, 5]\nprint(v)", "[1, 4, 5, 3]"),
        ("v = [1, 4, 3, 5]\nv[::2] = [1, 2]\nprint(v)", "[1, 4, 2, 5]"),
    ],
)
def test_valid_slice_assignments_still_work(source: str, expected: str) -> None:
    assert asyncio.run(Interpreter({}, {}).run(source)).strip() == expected


def test_slice_assignment_enforces_code_mode_size_limit() -> None:
    source = "values = [0] * 1000000\nvalues[:0] = values\nprint(len(values))"
    with pytest.raises(Limit, match="list would be too large"):
        asyncio.run(Interpreter({}, {}).run(source))


def test_http_input_placeholders_are_literal() -> None:
    async def go() -> None:
        cfg = agent_pb2.HTTPAgent(url="https://agent.invalid", output_path="answer")
        connector = HTTPConnector(cfg, Task.build(spec_of({}), record(), trial=0, run_id="review"))
        await connector.client.aclose()
        seen: list[Any] = []

        def handle(request: httpx.Request) -> httpx.Response:
            seen.append(json.loads(request.content))
            return httpx.Response(200, json={"answer": "ok"})

        connector.client = httpx.AsyncClient(transport=httpx.MockTransport(handle))
        text = 'Explain {{task_id}} and {{messages}} and {{input}} with "quotes"'
        try:
            await connector.send(text)
            assert seen == [{"input": text}]
        finally:
            await connector.aclose()

    asyncio.run(go())


def test_invalid_json_terminates_harness_consumer() -> None:
    async def go() -> None:
        cfg = agent_pb2.HTTPAgent(url="https://agent.invalid", output_path="answer")
        connector = HTTPConnector(cfg, Task.build(spec_of({}), record(), trial=0, run_id="review"))
        await connector.client.aclose()
        connector.client = httpx.AsyncClient(
            transport=httpx.MockTransport(
                lambda req: httpx.Response(200, text="<html>upstream unavailable</html>")
            )
        )
        spec = spec_of({"target": {"agent": {"http": {"url": cfg.url, "output_path": "answer"}}}})

        async def sandboxes() -> LocalSandboxClient:
            return LocalSandboxClient()

        harness = load_harness(spec, HarnessContext(sandboxes=sandboxes))
        task = Task.build(spec, record("hello"), trial=0, run_id="review")
        with patch("evalsi_harness.connectors.connector_for", return_value=connector):
            outcome = await asyncio.wait_for(run_task(task, harness), timeout=1)
        await harness.aclose()
        assert outcome.record is None
        assert "JSONDecodeError" in outcome.error
        assert connector.client.is_closed

    asyncio.run(go())


def test_replay_preserves_user_turns_without_judge(tmp_path: Path) -> None:
    class Model:
        name = "fake-model"

        def __init__(self) -> None:
            self.turns = iter(["What is your order number?", "Order 42 ships tomorrow."])

        async def complete(self, *args: Any, **kwargs: Any) -> ModelTurn:
            return ModelTurn(text=next(self.turns))

        async def aclose(self) -> None:
            pass

    def spec(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "fake-model",
                    "base_url": "https://model.invalid",
                },
                "harness": {
                    "builtin": {
                        "recording": {"mode": mode, "dir": str(tmp_path)},
                        "user_simulator": {"persona": "customer"},
                    }
                },
            }
        )

    responses = iter([{"message": "42", "done": False}, {"message": "", "done": True}])
    harness_module = importlib.import_module("evalsi_harness.harness")
    with patch.object(harness_module, "create_model", return_value=Model()):
        original = run(
            spec("record"),
            record("Where is my order?"),
            LocalSandboxClient(),
            judge=judge_answering(lambda p, s: next(responses)),
        )
    with patch.object(harness_module, "create_model", return_value=Model()):
        replay = run(spec("replay"), record("Where is my order?"), LocalSandboxClient(), judge=None)
    assert original.record is not None, original.error
    assert replay.record is not None, replay.error
    assert original.record.output is not None
    assert replay.record.output is not None
    before = original.record.output.as_text()
    after = replay.record.output.as_text()
    assert before == after == "Order 42 ships tomorrow."


@pytest.mark.parametrize("decision_recorded", [False, True])
def test_elicitation_belongs_to_its_tool_invocation(
    tmp_path: Path, decision_recorded: bool
) -> None:
    from evalsi_harness.cassette import CassetteTool
    from evalsi_harness.harness import _elicitation
    from evalsi_harness.models import ToolSpec
    from evalsi_harness.tools import ToolResult

    class User(UserSimulator):
        def __init__(self, answers: list[str]) -> None:
            self.answers = iter(answers)
            self.turns = 0

        async def reply(self, conversation: list[dict[str, Any]]) -> UserTurn:
            self.turns += 1
            return UserTurn(next(self.answers), False, Usage())

    class Question:
        spec = ToolSpec(name="question", description="", input_schema={})

        def __init__(self, user: UserSimulator) -> None:
            self.answer = _elicitation(user)
            self.calls = 0
            self.fail = True

        async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
            self.calls += 1
            if self.calls == 2 and self.fail and not decision_recorded:
                raise RuntimeError("interrupted before elicitation")
            result = await self.answer(
                "Proceed?",
                {
                    "type": "object",
                    "properties": {"answer": {"type": "string"}},
                },
            )
            if self.calls == 2 and self.fail:
                raise RuntimeError("interrupted after elicitation")
            return ToolResult(json.dumps(result))

    async def go() -> None:
        path = tmp_path / "session.jsonl"
        tape = Cassette(path, "record", user_simulator={"persona": "customer"})
        user = CassetteUserSimulator(tape, lambda: User(["first", "second"]))
        tool = CassetteTool(tape, Question(user))
        tape.record({"kind": "model", "turn": {"text": "first"}})
        assert "first" in (await tool.call({})).content
        tape.record({"kind": "model", "turn": {"text": "second"}})
        with pytest.raises(RuntimeError, match="interrupted"):
            await tool.call({})
        tape.save()
        resumed = Cassette(path, "resume", user_simulator={"persona": "customer"})
        user = CassetteUserSimulator(resumed, lambda: User(["new second"]))
        inner = Question(user)
        inner.fail = False
        tool = CassetteTool(resumed, inner)
        assert resumed.next_turn() is not None
        assert "first" in (await tool.call({})).content
        assert inner.calls == 0
        assert user.elicitations == 1
        assert resumed.next_turn() is not None
        result = await tool.call({})
        assert ("second" if decision_recorded else "new second") in result.content
        assert "first" not in result.content
        assert user.turns == 0
        assert user.elicitations == 2
        resumed.save()
        # No completed invocation leaves a stale decision for the continuation.
        assert all(not queue for queue in resumed._users.values())

    asyncio.run(go())


def test_parallel_elicitations_and_cached_fork_keep_invocation_order(tmp_path: Path) -> None:
    from evalsi_harness.cassette import CassetteTool
    from evalsi_harness.harness import _elicitation
    from evalsi_harness.tools import FunctionTool

    class User(UserSimulator):
        def __init__(self) -> None:
            self.turns = 0
            self.answers = iter(["first", "second"])

        async def reply(self, conversation: list[dict[str, Any]]) -> UserTurn:
            return UserTurn(next(self.answers), False, Usage())

    async def go() -> None:
        path = tmp_path / "session.jsonl"
        child = tmp_path / "child.jsonl"
        config = {"persona": "customer"}
        tape = Cassette(path, "record", user_simulator=config)
        answer = _elicitation(CassetteUserSimulator(tape, User))
        second_finished = asyncio.Event()
        called = 0

        async def ask() -> str:
            nonlocal called
            called += 1
            first = called == 1
            result = await answer(
                "Same question",
                {
                    "type": "object",
                    "properties": {"answer": {"type": "string"}},
                },
            )
            if first:
                await second_finished.wait()
            else:
                second_finished.set()
            return json.dumps(result)

        tape.record({"kind": "model", "turn": {"text": "ask twice"}})
        tool = CassetteTool(tape, FunctionTool("ask", ask))
        original = await asyncio.gather(tool.call({}), tool.call({}))
        assert "first" in original[0].content
        assert "second" in original[1].content
        tape.save()
        # Both calls have identical names/args and finish in reverse order.
        for mode, source in [("fork", path), ("replay", child)]:
            cached = Cassette(
                source,
                mode,
                user_simulator=config,
                branch_at_step=1 if mode == "fork" else 0,
                fork_to=child if mode == "fork" else None,
            )
            user = CassetteUserSimulator(cached, User)
            assert cached.next_turn() is not None
            cached.record({"kind": "model", "turn": {"text": "ask twice"}}, replayed=True)
            wrapper = CassetteTool(cached, FunctionTool("ask", ask))
            results = await asyncio.gather(wrapper.call({}), wrapper.call({}))
            assert results == original
            assert user.elicitations == 2
            assert called == 2
            cached.save()
        entries = [json.loads(line) for line in child.read_text().splitlines()[1:]]
        assert len({entry["id"] for entry in entries}) == len(entries)
        assert [entry["parent"] for entry in entries[1:]] == [e["id"] for e in entries[:-1]]

    asyncio.run(go())


def test_v3_simulator_logs_require_a_new_recording(tmp_path: Path) -> None:
    path = tmp_path / "old.jsonl"
    config = {"persona": "customer"}
    path.write_text(json.dumps({"kind": "header", "version": 3, "user_simulator": config}))
    with pytest.raises(CassetteError, match="version 4"):
        Cassette(path, "resume", user_simulator=config)

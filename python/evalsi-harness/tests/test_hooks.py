"""Hooks at the built-in agent's lifecycle points."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest

from evalsi.types import Usage
from evalsi_harness.cassette import Cassette, CassetteError
from evalsi_harness.events import FinalEvent, StepEvent
from evalsi_harness.hooks import (
    Continue,
    Deny,
    Hook,
    HookError,
    Hooks,
    ModelRequest,
    Rewrite,
    load_hooks,
)
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.task import TaskError
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.tools import FunctionTool, ToolResult
from evalsi_harness.trust import Trust
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then


class Scripted:
    name = "scripted"

    def __init__(self, *turns: ModelTurn) -> None:
        self.turns = list(turns)
        self.requests: list[tuple[list[dict[str, Any]], str]] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.requests.append(([dict(m) for m in messages], system))
        return self.turns.pop(0)

    async def aclose(self) -> None:
        pass


def call(name: str, **args: Any) -> ModelTurn:
    return ModelTurn(
        text="",
        tool_calls=[ToolCallRequest("c1", name, json.dumps(args))],
        stop_reason="tool_use",
        usage=Usage(input_tokens=10, output_tokens=2),
    )


def answer(text: str = "done") -> ModelTurn:
    return ModelTurn(
        text=text, stop_reason="end_turn", usage=Usage(input_tokens=10, output_tokens=2)
    )


def echo(text: str) -> str:
    return f"echo:{text}"


def run_loop(
    model: Scripted, hooks: list[Any], tools: list[Any] | None = None, **config: Any
) -> tuple[FinalEvent, list[Any]]:
    events: list[Any] = []
    loop = AgentLoop(
        model,
        tools or [FunctionTool("echo", echo, input_schema={"type": "object"})],
        LoopConfig(**config),
        events.append,
        hooks=Hooks([Hook(f"h{i}", h, {}) for i, h in enumerate(hooks)]),
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    return final, events


def hook_steps(events: list[Any]) -> list[Any]:
    return [e.step for e in events if isinstance(e, StepEvent) and e.step.name.startswith("hook:")]


class Gate:
    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        if json.loads(call.arguments).get("text") == "rm -rf":
            return Deny("that command is not allowed here")
        return None


def test_a_hook_can_deny_a_tool_call_and_the_agent_sees_why() -> None:
    model = Scripted(call("echo", text="rm -rf"), answer())
    final, events = run_loop(model, [Gate()])
    result = next(m for m in model.requests[1][0] if m["role"] == "tool")
    assert result["is_error"] is True
    assert "denied by a hook: that command is not allowed here" in result["content"]
    tool = next(e.step for e in events if isinstance(e, StepEvent) and e.step.type == "tool")
    assert tool.attributes["evalsi.tool.hook_denied"] is True
    (decision,) = hook_steps(events)
    assert decision.attributes["evalsi.hook.decision"] == "deny"
    assert decision.attributes["evalsi.hook.reason"] == "that command is not allowed here"
    assert final.stop_reason == "completed"


def test_the_denied_tool_never_runs() -> None:
    ran: list[str] = []

    def spy(text: str) -> str:
        ran.append(text)
        return "ok"

    model = Scripted(call("echo", text="rm -rf"), call("echo", text="fine"), answer())
    run_loop(model, [Gate()], tools=[FunctionTool("echo", spy, input_schema={"type": "object"})])
    assert ran == ["fine"]


class Rewriter:
    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        return Rewrite({"text": "safe"})


def test_a_hook_can_rewrite_arguments_and_the_original_is_recorded() -> None:
    model = Scripted(call("echo", text="risky"), answer())
    _, events = run_loop(model, [Rewriter()])
    tool = next(e.step for e in events if isinstance(e, StepEvent) and e.step.type == "tool")
    assert tool.output is not None
    assert tool.output.text == "echo:safe"
    assert tool.input is not None
    assert json.loads(tool.input.text or "")["text"] == "safe"
    (decision,) = hook_steps(events)
    assert json.loads(decision.attributes["evalsi.hook.original_arguments"]) == {"text": "risky"}


class Censor:
    async def after_tool(self, ctx: Any, call: ToolCallRequest, result: ToolResult) -> Any:
        return ToolResult(result.content.replace("echo", "[redacted]"))


def test_a_hook_can_replace_a_tool_result_and_the_step_shows_it() -> None:
    model = Scripted(call("echo", text="x"), answer())
    _, events = run_loop(model, [Censor()])
    tool = next(e.step for e in events if isinstance(e, StepEvent) and e.step.type == "tool")
    assert tool.output is not None
    assert tool.output.text == "[redacted]:x"
    result = next(m for m in model.requests[1][0] if m["role"] == "tool")
    assert result["content"] == "[redacted]:x"


class Reframe:
    def before_model(self, ctx: Any, request: ModelRequest) -> Any:
        return ModelRequest(
            [*request.messages, {"role": "user", "content": f"(step {ctx.step})"}],
            request.system + " Be brief.",
        )


def test_before_model_changes_one_call_not_the_conversation() -> None:
    model = Scripted(call("echo", text="x"), answer())
    run_loop(model, [Reframe()], system="Base.")
    first, second = model.requests
    assert first[1] == "Base. Be brief."
    assert first[0][-1]["content"] == "(step 0)"
    # The next call is built from the real conversation, not the earlier rewrite.
    assert [m["content"] for m in second[0] if m["role"] == "user"] == ["go", "(step 1)"]


class Verify:
    def __init__(self, config: dict[str, Any]) -> None:
        self.message = config.get("message", "Run the tests before you finish.")

    def on_stop(self, ctx: Any, final: FinalEvent) -> Any:
        return Continue(self.message)


def test_on_stop_can_send_the_agent_back_to_work_a_bounded_number_of_times() -> None:
    model = Scripted(*[answer(f"a{i}") for i in range(10)])
    final, events = run_loop(model, [Verify({})])
    # One run, three extra messages, then it is allowed to finish.
    assert len(model.requests) == 4
    assert final.stop_reason == "completed"
    assert final.output is not None
    assert final.output.text == "a3"
    assert [d.attributes["evalsi.hook.decision"] for d in hook_steps(events)] == ["continue"] * 3
    assert model.requests[1][0][-1] == {
        "role": "user",
        "content": "Run the tests before you finish.",
    }


class Seen:
    def __init__(self) -> None:
        self.steps: list[str] = []

    def on_compaction(self, ctx: Any, step: Any) -> None:
        self.steps.append(step.name)


def test_on_compaction_sees_context_steps() -> None:
    from evalsi_harness.context import ContextConfig

    class Sized(Scripted):
        """Reports the prompt size from what it was sent, as a provider does."""

        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            turn = await super().complete(messages, tools, **kw)
            chars = sum(len(str(m.get("content") or "")) for m in messages)
            turn.usage = Usage(input_tokens=chars // 4, output_tokens=2)
            return turn

    seen = Seen()
    turns = [call("echo", text="y" * 3000) for _ in range(12)] + [answer()]
    model = Sized(*turns)
    run_loop(
        model,
        [seen],
        context=ContextConfig(
            strategy="clear-tool-results",
            window_tokens=4000,
            trigger=0.3,
            target=0.1,
            keep_recent=1,
        ),
    )
    assert seen.steps
    assert set(seen.steps) == {"context:clear-tool-results"}


class Broken:
    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        raise RuntimeError("the gate is down")


def test_a_failing_hook_ends_the_run_with_its_error_rather_than_being_ignored() -> None:
    with pytest.raises(
        HookError, match="hook h0 failed in before_tool: RuntimeError: the gate is down"
    ):
        run_loop(Scripted(call("echo", text="x")), [Broken()])


class WrongType:
    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        return "deny"


def test_a_hook_returning_the_wrong_type_is_an_error() -> None:
    with pytest.raises(HookError, match="must return Deny, Rewrite or None"):
        run_loop(Scripted(call("echo", text="x")), [WrongType()])


class Ordered:
    def __init__(self, tag: str, log: list[str]) -> None:
        self.tag, self.log = tag, log

    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        self.log.append(self.tag)
        return None


def test_hooks_run_in_order_and_a_deny_stops_the_ones_after_it() -> None:
    log: list[str] = []
    model = Scripted(call("echo", text="rm -rf"), answer())
    run_loop(model, [Ordered("a", log), Gate(), Ordered("b", log)])
    assert log == ["a"]


def test_parallel_tool_calls_get_their_hooks_in_order_before_any_runs() -> None:
    order: list[str] = []

    class Spy:
        def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
            order.append(f"hook:{call.id}")

        def after_tool(self, ctx: Any, call: ToolCallRequest, result: ToolResult) -> Any:
            order.append(f"after:{call.id}")

    def slow(text: str) -> str:
        order.append(f"run:{text}")
        return text

    two = ModelTurn(
        text="",
        tool_calls=[
            ToolCallRequest("c1", "echo", '{"text": "a"}'),
            ToolCallRequest("c2", "echo", '{"text": "b"}'),
        ],
        stop_reason="tool_use",
    )
    run_loop(
        Scripted(two, answer()),
        [Spy()],
        tools=[FunctionTool("echo", slow, input_schema={"type": "object"})],
    )
    assert order[:2] == ["hook:c1", "hook:c2"]
    assert order[-2:] == ["after:c1", "after:c2"]


# --- loading, trust and replay -------------------------------------------------------


def test_load_hooks_builds_classes_with_their_config_and_checks_trust() -> None:
    hooks = load_hooks([(f"{__name__}:Verify", {"message": "check it"})], Trust())
    assert hooks.refs() == [f"{__name__}:Verify"]
    assert hooks.hooks[0].obj.message == "check it"
    assert load_hooks([(f"{__name__}:Gate", {})], Trust()).hooks[0].has("before_tool")
    with pytest.raises(TaskError, match="would run on the server"):
        load_hooks([(f"{__name__}:Gate", {})], Trust(commands=[], python=[]))
    load_hooks([(f"{__name__}:Gate", {})], Trust(commands=[], python=[f"{__name__}:Gate"]))
    for bad, match in (
        ("nomodule", "must be module:Class"),
        ("no_such_module_xyz:H", "cannot load hook"),
        (f"{__name__}:Missing", "cannot load hook"),
        (f"{__name__}:Plain", "has none of"),
    ):
        with pytest.raises(TaskError, match=match):
            load_hooks([(bad, {})], Trust())


class Plain:
    pass


def test_a_cassette_only_replays_with_the_hooks_it_was_recorded_with(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record", hooks=["m:A", "m:B"])
    tape.record(
        {
            "kind": "model",
            "turn": {"text": "hi", "tool_calls": [], "usage": {}, "stop_reason": "end_turn"},
        }
    )
    tape.save()
    assert Cassette(path, "replay", hooks=["m:A", "m:B"]).next_turn() is not None
    for other in ([], ["m:A"], ["m:B", "m:A"]):
        with pytest.raises(CassetteError, match="same hooks in the same order"):
            Cassette(path, "replay", hooks=other)


# --- through the harness ---------------------------------------------------------------


def test_hooks_named_in_the_spec_gate_a_real_run(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(
        calls_then(
            [
                {"tool_calls": [("bash", {"command": "echo forbidden > out.txt"})]},
                {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
                {"text": "done"},
            ]
        )
    )

    def spec(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {
                    "builtin": {
                        "hooks": [
                            {"python": f"{__name__}:BashGate", "config": {"ban": "forbidden"}}
                        ],
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {"checker": CHECK_HELLO},
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed  # the forbidden command was denied; the second one wrote hello
    assert first.trajectory is not None
    names = [s.name for s in first.trajectory.steps]
    assert f"hook:{__name__}:BashGate" in names
    sent = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.check is not None
    assert again.check.passed


class BashGate:
    def __init__(self, config: dict[str, Any]) -> None:
        self.ban = config["ban"]

    def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
        if self.ban in json.loads(call.arguments).get("command", ""):
            return Deny(f"commands containing {self.ban!r} are not allowed")
        return None

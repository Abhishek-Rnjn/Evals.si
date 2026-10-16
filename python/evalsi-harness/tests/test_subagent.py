"""delegate: a sub-agent with a fresh context, and only its report comes back."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

from evalsi.packs.agent_checks import handoff_correctness
from evalsi.types import Record, Trajectory, Usage
from evalsi_harness.events import StepEvent
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.subagent import SUBAGENT_SYSTEM
from evalsi_harness.tools import FunctionTool
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO


def usage(i: int = 100, o: int = 10) -> Usage:
    return Usage(input_tokens=i, output_tokens=o)


def delegate_call(task: str, call_id: str = "d1", **extra: Any) -> ModelTurn:
    args = json.dumps({"task": task, **extra})
    return ModelTurn(
        text="",
        tool_calls=[ToolCallRequest(call_id, "delegate", args)],
        stop_reason="tool_use",
        usage=usage(),
    )


class Router:
    """One model for parent and sub-agent: it answers by what it is sent."""

    name = "router"

    def __init__(self) -> None:
        self.requests: list[tuple[list[dict[str, Any]], str, list[str]]] = []
        self.sub_turns = 0

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.requests.append(([dict(m) for m in messages], system, [t.name for t in tools]))
        if system == SUBAGENT_SYSTEM:
            self.sub_turns += 1
            if self.sub_turns == 1:
                call = ToolCallRequest("s1", "read_big", json.dumps({"n": 1}))
                return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage())
            return ModelTurn(
                text="Report: the file has 3000 x's and the key is KEY-42.",
                stop_reason="end_turn",
                usage=usage(),
            )
        if not any(m["role"] == "tool" for m in messages):
            return delegate_call("Find the key in big.txt and tell me what it is.")
        return ModelTurn(text="All done.", stop_reason="end_turn", usage=usage())

    async def aclose(self) -> None:
        pass


def big(n: int) -> str:
    return "KEY-42 " + "x" * 3000


def run_loop(model: Any, **config: Any) -> tuple[Any, list[Any], AgentLoop]:
    events: list[Any] = []
    loop = AgentLoop(
        model,
        [FunctionTool("read_big", big, input_schema={"type": "object"})],
        LoopConfig(delegate_steps=10, **config),
        events.append,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "find the key"}]))
    return final, events, loop


def test_the_parent_gets_only_the_report_not_the_subagents_working() -> None:
    model = Router()
    final, _, _ = run_loop(model)
    assert final.stop_reason == "completed"
    parent_after = model.requests[-1][0]
    result = next(m for m in parent_after if m["role"] == "tool")
    assert result["content"] == "Report: the file has 3000 x's and the key is KEY-42."
    # The 3,000 characters the sub-agent read never reached the parent.
    assert sum(len(str(m.get("content") or "")) for m in parent_after) < 600


def test_the_subagent_starts_clean_with_the_tools_but_cannot_delegate() -> None:
    model = Router()
    run_loop(model)
    first_sub = next(r for r in model.requests if r[1] == SUBAGENT_SYSTEM)
    assert [m["content"] for m in first_sub[0]] == [
        "Find the key in big.txt and tell me what it is."
    ]
    assert first_sub[2] == ["read_big"]
    parent_first = model.requests[0]
    assert parent_first[2] == ["read_big", "delegate"]


def test_the_subagents_steps_are_on_the_trajectory_under_the_delegate_call() -> None:
    model = Router()
    final, events, _ = run_loop(model)
    steps = [e.step for e in events if isinstance(e, StepEvent)]
    delegate = next(s for s in steps if s.type == "tool" and s.name == "delegate")
    sub = next(s for s in steps if s.name == "subagent")
    handoff = next(s for s in steps if s.type == "handoff")
    assert handoff.parent_span_id == delegate.span_id
    assert sub.parent_span_id == handoff.span_id
    assert handoff_correctness(Record(id="delegate", trajectory=Trajectory(steps=steps))).passed
    assert sub.attributes["evalsi.subagent.steps"] == 2
    assert sub.attributes["evalsi.subagent.stop_reason"] == "completed"
    inner = [s for s in steps if s.parent_span_id == sub.span_id]
    assert [(s.type, s.name) for s in inner].count(("tool", "read_big")) == 1
    assert sum(1 for s in inner if s.type == "llm") == 2
    assert final.extra["subagents"] == 1
    assert final.extra["subagent_steps"] == 2


def test_the_subagents_spend_is_the_parents_spend() -> None:
    final, _, _ = run_loop(Router(), input_per_mtok=10.0, output_per_mtok=10.0)
    # parent: 2 calls, sub-agent: 2 calls, each 100 in and 10 out.
    assert final.usage.input_tokens == 400
    assert final.usage.output_tokens == 40
    assert final.usage.cost_usd == (400 + 40) * 10 / 1e6


def test_the_subagent_runs_inside_what_the_parent_has_left() -> None:
    class Hungry(Router):
        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            turn = await super().complete(messages, tools, **kw)
            if kw.get("system") == SUBAGENT_SYSTEM:
                turn.usage = usage(1000, 10)
            return turn

    # 600 tokens in all: the first call (110) leaves 490 for the sub-agent, whose first call alone
    # is 1,010, so it stops on its budget and says so.
    model = Hungry()
    final, events, _ = run_loop(model, max_tokens=600)
    sub = next(e.step for e in events if isinstance(e, StepEvent) and e.step.name == "subagent")
    assert sub.attributes["evalsi.subagent.stop_reason"] == "budget"
    assert final.stop_reason == "budget"  # and the parent is over its own limit too


def test_no_budget_left_is_an_error_the_agent_sees_not_a_free_subagent() -> None:
    model = Router()
    final, events, _ = run_loop(model, max_tokens=110, max_steps=5)
    # The parent's first call used 110 tokens: nothing is left, so delegate refuses.
    tool = next(e.step for e in events if isinstance(e, StepEvent) and e.step.name == "delegate")
    assert tool.error.startswith("there is no budget left")
    assert model.sub_turns == 0
    assert final.stop_reason == "budget"


def test_a_subagents_step_limit_is_capped_by_the_tool_and_the_request() -> None:
    class Endless(Router):
        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            if kw.get("system") == SUBAGENT_SYSTEM:
                self.requests.append((messages, SUBAGENT_SYSTEM, []))
                self.sub_turns += 1
                call = ToolCallRequest(f"s{self.sub_turns}", "read_big", "{}")
                return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage())
            return await super().complete(messages, tools, **kw)

    model = Endless()
    run_loop(model)
    assert model.sub_turns == 10  # delegate_steps
    asked = Endless()
    # Asking for fewer steps is honoured; asking for more is not.
    events: list[Any] = []
    loop = AgentLoop(
        asked,
        [FunctionTool("read_big", big, input_schema={"type": "object"})],
        LoopConfig(delegate_steps=10),
        events.append,
    )
    tool = loop.tools["delegate"]
    asyncio.run(tool.call({"task": "go", "max_steps": 3}))
    assert asked.sub_turns == 3
    asked.sub_turns = 0
    asyncio.run(tool.call({"task": "go", "max_steps": 99}))
    assert asked.sub_turns == 10
    assert asyncio.run(tool.call({"task": "  "})).is_error


def test_delegate_is_not_offered_unless_asked_and_runs_serially() -> None:
    plain = AgentLoop(Router(), [], LoopConfig(), lambda e: None)
    assert "delegate" not in plain.tools
    order: list[str] = []

    class Two(Router):
        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            if kw.get("system") == SUBAGENT_SYSTEM:
                order.append("sub:" + str(messages[0]["content"]))
                return ModelTurn(text="ok", stop_reason="end_turn", usage=usage())
            if not any(m["role"] == "tool" for m in messages):
                calls = [
                    ToolCallRequest("d1", "delegate", '{"task": "one"}'),
                    ToolCallRequest("d2", "delegate", '{"task": "two"}'),
                ]
                return ModelTurn(text="", tool_calls=calls, stop_reason="tool_use", usage=usage())
            return ModelTurn(text="done", stop_reason="end_turn", usage=usage())

    run_loop(Two())
    assert order == ["sub:one", "sub:two"]


def test_a_subagent_inherits_the_hooks_so_a_gate_covers_it() -> None:
    from evalsi_harness.hooks import Deny, Hook, Hooks

    class Gate:
        def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
            return Deny("no tools today") if call.name == "read_big" else None

    model = Router()
    events: list[Any] = []
    loop = AgentLoop(
        model,
        [FunctionTool("read_big", big, input_schema={"type": "object"})],
        LoopConfig(delegate_steps=10),
        events.append,
        hooks=Hooks([Hook("gate", Gate(), {})]),
    )
    asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    denied = [
        e.step
        for e in events
        if isinstance(e, StepEvent) and e.step.attributes.get("evalsi.tool.hook_denied")
    ]
    assert len(denied) == 1


def test_a_run_with_a_subagent_replays_exactly(model: Any, sandboxes: Any, tmp_path: Path) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        system = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
        if system == SUBAGENT_SYSTEM:
            sub_tools = [m for m in messages if m["role"] == "tool"]
            if not sub_tools:
                return {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}
            return {"text": "wrote out.txt"}
        if not any(m["role"] == "tool" for m in messages):
            return {"tool_calls": [("delegate", {"task": "write hello to out.txt"})]}
        return {"text": "finished"}

    server = model(script)

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
                        "tools": {"builtin": ["delegate"]},
                        "delegate_steps": 5,
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {"checker": CHECK_HELLO},
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed  # the sub-agent's bash command did the work
    assert first.metadata["agent"]["subagents"] == 1
    assert first.trajectory is not None
    assert any(s.name == "subagent" for s in first.trajectory.steps)
    sent = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent  # the sub-agent's calls were on the tape too
    assert again is not None
    assert again.check is not None
    assert again.check.passed
    assert again.metadata["agent"]["subagents"] == 1

"""The built-in reference agent: a tool-calling loop over any chat model.

Each model call and each tool call becomes a step. The loop stops when the
model answers without calling tools (and the simulated user, if any, is
done), or when a budget runs out: steps, tokens, spend or wall-clock time.
"""

from __future__ import annotations

import asyncio
import json
import time
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content, Message, Step, ToolCall, Usage
from evalsi_harness.events import Emit, FinalEvent, StepEvent, span_id
from evalsi_harness.models import ChatModel, ModelTurn, ToolCallRequest
from evalsi_harness.tools import Tool, ToolResult
from evalsi_harness.user_sim import UserSimulator

DEFAULT_SYSTEM = (
    "You are an autonomous agent completing a task. Use the tools available to you to "
    "do the work and verify it. When you are finished, reply with your final answer "
    "and do not call any more tools."
)


@dataclass
class LoopConfig:
    max_steps: int = 30
    max_tokens: int = 0
    max_usd: float = 0.0
    wall_clock_s: float = 0.0
    input_per_mtok: float = 0.0
    output_per_mtok: float = 0.0
    parallel: bool = True
    system: str = DEFAULT_SYSTEM


@dataclass
class LoopState:
    usage: Usage = field(default_factory=lambda: Usage(input_tokens=0, output_tokens=0))
    user_usage: Usage = field(default_factory=lambda: Usage(input_tokens=0, output_tokens=0))
    steps: int = 0
    tool_calls: int = 0


def _add(total: Usage, part: Usage) -> None:
    total.input_tokens = (total.input_tokens or 0) + (part.input_tokens or 0)
    total.output_tokens = (total.output_tokens or 0) + (part.output_tokens or 0)


class AgentLoop:
    def __init__(
        self,
        model: ChatModel,
        tools: list[Tool],
        config: LoopConfig,
        emit: Emit,
        *,
        user: UserSimulator | None = None,
    ) -> None:
        names = [t.spec.name for t in tools]
        dupes = {n for n in names if names.count(n) > 1}
        if dupes:
            raise ValueError(f"two tools share the name(s) {sorted(dupes)}")
        self.model = model
        self.tools = {t.spec.name: t for t in tools}
        self.config = config
        self.emit = emit
        self.user = user
        self.state = LoopState()
        self.root = span_id()

    def cost(self) -> float:
        u = self.state.usage
        return (
            (u.input_tokens or 0) * self.config.input_per_mtok
            + (u.output_tokens or 0) * self.config.output_per_mtok
        ) / 1e6

    def _over_budget(self, started: float) -> str:
        c, u = self.config, self.state.usage
        if self.state.steps >= c.max_steps:
            return "max_steps"
        if c.max_tokens and (u.input_tokens or 0) + (u.output_tokens or 0) >= c.max_tokens:
            return "budget"
        if c.max_usd and self.cost() >= c.max_usd:
            return "budget"
        if c.wall_clock_s and time.monotonic() - started >= c.wall_clock_s:
            return "timeout"
        return ""

    def _final(self, text: str, reason: str, error: str = "") -> FinalEvent:
        usage = Usage(
            input_tokens=self.state.usage.input_tokens,
            output_tokens=self.state.usage.output_tokens,
            cost_usd=self.cost()
            if self.config.input_per_mtok or self.config.output_per_mtok
            else None,
        )
        return FinalEvent(
            output=Content(text=text) if text or reason == "completed" else None,
            usage=usage,
            stop_reason=reason,
            error=error,
            extra={
                "steps": self.state.steps,
                "tool_calls": self.state.tool_calls,
                "user_usage": self.state.user_usage.to_dict(),
            },
        )

    async def run(self, conversation: list[dict[str, Any]]) -> FinalEvent:
        started = time.monotonic()
        try:
            async with asyncio.timeout(self.config.wall_clock_s or None):
                return await self._loop(list(conversation), started)
        except TimeoutError:
            return self._final("", "timeout")

    async def _loop(self, messages: list[dict[str, Any]], started: float) -> FinalEvent:
        specs = [t.spec for t in self.tools.values()]
        last_text = ""
        while True:
            if reason := self._over_budget(started):
                return self._final(last_text, reason)
            step_span = span_id()
            start_ns = time.time_ns()
            turn = await self.model.complete(messages, specs, system=self.config.system)
            self.state.steps += 1
            _add(self.state.usage, turn.usage)
            self.emit(StepEvent(self._llm_step(turn, step_span), start_ns=start_ns))
            messages.append(turn.as_message())
            last_text = turn.text or last_text
            if turn.tool_calls:
                await self._call_tools(turn.tool_calls, messages)
                continue
            if self.user is None:
                return self._final(turn.text, "completed")
            reply = await self.user.reply(messages)
            _add(self.state.user_usage, reply.usage)
            if reply.message:
                self.emit(
                    StepEvent(
                        Step(
                            type="user",
                            name="simulated-user",
                            output=Content(text=reply.message),
                            span_id=span_id(),
                            parent_span_id=self.root,
                        )
                    )
                )
            if reply.done or not reply.message:
                return self._final(turn.text, "completed")
            messages.append({"role": "user", "content": reply.message})

    def _llm_step(self, turn: ModelTurn, span: str) -> Step:
        return Step(
            type="llm",
            name=self.model.name,
            output=Content(
                messages=[
                    Message(
                        role="assistant",
                        content=turn.text,
                        tool_calls=[
                            ToolCall(id=c.id, name=c.name, arguments=c.arguments)
                            for c in turn.tool_calls
                        ],
                    )
                ]
            ),
            span_id=span,
            parent_span_id=self.root,
            usage=turn.usage,
            duration_ms=turn.usage.latency_ms,
            attributes={
                "gen_ai.request.model": self.model.name,
                "gen_ai.response.finish_reasons": [turn.stop_reason],
            },
        )

    async def _call_tools(
        self, calls: list[ToolCallRequest], messages: list[dict[str, Any]]
    ) -> None:
        async def one(call: ToolCallRequest) -> tuple[ToolCallRequest, ToolResult, Step]:
            span = span_id()
            start = time.perf_counter()
            tool = self.tools.get(call.name)
            try:
                args = json.loads(call.arguments or "{}")
            except json.JSONDecodeError as exc:
                result = ToolResult(f"the arguments are not valid JSON: {exc}", is_error=True)
            else:
                if not isinstance(args, dict):
                    result = ToolResult("the arguments must be a JSON object", is_error=True)
                elif tool is None:
                    known = ", ".join(sorted(self.tools)) or "none"
                    result = ToolResult(
                        f"there is no tool {call.name!r} (tools: {known})", is_error=True
                    )
                else:
                    result = await tool.call(args, span=span)
            step = Step(
                type="tool",
                name=call.name,
                input=Content(text=call.arguments),
                output=Content(text=result.content),
                error=result.content[:500] if result.is_error else "",
                span_id=span,
                parent_span_id=self.root,
                duration_ms=(time.perf_counter() - start) * 1000,
                attributes={
                    "gen_ai.tool.call.id": call.id,
                    **{f"evalsi.tool.{k}": v for k, v in result.metadata.items()},
                },
            )
            return call, result, step

        if self.config.parallel and len(calls) > 1:
            done = await asyncio.gather(*(one(c) for c in calls))
        else:
            done = [await one(c) for c in calls]
        for call, result, step in done:
            self.state.tool_calls += 1
            self.emit(StepEvent(step))
            messages.append(
                {
                    "role": "tool",
                    "tool_call_id": call.id,
                    "content": result.content,
                    "is_error": result.is_error,
                }
            )

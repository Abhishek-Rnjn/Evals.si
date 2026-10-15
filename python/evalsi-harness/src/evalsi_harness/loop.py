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
from evalsi_harness.context import ContextConfig, ContextView, make_strategy
from evalsi_harness.events import Emit, FinalEvent, StepEvent, span_id
from evalsi_harness.models import (
    ChatModel,
    ModelError,
    ModelTurn,
    ToolCallRequest,
    thinking_text,
)
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
    # Price of cached prompt tokens as a multiple of the input price. 1.0
    # assumes no discount, so max_usd stays an upper bound for providers
    # whose cache prices we do not know.
    cache_read_mult: float = 1.0
    cache_write_mult: float = 1.0
    # A turn cut off by the output limit is tried once more with this limit
    # (0: not retried). It is the config's, not the model's, so a replay makes
    # the same decision as the recorded run.
    truncation_retry_tokens: int = 0
    context: ContextConfig = field(default_factory=ContextConfig)
    parallel: bool = True
    system: str = DEFAULT_SYSTEM


@dataclass
class LoopState:
    usage: Usage = field(default_factory=lambda: Usage(input_tokens=0, output_tokens=0))
    user_usage: Usage = field(default_factory=lambda: Usage(input_tokens=0, output_tokens=0))
    steps: int = 0
    tool_calls: int = 0
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0
    truncated_turns: int = 0
    reasoning_tokens: int = 0
    truncation_retries: int = 0


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
        self.strategy = make_strategy(config.context)
        self.view = ContextView(
            model=model,
            system=config.system,
            tools_chars=len(
                json.dumps([(t.spec.name, t.spec.description, t.spec.input_schema) for t in tools])
            ),
            bill=self._bill,
        )

    def cost(self) -> float:
        u, c, st = self.state.usage, self.config, self.state
        plain = (u.input_tokens or 0) - st.cache_read_tokens - st.cache_write_tokens
        return (
            (
                plain
                + st.cache_read_tokens * c.cache_read_mult
                + st.cache_write_tokens * c.cache_write_mult
            )
            * c.input_per_mtok
            + (u.output_tokens or 0) * c.output_per_mtok
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
                "cache_read_tokens": self.state.cache_read_tokens,
                "cache_write_tokens": self.state.cache_write_tokens,
                "reasoning_tokens": self.state.reasoning_tokens,
                "truncation_retries": self.state.truncation_retries,
                **(
                    {"context": self.strategy.stats.to_dict()}
                    if self.config.context.strategy != "none"
                    else {}
                ),
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
            prepared = await self.strategy.prepare(messages, self.view, parent=self.root)
            if prepared.messages is not messages:
                messages[:] = prepared.messages
            for step in prepared.steps:
                self.emit(StepEvent(step))
            turn = await self._call_model(messages, specs)
            last_text = turn.text or last_text
            if turn.truncated:
                # Cut off by the output limit: a tool call may be half-written
                # and an answer is incomplete, so this is not a normal finish.
                self.state.truncated_turns += 1
                return self._final(turn.text, "budget", error="the model hit its output limit")
            if turn.stop_reason == "refusal":
                return self._final(turn.text, "refusal")
            messages.append(turn.as_message())
            if turn.stop_reason == "pause_turn" and not turn.tool_calls:
                continue  # the API asks for the turn to be resumed as it stands
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

    def _bill(self, turn: ModelTurn) -> None:
        """Spend on a model call that is not a step (a summary)."""
        self._account(turn)

    def _account(self, turn: ModelTurn) -> None:
        _add(self.state.usage, turn.usage)
        self.state.cache_read_tokens += turn.cache_read_tokens
        self.state.cache_write_tokens += turn.cache_write_tokens
        self.state.reasoning_tokens += turn.reasoning_tokens

    async def _call_model(self, messages: list[dict[str, Any]], specs: list[Any]) -> ModelTurn:
        """One model step. A turn cut off by the output limit is tried again,
        once, with a higher limit; both calls are steps and both are billed."""
        step_span = span_id()
        start_ns = time.time_ns()
        turn = await self.model.complete(messages, specs, system=self.config.system)
        self.view.observe(messages, turn.usage.input_tokens)
        self.state.steps += 1
        self._account(turn)
        self.emit(StepEvent(self._llm_step(turn, step_span), start_ns=start_ns))
        if not turn.truncated or not self.config.truncation_retry_tokens:
            return turn
        if self._over_budget(time.monotonic()):  # steps or spend left no room
            return turn
        self.state.truncation_retries += 1
        try:
            return await self._retry_cut_off(messages, specs, self.config.truncation_retry_tokens)
        except ModelError:
            # The provider refused the higher limit (above its output cap, or
            # past the context window). The cut-off turn stands, so the run
            # ends as a budget stop and is still checked.
            return turn

    async def _retry_cut_off(
        self, messages: list[dict[str, Any]], specs: list[Any], limit: int
    ) -> ModelTurn:
        step_span = span_id()
        start_ns = time.time_ns()
        turn = await self.model.complete(
            messages, specs, system=self.config.system, max_tokens=limit
        )
        self.state.steps += 1
        self._account(turn)
        self.emit(StepEvent(self._llm_step(turn, step_span), start_ns=start_ns))
        return turn

    def _llm_step(self, turn: ModelTurn, span: str) -> Step:
        return Step(
            type="llm",
            name=turn.model or self.model.name,
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
                "gen_ai.request.model": turn.model or self.model.name,
                "gen_ai.response.finish_reasons": [turn.stop_reason],
                "gen_ai.usage.cache_read.input_tokens": turn.cache_read_tokens,
                "gen_ai.usage.cache_creation.input_tokens": turn.cache_write_tokens,
                "gen_ai.usage.reasoning.output_tokens": turn.reasoning_tokens,
                **(
                    {
                        "evalsi.thinking.blocks": len(turn.thinking),
                        "evalsi.thinking.text": thinking_text(turn.thinking),
                    }
                    if turn.thinking
                    else {}
                ),
                **(
                    {"evalsi.model.switched_from": turn.switched_from} if turn.switched_from else {}
                ),
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

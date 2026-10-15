"""Frontier-model behaviour of the built-in agent's model clients and loop:
thinking across tool turns, prompt caching, stop reasons and retry-after."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx
import pytest

from evalsi.targets import TargetConfig
from evalsi_harness import models
from evalsi_harness.events import FinalEvent
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import (
    AnthropicChat,
    ModelTurn,
    OpenAIChat,
    ToolCallRequest,
    ToolSpec,
    retry_after,
    to_anthropic,
    with_cache_breakpoint,
)

THINKING = [
    {"type": "thinking", "thinking": "plan", "signature": "sig-1"},
    {"type": "redacted_thinking", "data": "opaque"},
]


def anthropic(handler: Any) -> AnthropicChat:
    cfg = TargetConfig(connector="anthropic", model="claude-x", api_key_env="NOPE")
    return AnthropicChat(cfg, httpx.AsyncClient(transport=httpx.MockTransport(handler)))


def reply(payload: dict[str, Any]) -> Any:
    return lambda request: httpx.Response(200, json=payload)


def test_thinking_blocks_come_back_unchanged_before_the_tool_use() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        if len(seen) == 1:
            return httpx.Response(
                200,
                json={
                    "content": [
                        *THINKING,
                        {
                            "type": "tool_use",
                            "id": "t1",
                            "name": "bash",
                            "input": {"command": "ls"},
                        },
                    ],
                    "stop_reason": "tool_use",
                    "usage": {"input_tokens": 10, "output_tokens": 5},
                },
            )
        return httpx.Response(
            200,
            json={
                "content": [{"type": "text", "text": "done"}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 12, "output_tokens": 2},
            },
        )

    async def go() -> None:
        model = anthropic(handler)
        spec = ToolSpec("bash", "run", {"type": "object"})
        first = await model.complete([{"role": "user", "content": "go"}], [spec])
        assert first.thinking == THINKING
        messages = [
            {"role": "user", "content": "go"},
            first.as_message(),
            {"role": "tool", "tool_call_id": "t1", "content": "a.txt"},
        ]
        await model.complete(messages, [spec])

    asyncio.run(go())
    assistant = seen[1]["messages"][1]
    assert [b["type"] for b in assistant["content"]] == [
        "thinking",
        "redacted_thinking",
        "tool_use",
    ]
    assert assistant["content"][:2] == THINKING


def test_cache_tokens_are_part_of_the_prompt_and_breakpoints_are_set() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "content": [{"type": "text", "text": "ok"}],
                "stop_reason": "end_turn",
                "usage": {
                    "input_tokens": 10,
                    "output_tokens": 3,
                    "cache_read_input_tokens": 800,
                    "cache_creation_input_tokens": 100,
                },
            },
        )

    turn = asyncio.run(
        anthropic(handler).complete(
            [{"role": "user", "content": "hi"}],
            [ToolSpec("a", "", {}), ToolSpec("b", "", {})],
            system="be brief",
        )
    )
    assert turn.usage.input_tokens == 910
    assert (turn.cache_read_tokens, turn.cache_write_tokens) == (800, 100)
    body = seen[0]
    assert body["system"][0]["cache_control"] == {"type": "ephemeral"}
    assert "cache_control" not in body["tools"][0]
    assert body["tools"][-1]["cache_control"] == {"type": "ephemeral"}
    assert body["messages"][-1]["content"][-1]["cache_control"] == {"type": "ephemeral"}


def test_the_cache_breakpoint_does_not_change_the_callers_messages() -> None:
    messages = [{"role": "user", "content": [{"type": "tool_result", "content": "x"}]}]
    out = with_cache_breakpoint(messages)
    assert "cache_control" in out[-1]["content"][-1]
    assert "cache_control" not in messages[-1]["content"][-1]
    assert with_cache_breakpoint([]) == []
    assert with_cache_breakpoint([{"role": "user", "content": ""}]) == [
        {"role": "user", "content": ""}
    ]


def test_openai_drops_thinking_and_reports_cached_tokens() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}],
                "usage": {
                    "prompt_tokens": 500,
                    "completion_tokens": 4,
                    "prompt_tokens_details": {"cached_tokens": 384},
                },
            },
        )

    cfg = TargetConfig(connector="openai-compatible", model="m", base_url="http://x/v1")
    model = OpenAIChat(cfg, httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    prior = {"role": "assistant", "content": "hm", "thinking_blocks": THINKING}
    turn = asyncio.run(model.complete([{"role": "user", "content": "q"}, prior], []))
    assert "thinking_blocks" not in seen[0]["messages"][1]
    assert turn.usage.input_tokens == 500
    assert turn.cache_read_tokens == 384


def test_to_anthropic_without_thinking_is_unchanged() -> None:
    out = to_anthropic([{"role": "assistant", "content": "hi"}])
    assert out == [{"role": "assistant", "content": [{"type": "text", "text": "hi"}]}]


def test_retry_after_is_honoured_capped_and_ignored_when_malformed() -> None:
    def header(value: str) -> httpx.Response:
        return httpx.Response(429, headers={"retry-after": value})

    assert retry_after(header("7")) == 7.0
    assert retry_after(header("9999")) == models.MAX_RETRY_AFTER_S
    assert retry_after(header("-3")) == 0.0
    assert retry_after(header("Wed, 21 Oct 2026 07:28:00 GMT")) == 0.0
    assert retry_after(httpx.Response(429)) == 0.0


def test_a_rate_limited_call_waits_at_least_retry_after(monkeypatch: pytest.MonkeyPatch) -> None:
    sleeps: list[float] = []

    async def fake_sleep(seconds: float) -> None:
        sleeps.append(seconds)

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            return httpx.Response(429, headers={"retry-after": "12"})
        return httpx.Response(
            200,
            json={"content": [], "stop_reason": "end_turn", "usage": {"input_tokens": 1}},
        )

    asyncio.run(anthropic(handler).complete([{"role": "user", "content": "q"}], []))
    assert calls == 2
    assert 12 <= sleeps[0] <= 15


class Scripted:
    """A model that returns the given turns in order."""

    name = "scripted"

    def __init__(self, *turns: ModelTurn) -> None:
        self.turns = list(turns)
        self.calls = 0
        self.limits: list[int] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.limits.append(max_tokens)
        self.calls += 1
        return self.turns.pop(0)

    async def aclose(self) -> None:
        pass


def run_loop(model: Scripted, **config: Any) -> FinalEvent:
    loop = AgentLoop(model, [], LoopConfig(**config), lambda event: None)
    return asyncio.run(loop.run([{"role": "user", "content": "go"}]))


def test_a_turn_cut_off_by_the_output_limit_is_not_a_completed_run() -> None:
    cut = ModelTurn(
        text="partial",
        tool_calls=[ToolCallRequest("t1", "bash", '{"command": "ec')],
        stop_reason="max_tokens",
    )
    final = run_loop(Scripted(cut))
    assert final.stop_reason == "budget"
    assert "output limit" in final.error
    assert final.output is not None
    assert final.output.text == "partial"
    # The same for chat completions' wording.
    assert run_loop(Scripted(ModelTurn(text="x", stop_reason="length"))).stop_reason == "budget"


def test_a_refusal_is_its_own_stop_reason() -> None:
    final = run_loop(Scripted(ModelTurn(text="I can't help", stop_reason="refusal")))
    assert final.stop_reason == "refusal"


def test_pause_turn_is_resumed() -> None:
    model = Scripted(
        ModelTurn(text="working", stop_reason="pause_turn"),
        ModelTurn(text="done", stop_reason="end_turn"),
    )
    final = run_loop(model)
    assert (final.stop_reason, model.calls) == ("completed", 2)
    assert final.output is not None
    assert final.output.text == "done"


def test_cached_tokens_are_priced_and_reported() -> None:
    from evalsi.types import Usage

    turn = ModelTurn(
        text="ok",
        stop_reason="end_turn",
        usage=Usage(input_tokens=1_000_000, output_tokens=0),
        cache_read_tokens=800_000,
        cache_write_tokens=100_000,
    )
    final = run_loop(
        Scripted(turn),
        input_per_mtok=10.0,
        cache_read_mult=0.1,
        cache_write_mult=1.25,
    )
    # At $10/Mtok: 100k plain ($1.00), 800k read at a tenth ($0.80), 100k written at 1.25x ($1.25).
    assert final.usage is not None
    assert final.usage.cost_usd == pytest.approx(1.0 + 0.8 + 1.25)
    assert final.extra["cache_read_tokens"] == 800_000
    assert final.extra["cache_write_tokens"] == 100_000


def test_a_thinking_turn_replays_exactly_from_a_cassette() -> None:
    from evalsi.types import Usage
    from evalsi_harness.cassette import _turn_from_dict, _turn_to_dict

    turn = ModelTurn(
        text="ok",
        tool_calls=[ToolCallRequest("t1", "bash", '{"command": "ls"}')],
        usage=Usage(input_tokens=900, output_tokens=7),
        stop_reason="tool_use",
        thinking=THINKING,
        cache_read_tokens=800,
        cache_write_tokens=50,
    )
    saved = json.loads(json.dumps(_turn_to_dict(turn)))
    assert _turn_from_dict(saved) == turn
    # A cassette written before thinking and cache counts existed still loads.
    old = {"text": "hi", "tool_calls": [], "usage": {}, "stop_reason": "end_turn"}
    assert _turn_from_dict(old) == ModelTurn(text="hi", stop_reason="end_turn")


def test_the_breakpoint_skips_trailing_thinking_blocks() -> None:
    blocks = [{"type": "text", "text": "a"}, *THINKING]
    out = with_cache_breakpoint([{"role": "assistant", "content": blocks}])
    assert out[0]["content"][0]["cache_control"] == {"type": "ephemeral"}
    assert all("cache_control" not in b for b in out[0]["content"][1:])
    only = [{"role": "assistant", "content": list(THINKING)}]
    assert with_cache_breakpoint(only) == only

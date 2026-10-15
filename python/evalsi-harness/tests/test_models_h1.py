"""The rest of H1: the Responses API, streaming, thinking configuration,
retrying a truncated turn, fallback models and the concurrency limit."""

from __future__ import annotations

import asyncio
import json
import threading
from dataclasses import replace
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import httpx
import pytest

from evalsi.targets import TargetConfig
from evalsi.types import Usage
from evalsi_harness import modelhttp
from evalsi_harness.fallback import FallbackModel, only_own_thinking
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.modelhttp import Gate, ModelError
from evalsi_harness.models import (
    AnthropicChat,
    ModelTurn,
    OpenAIChat,
    ToolCallRequest,
    ToolSpec,
    create_model,
    thinking_text,
)
from evalsi_harness.responses import OpenAIResponses, to_responses
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then

REASONING = {
    "type": "reasoning",
    "id": "rs_1",
    "summary": [{"type": "summary_text", "text": "check the file first"}],
    "encrypted_content": "enc-1",
}


def sse(*events: tuple[str, dict[str, Any]]) -> bytes:
    return b"".join(
        f"event: {name}\ndata: {json.dumps(data)}\n\n".encode() for name, data in events
    )


def transport_client(handler: Any) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


def sse_response(body: bytes) -> httpx.Response:
    return httpx.Response(200, content=body, headers={"content-type": "text/event-stream"})


OPENAI = TargetConfig(
    connector="openai-compatible", model="m", base_url="http://x/v1", api="responses"
)
CLAUDE = TargetConfig(connector="anthropic", model="claude-x", api_key_env="NOPE")


# --- Responses API -----------------------------------------------------------------


def test_messages_become_responses_items_with_reasoning_first() -> None:
    items = to_responses(
        [
            {"role": "user", "content": "go"},
            {
                "role": "assistant",
                "content": "looking",
                "thinking_blocks": [REASONING],
                "tool_calls": [
                    {
                        "id": "c1",
                        "type": "function",
                        "function": {"name": "bash", "arguments": "{}"},
                    }
                ],
            },
            {"role": "tool", "tool_call_id": "c1", "content": "boom", "is_error": True},
        ]
    )
    assert items == [
        {"role": "user", "content": "go"},
        REASONING,
        {"role": "assistant", "content": "looking"},
        {"type": "function_call", "call_id": "c1", "name": "bash", "arguments": "{}"},
        {"type": "function_call_output", "call_id": "c1", "output": "ERROR: boom"},
    ]


def responses_payload(**over: Any) -> dict[str, Any]:
    return {
        "status": "completed",
        "output": [
            REASONING,
            {
                "type": "function_call",
                "call_id": "c9",
                "name": "bash",
                "arguments": '{"command":"ls"}',
            },
        ],
        "usage": {
            "input_tokens": 300,
            "output_tokens": 90,
            "input_tokens_details": {"cached_tokens": 256},
            "output_tokens_details": {"reasoning_tokens": 60},
        },
        **over,
    }


def test_the_responses_client_sends_a_stateless_request_and_keeps_reasoning() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        assert request.url.path == "/v1/responses"
        return httpx.Response(200, json=responses_payload())

    cfg = replace(OPENAI, effort="high")
    model = OpenAIResponses(cfg, transport_client(handler))
    turn = asyncio.run(
        model.complete(
            [{"role": "user", "content": "go"}],
            [ToolSpec("bash", "run", {"type": "object"})],
            system="be brief",
        )
    )
    body = seen[0]
    assert body["store"] is False
    assert body["include"] == ["reasoning.encrypted_content"]
    assert body["instructions"] == "be brief"
    assert body["reasoning"] == {"effort": "high"}
    assert body["tools"][0] == {
        "type": "function",
        "name": "bash",
        "description": "run",
        "parameters": {"type": "object"},
    }
    assert turn.thinking == [REASONING]
    assert [(c.id, c.name) for c in turn.tool_calls] == [("c9", "bash")]
    assert turn.stop_reason == "tool_calls"
    assert (turn.cache_read_tokens, turn.reasoning_tokens) == (256, 60)
    assert turn.usage.input_tokens == 300


@pytest.mark.parametrize(
    ("over", "stop"),
    [
        ({"status": "incomplete", "incomplete_details": {"reason": "max_output_tokens"}}, "length"),
        ({"status": "incomplete", "incomplete_details": {"reason": "content_filter"}}, "refusal"),
        (
            {"output": [{"type": "message", "content": [{"type": "refusal", "refusal": "no"}]}]},
            "refusal",
        ),
        (
            {"output": [{"type": "message", "content": [{"type": "output_text", "text": "hi"}]}]},
            "stop",
        ),
    ],
)
def test_responses_stop_reasons(over: dict[str, Any], stop: str) -> None:
    model = OpenAIResponses(
        OPENAI, transport_client(lambda r: httpx.Response(200, json=responses_payload(**over)))
    )
    assert asyncio.run(model.complete([{"role": "user", "content": "q"}], [])).stop_reason == stop


def test_a_responses_error_body_is_a_model_error() -> None:
    model = OpenAIResponses(
        OPENAI,
        transport_client(lambda r: httpx.Response(200, json={"error": {"message": "bad"}})),
    )
    with pytest.raises(ModelError, match="bad"):
        asyncio.run(model.complete([{"role": "user", "content": "q"}], []))


def test_create_model_picks_the_responses_client() -> None:
    assert isinstance(create_model(OPENAI), OpenAIResponses)
    assert isinstance(create_model(replace(OPENAI, api="")), OpenAIChat)


def test_thinking_text_reads_both_providers_blocks() -> None:
    blocks: list[dict[str, Any]] = [
        {"type": "thinking", "thinking": "a", "signature": "s"},
        {"type": "redacted_thinking", "data": "x"},
        REASONING,
    ]
    assert thinking_text(blocks) == "a\ncheck the file first"
    assert thinking_text(blocks, limit=3) == "a\nc"


# --- streaming ---------------------------------------------------------------------


def test_an_anthropic_stream_is_rebuilt_with_thinking_tool_use_and_usage() -> None:
    body = sse(
        (
            "message_start",
            {"message": {"usage": {"input_tokens": 12, "cache_read_input_tokens": 5}}},
        ),
        (
            "content_block_start",
            {"index": 0, "content_block": {"type": "thinking", "thinking": ""}},
        ),
        (
            "content_block_delta",
            {"index": 0, "delta": {"type": "thinking_delta", "thinking": "hm"}},
        ),
        ("content_block_delta", {"index": 0, "delta": {"type": "thinking_delta", "thinking": "m"}}),
        (
            "content_block_delta",
            {"index": 0, "delta": {"type": "signature_delta", "signature": "sig"}},
        ),
        ("content_block_stop", {"index": 0}),
        ("content_block_start", {"index": 1, "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"index": 1, "delta": {"type": "text_delta", "text": "ok"}}),
        (
            "content_block_start",
            {
                "index": 2,
                "content_block": {"type": "tool_use", "id": "t1", "name": "bash", "input": {}},
            },
        ),
        (
            "content_block_delta",
            {"index": 2, "delta": {"type": "input_json_delta", "partial_json": '{"comm'}},
        ),
        (
            "content_block_delta",
            {"index": 2, "delta": {"type": "input_json_delta", "partial_json": 'and":"ls"}'}},
        ),
        ("message_delta", {"delta": {"stop_reason": "tool_use"}, "usage": {"output_tokens": 33}}),
        ("message_stop", {}),
    )
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return sse_response(body)

    model = AnthropicChat(replace(CLAUDE, stream=True), transport_client(handler))
    turn = asyncio.run(model.complete([{"role": "user", "content": "q"}], []))
    assert seen[0]["stream"] is True
    assert turn.text == "ok"
    assert turn.thinking == [{"type": "thinking", "thinking": "hmm", "signature": "sig"}]
    assert [(c.name, json.loads(c.arguments)) for c in turn.tool_calls] == [
        ("bash", {"command": "ls"})
    ]
    assert turn.stop_reason == "tool_use"
    assert turn.usage.output_tokens == 33
    assert turn.usage.input_tokens == 17  # 12 plus 5 read from the cache
    assert turn.cache_read_tokens == 5


def test_a_chat_stream_is_rebuilt_with_split_tool_calls_and_usage() -> None:
    def chunk(
        delta: dict[str, Any], finish: str | None = None, **extra: Any
    ) -> tuple[str, dict[str, Any]]:
        return "", {"choices": [{"delta": delta, "finish_reason": finish}], **extra}

    body = (
        sse(
            chunk({"content": "he"}),
            chunk({"content": "llo"}),
            chunk(
                {
                    "tool_calls": [
                        {"index": 0, "id": "c1", "function": {"name": "ba", "arguments": '{"a"'}}
                    ]
                }
            ),
            chunk(
                {"tool_calls": [{"index": 0, "function": {"name": "sh", "arguments": ":1}"}}]},
                "tool_calls",
            ),
            ("", {"choices": [], "usage": {"prompt_tokens": 40, "completion_tokens": 8}}),
        )
        + b"data: [DONE]\n\n"
    )
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return sse_response(body)

    cfg = TargetConfig(
        connector="openai-compatible", model="m", base_url="http://x/v1", stream=True
    )
    turn = asyncio.run(
        OpenAIChat(cfg, transport_client(handler)).complete([{"role": "user", "content": "q"}], [])
    )
    assert seen[0]["stream"] is True
    assert seen[0]["stream_options"] == {"include_usage": True}
    assert turn.text == "hello"
    assert [(c.id, c.name, c.arguments) for c in turn.tool_calls] == [("c1", "bash", '{"a":1}')]
    assert (turn.usage.input_tokens, turn.usage.output_tokens) == (40, 8)
    assert turn.stop_reason == "tool_calls"


def test_a_responses_stream_ends_with_the_whole_response() -> None:
    body = sse(
        ("response.created", {"response": {"status": "in_progress"}}),
        ("response.output_text.delta", {"delta": "x"}),
        ("response.completed", {"response": responses_payload()}),
    )
    model = OpenAIResponses(
        replace(OPENAI, stream=True), transport_client(lambda r: sse_response(body))
    )
    turn = asyncio.run(model.complete([{"role": "user", "content": "q"}], []))
    assert turn.thinking == [REASONING]
    assert turn.reasoning_tokens == 60


def test_a_stream_error_event_is_a_model_error() -> None:
    body = sse(
        ("error", {"type": "error", "error": {"type": "overloaded_error", "message": "busy"}})
    )
    model = AnthropicChat(
        replace(CLAUDE, stream=True), transport_client(lambda r: sse_response(body))
    )
    with pytest.raises(ModelError, match="overloaded_error"):
        asyncio.run(model.complete([{"role": "user", "content": "q"}], []))


def test_opening_a_stream_is_retried_but_a_broken_stream_is_not(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    sleeps: list[float] = []

    async def fake_sleep(seconds: float) -> None:
        sleeps.append(seconds)

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)
    attempts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        if attempts == 1:
            return httpx.Response(529, headers={"retry-after": "5"})
        return sse_response(
            sse(
                ("message_start", {"message": {"usage": {"input_tokens": 1}}}),
                (
                    "message_delta",
                    {"delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 1}},
                ),
            )
        )

    model = AnthropicChat(replace(CLAUDE, stream=True), transport_client(handler))
    turn = asyncio.run(model.complete([{"role": "user", "content": "q"}], []))
    assert attempts == 2
    assert turn.stop_reason == "end_turn"
    assert 5 <= sleeps[0] <= 7

    # A transport failure after events arrived must not be retried: it would
    # repeat the output.
    class Broken(httpx.AsyncByteStream):
        async def __aiter__(self) -> Any:
            yield sse(("message_start", {"message": {"usage": {}}}))
            raise httpx.ReadError("connection reset")

    calls = 0

    def broken(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(200, stream=Broken(), headers={"content-type": "text/event-stream"})

    model = AnthropicChat(replace(CLAUDE, stream=True), transport_client(broken))
    with pytest.raises(ModelError, match="connection reset"):
        asyncio.run(model.complete([{"role": "user", "content": "q"}], []))
    assert calls == 1


# --- thinking configuration --------------------------------------------------------


def capture_anthropic(config: TargetConfig, **kwargs: Any) -> dict[str, Any]:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(200, json={"content": [], "stop_reason": "end_turn", "usage": {}})

    asyncio.run(
        AnthropicChat(config, transport_client(handler)).complete(
            [{"role": "user", "content": "q"}], [], **kwargs
        )
    )
    return seen[0]


def test_thinking_modes_reach_the_request_and_temperature_is_dropped() -> None:
    body = capture_anthropic(replace(CLAUDE, thinking="adaptive", temperature=0.2))
    assert body["thinking"] == {"type": "adaptive"}
    assert "temperature" not in body
    body = capture_anthropic(
        replace(CLAUDE, thinking="enabled", thinking_budget=4000, temperature=0.2)
    )
    assert body["thinking"] == {"type": "enabled", "budget_tokens": 4000}
    assert "temperature" not in body
    body = capture_anthropic(replace(CLAUDE, thinking="disabled", temperature=0.2))
    assert body["thinking"] == {"type": "disabled"}
    assert body["temperature"] == 0.2
    assert "thinking" not in capture_anthropic(CLAUDE)


def test_a_raised_limit_stays_above_the_thinking_budget() -> None:
    cfg = replace(CLAUDE, thinking="enabled", thinking_budget=9000, max_tokens=10000)
    assert capture_anthropic(cfg, max_tokens=20000)["max_tokens"] == 20000
    assert capture_anthropic(cfg, max_tokens=5000)["max_tokens"] == 9001


def test_target_options_are_validated() -> None:
    bad: list[dict[str, Any]] = [
        {"thinking": "deep"},
        {"thinking": "adaptive", "connector": "openai-compatible", "base_url": "http://x"},
        {"thinking": "enabled", "thinking_budget": 0},
        {"thinking": "enabled", "thinking_budget": 20000, "max_tokens": 1000},
        {"api": "responses"},
        {"api": "completions", "connector": "openai-compatible", "base_url": "http://x"},
        {"max_concurrency": -1},
    ]
    for fields in bad:
        with pytest.raises(ValueError, match=r"thinking|api|max_concurrency"):
            TargetConfig(**{"connector": "anthropic", "model": "m", **fields})
    described = CLAUDE.describe()
    assert "thinking" not in described
    assert replace(CLAUDE, thinking="adaptive").describe()["thinking"] == "adaptive"


# --- retrying a truncated turn -----------------------------------------------------


class Scripted:
    name = "scripted"

    def __init__(self, *turns: ModelTurn) -> None:
        self.turns = list(turns)
        self.limits: list[int] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.limits.append(max_tokens)
        return self.turns.pop(0)

    async def aclose(self) -> None:
        pass


def cut() -> ModelTurn:
    return ModelTurn(
        text="par",
        tool_calls=[ToolCallRequest("t", "bash", '{"command": "ec')],
        stop_reason="max_tokens",
        usage=Usage(input_tokens=10, output_tokens=100),
    )


def run_loop(model: Scripted, **config: Any) -> Any:
    steps: list[Any] = []
    loop = AgentLoop(model, [], LoopConfig(**config), steps.append)
    final = asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    return final, steps


def test_a_truncated_turn_is_retried_once_with_a_higher_limit() -> None:
    model = Scripted(cut(), ModelTurn(text="done", stop_reason="end_turn", usage=Usage(10, 5)))
    final, steps = run_loop(model, truncation_retry_tokens=32000)
    assert model.limits == [0, 32000]
    assert final.stop_reason == "completed"
    assert final.extra["truncation_retries"] == 1
    assert final.extra["steps"] == 2  # the cut-off call is a step, and is billed
    assert final.usage.output_tokens == 105
    assert len(steps) == 2


def test_a_second_cut_ends_the_run_as_budget_and_zero_disables_the_retry() -> None:
    model = Scripted(cut(), cut())
    final, _ = run_loop(model, truncation_retry_tokens=32000)
    assert model.limits == [0, 32000]
    assert final.stop_reason == "budget"
    model = Scripted(cut())
    final, _ = run_loop(model)
    assert model.limits == [0]
    assert final.stop_reason == "budget"


class RefusesHigherLimits(Scripted):
    """The provider accepts the first call and refuses the raised limit."""

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        if max_tokens:
            raise ModelError("HTTP 400: max_tokens is too large for this model")
        return await super().complete(messages, tools, system=system, max_tokens=max_tokens)


def test_a_provider_that_refuses_the_higher_limit_leaves_a_budget_stop_not_an_error() -> None:
    model = RefusesHigherLimits(cut())
    final, steps = run_loop(model, truncation_retry_tokens=32000)
    assert final.stop_reason == "budget"
    assert final.error == "the model hit its output limit"
    assert final.extra["steps"] == 1
    assert len(steps) == 1


def test_the_retry_does_not_outrun_the_step_budget() -> None:
    model = Scripted(cut(), ModelTurn(text="never", stop_reason="end_turn"))
    final, _ = run_loop(model, truncation_retry_tokens=32000, max_steps=1)
    assert model.limits == [0]
    assert final.stop_reason == "budget"


def test_reasoning_tokens_and_thinking_text_reach_the_step() -> None:
    turn = ModelTurn(
        text="ok",
        stop_reason="end_turn",
        thinking=[{"type": "thinking", "thinking": "plan it", "signature": "s"}],
        reasoning_tokens=40,
        usage=Usage(10, 50),
    )
    final, steps = run_loop(Scripted(turn))
    attrs = steps[0].step.attributes
    assert attrs["evalsi.thinking.text"] == "plan it"
    assert attrs["evalsi.thinking.blocks"] == 1
    assert attrs["gen_ai.usage.reasoning.output_tokens"] == 40
    assert final.extra["reasoning_tokens"] == 40


# --- fallback models ---------------------------------------------------------------


class Flaky:
    def __init__(self, name: str, *, fails: int = 0, turn: ModelTurn | None = None) -> None:
        self.name = name
        self.fails = fails
        self.turn = turn or ModelTurn(text=f"from {name}", stop_reason="end_turn")
        self.seen: list[Any] = []
        self.closed = False

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.seen.append(messages)
        if self.fails:
            self.fails -= 1
            raise ModelError(f"{self.name} down")
        return replace(self.turn)

    async def aclose(self) -> None:
        self.closed = True


def test_a_failing_model_is_replaced_after_n_failures_and_the_switch_is_recorded() -> None:
    a, b = Flaky("a", fails=99), Flaky("b")
    chain = FallbackModel([a, b], after=2)
    turn = asyncio.run(chain.complete([{"role": "user", "content": "q"}], []))
    assert len(a.seen) == 2
    assert (turn.model, turn.switched_from, turn.text) == ("b", "a", "from b")
    assert chain.name == "b"
    # Sticky: the next call goes straight to b and is not a switch.
    turn = asyncio.run(chain.complete([{"role": "user", "content": "q"}], []))
    assert len(a.seen) == 2
    assert (turn.model, turn.switched_from) == ("b", "")


def test_a_raised_limit_that_is_refused_never_causes_a_switch() -> None:
    class RefusesRaised(Flaky):
        async def complete(
            self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
        ) -> ModelTurn:
            self.seen.append(messages)
            if max_tokens:
                raise ModelError("HTTP 400: too large")
            return replace(self.turn)

    a, b = RefusesRaised("a"), Flaky("b")
    chain = FallbackModel([a, b], after=1)
    with pytest.raises(ModelError, match="too large"):
        asyncio.run(chain.complete([], [], max_tokens=32000))
    assert (chain.current, len(a.seen), len(b.seen)) == (0, 1, 0)
    # An ordinary call still works on the same model.
    assert asyncio.run(chain.complete([], [])).model == "a"


def test_one_failure_below_the_limit_stays_on_the_model() -> None:
    a, b = Flaky("a", fails=1), Flaky("b")
    turn = asyncio.run(FallbackModel([a, b], after=2).complete([], []))
    assert (turn.model, turn.switched_from, len(b.seen)) == ("a", "", 0)


def test_when_every_model_fails_the_error_says_so_and_all_are_closed() -> None:
    a, b = Flaky("a", fails=99), Flaky("b", fails=99)
    chain = FallbackModel([a, b], after=1)
    with pytest.raises(ModelError, match=r"every model in the fallback chain failed.*b down"):
        asyncio.run(chain.complete([], []))
    asyncio.run(chain.aclose())
    assert a.closed
    assert b.closed


def test_reasoning_from_another_model_is_not_sent_on() -> None:
    mine = {"role": "assistant", "content": "x", "thinking_blocks": [REASONING], "thinking_by": "b"}
    theirs = {**mine, "thinking_by": "a"}
    untagged = {"role": "assistant", "content": "y", "thinking_blocks": [REASONING]}
    out = only_own_thinking([mine, theirs, untagged], "b")
    assert out[0] == mine
    assert "thinking_blocks" not in out[1]
    assert "thinking_by" not in out[1]
    assert out[2] == untagged
    assert "thinking_blocks" in theirs  # the caller's message is untouched
    a, b = Flaky("a", fails=99), Flaky("b")
    asyncio.run(FallbackModel([a, b], after=1).complete([theirs], []))
    assert "thinking_blocks" not in b.seen[0][0]


def test_turns_tag_their_thinking_with_the_model_that_made_it() -> None:
    turn = ModelTurn(text="x", thinking=[REASONING], model="b")
    assert turn.as_message()["thinking_by"] == "b"
    assert "thinking_by" not in ModelTurn(text="x", thinking=[REASONING]).as_message()


def test_create_model_builds_a_chain_from_fallbacks() -> None:
    cfg = replace(CLAUDE, fallbacks=(OPENAI,), fallback_after=3)
    chain = create_model(cfg)
    assert isinstance(chain, FallbackModel)
    assert [type(m) for m in chain.models] == [AnthropicChat, OpenAIResponses]
    assert chain.after == 3


class Refuser(BaseHTTPRequestHandler):
    def log_message(self, *args: Any) -> None:
        pass

    def do_POST(self) -> None:
        self.rfile.read(int(self.headers["Content-Length"]))
        data = b'{"error": "no"}'
        self.send_response(400)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def test_a_run_switches_to_its_fallback_and_the_step_says_so(
    model: Any, sandboxes: LocalSandboxClient
) -> None:
    from tests.harness_support import run

    bad = ThreadingHTTPServer(("127.0.0.1", 0), Refuser)
    threading.Thread(target=bad.serve_forever, daemon=True).start()
    try:
        good = model(calls_then([{"text": "all fine"}]))
        spec = spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "primary",
                    "base_url": f"http://127.0.0.1:{bad.server_address[1]}/v1",
                    "fallback_after": 1,
                    "fallbacks": [
                        {
                            "connector": "openai-compatible",
                            "model": "backup",
                            "base_url": good.base_url,
                        }
                    ],
                },
                "harness": {"builtin": {}},
                "environment": {"checker": CHECK_HELLO},
            }
        )
        outcome = run(spec, record(), sandboxes)
    finally:
        bad.shutdown()
        bad.server_close()
    assert outcome.error == ""
    assert outcome.record is not None
    assert outcome.record.metadata["agent"]["stop_reason"] == "completed"
    assert outcome.record.trajectory is not None
    llm = [s for s in outcome.record.trajectory.steps if s.type == "llm"]
    assert llm[0].name == "backup"
    assert llm[0].attributes["evalsi.model.switched_from"] == "primary"


# --- concurrency limit -------------------------------------------------------------


def test_the_gate_caps_concurrent_calls_per_endpoint() -> None:
    running = peak = 0

    async def go() -> None:
        nonlocal running, peak
        gate = Gate("endpoint-a", 2)
        sibling = Gate("endpoint-a", 2)  # another client for the same endpoint shares the limit

        async def call(g: Gate) -> None:
            nonlocal running, peak
            async with g:
                running += 1
                peak = max(peak, running)
                await asyncio.sleep(0.01)
                running -= 1

        await asyncio.gather(*(call(gate if i % 2 else sibling) for i in range(8)))

    asyncio.run(go())
    assert peak == 2


def test_the_gate_survives_successive_event_loops() -> None:
    async def use() -> None:
        async with Gate("shared-endpoint", 1):
            await asyncio.sleep(0)

    for _ in range(3):
        asyncio.run(use())


def test_a_finished_stream_is_closed_at_once() -> None:
    closed = False

    async def events() -> Any:
        nonlocal closed
        try:
            yield "response.completed", {"response": {"output": []}}
            yield "response.other", {}
        finally:
            closed = True

    from evalsi_harness.streaming import collect_responses

    asyncio.run(collect_responses(events()))
    assert closed


def test_no_limit_means_no_gate() -> None:
    assert not isinstance(modelhttp.gate_for("u", "k", 0), Gate)
    assert isinstance(modelhttp.gate_for("u", "k", 3), Gate)


# --- cassette ----------------------------------------------------------------------


def test_a_cassette_carries_its_version_and_old_ones_replay(tmp_path: Any) -> None:
    from evalsi_harness.cassette import Cassette

    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record")
    tape.record(
        {
            "kind": "model",
            "turn": {"text": "hi", "tool_calls": [], "usage": {}, "stop_reason": "end_turn"},
        }
    )
    tape.save()
    lines = [json.loads(x) for x in path.read_text().splitlines()]
    assert lines[0] == {"kind": "header", "version": 2, "context": {"strategy": "none"}}
    assert Cassette(path, "replay").next_turn() is not None
    # A version 1 file has no header.
    path.write_text("\n".join(json.dumps(x) for x in lines[1:]) + "\n")
    assert Cassette(path, "replay").next_turn() is not None


def test_cache_prices_apply_only_when_the_whole_chain_is_anthropic() -> None:
    from evalsi_harness.harness import BuiltinHarness

    class Live:
        def __init__(self, target: dict[str, Any]) -> None:
            self.task = type("T", (), {"spec": spec_of({"target": target})})()

    anthropic = {"connector": "anthropic", "model": "c"}
    other = {"connector": "openai-compatible", "model": "m", "base_url": "http://x/v1"}
    price = BuiltinHarness._cache_prices
    assert price(Live(anthropic)) == {"cache_read_mult": 0.1, "cache_write_mult": 1.25}  # type: ignore[arg-type]
    assert price(Live({**anthropic, "fallbacks": [anthropic]})) != {}  # type: ignore[arg-type]
    assert price(Live({**anthropic, "fallbacks": [other]})) == {}  # type: ignore[arg-type]
    assert price(Live(other)) == {}  # type: ignore[arg-type]

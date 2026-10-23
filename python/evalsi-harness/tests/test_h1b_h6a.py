"""H1-b (reasoning handoff, role models, server tools and pause_turn, stream
resume) and H6-a (SGLang token fields, the token_trace spec field)."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx
import pytest

from evalsi.run import target_config
from evalsi.targets import TargetConfig
from evalsi.types import Usage
from evalsi.v1alpha1 import run_pb2
from evalsi_harness.cassette import _turn_from_dict, _turn_to_dict
from evalsi_harness.context import ContextConfig
from evalsi_harness.events import StepEvent
from evalsi_harness.fallback import FallbackModel, hand_off
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.modelhttp import ModelError
from evalsi_harness.models import (
    AnthropicChat,
    ModelTurn,
    NamedModel,
    OpenAIChat,
    ToolCallRequest,
    ToolSpec,
    to_anthropic,
)
from evalsi_harness.streaming import StreamBroken
from tests.test_models_h1 import sse, sse_response, transport_client

TOOLS = [ToolSpec("bash", "run", {"type": "object", "properties": {}})]
CLAUDE = TargetConfig(connector="anthropic", model="claude-x", api_key_env="NOPE")
USER = [{"role": "user", "content": "go"}]


def run(coro: Any) -> Any:
    return asyncio.run(coro)


# --- 1. reasoning handoff ------------------------------------------------------------


THINKING = [
    {"type": "thinking", "thinking": "plan: read the file", "signature": "sig"},
    {"type": "redacted_thinking", "data": "xx"},
]
REASONING = {
    "type": "reasoning",
    "id": "rs_1",
    "summary": [{"type": "summary_text", "text": "check the file first"}],
    "encrypted_content": "enc",
}


def assistant(by: str, blocks: list[dict[str, Any]]) -> dict[str, Any]:
    return {"role": "assistant", "content": "ok", "thinking_blocks": blocks, "thinking_by": by}


def test_drop_removes_another_models_blocks_and_records_it() -> None:
    out, record = hand_off([*USER, assistant("a", THINKING)], "b", "drop")
    assert "thinking_blocks" not in out[1]
    assert out[1]["content"] == "ok"
    assert record["rule"] == "drop"
    assert record["blocks_dropped"] == 2
    assert record["text_chars"] == 0


def test_summarize_as_text_carries_the_readable_part_and_drops_the_signed_blocks() -> None:
    out, record = hand_off([*USER, assistant("claude", THINKING)], "gpt", "summarize-as-text")
    assert "thinking_blocks" not in out[1]
    assert "plan: read the file" in out[1]["content"]
    assert out[1]["content"].endswith("ok")
    assert "sig" not in json.dumps(out)
    assert record["text_chars"] == len("plan: read the file")
    assert record["from"] == ["claude"]
    # And a Responses reasoning summary goes the other way.
    out, _ = hand_off([*USER, assistant("gpt", [REASONING])], "claude", "summarize-as-text")
    assert "check the file first" in out[1]["content"]
    assert "enc" not in json.dumps(out)


def test_own_blocks_and_untagged_messages_are_untouched_and_nothing_is_recorded() -> None:
    messages = [*USER, assistant("b", THINKING)]
    out, record = hand_off(messages, "b", "summarize-as-text")
    assert out == messages
    assert record == {}


def test_a_bad_rule_is_refused() -> None:
    with pytest.raises(ValueError, match="reasoning_handoff"):
        hand_off(USER, "b", "keep")
    with pytest.raises(ValueError, match="reasoning_handoff"):
        TargetConfig(connector="anthropic", model="m", reasoning_handoff="keep")


class Fake:
    """A model that serves scripted outcomes (a turn, or an error to raise)."""

    def __init__(self, name: str, *outcomes: ModelTurn | ModelError) -> None:
        self.name = name
        self.outcomes = list(outcomes)
        self.seen: list[list[dict[str, Any]]] = []
        self.systems: list[str] = []

    async def complete(
        self,
        messages: list[dict[str, Any]],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        self.seen.append(messages)
        self.systems.append(system)
        outcome = self.outcomes.pop(0) if len(self.outcomes) > 1 else self.outcomes[0]
        if isinstance(outcome, ModelError):
            raise outcome
        return outcome

    async def aclose(self) -> None:
        pass


def test_a_fallback_applies_the_rule_and_the_turn_records_the_conversion() -> None:
    first = Fake("claude-x", ModelError("down"))
    second = Fake("gpt-y", ModelTurn(text="done"))
    chain = FallbackModel([first, second], after=1, handoff="summarize-as-text")
    turn = run(chain.complete([*USER, assistant("claude-x", THINKING)], TOOLS))
    assert turn.switched_from == "claude-x"
    assert turn.handoff["rule"] == "summarize-as-text"
    assert turn.handoff["to"] == "gpt-y"
    assert "plan: read the file" in second.seen[0][1]["content"]
    # The first model, tried with its own blocks, got them unchanged.
    assert first.seen[0][1]["thinking_blocks"] == THINKING
    # The conversion survives the cassette, so a replay serves it as recorded.
    again = _turn_from_dict(json.loads(json.dumps(_turn_to_dict(turn))))
    assert again.handoff == turn.handoff
    assert again.switched_from == "claude-x"


# --- 2. models for compaction and sub-agents ------------------------------------------


def llm_turn(text: str, tokens: tuple[int, int], calls: list[ToolCallRequest] | None = None) -> Any:
    return ModelTurn(
        text=text,
        tool_calls=calls or [],
        usage=Usage(input_tokens=tokens[0], output_tokens=tokens[1]),
        stop_reason="tool_calls" if calls else "stop",
    )


def steps_of(events: list[Any]) -> list[Any]:
    return [e.step for e in events if isinstance(e, StepEvent)]


def test_compaction_runs_on_its_own_model_named_on_the_step_and_priced_by_it() -> None:
    from evalsi_harness.tools import FunctionTool, ToolResult

    async def noisy() -> ToolResult:
        return ToolResult("x" * 20000)

    tool = FunctionTool("noisy", noisy, description="n")
    call = [ToolCallRequest("c1", "noisy", "{}")]
    main = Fake(
        "big",
        llm_turn("", (1_000_000, 0), call),
        llm_turn("", (1_000_000, 0), call),
        llm_turn("", (1_000_000, 0), call),
        llm_turn("", (1_000_000, 0), call),
        llm_turn("done", (1_000_000, 1_000_000)),
    )
    small = Fake("small", llm_turn("the summary", (1_000_000, 0)))
    events: list[Any] = []
    config = LoopConfig(
        input_per_mtok=10.0,
        output_per_mtok=10.0,
        model_prices={"small": (1.0, 1.0)},
        context=ContextConfig(
            strategy="compact",
            window_tokens=4000,
            keep_recent=0,
            keep_turns=1,
            trigger=0.5,
            target=0.2,
        ),
    )
    loop = AgentLoop(
        main,
        [tool],
        config,
        events.append,
        compaction_model=NamedModel(small),
    )
    final = run(loop.run(USER))
    compact = [s for s in steps_of(events) if s.name == "context:compact"]
    assert small.seen, "the summary was written by the compaction model"
    assert compact
    assert '"summary_model": "small"' in compact[0].output.text
    # Spend is counted per model: each summary's million tokens at 1.0, not 10.0.
    summaries = len(small.seen)
    assert final.extra["models"]["small"]["input_tokens"] == summaries * 1_000_000
    assert final.usage is not None
    main_cost = (5_000_000 + 1_000_000) * 10.0 / 1e6
    assert final.usage.cost_usd == pytest.approx(summaries * 1.0 + main_cost)


def test_a_subagent_runs_on_its_own_model_and_is_named_and_billed_under_it() -> None:
    delegate = [ToolCallRequest("d1", "delegate", json.dumps({"task": "look around"}))]
    main = Fake("main", llm_turn("", (100, 10), delegate), llm_turn("all done", (100, 10)))
    helper = Fake("helper", llm_turn("found it", (1_000_000, 0)))
    events: list[Any] = []
    config = LoopConfig(
        delegate_steps=3, input_per_mtok=10.0, output_per_mtok=10.0, model_prices={"helper": (1, 1)}
    )
    loop = AgentLoop(main, [], config, events.append, subagent_model=NamedModel(helper))
    final = run(loop.run(USER))
    assert helper.seen
    assert len(main.seen) == 2
    sub = [s for s in steps_of(events) if s.name == "subagent"]
    assert sub[0].attributes["evalsi.subagent.model"] == "helper"
    llm = [s for s in steps_of(events) if s.type == "llm" and s.name == "helper"]
    assert llm, "the sub-agent's step names its model"
    assert final.extra["models"]["helper"]["input_tokens"] == 1_000_000
    assert final.usage is not None
    assert final.usage.cost_usd == pytest.approx(1.0 + (200 * 10.0 + 20 * 10.0) / 1e6)


# --- 3. server tools and pause_turn -----------------------------------------------------

SEARCH = {"type": "web_search_20260209", "name": "web_search", "max_uses": 3}
SERVER_USE = {
    "type": "server_tool_use",
    "id": "srv1",
    "name": "web_search",
    "input": {"query": "evals"},
}


def test_server_tools_are_passed_through_and_a_paused_turn_continues_verbatim() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        seen.append(body)
        if len(seen) == 1:
            return httpx.Response(
                200,
                json={
                    "model": "claude-x-20260101",
                    "content": [{"type": "text", "text": "searching "}, SERVER_USE],
                    "stop_reason": "pause_turn",
                    "usage": {"input_tokens": 5, "output_tokens": 2},
                },
            )
        return httpx.Response(
            200,
            json={
                "model": "claude-x-20260101",
                "content": [{"type": "text", "text": "the answer"}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 9, "output_tokens": 3},
            },
        )

    config = TargetConfig(
        connector="anthropic", model="claude-x", api_key_env="NOPE", server_tools=(SEARCH,)
    )
    model = AnthropicChat(config, transport_client(handler))
    events: list[Any] = []
    loop = AgentLoop(model, [], LoopConfig(), events.append)
    final = run(loop.run(USER))
    assert final.stop_reason == "completed"
    assert final.output is not None
    assert final.output.as_text() == "the answer"
    # The definition goes out as given, even with no client tools.
    assert seen[0]["tools"][0]["type"] == "web_search_20260209"
    assert seen[0]["tools"][0]["max_uses"] == 3
    # The continuation sends the paused content as it was, with no "Continue." message.
    assert seen[1]["messages"][-1]["role"] == "assistant"
    assert {
        k: v for k, v in seen[1]["messages"][-1]["content"][1].items() if k != "cache_control"
    } == SERVER_USE
    assert [m["role"] for m in seen[1]["messages"]] == ["user", "assistant"]
    # The turn that was paused is a step, and names the model the provider served.
    llm = [s for s in steps_of(events) if s.type == "llm"]
    assert llm[0].attributes["gen_ai.response.model"] == "claude-x-20260101"
    assert llm[0].attributes["evalsi.server_tools.blocks"] == 1


def test_a_pause_that_never_ends_stops_at_the_continuation_limit() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "content": [SERVER_USE],
                "stop_reason": "pause_turn",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            },
        )

    model = AnthropicChat(CLAUDE, transport_client(handler))
    final = run(AgentLoop(model, [], LoopConfig(max_pause_continues=2), lambda e: None).run(USER))
    assert final.stop_reason == "budget"
    assert "still paused" in final.error


def test_server_tools_need_an_anthropic_target_and_a_type() -> None:
    with pytest.raises(ValueError, match="anthropic"):
        TargetConfig(
            connector="openai-compatible", model="m", base_url="http://x", server_tools=(SEARCH,)
        )
    with pytest.raises(ValueError, match="type and a name"):
        TargetConfig(connector="anthropic", model="m", server_tools=({"name": "x"},))


def test_server_blocks_never_reach_another_provider() -> None:
    turn = ModelTurn(text="t", raw_content=[SERVER_USE], model="claude-x")
    message = turn.as_message()
    assert to_anthropic([message])[0]["content"] == [SERVER_USE]
    out, record = hand_off([message], "gpt", "drop")
    assert "anthropic_content" not in out[0]
    assert record["server_blocks_dropped"] == 1


# --- 4. resuming a broken stream ---------------------------------------------------------


def anthropic_stream(*, cut: bool, text: str = "hello ", tool: bool = False) -> bytes:
    events: list[tuple[str, dict[str, Any]]] = [
        ("message_start", {"message": {"model": "claude-x-1", "usage": {"input_tokens": 4}}}),
        ("content_block_start", {"index": 0, "content_block": {"type": "text", "text": ""}}),
        (
            "content_block_delta",
            {"index": 0, "delta": {"type": "text_delta", "text": text}},
        ),
        ("content_block_stop", {"index": 0}),
    ]
    if tool:
        events += [
            (
                "content_block_start",
                {"index": 1, "content_block": {"type": "tool_use", "id": "t1", "name": "bash"}},
            ),
            (
                "content_block_delta",
                {"index": 1, "delta": {"type": "input_json_delta", "partial_json": '{"comm'}},
            ),
        ]
    if not cut:
        events.append(
            (
                "message_delta",
                {"delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 7}},
            )
        )
    return sse(*events)


def test_a_broken_anthropic_stream_is_tried_again_and_the_resume_is_recorded() -> None:
    bodies: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        return sse_response(anthropic_stream(cut=len(bodies) == 1))

    config = TargetConfig(
        connector="anthropic",
        model="claude-x",
        api_key_env="NOPE",
        stream=True,
        stream_resume_attempts=2,
    )
    turn = run(AnthropicChat(config, transport_client(handler)).complete(USER, []))
    assert len(bodies) == 2
    assert bodies[1]["messages"] == bodies[0]["messages"]  # a plain retry
    assert turn.text == "hello "
    assert turn.stream_resumes[0]["mode"] == "retry"
    assert turn.stream_resumes[0]["partial_chars"] == 6
    assert turn.response_model == "claude-x-1"


def test_prefill_mode_continues_from_the_partial_text() -> None:
    bodies: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        if len(bodies) == 1:
            return sse_response(anthropic_stream(cut=True, text="hello wor "))
        return sse_response(anthropic_stream(cut=False, text="ld"))

    config = TargetConfig(
        connector="anthropic",
        model="claude-x",
        api_key_env="NOPE",
        stream=True,
        stream_resume_attempts=1,
        stream_resume_mode="prefill",
    )
    turn = run(AnthropicChat(config, transport_client(handler)).complete(USER, []))
    # Whitespace at the end of a prefill is refused by the API, so it is trimmed.
    assert bodies[1]["messages"][-1] == {"role": "assistant", "content": "hello wor"}
    assert turn.text == "hello world"
    assert turn.stream_resumes[0]["mode"] == "prefill"


def test_a_tool_call_cut_mid_arguments_is_discarded_not_merged() -> None:
    bodies = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal bodies
        bodies += 1
        if bodies == 1:
            return sse_response(anthropic_stream(cut=True, tool=True))
        return sse_response(anthropic_stream(cut=False))

    config = TargetConfig(
        connector="anthropic",
        model="claude-x",
        api_key_env="NOPE",
        stream=True,
        stream_resume_attempts=1,
        stream_resume_mode="prefill",
    )
    turn = run(AnthropicChat(config, transport_client(handler)).complete(USER, TOOLS))
    assert turn.tool_calls == []
    record = turn.stream_resumes[0]
    assert record["tool_calls_cut"] == 1
    assert record["tool_calls_discarded"] == 1


def test_without_attempts_or_when_they_run_out_a_broken_stream_is_an_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return sse_response(anthropic_stream(cut=True, tool=True))

    config = TargetConfig(connector="anthropic", model="claude-x", api_key_env="NOPE", stream=True)
    with pytest.raises(StreamBroken) as info:
        run(AnthropicChat(config, transport_client(handler)).complete(USER, TOOLS))
    assert info.value.partial["tool_calls_cut"] == 1
    bounded = TargetConfig(
        connector="anthropic",
        model="claude-x",
        api_key_env="NOPE",
        stream=True,
        stream_resume_attempts=2,
    )
    with pytest.raises(ModelError) as again:
        run(AnthropicChat(bounded, transport_client(handler)).complete(USER, TOOLS))
    assert len(again.value.partial["resumes"]) == 2  # type: ignore[attr-defined]


def chat_chunk(**delta: Any) -> tuple[str, dict[str, Any]]:
    return "", {"model": "m-1", "choices": [{"index": 0, "delta": delta}]}


def test_an_openai_stream_cut_inside_a_tool_call_is_retried() -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        call = {"index": 0, "id": "c1", "function": {"name": "bash", "arguments": '{"command"'}}
        if calls == 1:
            return sse_response(sse(chat_chunk(content="part"), chat_chunk(tool_calls=[call])))
        done = {"index": 0, "id": "c1", "function": {"name": "bash", "arguments": '{"a": 1}'}}
        return sse_response(
            sse(
                chat_chunk(tool_calls=[done]),
                ("", {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]}),
            )
        )

    config = TargetConfig(
        connector="openai-compatible",
        model="m",
        base_url="http://x/v1",
        stream=True,
        stream_resume_attempts=1,
    )
    turn = run(OpenAIChat(config, transport_client(handler)).complete(USER, TOOLS))
    assert [c.arguments for c in turn.tool_calls] == ['{"a": 1}']
    assert turn.stream_resumes[0]["tool_calls_cut"] == 1
    assert turn.text == ""  # the partial text was not merged either (a retry, not a prefill)
    assert turn.response_model == "m-1"


# --- 5. the run spec's token_trace field -----------------------------------------------


def test_the_run_spec_sets_the_h1b_and_token_trace_fields_without_code() -> None:
    spec = run_pb2.RunSpec()
    t = spec.target
    t.connector, t.model, t.base_url = "openai-compatible", "m", "http://x/v1"
    t.token_trace, t.token_trace_server = True, "sglang"
    t.reasoning_handoff = "summarize-as-text"
    t.stream_resume_attempts = 3
    config = target_config(t)
    assert config.token_trace is True
    assert config.token_trace_server == "sglang"
    assert config.reasoning_handoff == "summarize-as-text"
    assert config.stream_resume_attempts == 3
    assert config.describe()["token_trace_server"] == "sglang"

    a = run_pb2.RunSpec().target
    a.connector, a.model = "anthropic", "claude-x"
    a.server_tools.add().update(
        {"type": "web_search_20260209", "name": "web_search", "max_uses": 5}
    )
    got = target_config(a).server_tools[0]
    assert got == {"type": "web_search_20260209", "name": "web_search", "max_uses": 5}
    assert isinstance(got["max_uses"], int)


def test_token_trace_still_needs_chat_completions() -> None:
    spec = run_pb2.RunSpec()
    spec.target.connector, spec.target.model = "anthropic", "claude-x"
    spec.target.token_trace = True
    with pytest.raises(ValueError, match="token_trace"):
        target_config(spec.target)


# --- 6. SGLang token fields --------------------------------------------------------------

SGLANG = TargetConfig(
    connector="openai-compatible",
    model="m",
    base_url="http://x/v1",
    token_trace=True,
    token_trace_server="sglang",
)


def sglang_logprobs(logprobs: list[float]) -> dict[str, Any]:
    # SGLang's entries carry the token text, not an id.
    return {
        "content": [
            {"token": f"t{i}", "bytes": [], "logprob": p, "top_logprobs": []}
            for i, p in enumerate(logprobs)
        ]
    }


def test_sglang_is_asked_with_its_own_fields_and_parsed_from_its_own() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "model": "m",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "hi"},
                        "finish_reason": "stop",
                        "logprobs": sglang_logprobs([-0.5, -0.25]),
                        "prompt_token_ids": [1, 2, 3],
                        "response_token_ids": [7, 8],
                    }
                ],
                "usage": {"prompt_tokens": 3, "completion_tokens": 2},
            },
        )

    turn = run(OpenAIChat(SGLANG, transport_client(handler)).complete(USER, TOOLS))
    assert seen[0]["return_token_ids"] is True
    assert "return_tokens_as_token_ids" not in seen[0]  # a vLLM field
    assert turn.tokens is not None
    assert turn.tokens.prompt_token_ids == [1, 2, 3]
    assert turn.tokens.completion_token_ids == [7, 8]
    assert turn.tokens.logprobs == [-0.5, -0.25]
    assert turn.tokens.missing == []


def test_sglang_streams_its_ids_in_the_sglext_block() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return sse_response(
            sse(
                ("", {"model": "m", "choices": [{"index": 0, "delta": {"content": "hi"}}]}),
                (
                    "",
                    {
                        "choices": [
                            {
                                "index": 0,
                                "delta": {},
                                "finish_reason": "stop",
                                "logprobs": sglang_logprobs([-0.5, -0.25]),
                            }
                        ],
                        "sglext": {"input_ids": [1, 2], "output_ids": [[7, 8]]},
                    },
                ),
            )
        )

    config = TargetConfig(**{**SGLANG.__dict__, "stream": True})
    turn = run(OpenAIChat(config, transport_client(handler)).complete(USER, TOOLS))
    # SGLang refuses return_token_ids on a stream, so the sglext fields are asked for.
    assert "return_token_ids" not in seen[0]
    assert seen[0]["return_input_ids_in_sglext"] is True
    assert turn.tokens is not None
    assert turn.tokens.prompt_token_ids == [1, 2]
    assert turn.tokens.completion_token_ids == [7, 8]
    assert turn.tokens.missing == []


def test_sglang_that_returns_no_ids_says_so() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "choices": [{"index": 0, "message": {"content": "hi"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 3, "completion_tokens": 2},
            },
        )

    turn = run(OpenAIChat(SGLANG, transport_client(handler)).complete(USER, TOOLS))
    assert turn.tokens is not None
    assert "prompt_token_ids" in turn.tokens.missing
    assert "completion_token_ids" in turn.tokens.missing

"""Token-in, token-out: a turn's token IDs and log-probabilities from a vLLM-style server."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx
import pytest

from evalsi.targets import TargetConfig
from evalsi_harness.cassette import _turn_from_dict, _turn_to_dict
from evalsi_harness.modelhttp import ModelError
from evalsi_harness.models import OpenAIChat, ToolSpec, create_model
from tests.test_models_h1 import sse, sse_response, transport_client

TARGET = TargetConfig(
    connector="openai-compatible", model="m", base_url="http://x/v1", token_trace=True
)
TOOLS = [ToolSpec("bash", "run", {"type": "object", "properties": {}})]


def entries(ids: list[int], logprobs: list[float]) -> list[dict[str, Any]]:
    return [{"token": f"token_id:{i}", "logprob": p} for i, p in zip(ids, logprobs, strict=True)]


def reply(**choice: Any) -> dict[str, Any]:
    return {
        "choices": [
            {"message": {"content": "hi"}, "finish_reason": "stop", **choice},
        ],
        "usage": {"prompt_tokens": 3, "completion_tokens": 2},
        "prompt_token_ids": [1, 2, 3],
    }


def complete(model: OpenAIChat) -> Any:
    return asyncio.run(model.complete([{"role": "user", "content": "go"}], TOOLS))


def test_the_request_asks_vllm_for_token_ids_and_logprobs() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(
            200,
            json=reply(token_ids=[7, 8], logprobs={"content": entries([7, 8], [-0.1, -0.2])}),
        )

    turn = complete(OpenAIChat(TARGET, transport_client(handler)))
    assert {
        k: seen[0][k]
        for k in ("logprobs", "top_logprobs", "return_tokens_as_token_ids", "return_token_ids")
    } == {
        "logprobs": True,
        "top_logprobs": 0,
        "return_tokens_as_token_ids": True,
        "return_token_ids": True,
    }
    assert turn.tokens is not None
    assert turn.tokens.prompt_token_ids == [1, 2, 3]
    assert turn.tokens.completion_token_ids == [7, 8]
    assert turn.tokens.logprobs == [-0.1, -0.2]
    assert turn.tokens.missing == []


def test_nothing_is_requested_or_kept_by_default() -> None:
    seen: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(200, json=reply())

    plain = TargetConfig(connector="openai-compatible", model="m", base_url="http://x/v1")
    turn = complete(OpenAIChat(plain, transport_client(handler)))
    assert turn.tokens is None
    assert "logprobs" not in seen[0]
    assert "return_token_ids" not in seen[0]
    assert "token_trace" not in plain.describe()
    assert TARGET.describe()["token_trace"] is True


def test_ids_come_from_the_logprob_tokens_when_the_server_sends_no_token_ids() -> None:
    payload = reply(logprobs={"content": entries([5], [-1.5])})
    del payload["prompt_token_ids"]
    turn = complete(
        OpenAIChat(TARGET, transport_client(lambda r: httpx.Response(200, json=payload)))
    )
    assert turn.tokens is not None
    assert turn.tokens.completion_token_ids == [5]
    # The prompt's tokens are a gap, named, not an empty list taken for data.
    assert turn.tokens.missing == ["prompt_token_ids"]


def test_a_server_that_ignores_the_request_leaves_every_gap_named() -> None:
    payload = reply()
    del payload["prompt_token_ids"]
    turn = complete(
        OpenAIChat(TARGET, transport_client(lambda r: httpx.Response(200, json=payload)))
    )
    assert turn.tokens is not None
    assert set(turn.tokens.missing) == {"prompt_token_ids", "completion_token_ids", "logprobs"}


@pytest.mark.parametrize(
    ("choice", "message"),
    [
        (
            {
                "token_ids": [7, 9],
                "logprobs": {
                    "content": [
                        {"token": "token_id:7", "logprob": -1},
                        {"token": "token_id:8", "logprob": -1},
                    ]
                },
            },
            "disagree",
        ),
        (
            {
                "token_ids": [7, 8],
                "logprobs": {"content": [{"token": "token_id:7", "logprob": -1}]},
            },
            "log-probabilities",
        ),
        ({"token_ids": "7"}, "not a list"),
    ],
)
def test_inconsistent_token_data_is_an_error(choice: dict[str, Any], message: str) -> None:
    payload = reply(**choice)
    with pytest.raises(ModelError, match=message):
        complete(OpenAIChat(TARGET, transport_client(lambda r: httpx.Response(200, json=payload))))


def test_a_stream_carries_the_trace_in_pieces() -> None:
    def chunk(**choice: Any) -> tuple[str, dict[str, Any]]:
        return (
            "",
            {
                "choices": [choice],
                **(
                    {"prompt_token_ids": [1, 2, 3]}
                    if choice.get("delta", {}).get("content") != "o"
                    else {}
                ),
            },
        )

    events = sse(
        chunk(delta={"content": "h"}, token_ids=[7], logprobs={"content": entries([7], [-0.5])}),
        chunk(delta={"content": "o"}, token_ids=[8], logprobs={"content": entries([8], [-0.25])}),
        chunk(delta={}, finish_reason="stop"),
        ("", {"choices": [], "usage": {"prompt_tokens": 3, "completion_tokens": 2}}),
    )
    streamed = TargetConfig(
        connector="openai-compatible",
        model="m",
        base_url="http://x/v1",
        token_trace=True,
        stream=True,
    )
    turn = complete(OpenAIChat(streamed, transport_client(lambda r: sse_response(events))))
    assert turn.text == "ho"
    assert turn.tokens is not None
    assert turn.tokens.prompt_token_ids == [1, 2, 3]
    assert turn.tokens.completion_token_ids == [7, 8]
    assert turn.tokens.logprobs == [-0.5, -0.25]


def test_a_recorded_turn_replays_with_its_tokens() -> None:
    turn = complete(
        OpenAIChat(
            TARGET,
            transport_client(
                lambda r: httpx.Response(
                    200,
                    json=reply(
                        token_ids=[7, 8], logprobs={"content": entries([7, 8], [-0.1, -0.2])}
                    ),
                )
            ),
        )
    )
    back = _turn_from_dict(json.loads(json.dumps(_turn_to_dict(turn))))
    assert back.tokens == turn.tokens
    # A turn without tokens stays without them, and its record has no key.
    plain = _turn_from_dict(json.loads(json.dumps(_turn_to_dict(_turn_from_dict({"text": "x"})))))
    assert plain.tokens is None
    assert "tokens" not in _turn_to_dict(plain)


def test_the_option_is_only_for_chat_completions_targets() -> None:
    with pytest.raises(ValueError, match="token_trace"):
        TargetConfig(connector="anthropic", model="c", token_trace=True)
    with pytest.raises(ValueError, match="token_trace"):
        TargetConfig(
            connector="openai-compatible",
            model="m",
            base_url="http://x",
            api="responses",
            token_trace=True,
        )
    assert isinstance(create_model(TARGET), OpenAIChat)


def test_a_fallback_without_the_option_records_that_tokens_are_absent() -> None:
    from evalsi_harness.fallback import FallbackModel

    class Down:
        name = "down"

        async def complete(self, *a: Any, **k: Any) -> Any:
            raise ModelError("overloaded")

        async def aclose(self) -> None: ...

    class Plain:
        name = "plain"

        async def complete(self, *a: Any, **k: Any) -> Any:
            from evalsi_harness.models import ModelTurn

            return ModelTurn(text="from the fallback")

        async def aclose(self) -> None: ...

    model = FallbackModel([Down(), Plain()], after=1)
    turn = asyncio.run(model.complete([{"role": "user", "content": "go"}], []))
    assert turn.text == "from the fallback"
    assert turn.tokens is None


def test_a_tool_call_turn_without_logprobs_names_its_gaps() -> None:
    # Most rollout turns are only tool calls: empty text must not hide missing tokens.
    payload = {
        "choices": [
            {
                "message": {
                    "content": "",
                    "tool_calls": [
                        {
                            "id": "c",
                            "type": "function",
                            "function": {"name": "bash", "arguments": "{}"},
                        }
                    ],
                },
                "finish_reason": "tool_calls",
            }
        ],
        "usage": {"prompt_tokens": 3, "completion_tokens": 9},
        "prompt_token_ids": [1, 2, 3],
    }
    turn = complete(
        OpenAIChat(TARGET, transport_client(lambda r: httpx.Response(200, json=payload)))
    )
    assert turn.tokens is not None
    assert set(turn.tokens.missing) == {"completion_token_ids", "logprobs"}

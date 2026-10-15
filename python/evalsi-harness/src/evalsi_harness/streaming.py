"""Rebuilds a complete response from a stream of server-sent events, in the
shape each API returns without streaming, so one parser serves both."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Callable, Coroutine
from contextlib import aclosing
from functools import wraps
from typing import Any

from evalsi_harness.modelhttp import ModelError

Events = AsyncIterator[tuple[str, dict[str, Any]]]


def _closing(
    collect: Callable[[Events], Coroutine[Any, Any, dict[str, Any]]],
) -> Callable[[Events], Coroutine[Any, Any, dict[str, Any]]]:
    """Closes the event stream when the collector returns or raises, so the
    connection and the concurrency permit are released at once, not when the
    generator is garbage collected."""

    @wraps(collect)
    async def wrapper(events: Events) -> dict[str, Any]:
        async with aclosing(events):  # type: ignore[type-var]
            result = await collect(events)
        return result

    return wrapper


@_closing
async def collect_anthropic(events: Events) -> dict[str, Any]:
    blocks: dict[int, dict[str, Any]] = {}
    partial: dict[int, list[str]] = {}
    usage: dict[str, Any] = {}
    stop_reason = ""
    async for name, data in events:
        if name == "error":
            err = data.get("error") or {}
            raise ModelError(f"model stream error: {err.get('type', '')}: {err.get('message', '')}")
        if name == "message_start":
            usage.update((data.get("message") or {}).get("usage") or {})
        elif name == "content_block_start":
            blocks[data["index"]] = dict(data["content_block"])
        elif name == "content_block_delta":
            block, delta = blocks[data["index"]], data["delta"]
            kind = delta.get("type")
            if kind == "text_delta":
                block["text"] = block.get("text", "") + delta.get("text", "")
            elif kind == "thinking_delta":
                block["thinking"] = block.get("thinking", "") + delta.get("thinking", "")
            elif kind == "signature_delta":
                # A signature may arrive in pieces, like the thinking it signs.
                block["signature"] = block.get("signature", "") + delta.get("signature", "")
            elif kind == "input_json_delta":
                partial.setdefault(data["index"], []).append(delta.get("partial_json", ""))
        elif name == "message_delta":
            stop_reason = (data.get("delta") or {}).get("stop_reason") or stop_reason
            usage.update(data.get("usage") or {})
    if not stop_reason:
        # A stream that ends before message_delta was cut off: its text may be partial and
        # its tool calls unfinished, so it is not a response.
        raise ModelError("the stream ended before the response completed (no stop reason)")
    for index, parts in partial.items():
        try:
            parsed = json.loads("".join(parts) or "{}")
        except json.JSONDecodeError:
            parsed = {}  # cut off by max_tokens; the stop reason says so
        blocks[index]["input"] = parsed
    for block in blocks.values():
        if block.get("type") == "tool_use":
            block.setdefault("input", {})
    return {
        "content": [blocks[i] for i in sorted(blocks)],
        "stop_reason": stop_reason,
        "usage": usage,
    }


@_closing
async def collect_chat(events: Events) -> dict[str, Any]:
    text: list[str] = []
    refusal: list[str] = []
    calls: dict[int, dict[str, Any]] = {}
    finish = ""
    usage: dict[str, Any] = {}
    async for _, data in events:
        if "error" in data:
            raise ModelError(f"model stream error: {data['error']!r:.300}")
        usage = data.get("usage") or usage
        for choice in data.get("choices") or []:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                text.append(delta["content"])
            if delta.get("refusal"):
                refusal.append(delta["refusal"])
            for call in delta.get("tool_calls") or []:
                slot = calls.setdefault(
                    call.get("index", 0),
                    {"id": "", "type": "function", "function": {"name": "", "arguments": ""}},
                )
                slot["id"] = call.get("id") or slot["id"]
                fn = call.get("function") or {}
                slot["function"]["name"] += fn.get("name") or ""
                slot["function"]["arguments"] += fn.get("arguments") or ""
            finish = choice.get("finish_reason") or finish
    if not finish:
        raise ModelError("the stream ended before the response completed (no finish reason)")
    message: dict[str, Any] = {"content": "".join(text)}
    if refusal:
        message["refusal"] = "".join(refusal)
    if calls:
        message["tool_calls"] = [calls[i] for i in sorted(calls)]
    return {"choices": [{"message": message, "finish_reason": finish}], "usage": usage}


@_closing
async def collect_responses(events: Events) -> dict[str, Any]:
    """The Responses API ends its stream with the whole response."""
    async for name, data in events:
        if name in ("response.completed", "response.incomplete"):
            response: dict[str, Any] = data.get("response") or {}
            return response
        if name in ("response.failed", "error"):
            err = (data.get("response") or {}).get("error") or data.get("error") or data
            raise ModelError(f"model stream error: {err!r:.300}")
    raise ModelError("the stream ended before the response completed")

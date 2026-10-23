"""Rebuilds a complete response from a stream of server-sent events, in the
shape each API returns without streaming, so one parser serves both.

A stream that breaks after events arrived (the connection drops, the server
sends an error event, or the stream ends without its stop reason) raises
:class:`StreamBroken` with what had arrived: the text so far, and how many
tool calls were complete and how many were cut off mid-arguments. The model
clients decide whether to try again (``TargetConfig.stream_resume_attempts``,
see :func:`resume_record`); a partial tool call is never turned into a call.
"""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Callable, Coroutine
from contextlib import aclosing
from functools import wraps
from typing import Any

from evalsi_harness.modelhttp import ModelError

Events = AsyncIterator[tuple[str, dict[str, Any]]]


class StreamBroken(ModelError):
    """A stream that broke mid-response. ``partial`` holds what had arrived:
    ``text``, ``tool_calls`` (complete ones, kept only as a count: a retry asks
    for them again), ``tool_calls_cut`` (cut off mid-arguments, discarded),
    ``thinking_blocks`` and the ``usage`` reported so far."""

    def __init__(self, message: str, partial: dict[str, Any]) -> None:
        super().__init__(message)
        self.partial = partial


def resume_record(attempt: int, exc: StreamBroken, mode: str) -> dict[str, Any]:
    """What a step records about one broken stream and how it was resumed."""
    p = exc.partial
    return {
        "attempt": attempt,
        "mode": mode,
        "error": str(exc)[:300],
        "partial_chars": len(p.get("text") or ""),
        # Complete calls are asked for again; cut ones are thrown away.
        "tool_calls_discarded": int(p.get("tool_calls") or 0) + int(p.get("tool_calls_cut") or 0),
        "tool_calls_cut": int(p.get("tool_calls_cut") or 0),
        "thinking_blocks_discarded": int(p.get("thinking_blocks") or 0),
    }


async def collect_resuming(
    collect: Callable[[Events], Coroutine[Any, Any, dict[str, Any]]],
    start: Callable[[dict[str, Any]], Events],
    body: dict[str, Any],
    attempts: int,
    *,
    prefill: bool = False,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """A streamed response, with up to ``attempts`` more tries after a stream that
    breaks mid-response, and a record of each. A try either asks again from the
    start ("retry"), or, with ``prefill`` (Anthropic Messages, models that accept
    a prefill, thinking off), sends the text so far back as the start of the
    assistant's reply so the model continues it; the continuation is joined to
    that text. Whatever the mode, a tool call from a broken stream is never kept:
    a cut one cannot be repaired, and the model is asked for its calls again."""
    resumes: list[dict[str, Any]] = []
    prefix = ""
    for attempt in range(attempts + 1):
        sent = body
        if prefix:
            sent = {
                **body,
                "messages": [*body["messages"], {"role": "assistant", "content": prefix}],
            }
        try:
            payload = await collect(start(sent))
        except StreamBroken as exc:
            if attempt == attempts:
                if resumes:
                    exc.partial["resumes"] = resumes
                raise
            # The API refuses a prefill that ends in whitespace.
            joined = (prefix + str(exc.partial.get("text") or "")).rstrip()
            mode = "prefill" if prefill and joined else "retry"
            prefix = joined if mode == "prefill" else ""
            resumes.append(resume_record(attempt + 1, exc, mode))
            continue
        if prefix:
            content = list(payload.get("content") or [])
            if content and content[0].get("type") == "text":
                content[0] = {**content[0], "text": prefix + content[0].get("text", "")}
            else:
                content.insert(0, {"type": "text", "text": prefix})
            payload = {**payload, "content": content}
        return payload, resumes
    raise AssertionError("unreachable")


def _complete_json(text: str) -> bool:
    try:
        json.loads(text or "{}")
    except json.JSONDecodeError:
        return False
    return True


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


class _Reader:
    """The events of one stream; a failure after the first event is a broken
    stream (``broken`` builds the error with the partial output), one before it
    is the request failing, raised as it is."""

    def __init__(self, events: Events, broken: Callable[[str], StreamBroken]) -> None:
        self._events = aiter(events)
        self._broken = broken
        self.seen = False

    def __aiter__(self) -> _Reader:
        return self

    async def __anext__(self) -> tuple[str, dict[str, Any]]:
        try:
            event = await anext(self._events)
        except StopAsyncIteration:
            raise
        except StreamBroken:
            raise
        except ModelError as exc:
            if not self.seen:
                raise
            raise self._broken(str(exc)) from exc
        self.seen = True
        return event


@_closing
async def collect_anthropic(events: Events) -> dict[str, Any]:
    blocks: dict[int, dict[str, Any]] = {}
    partial: dict[int, list[str]] = {}
    stopped: set[int] = set()
    usage: dict[str, Any] = {}
    stop_reason = ""
    model = ""

    def broken(message: str) -> StreamBroken:
        ordered = [blocks[i] for i in sorted(blocks)]
        calls = [i for i, b in blocks.items() if b.get("type") == "tool_use"]
        complete = [
            i for i in calls if i in stopped and _complete_json("".join(partial.get(i, [])))
        ]
        return StreamBroken(
            message,
            {
                "text": "".join(b.get("text", "") for b in ordered if b.get("type") == "text"),
                "tool_calls": len(complete),
                "tool_calls_cut": len(calls) - len(complete),
                "thinking_blocks": sum(
                    1 for b in ordered if b.get("type") in ("thinking", "redacted_thinking")
                ),
                "usage": dict(usage),
            },
        )

    async for name, data in _Reader(events, broken):
        if name == "error":
            err = data.get("error") or {}
            raise broken(f"model stream error: {err.get('type', '')}: {err.get('message', '')}")
        if name == "message_start":
            message = data.get("message") or {}
            usage.update(message.get("usage") or {})
            model = message.get("model") or model
        elif name == "content_block_start":
            blocks[data["index"]] = dict(data["content_block"])
        elif name == "content_block_stop":
            stopped.add(data.get("index", -1))
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
        raise broken("the stream ended before the response completed (no stop reason)")
    for index, parts in partial.items():
        try:
            parsed = json.loads("".join(parts) or "{}")
        except json.JSONDecodeError:
            parsed = {}  # cut off by max_tokens; the stop reason says so
        blocks[index]["input"] = parsed
    for block in blocks.values():
        if block.get("type") in ("tool_use", "server_tool_use"):
            block.setdefault("input", {})
    return {
        "content": [blocks[i] for i in sorted(blocks)],
        "stop_reason": stop_reason,
        "usage": usage,
        **({"model": model} if model else {}),
    }


@_closing
async def collect_chat(events: Events) -> dict[str, Any]:  # noqa: PLR0915
    text: list[str] = []
    refusal: list[str] = []
    calls: dict[int, dict[str, Any]] = {}
    finish = ""
    usage: dict[str, Any] = {}
    # vLLM's token trace, in the pieces the stream carries it in, and SGLang's
    # (its sglext block, whole, in a late chunk).
    prompt_ids: Any = None
    token_ids: list[Any] | None = None
    logprob_entries: list[Any] | None = None
    sglext: dict[str, Any] = {}
    model = fingerprint = ""

    def broken(message: str) -> StreamBroken:
        cut = sum(1 for c in calls.values() if not _complete_json(c["function"]["arguments"]))
        return StreamBroken(
            message,
            {
                "text": "".join(text),
                "tool_calls": len(calls) - cut,
                "tool_calls_cut": cut,
                "thinking_blocks": 0,
                "usage": dict(usage),
            },
        )

    async for _, data in _Reader(events, broken):
        if "error" in data:
            raise broken(f"model stream error: {data['error']!r:.300}")
        usage = data.get("usage") or usage
        model = data.get("model") or model
        fingerprint = data.get("system_fingerprint") or fingerprint
        if isinstance(data.get("sglext"), dict):
            sglext.update({k: v for k, v in data["sglext"].items() if v is not None})
        if data.get("prompt_token_ids") is not None:
            prompt_ids = data["prompt_token_ids"]
        for choice in data.get("choices") or []:
            if isinstance(choice.get("token_ids"), list):
                token_ids = (token_ids or []) + choice["token_ids"]
            block = choice.get("logprobs")
            if isinstance(block, dict) and isinstance(block.get("content"), list):
                logprob_entries = (logprob_entries or []) + block["content"]
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
        raise broken("the stream ended before the response completed (no finish reason)")
    message: dict[str, Any] = {"content": "".join(text)}
    if refusal:
        message["refusal"] = "".join(refusal)
    if calls:
        message["tool_calls"] = [calls[i] for i in sorted(calls)]
    chosen: dict[str, Any] = {"message": message, "finish_reason": finish}
    if token_ids is not None:
        chosen["token_ids"] = token_ids
    if logprob_entries is not None:
        chosen["logprobs"] = {"content": logprob_entries}
    result: dict[str, Any] = {"choices": [chosen], "usage": usage}
    if prompt_ids is not None:
        result["prompt_token_ids"] = prompt_ids
    if sglext:
        result["sglext"] = sglext
    if model:
        result["model"] = model
    if fingerprint:
        result["system_fingerprint"] = fingerprint
    return result


@_closing
async def collect_responses(events: Events) -> dict[str, Any]:
    """The Responses API ends its stream with the whole response."""
    text: list[str] = []
    calls = cut = 0

    def broken(message: str) -> StreamBroken:
        return StreamBroken(
            message,
            {
                "text": "".join(text),
                "tool_calls": calls,
                "tool_calls_cut": cut,
                "thinking_blocks": 0,
                "usage": {},
            },
        )

    async for name, data in _Reader(events, broken):
        if name in ("response.completed", "response.incomplete"):
            response: dict[str, Any] = data.get("response") or {}
            return response
        if name == "response.failed":
            # The model failed the response, as a non-streamed call would: not a break.
            err = (data.get("response") or {}).get("error") or data
            raise ModelError(f"model stream error: {err!r:.300}")
        if name == "error":
            raise broken(f"model stream error: {data.get('error') or data!r:.300}")
        if name == "response.output_text.delta":
            text.append(str(data.get("delta") or ""))
        elif (
            name == "response.output_item.added"
            and (data.get("item") or {}).get("type") == "function_call"
        ):
            cut += 1  # open until its arguments are done
        elif name == "response.function_call_arguments.done":
            calls, cut = calls + 1, max(cut - 1, 0)
    raise broken("the stream ended before the response completed")

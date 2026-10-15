"""Tool-calling chat models for the built-in reference agent.

Two protocols, spoken directly over HTTP so the harness stays light:

- ``openai-compatible``: chat completions with ``tools`` (vLLM, SGLang,
  Ollama, LiteLLM, gateways, OpenAI itself).
- ``anthropic``: the Messages API with ``tools``.

Conversations are kept in the chat-completions shape (system, user,
assistant with ``tool_calls``, tool messages) and translated for Anthropic.
"""

from __future__ import annotations

import asyncio
import json
import random
import time
from dataclasses import dataclass, field
from typing import Any, Protocol

import httpx

from evalsi.targets import RETRY_STATUS, TargetConfig, api_key
from evalsi.types import Usage

Message = dict[str, Any]


class ModelError(RuntimeError):
    """The model call failed after retries."""


@dataclass
class ToolCallRequest:
    id: str
    name: str
    # Raw JSON, exactly as the model produced it.
    arguments: str


@dataclass
class ModelTurn:
    text: str
    tool_calls: list[ToolCallRequest] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    stop_reason: str = ""
    # Anthropic thinking and redacted_thinking blocks, exactly as returned
    # (signatures included): the API needs them back with the tool results.
    thinking: list[dict[str, Any]] = field(default_factory=list)
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0

    @property
    def truncated(self) -> bool:
        """The model hit its output limit, so a tool call may be cut off."""
        return self.stop_reason in ("max_tokens", "length")

    def as_message(self) -> Message:
        msg: Message = {"role": "assistant", "content": self.text}
        if self.thinking:
            msg["thinking_blocks"] = self.thinking
        if self.tool_calls:
            msg["tool_calls"] = [
                {
                    "id": c.id,
                    "type": "function",
                    "function": {"name": c.name, "arguments": c.arguments},
                }
                for c in self.tool_calls
            ]
        return msg


@dataclass
class ToolSpec:
    name: str
    description: str
    input_schema: dict[str, Any]


class ChatModel(Protocol):
    name: str

    async def complete(
        self, messages: list[Message], tools: list[ToolSpec], *, system: str = ""
    ) -> ModelTurn: ...

    async def aclose(self) -> None: ...


MAX_RETRY_AFTER_S = 60.0


def retry_after(response: httpx.Response) -> float:
    """Seconds the server asked us to wait (``retry-after``), capped; 0 if absent."""
    try:
        return min(max(float(response.headers.get("retry-after", "0")), 0.0), MAX_RETRY_AFTER_S)
    except ValueError:
        return 0.0


async def _post(
    client: httpx.AsyncClient, url: str, body: dict[str, Any], headers: dict[str, str]
) -> dict[str, Any]:
    delay = 1.0
    for attempt in range(1, 6):
        wait = delay
        try:
            response = await client.post(url, json=body, headers=headers)
        except httpx.TransportError as exc:
            if attempt == 5:
                raise ModelError(f"model request failed: {exc}") from exc
        else:
            if response.status_code < 400:
                payload: dict[str, Any] = response.json()
                return payload
            if response.status_code not in RETRY_STATUS or attempt == 5:
                raise ModelError(
                    f"model returned HTTP {response.status_code}: {response.text[:400]}"
                )
            wait = max(delay, retry_after(response))
        await asyncio.sleep(wait + random.uniform(0, wait / 4))
        delay *= 2
    raise AssertionError("unreachable")


class OpenAIChat:
    def __init__(self, config: TargetConfig, client: httpx.AsyncClient | None = None) -> None:
        self.config = config
        self.name = config.model
        self._client = client or httpx.AsyncClient(timeout=config.timeout_s)
        self._url = config.base_url.rstrip("/") + "/chat/completions"
        self._headers = {"Content-Type": "application/json"}
        key = api_key(config.api_key_env, "OPENAI_API_KEY")
        if key:
            self._headers["Authorization"] = f"Bearer {key}"

    async def complete(
        self, messages: list[Message], tools: list[ToolSpec], *, system: str = ""
    ) -> ModelTurn:
        body: dict[str, Any] = {
            "model": self.config.model,
            "messages": ([{"role": "system", "content": system}] if system else [])
            + [to_openai(m) for m in messages],
            "max_tokens": self.config.max_tokens,
        }
        if tools:
            body["tools"] = [
                {
                    "type": "function",
                    "function": {
                        "name": t.name,
                        "description": t.description,
                        "parameters": t.input_schema,
                    },
                }
                for t in tools
            ]
        if self.config.temperature is not None:
            body["temperature"] = self.config.temperature
        start = time.perf_counter()
        payload = await _post(self._client, self._url, body, self._headers)
        try:
            choice = payload["choices"][0]
            message = choice["message"]
        except (KeyError, IndexError, TypeError) as exc:
            raise ModelError(f"unexpected chat-completions response: {payload!r:.300}") from exc
        calls = [
            ToolCallRequest(
                id=c.get("id") or f"call_{i}",
                name=c["function"]["name"],
                arguments=c["function"].get("arguments") or "{}",
            )
            for i, c in enumerate(message.get("tool_calls") or [])
        ]
        usage = payload.get("usage") or {}
        details = usage.get("prompt_tokens_details") or {}
        return ModelTurn(
            text=message.get("content") or "",
            tool_calls=calls,
            usage=Usage(
                input_tokens=usage.get("prompt_tokens"),
                output_tokens=usage.get("completion_tokens"),
                latency_ms=(time.perf_counter() - start) * 1000,
            ),
            stop_reason=choice.get("finish_reason") or "",
            # A part of prompt_tokens, as in Anthropic's normalized total.
            cache_read_tokens=int(details.get("cached_tokens") or 0),
        )

    async def aclose(self) -> None:
        await self._client.aclose()


def to_openai(message: Message) -> Message:
    """Drops the fields chat completions does not know (a tool result's is_error,
    an assistant turn's thinking_blocks)."""
    if message.get("role") == "assistant" and "thinking_blocks" in message:
        return {k: v for k, v in message.items() if k != "thinking_blocks"}
    if message.get("role") != "tool":
        return message
    content = message.get("content") or ""
    if message.get("is_error"):
        content = f"ERROR: {content}"
    return {"role": "tool", "tool_call_id": message["tool_call_id"], "content": content}


def to_anthropic(messages: list[Message]) -> list[dict[str, Any]]:
    """Chat-completions messages as Anthropic messages (tool results grouped
    into one user turn, as the API requires)."""
    out: list[dict[str, Any]] = []
    for msg in messages:
        role = msg["role"]
        if role == "tool":
            block = {
                "type": "tool_result",
                "tool_use_id": msg["tool_call_id"],
                "content": msg.get("content") or "",
            }
            if msg.get("is_error"):
                block["is_error"] = True
            if out and out[-1]["role"] == "user" and isinstance(out[-1]["content"], list):
                out[-1]["content"].append(block)
            else:
                out.append({"role": "user", "content": [block]})
        elif role == "assistant":
            blocks: list[dict[str, Any]] = list(msg.get("thinking_blocks") or [])
            if msg.get("content"):
                blocks.append({"type": "text", "text": msg["content"]})
            for call in msg.get("tool_calls") or []:
                try:
                    args = json.loads(call["function"]["arguments"] or "{}")
                except json.JSONDecodeError:
                    args = {}
                blocks.append(
                    {
                        "type": "tool_use",
                        "id": call["id"],
                        "name": call["function"]["name"],
                        "input": args if isinstance(args, dict) else {},
                    }
                )
            out.append({"role": "assistant", "content": blocks or ""})
        elif role == "user":
            out.append({"role": "user", "content": msg.get("content") or ""})
    return out


EPHEMERAL = {"type": "ephemeral"}


def with_cache_breakpoint(messages: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Marks the end of the conversation as a cache breakpoint, so the next
    call reads everything before it from the cache (system and tools carry
    their own). Messages are copied, never changed in place."""
    if not messages:
        return messages
    last = dict(messages[-1])
    content = last["content"]
    if isinstance(content, str):
        if not content:
            return messages
        content = [{"type": "text", "text": content}]
    elif content:
        content = list(content)
    else:
        return messages
    # Thinking blocks cannot carry cache_control: mark the last block before them.
    index = len(content) - 1
    while index > 0 and content[index].get("type") in ("thinking", "redacted_thinking"):
        index -= 1
    if content[index].get("type") in ("thinking", "redacted_thinking"):
        return messages
    content[index] = {**content[index], "cache_control": EPHEMERAL}
    last["content"] = content
    return [*messages[:-1], last]


class AnthropicChat:
    def __init__(self, config: TargetConfig, client: httpx.AsyncClient | None = None) -> None:
        self.config = config
        self.name = config.model
        self._client = client or httpx.AsyncClient(timeout=config.timeout_s)
        base = (config.base_url or "https://api.anthropic.com").rstrip("/")
        self._url = base + "/v1/messages"
        self._headers = {"content-type": "application/json", "anthropic-version": "2023-06-01"}
        key = api_key(config.api_key_env, "ANTHROPIC_API_KEY")
        if key:
            self._headers["x-api-key"] = key

    async def complete(
        self, messages: list[Message], tools: list[ToolSpec], *, system: str = ""
    ) -> ModelTurn:
        body: dict[str, Any] = {
            "model": self.config.model,
            "max_tokens": self.config.max_tokens,
            "messages": with_cache_breakpoint(to_anthropic(messages)),
        }
        if system:
            body["system"] = [{"type": "text", "text": system, "cache_control": EPHEMERAL}]
        if tools:
            body["tools"] = [
                {"name": t.name, "description": t.description, "input_schema": t.input_schema}
                for t in tools
            ]
            body["tools"][-1]["cache_control"] = EPHEMERAL
        if self.config.effort:
            body["output_config"] = {"effort": self.config.effort}
        if self.config.temperature is not None:
            body["temperature"] = self.config.temperature
        start = time.perf_counter()
        payload = await _post(self._client, self._url, body, self._headers)
        text, calls, thinking = [], [], []
        for block in payload.get("content") or []:
            if block.get("type") in ("thinking", "redacted_thinking"):
                thinking.append(block)
            elif block.get("type") == "text":
                text.append(block.get("text", ""))
            elif block.get("type") == "tool_use":
                calls.append(
                    ToolCallRequest(
                        id=block["id"],
                        name=block["name"],
                        arguments=json.dumps(block.get("input") or {}),
                    )
                )
        usage = payload.get("usage") or {}
        cache_read = int(usage.get("cache_read_input_tokens") or 0)
        cache_write = int(usage.get("cache_creation_input_tokens") or 0)
        return ModelTurn(
            text="".join(text),
            tool_calls=calls,
            usage=Usage(
                # Anthropic reports cache tokens apart from input_tokens; the
                # total prompt size is the sum, as with chat completions.
                input_tokens=(
                    None
                    if usage.get("input_tokens") is None
                    else usage["input_tokens"] + cache_read + cache_write
                ),
                output_tokens=usage.get("output_tokens"),
                latency_ms=(time.perf_counter() - start) * 1000,
            ),
            stop_reason=payload.get("stop_reason") or "",
            thinking=thinking,
            cache_read_tokens=cache_read,
            cache_write_tokens=cache_write,
        )

    async def aclose(self) -> None:
        await self._client.aclose()


def create_model(config: TargetConfig) -> ChatModel:
    if config.connector == "anthropic":
        return AnthropicChat(config)
    return OpenAIChat(config)

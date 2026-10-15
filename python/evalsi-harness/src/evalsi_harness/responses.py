"""The OpenAI Responses API for the built-in agent.

Reasoning models keep their chain of thought server-side. With ``store`` off,
the encrypted reasoning items of a turn must be sent back with the tool
results, or the model loses its plan between calls; they travel in the
assistant message's ``thinking_blocks``, like Anthropic's thinking blocks.
"""

from __future__ import annotations

import time
from typing import Any

import httpx

from evalsi.targets import TargetConfig, api_key
from evalsi.types import Usage
from evalsi_harness.modelhttp import ModelError, gate_for, post_json, stream_events
from evalsi_harness.models import Message, ModelTurn, ToolCallRequest, ToolSpec
from evalsi_harness.streaming import collect_responses


def to_responses(messages: list[Message]) -> list[dict[str, Any]]:
    """Chat-completions messages as Responses API input items."""
    items: list[dict[str, Any]] = []
    for msg in messages:
        role = msg["role"]
        if role == "tool":
            content = msg.get("content") or ""
            if msg.get("is_error"):
                content = f"ERROR: {content}"
            items.append(
                {"type": "function_call_output", "call_id": msg["tool_call_id"], "output": content}
            )
        elif role == "assistant":
            items.extend(msg.get("thinking_blocks") or [])
            if msg.get("content"):
                items.append({"role": "assistant", "content": msg["content"]})
            for call in msg.get("tool_calls") or []:
                items.append(
                    {
                        "type": "function_call",
                        "call_id": call["id"],
                        "name": call["function"]["name"],
                        "arguments": call["function"]["arguments"] or "{}",
                    }
                )
        elif role == "user":
            items.append({"role": "user", "content": msg.get("content") or ""})
    return items


class OpenAIResponses:
    def __init__(self, config: TargetConfig, client: httpx.AsyncClient | None = None) -> None:
        self.config = config
        self.name = config.model
        self._client = client or httpx.AsyncClient(timeout=config.timeout_s)
        self._url = config.base_url.rstrip("/") + "/responses"
        self._headers = {"Content-Type": "application/json"}
        key = api_key(config.api_key_env, "OPENAI_API_KEY")
        if key:
            self._headers["Authorization"] = f"Bearer {key}"
        self._gate = gate_for(self._url, config.api_key_env, config.max_concurrency)

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        body: dict[str, Any] = {
            "model": self.config.model,
            "input": to_responses(messages),
            "max_output_tokens": max_tokens or self.config.max_tokens,
            # Stateless: reasoning comes back encrypted and is sent back by us.
            "store": False,
            "include": ["reasoning.encrypted_content"],
        }
        if system:
            body["instructions"] = system
        if tools:
            body["tools"] = [
                {
                    "type": "function",
                    "name": t.name,
                    "description": t.description,
                    "parameters": t.input_schema,
                }
                for t in tools
            ]
        if self.config.effort:
            body["reasoning"] = {"effort": self.config.effort}
        if self.config.temperature is not None:
            body["temperature"] = self.config.temperature
        start = time.perf_counter()
        if self.config.stream:
            body["stream"] = True
            payload = await collect_responses(
                stream_events(self._client, self._url, body, self._headers, self._gate)
            )
        else:
            payload = await post_json(self._client, self._url, body, self._headers, self._gate)
        return self._turn(payload, (time.perf_counter() - start) * 1000)

    def _turn(self, payload: dict[str, Any], latency_ms: float) -> ModelTurn:
        if payload.get("error"):
            raise ModelError(f"model returned an error: {payload['error']!r:.300}")
        text: list[str] = []
        calls: list[ToolCallRequest] = []
        reasoning: list[dict[str, Any]] = []
        refused = False
        for item in payload.get("output") or []:
            kind = item.get("type")
            if kind == "reasoning":
                reasoning.append(item)
            elif kind == "message":
                for part in item.get("content") or []:
                    if part.get("type") == "output_text":
                        text.append(part.get("text", ""))
                    elif part.get("type") == "refusal":
                        refused = True
                        text.append(part.get("refusal", ""))
            elif kind == "function_call":
                calls.append(
                    ToolCallRequest(
                        id=item.get("call_id") or item.get("id") or f"call_{len(calls)}",
                        name=item["name"],
                        arguments=item.get("arguments") or "{}",
                    )
                )
        reason = (payload.get("incomplete_details") or {}).get("reason") or ""
        if payload.get("status") == "incomplete" and reason == "max_output_tokens":
            stop = "length"
        elif refused or reason == "content_filter":
            stop = "refusal"
        else:
            stop = "tool_calls" if calls else "stop"
        usage = payload.get("usage") or {}
        return ModelTurn(
            text="".join(text),
            tool_calls=calls,
            usage=Usage(
                input_tokens=usage.get("input_tokens"),
                output_tokens=usage.get("output_tokens"),
                latency_ms=latency_ms,
            ),
            stop_reason=stop,
            thinking=reasoning,
            cache_read_tokens=int(
                (usage.get("input_tokens_details") or {}).get("cached_tokens") or 0
            ),
            reasoning_tokens=int(
                (usage.get("output_tokens_details") or {}).get("reasoning_tokens") or 0
            ),
        )

    async def aclose(self) -> None:
        await self._client.aclose()

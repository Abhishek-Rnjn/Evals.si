"""Tool-calling chat models for the built-in reference agent.

Three protocols, spoken directly over HTTP so the harness stays light:

- ``openai-compatible``: chat completions with ``tools`` (vLLM, SGLang,
  Ollama, LiteLLM, gateways, OpenAI itself), or with ``api: responses`` the
  Responses API (``responses.py``).
- ``anthropic``: the Messages API with ``tools``.

Conversations are kept in the chat-completions shape (system, user,
assistant with ``tool_calls``, tool messages) and translated for Anthropic
and the Responses API. An assistant message may carry ``thinking_blocks``:
the provider's own reasoning items, which only that provider can read.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field, replace
from typing import Any, Protocol

import httpx

from evalsi.targets import TargetConfig, api_key
from evalsi.types import Usage
from evalsi_harness.modelhttp import (
    MAX_RETRY_AFTER_S,
    ModelError,
    gate_for,
    post_json,
    retry_after,
    stream_events,
)
from evalsi_harness.streaming import collect_anthropic, collect_chat, collect_resuming

__all__ = ["MAX_RETRY_AFTER_S", "ModelError", "retry_after"]

Message = dict[str, Any]


@dataclass
class ToolCallRequest:
    id: str
    name: str
    # Raw JSON, exactly as the model produced it.
    arguments: str


@dataclass
class TokenTrace:
    """The exact tokens of one turn, for a trainer: the prompt the server
    tokenized, the tokens it generated and the log-probability of each.
    ``missing`` names what the server did not return (``prompt_token_ids``
    when it ignored ``return_token_ids``), so a gap is never an empty list
    taken for data."""

    prompt_token_ids: list[int] = field(default_factory=list)
    completion_token_ids: list[int] = field(default_factory=list)
    logprobs: list[float] = field(default_factory=list)
    missing: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "prompt_token_ids": self.prompt_token_ids,
            "completion_token_ids": self.completion_token_ids,
            "logprobs": self.logprobs,
            "missing": self.missing,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> TokenTrace:
        return cls(
            prompt_token_ids=list(data.get("prompt_token_ids") or []),
            completion_token_ids=list(data.get("completion_token_ids") or []),
            logprobs=[float(x) for x in data.get("logprobs") or []],
            missing=list(data.get("missing") or []),
        )


# What a vLLM chat-completions server is asked for to return a turn's tokens
# (vLLM's OpenAI-compatible server: ``return_token_ids`` adds ``prompt_token_ids``
# to the response and ``token_ids`` to each choice; ``return_tokens_as_token_ids``
# makes each log-probability entry's ``token`` read ``token_id:<int>``).
TOKEN_TRACE_REQUEST: dict[str, Any] = {
    "logprobs": True,
    "top_logprobs": 0,
    "return_tokens_as_token_ids": True,
    "return_token_ids": True,
}

# The same from an SGLang server, coded against its OpenAI-compatible protocol
# (sglang main as of 2026-10-22, python/sglang/srt/entrypoints/openai/protocol.py
# and serving_chat.py; not run against a live server). Without streaming,
# ``return_token_ids`` adds ``prompt_token_ids`` and ``response_token_ids`` to each
# choice. SGLang refuses ``return_token_ids`` on a stream; there the ids come in
# the response's ``sglext`` block (``input_ids``, and ``output_ids`` per choice).
# Log-probability entries carry the token's text, not its id.
SGLANG_TOKEN_TRACE_REQUEST: dict[str, Any] = {
    "logprobs": True,
    "top_logprobs": 0,
    "return_token_ids": True,
}
SGLANG_STREAM_TOKEN_TRACE_REQUEST: dict[str, Any] = {
    "logprobs": True,
    "top_logprobs": 0,
    "return_input_ids_in_sglext": True,
    "return_output_ids_in_sglext": True,
}


def token_trace_request(server: str, stream: bool) -> dict[str, Any]:
    """The request fields that ask ``server`` ("vllm" or "sglang") for a turn's tokens."""
    if server == "sglang":
        return SGLANG_STREAM_TOKEN_TRACE_REQUEST if stream else SGLANG_TOKEN_TRACE_REQUEST
    return TOKEN_TRACE_REQUEST


def _int_list(value: Any, what: str) -> list[int]:
    if not isinstance(value, list) or not all(
        isinstance(x, int) and not isinstance(x, bool) for x in value
    ):
        raise ModelError(f"{what} is not a list of token IDs: {value!r:.200}")
    return list(value)


def token_trace(payload: dict[str, Any], choice: dict[str, Any]) -> TokenTrace:
    """A turn's :class:`TokenTrace` from a chat-completions response (vLLM's
    fields, see ``TOKEN_TRACE_REQUEST``). A server that returns some of it
    gets a trace that names the rest in ``missing``; ids and log-probabilities
    that disagree in length or value are an error."""
    message = choice.get("message") or {}
    raw_ext = payload.get("sglext")
    sglext: dict[str, Any] = raw_ext if isinstance(raw_ext, dict) else {}
    prompt = payload.get("prompt_token_ids")
    if prompt is None:
        prompt = choice.get("prompt_token_ids", message.get("prompt_token_ids"))
    if prompt is None:
        prompt = sglext.get("input_ids")
    block = choice.get("logprobs")
    entries = block.get("content") if isinstance(block, dict) else None
    from_logprobs: list[int] = []
    logprobs: list[float] = []
    for entry in entries or []:
        token = entry.get("token") if isinstance(entry, dict) else None
        value = entry.get("logprob") if isinstance(entry, dict) else None
        if not isinstance(value, int | float) or isinstance(value, bool):
            raise ModelError(f"a log-probability entry has no numeric logprob: {entry!r:.200}")
        logprobs.append(float(value))
        if isinstance(token, str) and token.startswith("token_id:"):
            try:
                from_logprobs.append(int(token.removeprefix("token_id:")))
            except ValueError as exc:
                raise ModelError(
                    f"a log-probability token is not token_id:<int>: {token!r}"
                ) from exc
    returned = choice.get("token_ids")
    if returned is None:
        returned = choice.get("response_token_ids")  # SGLang
    if returned is None and isinstance(sglext.get("output_ids"), list):
        per_choice = sglext["output_ids"]
        index = choice.get("index", 0) if isinstance(choice.get("index", 0), int) else 0
        if 0 <= index < len(per_choice):
            returned = per_choice[index]
    completion = _int_list(returned, "choice.token_ids") if returned is not None else from_logprobs
    if returned is not None and len(from_logprobs) == len(completion) and returned != from_logprobs:
        raise ModelError("the response's token_ids disagree with its log-probability tokens")
    if logprobs and len(logprobs) != len(completion):
        raise ModelError(
            f"the response has {len(logprobs)} log-probabilities for {len(completion)} tokens"
        )
    usage = payload.get("usage") or {}
    # A turn that generated anything (text, a tool call, or tokens the server counted)
    # has completion tokens and log-probabilities to return.
    generated = bool(
        message.get("content")
        or message.get("tool_calls")
        or (usage.get("completion_tokens") or 0) > 0
    )
    missing = []
    if prompt is None:
        missing.append("prompt_token_ids")
    if not completion and generated:
        missing.append("completion_token_ids")
    if not logprobs and generated:
        missing.append("logprobs")
    return TokenTrace(
        prompt_token_ids=_int_list(prompt, "prompt_token_ids") if prompt is not None else [],
        completion_token_ids=completion,
        logprobs=logprobs,
        missing=missing,
    )


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
    # Reasoning tokens, where the provider reports them (a part of the output).
    reasoning_tokens: int = 0
    # The model that served the turn, when it is not the client's own (a
    # fallback), and the one it replaced when this turn switched models.
    model: str = ""
    switched_from: str = ""
    # The turn's tokens and log-probabilities, when the client was asked for
    # them (``TargetConfig.token_trace``); None otherwise, and for a turn
    # served by a model that cannot (a fallback).
    tokens: TokenTrace | None = None
    # The model as the provider named it in the response (a dated snapshot, say),
    # and OpenAI's system_fingerprint: what a determinism record keeps.
    response_model: str = ""
    system_fingerprint: str = ""
    # Anthropic: the response's content blocks exactly as returned, when they
    # hold blocks this client has no field for (server tool calls and results).
    # They are sent back as they are: a paused turn continues from them.
    raw_content: list[dict[str, Any]] = field(default_factory=list)
    # Another model's reasoning in the messages this turn was sent, and what was
    # done with it (see fallback.py), and the broken streams this turn resumed.
    handoff: dict[str, Any] = field(default_factory=dict)
    stream_resumes: list[dict[str, Any]] = field(default_factory=list)

    @property
    def truncated(self) -> bool:
        """The model hit its output limit, so a tool call may be cut off."""
        return self.stop_reason in ("max_tokens", "length")

    def as_message(self) -> Message:
        msg: Message = {"role": "assistant", "content": self.text}
        if self.thinking:
            msg["thinking_blocks"] = self.thinking
        if self.raw_content:
            msg["anthropic_content"] = self.raw_content
        if (self.thinking or self.raw_content) and self.model:
            # Whose provider-specific blocks these are (see fallback.py).
            msg["thinking_by"] = self.model
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


def thinking_text(blocks: list[dict[str, Any]], limit: int = 8000) -> str:
    """The readable part of reasoning blocks (not signatures or encrypted
    content), for the trace."""
    parts: list[str] = []
    for block in blocks:
        if block.get("type") == "thinking":
            parts.append(str(block.get("thinking") or ""))
        elif block.get("type") == "reasoning":
            parts.extend(str(x.get("text") or "") for x in block.get("summary") or [])
    return "\n".join(p for p in parts if p)[:limit]


class ChatModel(Protocol):
    name: str

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn: ...

    async def aclose(self) -> None: ...


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
            "messages": ([{"role": "system", "content": system}] if system else [])
            + [out for m in messages for out in to_openai_all(m)],
            "max_tokens": max_tokens or self.config.max_tokens,
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
        if self.config.token_trace:
            body.update(token_trace_request(self.config.token_trace_server, self.config.stream))
        start = time.perf_counter()
        resumes: list[dict[str, Any]] = []
        if self.config.stream:
            body["stream"] = True
            body["stream_options"] = {"include_usage": True}
            payload, resumes = await collect_resuming(
                collect_chat, self._events, body, self.config.stream_resume_attempts
            )
        else:
            payload = await post_json(self._client, self._url, body, self._headers, self._gate)
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
        finish = choice.get("finish_reason") or ""
        refusal = message.get("refusal") or ""
        if refusal or finish == "content_filter":
            # The model declined, or the provider's filter stopped it: not a normal answer.
            finish = "refusal"
        return ModelTurn(
            text=message.get("content") or refusal,
            tool_calls=calls,
            usage=Usage(
                input_tokens=usage.get("prompt_tokens"),
                output_tokens=usage.get("completion_tokens"),
                latency_ms=(time.perf_counter() - start) * 1000,
            ),
            stop_reason=finish,
            # A part of prompt_tokens, as in Anthropic's normalized total.
            cache_read_tokens=int(details.get("cached_tokens") or 0),
            tokens=token_trace(payload, choice) if self.config.token_trace else None,
            response_model=str(payload.get("model") or ""),
            system_fingerprint=str(payload.get("system_fingerprint") or ""),
            stream_resumes=resumes,
        )

    def _events(self, body: dict[str, Any]) -> Any:
        return stream_events(self._client, self._url, body, self._headers, self._gate)

    async def aclose(self) -> None:
        await self._client.aclose()


# What an assistant message carries for its own provider only.
PROVIDER_KEYS = frozenset({"thinking_blocks", "thinking_by", "anthropic_content"})


def to_openai_all(message: Message) -> list[Message]:
    """``to_openai`` plus, for a tool result that carries images, the user message that
    shows them: chat-completions tool messages are text only."""
    out = [to_openai(message)]
    images = message.get("images") if message.get("role") == "tool" else None
    if images:
        parts: list[dict[str, Any]] = [{"type": "text", "text": "Image(s) from the tool result:"}]
        parts += [
            {
                "type": "image_url",
                "image_url": {"url": f"data:{i['media_type']};base64,{i['data']}"},
            }
            for i in images
        ]
        out.append({"role": "user", "content": parts})
    return out


def to_openai(message: Message) -> Message:
    """Drops the fields chat completions does not know (a tool result's is_error,
    an assistant turn's thinking_blocks)."""
    if message.get("role") == "assistant" and PROVIDER_KEYS & message.keys():
        return {k: v for k, v in message.items() if k not in PROVIDER_KEYS}
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
            block: dict[str, Any] = {
                "type": "tool_result",
                "tool_use_id": msg["tool_call_id"],
                "content": msg.get("content") or "",
            }
            if msg.get("images"):
                block["content"] = [
                    {"type": "text", "text": msg.get("content") or "(no text)"},
                    *(
                        {
                            "type": "image",
                            "source": {
                                "type": "base64",
                                "media_type": i["media_type"],
                                "data": i["data"],
                            },
                        }
                        for i in msg["images"]
                    ),
                ]
            if msg.get("is_error"):
                block["is_error"] = True
            if out and out[-1]["role"] == "user" and isinstance(out[-1]["content"], list):
                out[-1]["content"].append(block)
            else:
                out.append({"role": "user", "content": [block]})
        elif role == "assistant":
            blocks: list[dict[str, Any]]
            if msg.get("anthropic_content"):
                # Server tool calls and results: sent back exactly as they came.
                blocks = list(msg["anthropic_content"])
            else:
                blocks = list(msg.get("thinking_blocks") or [])
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
            if out and out[-1]["role"] == "assistant" and isinstance(out[-1]["content"], list):
                # A turn the API paused (pause_turn) and its continuation are one turn.
                out[-1]["content"].extend(blocks)
            else:
                out.append({"role": "assistant", "content": blocks or ""})
        elif role == "user":
            text = msg.get("content") or ""
            if out and out[-1]["role"] == "user" and isinstance(out[-1]["content"], list):
                # Text that follows tool results goes in the same user turn.
                out[-1]["content"].append({"type": "text", "text": text})
            else:
                out.append({"role": "user", "content": text})
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
            "max_tokens": max_tokens or self.config.max_tokens,
            "messages": with_cache_breakpoint(to_anthropic(messages)),
        }
        if system:
            body["system"] = [{"type": "text", "text": system, "cache_control": EPHEMERAL}]
        if tools or self.config.server_tools:
            body["tools"] = [
                {"name": t.name, "description": t.description, "input_schema": t.input_schema}
                for t in tools
            ] + [dict(t) for t in self.config.server_tools]
            body["tools"][-1]["cache_control"] = EPHEMERAL
        if self.config.effort:
            body["output_config"] = {"effort": self.config.effort}
        thinking_on = self.config.thinking in ("adaptive", "enabled")
        if self.config.thinking == "adaptive":
            body["thinking"] = {"type": "adaptive"}
        elif self.config.thinking == "enabled":
            body["thinking"] = {"type": "enabled", "budget_tokens": self.config.thinking_budget}
            # The budget must stay below the limit, also when the limit is raised.
            body["max_tokens"] = max(body["max_tokens"], self.config.thinking_budget + 1)
        elif self.config.thinking == "disabled":
            body["thinking"] = {"type": "disabled"}
        # The API refuses a temperature while the model is thinking.
        if self.config.temperature is not None and not thinking_on:
            body["temperature"] = self.config.temperature
        start = time.perf_counter()
        resumes: list[dict[str, Any]] = []
        if self.config.stream:
            body["stream"] = True
            payload, resumes = await collect_resuming(
                collect_anthropic,
                lambda sent: stream_events(
                    self._client, self._url, sent, self._headers, self._gate
                ),
                body,
                self.config.stream_resume_attempts,
                # A prefill cannot follow thinking; the current models refuse one anyway.
                prefill=self.config.stream_resume_mode == "prefill" and not thinking_on,
            )
        else:
            payload = await post_json(self._client, self._url, body, self._headers, self._gate)
        text, calls, thinking = [], [], []
        content = payload.get("content") or []
        for block in content:
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
            response_model=str(payload.get("model") or ""),
            # Server tool calls and results have no field of their own: the whole
            # content goes back as it came (and a paused turn continues from it).
            raw_content=list(content)
            if any(b.get("type") not in OWN_BLOCKS for b in content)
            else [],
            stream_resumes=resumes,
        )

    async def aclose(self) -> None:
        await self._client.aclose()


# Anthropic content blocks the client maps to fields of its own.
OWN_BLOCKS = frozenset({"text", "tool_use", "thinking", "redacted_thinking"})


class NamedModel:
    """A model for a role apart from the agent's own turns (compaction, sub-agents):
    every turn it serves names it (``ModelTurn.model``), so the step, the spend and
    a recording say which model it was, and a replay knows without the model."""

    def __init__(self, inner: ChatModel) -> None:
        self.inner = inner
        self.name = inner.name

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        turn = await self.inner.complete(messages, tools, system=system, max_tokens=max_tokens)
        turn.model = turn.model or self.name
        return turn

    async def aclose(self) -> None:
        await self.inner.aclose()


def create_model(config: TargetConfig) -> ChatModel:
    model: ChatModel
    if config.connector == "anthropic":
        model = AnthropicChat(config)
    elif config.api == "responses":
        from evalsi_harness.responses import OpenAIResponses

        model = OpenAIResponses(config)
    else:
        model = OpenAIChat(config)
    if config.fallbacks:
        from evalsi_harness.fallback import FallbackModel

        return FallbackModel(
            [model, *(create_model(replace(f, fallbacks=())) for f in config.fallbacks)],
            after=config.fallback_after or 2,
            handoff=config.reasoning_handoff or "drop",
        )
    return model

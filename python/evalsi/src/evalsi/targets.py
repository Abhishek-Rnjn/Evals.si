"""Targets: the systems under test that a run sends records to.

Two connectors ship in Phase 1:

- ``openai-compatible``: any chat-completions server (vLLM, SGLang, Ollama,
  LiteLLM, AI gateways, OpenAI itself).
- ``anthropic``: Claude through the official ``anthropic`` SDK.

A record's input becomes the user message (or the whole conversation when it
is a list of chat messages). The target's reply becomes the record's output,
with token usage and latency.

No server-side model fallback is used for targets: the model under test must
be exactly the one named in the spec.
"""

from __future__ import annotations

import asyncio
import os
import time
from dataclasses import dataclass
from typing import Any, Protocol

import httpx

from evalsi.types import Content, Record, Usage

CONNECTORS = ("openai-compatible", "anthropic")
RETRY_STATUS = {408, 409, 429, 500, 502, 503, 504}


class TargetError(RuntimeError):
    """The target failed for one record."""


NO_KEY = "none"


def api_key(api_key_env: str, default: str) -> str | None:
    """The key a connector sends: the named variable, the connector's default
    variable when none is named, and no key for ``api_key_env: none`` (a model
    server without authentication; on a server with credential grants it is
    how a target says it needs none)."""
    if api_key_env == NO_KEY:
        return None
    return os.environ.get(api_key_env or default)


@dataclass(frozen=True)
class TargetConfig:
    connector: str
    model: str
    base_url: str = ""
    api_key_env: str = ""
    system_prompt: str = ""
    max_tokens: int = 16000
    temperature: float | None = None
    effort: str = ""
    timeout_s: float = 600.0

    def __post_init__(self) -> None:
        if self.connector not in CONNECTORS:
            raise ValueError(
                f"unknown target connector {self.connector!r}; use one of {CONNECTORS}"
            )
        if not self.model:
            raise ValueError("a target needs a model")
        if self.connector == "openai-compatible" and not self.base_url:
            raise ValueError("the openai-compatible target needs base_url")

    def describe(self) -> dict[str, Any]:
        return {
            "connector": self.connector,
            "model": self.model,
            "base_url": self.base_url,
            "system_prompt": self.system_prompt,
            "max_tokens": self.max_tokens,
            "temperature": self.temperature,
            "effort": self.effort,
        }


@dataclass
class Generation:
    output: Content | None
    usage: Usage
    error: str = ""


def _messages(record: Record) -> list[dict[str, str]]:
    if record.input is None:
        raise TargetError("record has no input to send to the target")
    if record.input.messages is not None:
        return [
            {"role": m.role, "content": m.content}
            for m in record.input.messages
            if m.role != "system"
        ]
    return [{"role": "user", "content": record.input.as_text()}]


def _system(config: TargetConfig, record: Record) -> str:
    if record.input is not None and record.input.messages is not None:
        system = [m.content for m in record.input.messages if m.role == "system"]
        if system:
            return "\n\n".join(system)
    return config.system_prompt


class OpenAICompatibleTarget:
    def __init__(self, config: TargetConfig, client: httpx.AsyncClient | None = None) -> None:
        self.config = config
        self._client = client or httpx.AsyncClient(timeout=config.timeout_s)
        self._url = config.base_url.rstrip("/") + "/chat/completions"
        self._headers = {"Content-Type": "application/json"}
        key = api_key(config.api_key_env, "OPENAI_API_KEY")
        if key:
            self._headers["Authorization"] = f"Bearer {key}"

    async def generate(self, record: Record) -> Generation:
        messages = _messages(record)
        system = _system(self.config, record)
        if system:
            messages = [{"role": "system", "content": system}, *messages]
        body: dict[str, Any] = {
            "model": self.config.model,
            "messages": messages,
            "max_tokens": self.config.max_tokens,
        }
        if self.config.temperature is not None:
            body["temperature"] = self.config.temperature
        start = time.perf_counter()
        payload = await self._post(body)
        latency = (time.perf_counter() - start) * 1000
        try:
            choice = payload["choices"][0]
            text = choice["message"]["content"] or ""
        except (KeyError, IndexError, TypeError) as exc:
            raise TargetError(f"unexpected chat-completions response: {payload!r:.300}") from exc
        usage = payload.get("usage") or {}
        return Generation(
            output=Content(text=text),
            usage=Usage(
                input_tokens=usage.get("prompt_tokens"),
                output_tokens=usage.get("completion_tokens"),
                latency_ms=latency,
            ),
            error="truncated: finish_reason=length"
            if choice.get("finish_reason") == "length"
            else "",
        )

    async def _post(self, body: dict[str, Any]) -> dict[str, Any]:
        delay = 1.0
        for attempt in range(1, 5):
            try:
                response = await self._client.post(self._url, json=body, headers=self._headers)
            except httpx.TransportError as exc:
                if attempt == 4:
                    raise TargetError(f"target request failed: {exc}") from exc
            else:
                if response.status_code < 400:
                    payload: dict[str, Any] = response.json()
                    return payload
                if response.status_code not in RETRY_STATUS or attempt == 4:
                    raise TargetError(
                        f"target returned HTTP {response.status_code}: {response.text[:300]}"
                    )
            await asyncio.sleep(delay)
            delay *= 2
        raise AssertionError("unreachable")

    async def aclose(self) -> None:
        await self._client.aclose()


class AnthropicTarget:
    def __init__(self, config: TargetConfig, client: Any = None) -> None:
        self.config = config
        if client is None:
            try:
                import anthropic
            except ImportError as exc:  # pragma: no cover - depends on installed extras
                raise TargetError(
                    "the anthropic target needs the SDK: pip install 'evalsi[anthropic]'"
                ) from exc
            api_key = os.environ.get(config.api_key_env) if config.api_key_env else None
            client = anthropic.AsyncAnthropic(
                api_key=api_key, base_url=config.base_url or None, timeout=config.timeout_s
            )
        self._client = client

    async def generate(self, record: Record) -> Generation:
        import anthropic

        kwargs: dict[str, Any] = {}
        system = _system(self.config, record)
        if system:
            kwargs["system"] = system
        if self.config.effort:
            kwargs["output_config"] = {"effort": self.config.effort}
        if self.config.temperature is not None:
            kwargs["temperature"] = self.config.temperature
        start = time.perf_counter()
        try:
            message = await self._client.messages.create(
                model=self.config.model,
                max_tokens=self.config.max_tokens,
                messages=_messages(record),
                **kwargs,
            )
        except anthropic.APIStatusError as exc:
            raise TargetError(
                f"anthropic target returned HTTP {exc.status_code}: {exc.message}"
            ) from exc
        except anthropic.APIConnectionError as exc:
            raise TargetError(f"could not reach the anthropic target: {exc}") from exc
        latency = (time.perf_counter() - start) * 1000
        text = "".join(b.text for b in message.content if b.type == "text")
        usage = Usage(
            input_tokens=message.usage.input_tokens,
            output_tokens=message.usage.output_tokens,
            latency_ms=latency,
        )
        if message.stop_reason == "refusal":
            details = message.stop_details
            category = getattr(details, "category", None) if details is not None else None
            # A refusal is the target's real answer, so it is kept as the output.
            return Generation(
                output=Content(text=text), usage=usage, error=f"refusal (category: {category})"
            )
        error = "truncated: stop_reason=max_tokens" if message.stop_reason == "max_tokens" else ""
        return Generation(output=Content(text=text), usage=usage, error=error)

    async def aclose(self) -> None:
        await self._client.close()


class Target(Protocol):
    """What runs a record against the system under test; connectors and test fakes alike."""

    async def generate(self, record: Record) -> Generation: ...

    async def aclose(self) -> None: ...


def create_target(config: TargetConfig) -> OpenAICompatibleTarget | AnthropicTarget:
    if config.connector == "anthropic":
        return AnthropicTarget(config)
    return OpenAICompatibleTarget(config)


async def generate_all(
    target: Target, records: list[Record], *, concurrency: int = 8
) -> list[Generation]:
    """Run the target on every record. Per-record failures become Generation.error."""
    semaphore = asyncio.Semaphore(concurrency)

    async def one(record: Record) -> Generation:
        async with semaphore:
            try:
                return await target.generate(record)
            except TargetError as exc:
                return Generation(output=None, usage=Usage(), error=str(exc))

    return list(await asyncio.gather(*(one(r) for r in records)))

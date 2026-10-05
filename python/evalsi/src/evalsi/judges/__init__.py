"""Judge models for model-graded evaluators.

A judge is configured explicitly; there is no default model, so nothing is
ever billed by surprise. Two providers ship in Phase 0:

- ``openai-compatible``: any server that speaks the OpenAI chat-completions
  protocol (vLLM, SGLang, Ollama, LiteLLM, AI gateways, OpenAI itself).
- ``anthropic``: Claude through the official ``anthropic`` SDK
  (``pip install 'evalsi[anthropic]'``).

Every judge call goes through a content-hash cache, so re-running an
unchanged evaluation costs nothing.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi.judges.cache import JudgeCache

PROVIDERS = ("openai-compatible", "anthropic")
DEFAULT_API_KEY_ENV = {"openai-compatible": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY"}


class JudgeError(RuntimeError):
    """The judge call failed or returned something unusable for one record."""


class JudgeFatalError(JudgeError):
    """The judge is misconfigured (bad credentials, unknown model). Retrying
    other records cannot help, so the whole run stops."""


@dataclass(frozen=True)
class JudgeConfig:
    provider: str
    model: str
    base_url: str | None = None
    # Name of the environment variable holding the key; the key itself is never stored.
    api_key_env: str | None = None
    max_tokens: int = 16000
    # OpenAI-compatible only. Leave unset for Claude models, which reject sampling params.
    temperature: float | None = None
    # Anthropic only: output_config.effort ("low" ... "max").
    effort: str | None = None
    # OpenAI-compatible only: "json_schema", "json_object" or "none".
    response_format: str = "json_schema"
    timeout_s: float = 120.0

    def __post_init__(self) -> None:
        if self.provider not in PROVIDERS:
            raise ValueError(f"unknown judge provider {self.provider!r}; use one of {PROVIDERS}")
        if not self.model:
            raise ValueError("a judge needs an explicit model")
        if self.provider == "openai-compatible" and not self.base_url:
            raise ValueError(
                "the openai-compatible judge needs base_url, e.g. https://api.openai.com/v1 "
                "or http://localhost:8000/v1"
            )

    @classmethod
    def from_env(cls) -> JudgeConfig | None:
        """Read ``EVALSI_JUDGE_*`` environment variables; ``None`` if no model is set."""
        model = os.environ.get("EVALSI_JUDGE_MODEL")
        if not model:
            return None
        return cls(
            provider=os.environ.get("EVALSI_JUDGE_PROVIDER", "openai-compatible"),
            model=model,
            base_url=os.environ.get("EVALSI_JUDGE_BASE_URL") or None,
            api_key_env=os.environ.get("EVALSI_JUDGE_API_KEY_ENV") or None,
            effort=os.environ.get("EVALSI_JUDGE_EFFORT") or None,
        )

    @property
    def key_env(self) -> str:
        return self.api_key_env or DEFAULT_API_KEY_ENV[self.provider]

    def describe(self) -> dict[str, Any]:
        """Everything that identifies the judge, without secrets. Used for the
        run manifest and the cache key."""
        return {
            "provider": self.provider,
            "model": self.model,
            "base_url": self.base_url,
            "max_tokens": self.max_tokens,
            "temperature": self.temperature,
            "effort": self.effort,
            "response_format": self.response_format,
        }


@dataclass
class JudgeResponse:
    data: dict[str, Any]
    model: str
    input_tokens: int | None = None
    output_tokens: int | None = None
    cached: bool = False
    raw: dict[str, Any] = field(default_factory=dict)


class JudgeBackend(Protocol):
    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse: ...

    async def aclose(self) -> None: ...


class JudgeClient:
    """What evaluators use: a backend plus the cache."""

    def __init__(
        self, config: JudgeConfig, backend: JudgeBackend, cache: JudgeCache | None = None
    ) -> None:
        self.config = config
        self.backend = backend
        self.cache = cache

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        key = _cache_key(self.config, system, prompt, schema)
        if self.cache is not None:
            hit = self.cache.get(key)
            if hit is not None:
                return JudgeResponse(data=hit["data"], model=hit["model"], cached=True)
        response = await self.backend.complete_json(system=system, prompt=prompt, schema=schema)
        if self.cache is not None:
            self.cache.put(key, {"data": response.data, "model": response.model})
        return response

    async def aclose(self) -> None:
        await self.backend.aclose()
        if self.cache is not None:
            self.cache.close()


def _cache_key(config: JudgeConfig, system: str, prompt: str, schema: dict[str, Any]) -> str:
    payload = json.dumps(
        {"judge": config.describe(), "system": system, "prompt": prompt, "schema": schema},
        sort_keys=True,
        ensure_ascii=False,
    )
    return hashlib.sha256(payload.encode()).hexdigest()


def create_judge(config: JudgeConfig, *, cache: JudgeCache | None = None) -> JudgeClient:
    backend: JudgeBackend
    if config.provider == "anthropic":
        from evalsi.judges.anthropic import AnthropicJudge

        backend = AnthropicJudge(config)
    else:
        from evalsi.judges.openai_compatible import OpenAICompatibleJudge

        backend = OpenAICompatibleJudge(config)
    return JudgeClient(config, backend, cache)


def parse_json_object(text: str) -> dict[str, Any]:
    """Parse a JSON object from model text, tolerating code fences and prose
    around it (servers without structured output support)."""
    text = text.strip()
    try:
        value = json.loads(text)
    except json.JSONDecodeError:
        start, end = text.find("{"), text.rfind("}")
        if start == -1 or end <= start:
            raise JudgeError(f"judge did not return JSON: {text[:200]!r}") from None
        try:
            value = json.loads(text[start : end + 1])
        except json.JSONDecodeError as exc:
            raise JudgeError(f"judge returned malformed JSON: {text[:200]!r}") from exc
    if not isinstance(value, dict):
        raise JudgeError(f"judge returned JSON that is not an object: {text[:200]!r}")
    return value

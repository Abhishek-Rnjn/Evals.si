"""Judge backend for servers that speak the OpenAI chat-completions protocol."""

from __future__ import annotations

import asyncio
import os
from typing import Any

import httpx

from evalsi.judges import (
    JudgeConfig,
    JudgeError,
    JudgeFatalError,
    JudgeResponse,
    parse_json_object,
)

RETRY_STATUS = {408, 409, 429, 500, 502, 503, 504}
FATAL_STATUS = {401, 403, 404}
MAX_ATTEMPTS = 4


class OpenAICompatibleJudge:
    def __init__(self, config: JudgeConfig, client: httpx.AsyncClient | None = None) -> None:
        self.config = config
        headers = {"Content-Type": "application/json"}
        api_key = os.environ.get(config.key_env)
        if api_key:
            headers["Authorization"] = f"Bearer {api_key}"
        self._client = client or httpx.AsyncClient(timeout=config.timeout_s)
        self._headers = headers
        self._url = f"{(config.base_url or '').rstrip('/')}/chat/completions"

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        body: dict[str, Any] = {
            "model": self.config.model,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": prompt},
            ],
            "max_tokens": self.config.max_tokens,
        }
        if self.config.temperature is not None:
            body["temperature"] = self.config.temperature
        if self.config.response_format == "json_schema":
            body["response_format"] = {
                "type": "json_schema",
                "json_schema": {"name": "judgement", "schema": schema, "strict": True},
            }
        elif self.config.response_format == "json_object":
            body["response_format"] = {"type": "json_object"}

        payload = await self._post(body)
        try:
            choice = payload["choices"][0]
            text = choice["message"]["content"] or ""
        except (KeyError, IndexError, TypeError) as exc:
            raise JudgeError(f"unexpected chat-completions response: {payload!r:.300}") from exc
        if choice.get("finish_reason") == "length":
            raise JudgeError("judge output was truncated (finish_reason=length); raise max_tokens")
        usage = payload.get("usage") or {}
        return JudgeResponse(
            data=parse_json_object(text),
            model=str(payload.get("model", self.config.model)),
            input_tokens=usage.get("prompt_tokens"),
            output_tokens=usage.get("completion_tokens"),
        )

    async def _post(self, body: dict[str, Any]) -> dict[str, Any]:
        delay = 1.0
        for attempt in range(1, MAX_ATTEMPTS + 1):
            try:
                response = await self._client.post(self._url, json=body, headers=self._headers)
            except httpx.TransportError as exc:
                if attempt == MAX_ATTEMPTS:
                    raise JudgeError(f"judge request failed: {exc}") from exc
            else:
                if response.status_code < 400:
                    payload: dict[str, Any] = response.json()
                    return payload
                if response.status_code in FATAL_STATUS:
                    raise JudgeFatalError(
                        f"judge is misconfigured: HTTP {response.status_code}: "
                        f"{response.text[:300]}"
                    )
                if response.status_code not in RETRY_STATUS or attempt == MAX_ATTEMPTS:
                    raise JudgeError(
                        f"judge returned HTTP {response.status_code}: {response.text[:300]}"
                    )
                retry_after = response.headers.get("retry-after", "")
                if retry_after.replace(".", "", 1).isdigit():
                    delay = max(delay, float(retry_after))
            await asyncio.sleep(delay)
            delay *= 2
        raise AssertionError("unreachable")

    async def aclose(self) -> None:
        await self._client.aclose()

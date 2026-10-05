"""Judge backend for Claude, through the official ``anthropic`` SDK."""

from __future__ import annotations

import os
from typing import Any

from evalsi.judges import JudgeConfig, JudgeError, JudgeFatalError, JudgeResponse, parse_json_object


class AnthropicJudge:
    def __init__(self, config: JudgeConfig, client: Any = None) -> None:
        self.config = config
        if client is None:
            try:
                import anthropic
            except ImportError as exc:  # pragma: no cover - depends on installed extras
                raise JudgeFatalError(
                    "the anthropic judge needs the SDK: pip install 'evalsi[anthropic]'"
                ) from exc
            # With no explicit key the SDK resolves credentials itself
            # (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN or an `ant auth login` profile).
            api_key = os.environ.get(config.api_key_env) if config.api_key_env else None
            client = anthropic.AsyncAnthropic(
                api_key=api_key, base_url=config.base_url, timeout=config.timeout_s
            )
        self._client = client

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        import anthropic

        output_config: dict[str, Any] = {"format": {"type": "json_schema", "schema": schema}}
        if self.config.effort:
            output_config["effort"] = self.config.effort
        kwargs: dict[str, Any] = {}
        if self.config.temperature is not None:
            kwargs["temperature"] = self.config.temperature
        # No server-side model fallback on purpose: a judge that silently switches
        # models would make scores incomparable across records and runs.
        try:
            message = await self._client.messages.create(
                model=self.config.model,
                max_tokens=self.config.max_tokens,
                system=system,
                messages=[{"role": "user", "content": prompt}],
                output_config=output_config,
                **kwargs,
            )
        except (
            anthropic.AuthenticationError,
            anthropic.PermissionDeniedError,
            anthropic.NotFoundError,
        ) as exc:
            raise JudgeFatalError(f"anthropic judge is misconfigured: {exc.message}") from exc
        except anthropic.RateLimitError as exc:
            raise JudgeError(
                f"anthropic judge is rate limited after retries: {exc.message}"
            ) from exc
        except anthropic.APIStatusError as exc:
            raise JudgeError(
                f"anthropic judge returned HTTP {exc.status_code}: {exc.message}"
            ) from exc
        except anthropic.APIConnectionError as exc:
            raise JudgeError(f"could not reach the anthropic judge: {exc}") from exc

        if message.stop_reason == "refusal":
            details = message.stop_details
            category = getattr(details, "category", None) if details is not None else None
            raise JudgeError(f"judge declined to grade this record (category: {category})")
        if message.stop_reason == "max_tokens":
            raise JudgeError("judge output was truncated (max_tokens); raise max_tokens")
        text = next((b.text for b in message.content if b.type == "text"), None)
        if text is None:
            raise JudgeError("judge response had no text block")
        return JudgeResponse(
            data=parse_json_object(text),
            model=message.model,
            input_tokens=message.usage.input_tokens,
            output_tokens=message.usage.output_tokens,
        )

    async def aclose(self) -> None:
        await self._client.close()

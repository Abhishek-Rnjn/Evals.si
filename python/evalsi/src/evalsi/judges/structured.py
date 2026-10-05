"""Structured judge calls for adapters that bring their own prompts.

Frameworks such as DeepEval and RAGAS ask their model for a pydantic object.
``JudgeSession`` answers those requests through the run's judge, so the calls
are cached, rate-limited and costed like our own, and tallies what one
evaluation spent.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from evalsi.judges import JudgeClient, JudgeError
from evalsi.types import Usage

ADAPTER_SYSTEM = (
    "You are an evaluation judge. Follow the instructions in the user message and reply "
    "with JSON that matches the requested schema."
)

_DROP = frozenset({"title", "default", "examples"})
_NAMED = frozenset({"properties", "$defs", "definitions"})


def strict_json_schema(schema: dict[str, Any]) -> dict[str, Any]:
    """Adapt a JSON schema, such as pydantic's, to strict structured output.

    Every object with properties is closed and lists all of them as required
    (optional fields stay nullable where the schema allows null), and the
    annotation keywords strict mode rejects are dropped.
    """

    def walk(node: Any) -> Any:
        if isinstance(node, list):
            return [walk(item) for item in node]
        if not isinstance(node, dict):
            return node
        out: dict[str, Any] = {}
        for key, value in node.items():
            if key in _DROP:
                continue
            if key in _NAMED and isinstance(value, dict):
                out[key] = {name: walk(sub) for name, sub in value.items()}
            else:
                out[key] = walk(value)
        if out.get("type") == "object" and isinstance(out.get("properties"), dict):
            out["additionalProperties"] = False
            out["required"] = list(out["properties"])
        return out

    result: dict[str, Any] = walk(schema)
    return result


@dataclass
class JudgeSession:
    """One evaluation's judge calls: answers structured requests and tallies usage."""

    judge: JudgeClient
    system: str = ADAPTER_SYSTEM
    calls: int = 0
    cached_calls: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    models: set[str] = field(default_factory=set)

    async def complete(self, prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        response = await self.judge.complete_json(
            system=self.system, prompt=prompt, schema=strict_json_schema(schema)
        )
        self.calls += 1
        self.models.add(response.model)
        if response.cached:
            self.cached_calls += 1
        else:
            self.input_tokens += response.input_tokens or 0
            self.output_tokens += response.output_tokens or 0
        return response.data

    async def complete_model(self, prompt: str, model: Any) -> Any:
        """Ask for an instance of the pydantic class ``model``."""
        data = await self.complete(prompt, model.model_json_schema())
        try:
            return model.model_validate(data)
        except Exception as exc:  # pydantic.ValidationError, without importing pydantic
            raise JudgeError(f"judge reply does not match {model.__name__}: {exc}") from exc

    def cost(self) -> Usage | None:
        if self.calls == self.cached_calls:
            return None
        return Usage(input_tokens=self.input_tokens, output_tokens=self.output_tokens)

    def metadata(self) -> dict[str, Any]:
        return {
            "judge_model": ", ".join(sorted(self.models)),
            "judge_calls": self.calls,
            "cached": self.calls > 0 and self.calls == self.cached_calls,
        }

"""Helpers for testing evaluators and adapters without a real judge.

``scripted_judge`` builds an ``EvalContext`` whose judge answers each call
with ``reply(prompt, schema)`` and records the calls, so judge-based
evaluators, including adapters' framework metrics, run offline::

    ctx, judge = scripted_judge(lambda prompt, schema: {"score": 5, "reasoning": "ok"})
    scores = asyncio.run(my_evaluator.bind().run(record, ctx))
"""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from evalsi.evaluator import EvalContext, EvaluatorDef
from evalsi.judges import JudgeClient, JudgeConfig, JudgeFatalError, JudgeResponse
from evalsi.types import Record, Score

Reply = Callable[[str, dict[str, Any]], dict[str, Any]]
# prompt -> the first token's top logprobs (token -> log probability).
Logprobs = Callable[[str], dict[str, float]]

TEST_JUDGE = JudgeConfig(provider="openai-compatible", model="scripted", base_url="http://judge")


@dataclass
class ScriptedJudge:
    """A judge backend that answers from a function and records each call."""

    reply: Reply
    # Answers next_token_logprobs; without it the judge has no logprobs, like Claude.
    logprobs: Logprobs | None = None
    calls: list[tuple[str, dict[str, Any]]] = field(default_factory=list)

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        self.calls.append((prompt, schema))
        return JudgeResponse(
            data=self.reply(prompt, schema), model="scripted", input_tokens=10, output_tokens=5
        )

    async def next_token_logprobs(
        self, *, system: str, prompt: str, top_logprobs: int
    ) -> JudgeResponse:
        if self.logprobs is None:
            raise JudgeFatalError("the scripted judge has no logprobs")
        self.calls.append((prompt, {"top_logprobs": top_logprobs}))
        return JudgeResponse(
            data={"top_logprobs": self.logprobs(prompt)},
            model="scripted",
            input_tokens=10,
            output_tokens=1,
        )

    async def aclose(self) -> None:
        return None


def scripted_judge(
    reply: Reply, logprobs: Logprobs | None = None
) -> tuple[EvalContext, ScriptedJudge]:
    backend = ScriptedJudge(reply, logprobs)
    return EvalContext(judge=JudgeClient(TEST_JUDGE, backend)), backend


def score(
    definition: EvaluatorDef,
    target: Record | list[Record],
    ctx: EvalContext | None = None,
    **params: Any,
) -> dict[str, Score]:
    """Run an evaluator on one record (or a dataset) and key its scores by name."""
    scores = asyncio.run(definition.bind(params).run(target, ctx or EvalContext()))
    return {s.name: s for s in scores}

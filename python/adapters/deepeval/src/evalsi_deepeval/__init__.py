"""DeepEval metrics as Evals.si evaluators.

Each metric runs on DeepEval's own prompts and scoring, but its model calls go
through the run's judge (``JudgeModel``), so they are cached, rate-limited and
costed like every other judge call, and no provider key is read behind your
back. DeepEval is imported only when a metric runs, so installing the adapter
does not slow down ``evalsi`` itself.

Record fields map onto ``LLMTestCase`` as: input -> input, output ->
actual_output, reference -> expected_output, context -> context and
retrieval_context, trajectory tool calls -> tools_called, and the reference's
``tool_calls`` -> expected_tools.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from evalsi.evaluator import (
    EvalContext,
    EvaluatorDef,
    EvaluatorSpec,
    MetricSpec,
    Requirements,
    Scope,
    ScoreType,
    SkipRecord,
)
from evalsi.judges import JudgeError
from evalsi.judges.structured import JudgeSession
from evalsi.packs.agent import expected_calls
from evalsi.registry import Pack
from evalsi.types import Record, Score

if TYPE_CHECKING:
    from deepeval.metrics import BaseMetric

__version__ = "0.1.0"
UPSTREAM = "deepeval==4.2.8"


def _quiet_deepeval() -> None:
    # No telemetry and no surprise .env loading inside a client's environment.
    os.environ.setdefault("DEEPEVAL_TELEMETRY_OPT_OUT", "1")
    os.environ.setdefault("DEEPEVAL_DISABLE_DOTENV", "1")


def judge_model(session: JudgeSession) -> Any:
    """A ``DeepEvalBaseLLM`` that answers through ``session``."""
    _quiet_deepeval()
    from deepeval.models import DeepEvalBaseLLM

    class JudgeModel(DeepEvalBaseLLM):  # type: ignore[no-untyped-call]
        def __init__(self) -> None:
            super().__init__(model="evalsi-judge")

        def load_model(self) -> JudgeModel:
            return self

        def get_model_name(self) -> str:
            return f"evalsi judge ({session.judge.config.model})"

        def generate(self, prompt: str, schema: Any = None) -> Any:
            raise JudgeError("the Evals.si judge model is async only; run metrics with a_measure")

        async def a_generate(self, prompt: str, schema: Any = None) -> Any:
            if schema is not None:
                return await session.complete_model(prompt, schema)
            data = await session.complete(
                prompt,
                {
                    "type": "object",
                    "properties": {"response": {"type": "string"}},
                    "required": ["response"],
                },
            )
            return str(data.get("response", ""))

    return JudgeModel()


@dataclass(frozen=True)
class MetricInfo:
    slug: str
    cls: str
    description: str
    fields: tuple[str, ...]
    higher_is_better: bool = True


# Fields are the record fields a metric reads (DeepEval's required params).
METRICS = (
    MetricInfo(
        "faithfulness",
        "FaithfulnessMetric",
        "Share of claims in the output that the retrieved context supports.",
        ("input", "output", "context"),
    ),
    MetricInfo(
        "answer-relevancy",
        "AnswerRelevancyMetric",
        "Share of statements in the output that are relevant to the input.",
        ("input", "output"),
    ),
    MetricInfo(
        "contextual-precision",
        "ContextualPrecisionMetric",
        "Whether relevant retrieved chunks are ranked above irrelevant ones.",
        ("input", "output", "reference", "context"),
    ),
    MetricInfo(
        "contextual-recall",
        "ContextualRecallMetric",
        "Share of the expected output that the retrieved context supports.",
        ("input", "reference", "context"),
    ),
    MetricInfo(
        "contextual-relevancy",
        "ContextualRelevancyMetric",
        "Share of retrieved statements relevant to the input.",
        ("input", "output", "context"),
    ),
    MetricInfo(
        "hallucination",
        "HallucinationMetric",
        "Share of the given context that the output contradicts.",
        ("input", "output", "context"),
        higher_is_better=False,
    ),
    MetricInfo(
        "bias",
        "BiasMetric",
        "Share of opinions in the output that are biased.",
        ("input", "output"),
        higher_is_better=False,
    ),
    MetricInfo(
        "toxicity",
        "ToxicityMetric",
        "Share of opinions in the output that are toxic.",
        ("input", "output"),
        higher_is_better=False,
    ),
    MetricInfo(
        "summarization",
        "SummarizationMetric",
        "How well the output summarizes the input: alignment and coverage.",
        ("input", "output"),
    ),
    MetricInfo(
        "tool-correctness",
        "ToolCorrectnessMetric",
        "Whether the agent called the expected tools (from the reference's tool_calls).",
        ("input", "trajectory", "reference"),
    ),
    MetricInfo(
        "argument-correctness",
        "ArgumentCorrectnessMetric",
        "Whether the agent's tool-call arguments fit the input.",
        ("input", "trajectory"),
    ),
)


def _texts(record: Record) -> list[str]:
    return [c.as_text() for c in record.context]


def _tool_calls(record: Record) -> list[Any]:
    from deepeval.test_case import ToolCall

    assert record.trajectory is not None
    calls = []
    for use in record.trajectory.tool_uses():
        try:
            args = json.loads(use.arguments) if use.arguments else None
        except json.JSONDecodeError:
            args = {"input": use.arguments}
        calls.append(
            ToolCall(
                name=use.name,
                input_parameters=args if isinstance(args, dict) else None,
                output=use.error or use.result or None,
            )
        )
    return calls


def _expected_tools(record: Record) -> list[Any]:
    from deepeval.test_case import ToolCall

    return [ToolCall(name=n, input_parameters=a) for n, a in expected_calls(record)]


def to_test_case(record: Record, fields: tuple[str, ...] = ()) -> Any:
    """The ``LLMTestCase`` for a record; ``fields`` adds the agent fields."""
    _quiet_deepeval()
    from deepeval.test_case import LLMTestCase

    kwargs: dict[str, Any] = {
        "input": record.input.as_text() if record.input else "",
        "actual_output": record.output.as_text() if record.output else None,
        "expected_output": record.reference.as_text() if record.reference else None,
        "context": _texts(record) or None,
        "retrieval_context": _texts(record) or None,
        "name": record.id,
    }
    if "trajectory" in fields:
        kwargs["tools_called"] = _tool_calls(record)
    if "reference" in fields and "trajectory" in fields:
        kwargs["expected_output"] = None
        kwargs["expected_tools"] = _expected_tools(record)
    return LLMTestCase(**kwargs)


async def measure(
    metric: BaseMetric, record: Record, session: JudgeSession, fields: tuple[str, ...] = ()
) -> Score:
    await metric.a_measure(to_test_case(record, fields), _show_indicator=False)
    if metric.score is None:
        raise JudgeError(metric.error or "DeepEval returned no score")
    return Score(
        number=float(metric.score),
        explanation=metric.reason or "",
        cost=session.cost(),
        metadata={
            **session.metadata(),
            "success": bool(metric.success),
            "threshold": metric.threshold,
            "upstream": UPSTREAM,
        },
    )


def _session(ctx: EvalContext) -> JudgeSession:
    if ctx.judge is None:
        raise JudgeError("DeepEval metrics need a judge; none is configured")
    return JudgeSession(ctx.judge)


def _requirements(fields: tuple[str, ...]) -> Requirements:
    return Requirements(
        input="input" in fields,
        output="output" in fields,
        reference="reference" in fields,
        context="context" in fields,
        trajectory="trajectory" in fields,
        judge=True,
    )


def _metric_evaluator(info: MetricInfo) -> EvaluatorDef:
    async def run(
        record: Record,
        *,
        ctx: EvalContext,
        threshold: float = 0.5,
        strict_mode: bool = False,
        include_reason: bool = True,
        options: dict[str, Any] | None = None,
    ) -> Score:
        _quiet_deepeval()
        import deepeval.metrics

        session = _session(ctx)
        cls = getattr(deepeval.metrics, info.cls)
        metric = cls(
            model=judge_model(session),
            threshold=threshold,
            strict_mode=strict_mode,
            include_reason=include_reason,
            async_mode=True,
            **(options or {}),
        )
        return await measure(metric, record, session, info.fields)

    spec = EvaluatorSpec(
        name=f"deepeval/{info.slug}",
        version=__version__,
        description=f"{info.description} DeepEval {info.cls}, judged by the run's judge.",
        scope=Scope.RECORD,
        requires=_requirements(info.fields),
        outputs=(
            MetricSpec(
                info.slug,
                ScoreType.NUMBER,
                min=0.0,
                max=1.0,
                higher_is_better=info.higher_is_better,
            ),
        ),
        params={"threshold": 0.5, "strict_mode": False, "include_reason": True, "options": None},
    )
    return EvaluatorDef(spec=spec, fn=run, wants_ctx=True)


_GEVAL_FIELDS = {
    "input": "input",
    "actual_output": "output",
    "expected_output": "reference",
    "context": "context",
    "retrieval_context": "context",
}


async def _g_eval(
    record: Record,
    *,
    ctx: EvalContext,
    criteria: str | None = None,
    evaluation_steps: list[str] | None = None,
    evaluation_params: list[str] | None = None,
    name: str = "g-eval",
    threshold: float = 0.5,
    strict_mode: bool = False,
) -> Score:
    if not criteria and not evaluation_steps:
        raise ValueError("deepeval/g-eval needs criteria or evaluation_steps")
    params = evaluation_params or ["input", "actual_output"]
    unknown = sorted(set(params) - set(_GEVAL_FIELDS))
    if unknown:
        raise ValueError(f"unknown evaluation_params {unknown}; use {sorted(_GEVAL_FIELDS)}")
    for param in params:
        field = _GEVAL_FIELDS[param]
        value = getattr(record, field)
        if not value:
            raise SkipRecord(f"record has no {field} (needed for {param})")
    _quiet_deepeval()
    from deepeval.metrics import GEval
    from deepeval.test_case import SingleTurnParams

    session = _session(ctx)
    metric = GEval(
        name=name,
        criteria=criteria,
        evaluation_steps=evaluation_steps,
        evaluation_params=[SingleTurnParams(p) for p in params],
        model=judge_model(session),
        threshold=threshold,
        strict_mode=strict_mode,
        async_mode=True,
    )
    return await measure(metric, record, session)


G_EVAL = EvaluatorDef(
    spec=EvaluatorSpec(
        name="deepeval/g-eval",
        version=__version__,
        description=(
            "DeepEval G-Eval: a judge scores the record against your criteria or evaluation "
            "steps. evaluation_params picks the fields it sees (input, actual_output, "
            "expected_output, context, retrieval_context)."
        ),
        scope=Scope.RECORD,
        requires=Requirements(output=False, judge=True),
        outputs=(MetricSpec("g-eval", ScoreType.NUMBER, min=0.0, max=1.0, higher_is_better=True),),
        params={
            "criteria": None,
            "evaluation_steps": None,
            "evaluation_params": None,
            "name": "g-eval",
            "threshold": 0.5,
            "strict_mode": False,
        },
    ),
    fn=_g_eval,
    wants_ctx=True,
)

PACK = Pack(
    name="deepeval",
    description=f"DeepEval metrics ({UPSTREAM}) run through the Evals.si judge.",
    evaluators=[*(_metric_evaluator(m) for m in METRICS), G_EVAL],
)

__all__ = ["METRICS", "PACK", "judge_model", "measure", "to_test_case"]

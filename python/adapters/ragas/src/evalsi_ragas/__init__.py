"""RAGAS metrics as Evals.si evaluators.

Metrics come from ``ragas.metrics.collections`` and keep RAGAS's prompts and
scoring; their LLM calls go through the run's judge (``judge_llm``), so they
are cached, rate-limited and costed, and RAGAS never reads a provider key.
Metrics that need embeddings (answer relevancy, semantic similarity) are not
offered: the judge is a chat model.

Record fields map onto RAGAS inputs as: input -> user_input, output ->
response, context -> retrieved_contexts, reference -> reference. Agent metrics
get ``user_input`` as RAGAS messages built from the trajectory, and the
reference's ``tool_calls`` as ``reference_tool_calls``. RAGAS is imported only
when a metric runs.
"""

from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass
from typing import Any

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

__version__ = "0.1.0"
UPSTREAM = "ragas==0.4.3"


def _quiet_ragas() -> None:
    os.environ.setdefault("RAGAS_DO_NOT_TRACK", "true")


def judge_llm(session: JudgeSession) -> Any:
    """A RAGAS ``InstructorBaseRagasLLM`` that answers through ``session``."""
    _quiet_ragas()
    from ragas.llms.base import InstructorBaseRagasLLM

    class JudgeLLM(InstructorBaseRagasLLM):
        def generate(self, prompt: str, response_model: Any) -> Any:
            raise JudgeError("the Evals.si judge is async only; RAGAS metrics run with ascore")

        async def agenerate(self, prompt: str, response_model: Any) -> Any:
            return await session.complete_model(prompt, response_model)

    return JudgeLLM()


@dataclass(frozen=True)
class MetricInfo:
    slug: str
    cls: str
    description: str
    # RAGAS ascore parameters, which decide what a record must contain.
    inputs: tuple[str, ...]
    llm: bool = True
    higher_is_better: bool = True
    extra_params: tuple[str, ...] = ()


METRICS = (
    MetricInfo(
        "faithfulness",
        "Faithfulness",
        "Share of claims in the response that the retrieved contexts support.",
        ("user_input", "response", "retrieved_contexts"),
    ),
    MetricInfo(
        "context-precision",
        "ContextPrecisionWithReference",
        "Whether useful retrieved contexts rank above useless ones, judged against the reference.",
        ("user_input", "reference", "retrieved_contexts"),
    ),
    MetricInfo(
        "context-utilization",
        "ContextUtilization",
        "Context precision judged against the response instead of a reference.",
        ("user_input", "response", "retrieved_contexts"),
    ),
    MetricInfo(
        "context-recall",
        "ContextRecall",
        "Share of reference statements the retrieved contexts support.",
        ("user_input", "retrieved_contexts", "reference"),
    ),
    MetricInfo(
        "context-entity-recall",
        "ContextEntityRecall",
        "Share of entities in the reference that appear in the retrieved contexts.",
        ("reference", "retrieved_contexts"),
    ),
    MetricInfo(
        "context-relevance",
        "ContextRelevance",
        "How relevant the retrieved contexts are to the question.",
        ("user_input", "retrieved_contexts"),
    ),
    MetricInfo(
        "response-groundedness",
        "ResponseGroundedness",
        "How well the retrieved contexts ground the response.",
        ("response", "retrieved_contexts"),
    ),
    MetricInfo(
        "answer-accuracy",
        "AnswerAccuracy",
        "Agreement of the response with the reference answer.",
        ("user_input", "response", "reference"),
    ),
    MetricInfo(
        "factual-correctness",
        "FactualCorrectness",
        "Claim-level precision, recall or F1 of the response against the reference "
        "(options: mode, atomicity, coverage).",
        ("response", "reference"),
    ),
    MetricInfo(
        "noise-sensitivity",
        "NoiseSensitivity",
        "How often the response states incorrect claims drawn from retrieved contexts.",
        ("user_input", "response", "reference", "retrieved_contexts"),
        higher_is_better=False,
    ),
    MetricInfo(
        "agent-goal-accuracy",
        "AgentGoalAccuracyWithReference",
        "Whether the agent's conversation reached the outcome in the reference.",
        ("messages", "reference"),
    ),
    MetricInfo(
        "agent-goal-accuracy-without-reference",
        "AgentGoalAccuracyWithoutReference",
        "Whether the agent's conversation reached the goal the judge infers from it.",
        ("messages",),
    ),
    MetricInfo(
        "topic-adherence",
        "TopicAdherence",
        "Whether the agent stays on the allowed topics (reference_topics param or "
        "metadata['reference_topics']).",
        ("messages", "reference_topics"),
        extra_params=("reference_topics",),
    ),
    MetricInfo(
        "tool-call-accuracy",
        "ToolCallAccuracy",
        "Tool calls in the trajectory against the reference's tool_calls, arguments "
        "included (options: strict_order).",
        ("messages", "reference_tool_calls"),
        llm=False,
    ),
    MetricInfo(
        "tool-call-f1",
        "ToolCallF1",
        "F1 of tool calls in the trajectory against the reference's tool_calls.",
        ("messages", "reference_tool_calls"),
        llm=False,
    ),
)


def to_messages(record: Record) -> list[Any]:
    """The record as RAGAS messages: the input, then the trajectory's model
    turns, tool calls and tool results, then the final output."""
    from ragas.messages import AIMessage, HumanMessage, ToolCall, ToolMessage

    messages: list[Any] = [HumanMessage(content=record.input.as_text() if record.input else "")]
    steps = record.trajectory.steps if record.trajectory else []
    has_tool_steps = any(s.type == "tool" for s in steps)
    for step in steps:
        if step.type == "llm":
            calls = []
            if not has_tool_steps and step.output is not None:
                calls = [
                    ToolCall(name=c.name, args=_args(c.arguments))
                    for m in step.output.messages or []
                    for c in m.tool_calls
                ]
            text = step.output.as_text() if step.output is not None and not calls else ""
            messages.append(AIMessage(content=text, tool_calls=calls or None))
        elif step.type == "tool":
            if not isinstance(messages[-1], AIMessage | ToolMessage):
                messages.append(AIMessage(content="", tool_calls=[]))
            owner = next(m for m in reversed(messages) if isinstance(m, AIMessage))
            owner.tool_calls = [
                *(owner.tool_calls or []),
                ToolCall(name=step.name, args=_args(step.input.as_text() if step.input else "")),
            ]
            result = step.error or (step.output.as_text() if step.output else "")
            messages.append(ToolMessage(content=result))
    final = record.output.as_text() if record.output else ""
    last = messages[-1]
    if final and not (isinstance(last, AIMessage) and last.content == final):
        messages.append(AIMessage(content=final))
    return messages


def _args(raw: str) -> dict[str, Any]:
    try:
        value = json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        return {"input": raw}
    return value if isinstance(value, dict) else {"input": value}


def _reference_tool_calls(record: Record) -> list[Any]:
    from ragas.messages import ToolCall

    return [ToolCall(name=n, args=a or {}) for n, a in expected_calls(record)]


def ragas_inputs(info: MetricInfo, record: Record, params: dict[str, Any]) -> dict[str, Any]:
    """Keyword arguments for the metric's ``ascore``."""
    kwargs: dict[str, Any] = {}
    for name in info.inputs:
        if name == "messages":
            kwargs["user_input"] = to_messages(record)
        elif name == "user_input":
            kwargs[name] = record.input.as_text() if record.input else ""
        elif name == "response":
            kwargs[name] = record.output.as_text() if record.output else ""
        elif name == "retrieved_contexts":
            kwargs[name] = [c.as_text() for c in record.context]
        elif name == "reference":
            kwargs[name] = record.reference.as_text() if record.reference else ""
        elif name == "reference_tool_calls":
            kwargs[name] = _reference_tool_calls(record)
        elif name == "reference_topics":
            topics = params.get("reference_topics") or record.metadata.get("reference_topics")
            if not topics:
                raise SkipRecord("no reference_topics param and no metadata['reference_topics']")
            kwargs[name] = list(topics)
    return kwargs


def _requirements(info: MetricInfo) -> Requirements:
    inputs = set(info.inputs)
    return Requirements(
        input=bool(inputs & {"user_input", "messages"}),
        output="response" in inputs,
        reference=bool(inputs & {"reference", "reference_tool_calls"}),
        context="retrieved_contexts" in inputs,
        trajectory="messages" in inputs,
        judge=info.llm,
    )


def _evaluator(info: MetricInfo) -> EvaluatorDef:
    async def run(record: Record, *, ctx: EvalContext, **params: Any) -> Score:
        _quiet_ragas()
        from ragas.metrics import collections

        options = dict(params.get("options") or {})
        session = None
        if info.llm:
            if ctx.judge is None:
                raise JudgeError("RAGAS metrics need a judge; none is configured")
            session = JudgeSession(ctx.judge)
            options["llm"] = judge_llm(session)
        metric = getattr(collections, info.cls)(**options)
        result = await metric.ascore(**ragas_inputs(info, record, params))
        value = result.value
        if value is None or (isinstance(value, float) and math.isnan(value)):
            raise SkipRecord(f"RAGAS {info.cls} returned no score: {result.reason or 'NaN'}")
        metadata: dict[str, Any] = {"upstream": UPSTREAM}
        if session is not None:
            metadata.update(session.metadata())
        return Score(
            number=float(value),
            explanation=result.reason or "",
            cost=session.cost() if session else None,
            metadata=metadata,
        )

    params: dict[str, Any] = {"options": None}
    params.update({p: None for p in info.extra_params})
    spec = EvaluatorSpec(
        name=f"ragas/{info.slug}",
        version=__version__,
        description=f"{info.description} RAGAS {info.cls}"
        + (", judged by the run's judge." if info.llm else "."),
        scope=Scope.RECORD,
        requires=_requirements(info),
        outputs=(
            MetricSpec(
                info.slug,
                ScoreType.NUMBER,
                min=0.0,
                max=1.0,
                higher_is_better=info.higher_is_better,
            ),
        ),
        params=params,
    )
    return EvaluatorDef(spec=spec, fn=run, wants_ctx=True)


PACK = Pack(
    name="ragas",
    tier="wrapped",
    description=f"RAGAS metrics ({UPSTREAM}) run through the Evals.si judge.",
    evaluators=[_evaluator(m) for m in METRICS],
)

__all__ = ["METRICS", "PACK", "judge_llm", "ragas_inputs", "to_messages"]

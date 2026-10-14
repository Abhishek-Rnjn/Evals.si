"""DeepEval metrics as Evals.si evaluators.

Each metric runs on DeepEval's own prompts and scoring, but its model calls go
through the run's judge (``JudgeModel``), so they are cached, rate-limited and
costed like every other judge call, and no provider key is read behind your
back. DeepEval is imported only when a metric runs, so installing the adapter
does not slow down ``evalsi`` itself.

Record fields map onto ``LLMTestCase`` as: input -> input, output ->
actual_output, reference -> expected_output, context -> context and
retrieval_context, trajectory tool calls -> tools_called, and the reference's
``tool_calls`` -> expected_tools. Metrics that read an agent trace get the
trajectory as a DeepEval trace (its steps as nested spans). Conversational
metrics get a ``ConversationalTestCase`` whose turns are the input's chat
messages followed by the output.

DeepEval's System One decisions (``JevEval`` and the ``hybrid`` and
``system_one`` eval modes) are answered by the run's judge too; see
``evalsi_deepeval.system_one``.
"""

from __future__ import annotations

import inspect
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
from evalsi.types import Content, Message, Record, Score, Step
from evalsi_deepeval.system_one import system_one_model

if TYPE_CHECKING:
    from deepeval.metrics import BaseMetric

__version__ = "0.1.0"
UPSTREAM = "deepeval==4.2.8"


def _quiet_deepeval() -> None:
    # No telemetry and no surprise .env loading inside a client's environment.
    os.environ.setdefault("DEEPEVAL_TELEMETRY_OPT_OUT", "1")
    os.environ.setdefault("DEEPEVAL_DISABLE_DOTENV", "1")


# A required param: bind() refuses to run the evaluator until it is set.
REQUIRED: Any = inspect.Parameter.empty
EVAL_MODES = ("llm", "hybrid", "system_one")


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
    # Constructor arguments the metric cannot run without, exposed as required params.
    config: tuple[str, ...] = ()
    # Give the metric the trajectory as a trace whenever the record has one.
    trace: bool = False
    # Scores a ConversationalTestCase built from the record's chat messages.
    conversational: bool = False


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
    MetricInfo(
        "task-completion",
        "TaskCompletionMetric",
        "How fully the agent completed the task in the input (uses the trajectory if present).",
        ("input", "output"),
        trace=True,
    ),
    MetricInfo(
        "step-efficiency",
        "StepEfficiencyMetric",
        "Whether the agent reached its result without redundant or needless steps.",
        ("input", "output", "trajectory"),
    ),
    MetricInfo(
        "plan-quality",
        "PlanQualityMetric",
        "How logical, complete and efficient the plan in the agent's trajectory is.",
        ("input", "output", "trajectory"),
    ),
    MetricInfo(
        "plan-adherence",
        "PlanAdherenceMetric",
        "How closely the agent's actions followed the plan it made.",
        ("input", "output", "trajectory"),
    ),
    MetricInfo(
        "prompt-alignment",
        "PromptAlignmentMetric",
        "Share of the given prompt_instructions that the output follows.",
        ("input", "output"),
        config=("prompt_instructions",),
    ),
    MetricInfo(
        "pii-leakage",
        "PIILeakageMetric",
        "Share of statements in the output that leak no personal data (1 is no leakage).",
        ("input", "output"),
    ),
    MetricInfo(
        "misuse",
        "MisuseMetric",
        "Share of the output that stays within the chatbot's domain (1 is no misuse).",
        ("input", "output"),
        config=("domain",),
    ),
    MetricInfo(
        "non-advice",
        "NonAdviceMetric",
        "Share of the output that gives none of the advice_types (1 is no such advice).",
        ("input", "output"),
        config=("advice_types",),
    ),
    MetricInfo(
        "role-violation",
        "RoleViolationMetric",
        "Whether the output stays within the given role (1 is no violation).",
        ("input", "output"),
        config=("role",),
    ),
    MetricInfo(
        "knowledge-retention",
        "KnowledgeRetentionMetric",
        "Whether the assistant remembers what the user told it earlier in the conversation.",
        ("conversation",),
        conversational=True,
    ),
    MetricInfo(
        "conversation-completeness",
        "ConversationCompletenessMetric",
        "Share of the user's intentions in the conversation that the assistant satisfied.",
        ("conversation",),
        conversational=True,
    ),
    MetricInfo(
        "turn-relevancy",
        "TurnRelevancyMetric",
        "Share of assistant turns that are relevant to the conversation so far.",
        ("conversation",),
        conversational=True,
    ),
    MetricInfo(
        "role-adherence",
        "RoleAdherenceMetric",
        "Share of assistant turns that stay in the given chatbot_role.",
        ("conversation",),
        config=("chatbot_role",),
        conversational=True,
    ),
    MetricInfo(
        "topic-adherence",
        "TopicAdherenceMetric",
        "Whether the assistant answers on relevant_topics and declines the rest.",
        ("conversation",),
        config=("relevant_topics",),
        conversational=True,
    ),
    MetricInfo(
        "goal-accuracy",
        "GoalAccuracyMetric",
        "Whether the assistant reached the user's goal in the conversation, and planned well.",
        ("conversation",),
        conversational=True,
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


def _text(content: Content | None) -> str | None:
    """Text as is; chat messages and JSON as JSON, so no turn is lost."""
    if content is None:
        return None
    if content.text is not None:
        return content.text
    return json.dumps(content.to_dict(), ensure_ascii=False)


_SPAN_TYPES = {"llm": "llm", "tool": "tool", "retrieval": "retriever", "agent": "agent"}


def _span(step: Step) -> dict[str, Any]:
    span: dict[str, Any] = {
        "name": step.name or step.type,
        "type": _SPAN_TYPES.get(step.type, "base"),
        "input": _text(step.input),
        "output": _text(step.output),
        "error": step.error or None,
    }
    return {**{k: v for k, v in span.items() if v is not None}, "children": []}


def to_trace(record: Record) -> dict[str, Any]:
    """The record's trajectory as a DeepEval trace: one agent span whose
    children are the steps, nested by ``parent_span_id``."""
    assert record.trajectory is not None
    steps = record.trajectory.steps
    spans = [_span(s) for s in steps]
    seen: dict[str, dict[str, Any]] = {}
    roots: list[dict[str, Any]] = []
    for step, span in zip(steps, spans, strict=True):
        # Only an earlier step can be a parent, so a malformed trace cannot cycle.
        parent = seen.get(step.parent_span_id) if step.parent_span_id else None
        (parent["children"] if parent is not None else roots).append(span)
        if step.span_id:
            seen.setdefault(step.span_id, span)
    root: dict[str, Any] = {"name": "agent", "type": "agent", "children": roots}
    if record.input is not None:
        root["input"] = _text(record.input)
    if record.output is not None:
        root["output"] = record.output.as_text()
    return root


def to_test_case(record: Record, fields: tuple[str, ...] = (), trace: bool = False) -> Any:
    """The ``LLMTestCase`` for a record; ``fields`` adds the agent fields and
    ``trace`` attaches the trajectory, when there is one, as a trace."""
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
    case = LLMTestCase(**kwargs)
    if (trace or "trajectory" in fields) and record.trajectory is not None:
        case._trace_dict = to_trace(record)
    return case


def conversation(record: Record) -> list[Message]:
    """The record's chat: the input's messages, then the output's."""
    before = list(record.input.messages or []) if record.input else []
    if record.input is not None and record.input.messages is None:
        before = [Message(role="user", content=record.input.as_text())]
    if record.output is None:
        return before
    if record.output.messages is None:
        return [*before, Message(role="assistant", content=record.output.as_text())]
    after = list(record.output.messages)
    # An output that repeats the input's messages is the whole transcript.
    return after if after[: len(before)] == before else [*before, *after]


def to_conversation(record: Record, **fields: Any) -> Any:
    """The ``ConversationalTestCase`` for a record's chat. System messages are
    dropped and tool messages become the outputs of the assistant's tool calls."""
    _quiet_deepeval()
    from deepeval.test_case import ConversationalTestCase, ToolCall, Turn

    turns: list[Any] = []
    calls: dict[str, Any] = {}
    for message in conversation(record):
        if message.role == "tool":
            if message.tool_call_id in calls:
                calls[message.tool_call_id].output = message.content
            continue
        if message.role not in ("user", "assistant"):
            continue
        tools = []
        for call in message.tool_calls:
            try:
                args = json.loads(call.arguments) if call.arguments else None
            except json.JSONDecodeError:
                args = {"input": call.arguments}
            tools.append(
                ToolCall(name=call.name, input_parameters=args if isinstance(args, dict) else None)
            )
            if call.id:
                calls[call.id] = tools[-1]
        if message.content or tools:
            turns.append(
                Turn(
                    role="user" if message.role == "user" else "assistant",
                    content=message.content,
                    tools_called=tools or None,
                )
            )
    roles = {t.role for t in turns}
    if roles != {"user", "assistant"}:
        raise SkipRecord("record has no conversation (needs user and assistant messages)")
    return ConversationalTestCase(
        turns=turns,
        context=_texts(record) or None,
        name=record.id,
        **{k: v for k, v in fields.items() if v is not None},
    )


async def _measure(metric: Any, test_case: Any, session: JudgeSession) -> Score:
    await metric.a_measure(test_case, _show_indicator=False)
    if metric.score is None:
        raise JudgeError(metric.error or "DeepEval returned no score")
    metadata = {
        **session.metadata(),
        "success": bool(metric.success),
        "threshold": metric.threshold,
        "upstream": UPSTREAM,
    }
    # System One decisions report how sure the judge was, question by question.
    if getattr(metric, "confidence", None) is not None:
        metadata["confidence"] = metric.confidence
    if getattr(metric, "score_breakdown", None):
        metadata["score_breakdown"] = metric.score_breakdown
    return Score(
        number=float(metric.score),
        explanation=metric.reason or "",
        cost=session.cost(),
        metadata=metadata,
    )


async def measure(
    metric: BaseMetric, record: Record, session: JudgeSession, fields: tuple[str, ...] = ()
) -> Score:
    return await _measure(metric, to_test_case(record, fields), session)


def _session(ctx: EvalContext) -> JudgeSession:
    if ctx.judge is None:
        raise JudgeError("DeepEval metrics need a judge; none is configured")
    return JudgeSession(ctx.judge)


def _requirements(fields: tuple[str, ...]) -> Requirements:
    return Requirements(
        input="input" in fields or "conversation" in fields,
        output="output" in fields or "conversation" in fields,
        reference="reference" in fields,
        context="context" in fields,
        trajectory="trajectory" in fields,
        judge=True,
    )


def _models(session: JudgeSession, eval_mode: str) -> dict[str, Any]:
    """The judge as DeepEval's LLM and as its System One model."""
    if eval_mode not in EVAL_MODES:
        raise ValueError(f"eval_mode must be one of {list(EVAL_MODES)}, not {eval_mode!r}")
    # Passed explicitly, so DEEPEVAL_EVAL_MODE in the worker never routes to TypeSafe.
    return {
        "model": judge_model(session),
        "system_one_model": system_one_model(session),
        "eval_mode": eval_mode,
    }


# Config that DeepEval reads from the conversational test case, not the metric.
_TEST_CASE_CONFIG = frozenset({"chatbot_role"})


def _metric_evaluator(info: MetricInfo) -> EvaluatorDef:
    async def run(
        record: Record,
        *,
        ctx: EvalContext,
        threshold: float = 0.5,
        strict_mode: bool = False,
        include_reason: bool = True,
        eval_mode: str = "llm",
        options: dict[str, Any] | None = None,
        **config: Any,
    ) -> Score:
        _quiet_deepeval()
        import deepeval.metrics

        on_case = {k: v for k, v in config.items() if k in _TEST_CASE_CONFIG}
        test_case = (
            to_conversation(record, **on_case)
            if info.conversational
            else to_test_case(record, info.fields, trace=info.trace)
        )
        session = _session(ctx)
        cls = getattr(deepeval.metrics, info.cls)
        metric = cls(
            **_models(session, eval_mode),
            threshold=threshold,
            strict_mode=strict_mode,
            include_reason=include_reason,
            async_mode=True,
            **{k: v for k, v in config.items() if k not in _TEST_CASE_CONFIG},
            **(options or {}),
        )
        return await _measure(metric, test_case, session)

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
        params={
            "threshold": 0.5,
            "strict_mode": False,
            "include_reason": True,
            "eval_mode": "llm",
            "options": None,
            **dict.fromkeys(info.config, REQUIRED),
        },
    )
    return EvaluatorDef(spec=spec, fn=run, wants_ctx=True)


# Record fields behind each DeepEval test case param; "trace" is the trajectory
# as a whole, for criteria or questions about everything the agent did.
_SINGLE_TURN_FIELDS = {
    "input": "input",
    "actual_output": "output",
    "expected_output": "reference",
    "context": "context",
    "retrieval_context": "context",
    "tools_called": "trajectory",
    "trace": "trajectory",
}


def _single_turn(record: Record, params: list[str], name: str) -> tuple[Any, list[Any] | None]:
    """The test case and DeepEval params for G-Eval and JevEval; ``None``
    params means the metric judges the trace."""
    from deepeval.test_case import SingleTurnParams

    unknown = sorted(set(params) - set(_SINGLE_TURN_FIELDS))
    if unknown:
        raise ValueError(f"unknown evaluation_params {unknown}; use {sorted(_SINGLE_TURN_FIELDS)}")
    if "trace" in params and len(params) > 1:
        raise ValueError(f"{name}: evaluation_params 'trace' cannot be combined with others")
    for param in params:
        field = _SINGLE_TURN_FIELDS[param]
        if not getattr(record, field):
            raise SkipRecord(f"record has no {field} (needed for {param})")
    fields = ("trajectory",) if {"trace", "tools_called"} & set(params) else ()
    test_case = to_test_case(record, fields)
    if params == ["trace"]:
        return test_case, None
    return test_case, [SingleTurnParams(p) for p in params]


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
    test_case, params = _single_turn(
        record, evaluation_params or ["input", "actual_output"], "deepeval/g-eval"
    )
    _quiet_deepeval()
    from deepeval.metrics import GEval

    session = _session(ctx)
    metric = GEval(
        name=name,
        criteria=criteria,
        evaluation_steps=evaluation_steps,
        evaluation_params=params,
        model=judge_model(session),
        threshold=threshold,
        strict_mode=strict_mode,
        async_mode=True,
    )
    return await _measure(metric, test_case, session)


G_EVAL = EvaluatorDef(
    spec=EvaluatorSpec(
        name="deepeval/g-eval",
        version=__version__,
        description=(
            "DeepEval G-Eval: a judge scores the record against your criteria or evaluation "
            "steps. evaluation_params picks the fields it sees (input, actual_output, "
            "expected_output, context, retrieval_context, tools_called), or ['trace'] for "
            "the whole trajectory."
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


def jev_questions(questions: Any) -> list[Any]:
    """DeepEval ``Noul`` / ``Score`` / ``Choice`` questions from plain dicts.

    ``{"statement": ...}`` is a Noul, ``{"question": ..., "levels": [...]}`` a
    Score and ``{"question": ..., "options": {name: credit}}`` a Choice; a
    ``type`` key ("noul", "score", "choice") may say so explicitly, and every
    kind takes an optional ``weight``.
    """
    _quiet_deepeval()
    from deepeval.metrics.jev_eval import Choice, Noul
    from deepeval.metrics.jev_eval import Score as JevScore

    kinds: dict[str, Any] = {"noul": Noul, "score": JevScore, "choice": Choice}
    if not isinstance(questions, list) or not questions:
        raise ValueError("jev-eval needs questions: a non-empty list")
    out = []
    for i, raw in enumerate(questions):
        if not isinstance(raw, dict):
            raise ValueError(f"question {i} must be an object, not {type(raw).__name__}")
        kind = raw.get("type") or (
            "noul" if "statement" in raw else "score" if "levels" in raw else "choice"
        )
        if kind not in kinds:
            raise ValueError(f"question {i} has unknown type {kind!r}; use {sorted(kinds)}")
        try:
            out.append(kinds[kind](**{k: v for k, v in raw.items() if k != "type"}))
        except Exception as exc:  # pydantic.ValidationError, without importing pydantic
            raise ValueError(f"question {i}: {exc}") from exc
    return out


_JEV_QUESTIONS_DOC = (
    "questions: a list of {statement} (true or false), {question, levels} (ordered levels, "
    "worst first) or {question, options: {name: credit 0-1, or null for not applicable}}, "
    "each with an optional weight."
)


async def _jev_eval(
    record: Record,
    *,
    ctx: EvalContext,
    questions: list[dict[str, Any]],
    evaluation_params: list[str] | None = None,
    name: str = "jev-eval",
    threshold: float = 0.5,
    strict_mode: bool = False,
) -> Score:
    parsed = jev_questions(questions)
    test_case, params = _single_turn(
        record, evaluation_params or ["input", "actual_output"], "deepeval/jev-eval"
    )
    from deepeval.metrics import JevEval

    session = _session(ctx)
    metric = JevEval(
        name=name,
        evaluation_params=params,
        questions=parsed,
        system_one_model=system_one_model(session),
        threshold=threshold,
        strict_mode=strict_mode,
        async_mode=True,
    )
    return await _measure(metric, test_case, session)


JEV_EVAL = EvaluatorDef(
    spec=EvaluatorSpec(
        name="deepeval/jev-eval",
        version=__version__,
        description=(
            "DeepEval JevEval: the judge answers your questions about the record with "
            "probabilities, and the score is their weighted mean. "
            + _JEV_QUESTIONS_DOC
            + " evaluation_params picks the fields it sees, as for g-eval."
        ),
        scope=Scope.RECORD,
        requires=Requirements(output=False, judge=True),
        outputs=(
            MetricSpec("jev-eval", ScoreType.NUMBER, min=0.0, max=1.0, higher_is_better=True),
        ),
        params={
            "questions": REQUIRED,
            "evaluation_params": None,
            "name": "jev-eval",
            "threshold": 0.5,
            "strict_mode": False,
        },
    ),
    fn=_jev_eval,
    wants_ctx=True,
)


_MULTI_TURN_PARAMS = ("role", "content", "context", "tools_called", "chatbot_role", "scenario")


def _multi_turn(params: list[str] | None, name: str) -> list[Any]:
    from deepeval.test_case import MultiTurnParams

    params = params or ["role", "content"]
    unknown = sorted(set(params) - set(_MULTI_TURN_PARAMS))
    if unknown:
        raise ValueError(f"{name}: unknown evaluation_params {unknown}; use {_MULTI_TURN_PARAMS}")
    return [MultiTurnParams(p) for p in params]


async def _conversational_g_eval(
    record: Record,
    *,
    ctx: EvalContext,
    criteria: str | None = None,
    evaluation_steps: list[str] | None = None,
    evaluation_params: list[str] | None = None,
    chatbot_role: str | None = None,
    scenario: str | None = None,
    name: str = "conversational-g-eval",
    threshold: float = 0.5,
    strict_mode: bool = False,
) -> Score:
    if not criteria and not evaluation_steps:
        raise ValueError("deepeval/conversational-g-eval needs criteria or evaluation_steps")
    params = _multi_turn(evaluation_params, "deepeval/conversational-g-eval")
    test_case = to_conversation(record, chatbot_role=chatbot_role, scenario=scenario)
    from deepeval.metrics import ConversationalGEval

    session = _session(ctx)
    metric = ConversationalGEval(
        name=name,
        criteria=criteria,
        evaluation_steps=evaluation_steps,
        evaluation_params=params,
        model=judge_model(session),
        threshold=threshold,
        strict_mode=strict_mode,
        async_mode=True,
    )
    return await _measure(metric, test_case, session)


async def _conversational_jev_eval(
    record: Record,
    *,
    ctx: EvalContext,
    questions: list[dict[str, Any]],
    evaluation_params: list[str] | None = None,
    chatbot_role: str | None = None,
    scenario: str | None = None,
    name: str = "conversational-jev-eval",
    threshold: float = 0.5,
    strict_mode: bool = False,
) -> Score:
    parsed = jev_questions(questions)
    params = _multi_turn(evaluation_params, "deepeval/conversational-jev-eval")
    test_case = to_conversation(record, chatbot_role=chatbot_role, scenario=scenario)
    from deepeval.metrics import ConversationalJevEval

    session = _session(ctx)
    metric = ConversationalJevEval(
        name=name,
        evaluation_params=params,
        questions=parsed,
        system_one_model=system_one_model(session),
        threshold=threshold,
        strict_mode=strict_mode,
        async_mode=True,
    )
    return await _measure(metric, test_case, session)


_CONVERSATION_PARAMS_DOC = (
    "The turns are the input's chat messages followed by the output. evaluation_params "
    f"picks what it sees ({', '.join(_MULTI_TURN_PARAMS)}; default role and content)."
)

CONVERSATIONAL_G_EVAL = EvaluatorDef(
    spec=EvaluatorSpec(
        name="deepeval/conversational-g-eval",
        version=__version__,
        description=(
            "DeepEval ConversationalGEval: G-Eval over a whole conversation. "
            + _CONVERSATION_PARAMS_DOC
        ),
        scope=Scope.RECORD,
        requires=Requirements(input=True, output=True, judge=True),
        outputs=(
            MetricSpec(
                "conversational-g-eval",
                ScoreType.NUMBER,
                min=0.0,
                max=1.0,
                higher_is_better=True,
            ),
        ),
        params={
            "criteria": None,
            "evaluation_steps": None,
            "evaluation_params": None,
            "chatbot_role": None,
            "scenario": None,
            "name": "conversational-g-eval",
            "threshold": 0.5,
            "strict_mode": False,
        },
    ),
    fn=_conversational_g_eval,
    wants_ctx=True,
)

CONVERSATIONAL_JEV_EVAL = EvaluatorDef(
    spec=EvaluatorSpec(
        name="deepeval/conversational-jev-eval",
        version=__version__,
        description=(
            "DeepEval ConversationalJevEval: JevEval over a whole conversation. "
            + _JEV_QUESTIONS_DOC
            + " "
            + _CONVERSATION_PARAMS_DOC
        ),
        scope=Scope.RECORD,
        requires=Requirements(input=True, output=True, judge=True),
        outputs=(
            MetricSpec(
                "conversational-jev-eval",
                ScoreType.NUMBER,
                min=0.0,
                max=1.0,
                higher_is_better=True,
            ),
        ),
        params={
            "questions": REQUIRED,
            "evaluation_params": None,
            "chatbot_role": None,
            "scenario": None,
            "name": "conversational-jev-eval",
            "threshold": 0.5,
            "strict_mode": False,
        },
    ),
    fn=_conversational_jev_eval,
    wants_ctx=True,
)

PACK = Pack(
    name="deepeval",
    tier="wrapped",
    description=f"DeepEval metrics ({UPSTREAM}) run through the Evals.si judge.",
    evaluators=[
        *(_metric_evaluator(m) for m in METRICS),
        G_EVAL,
        JEV_EVAL,
        CONVERSATIONAL_G_EVAL,
        CONVERSATIONAL_JEV_EVAL,
    ],
)

__all__ = [
    "METRICS",
    "PACK",
    "conversation",
    "jev_questions",
    "judge_model",
    "measure",
    "system_one_model",
    "to_conversation",
    "to_test_case",
    "to_trace",
]

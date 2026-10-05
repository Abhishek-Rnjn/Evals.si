"""Inspect AI and Evals.si, in both directions.

- **Import**: ``inspect://logs/run.eval`` (or ``read_log``) turns an Inspect
  log into records: input, final output, target as reference, the
  conversation as a trajectory (model turns, tool calls and results), token
  usage, and Inspect's own scores in metadata. Any Evals.si evaluator can then
  re-score the samples, and runs can gate on them.
- **Export**: ``evalsi_scorer("builtin/faithfulness")`` is an Inspect scorer
  that runs an Evals.si evaluator, so Inspect tasks gain our packs and judges.

Running Inspect tasks under Evals.si sandboxes arrives with the harness in
Phase 2.
"""

from __future__ import annotations

import json
from collections.abc import Mapping
from typing import Any

from inspect_ai.log import EvalLog, EvalSample, read_eval_log
from inspect_ai.model import ChatMessage, ChatMessageAssistant, ChatMessageTool
from inspect_ai.scorer import Score as InspectScore
from inspect_ai.scorer import Scorer, Target, mean, scorer, stderr
from inspect_ai.solver import TaskState

from evalsi.evaluator import EvalContext, SkipRecord
from evalsi.judges import JudgeClient, JudgeConfig, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.registry import default_registry
from evalsi.types import Content, Message, Record, Score, Step, ToolCall, Trajectory, Usage

__version__ = "0.1.0"
UPSTREAM = "inspect-ai==0.3.276"


def _truthy(value: str | bool) -> bool:
    return value if isinstance(value, bool) else value.lower() not in ("0", "false", "no")


def read_log(path: str, *, scores: str | bool = True) -> list[Record]:
    """Records from an Inspect log (``.eval`` or ``.json``), one per sample and epoch.

    ``scores=false`` leaves Inspect's scores out of the metadata.
    """
    log = read_eval_log(path)
    if not log.samples:
        return []
    epochs = log.eval.config.epochs or 1
    return [sample_to_record(s, log, epochs=epochs, scores=_truthy(scores)) for s in log.samples]


def sample_to_record(
    sample: EvalSample, log: EvalLog | None = None, *, epochs: int = 1, scores: bool = True
) -> Record:
    metadata: dict[str, Any] = dict(sample.metadata or {})
    metadata["inspect_epoch"] = sample.epoch
    if log is not None:
        metadata["inspect_task"] = log.eval.task
        metadata["inspect_model"] = log.eval.model
    if scores and sample.scores:
        metadata["inspect_scores"] = {k: _plain(v.value) for k, v in sample.scores.items()}
    if sample.error is not None:
        metadata["inspect_error"] = sample.error.message
    record_id = str(sample.id)
    if epochs > 1:
        record_id = f"{record_id}#{sample.epoch}"
    completion = sample.output.completion if sample.output else ""
    return Record(
        id=record_id,
        input=_input(sample.input),
        output=Content(text=completion) if completion else None,
        reference=_reference(sample.target),
        usage=_usage(sample),
        metadata=metadata,
        trajectory=messages_to_trajectory(sample.messages, trace_id=sample.uuid or ""),
    )


def _plain(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {k: _plain(v) for k, v in value.items()}
    if isinstance(value, list | tuple):
        return [_plain(v) for v in value]
    return value


def _input(value: str | list[ChatMessage]) -> Content:
    if isinstance(value, str):
        return Content(text=value)
    return Content(messages=[Message(role=m.role, content=m.text) for m in value])


def _reference(target: str | list[str]) -> Content | None:
    if isinstance(target, str):
        return Content(text=target) if target else None
    if not target:
        return None
    return Content(text=target[0]) if len(target) == 1 else Content.from_value(list(target))


def _usage(sample: EvalSample) -> Usage | None:
    usage = Usage(latency_ms=sample.total_time * 1000 if sample.total_time else None)
    for model_usage in (sample.model_usage or {}).values():
        usage.input_tokens = (usage.input_tokens or 0) + model_usage.input_tokens
        usage.output_tokens = (usage.output_tokens or 0) + model_usage.output_tokens
        if model_usage.total_cost is not None:
            usage.cost_usd = (usage.cost_usd or 0.0) + model_usage.total_cost
    return usage


def messages_to_trajectory(messages: list[ChatMessage], *, trace_id: str = "") -> Trajectory:
    """Model turns become ``llm`` steps (with their tool calls), tool results
    become ``tool`` steps carrying the arguments they were called with."""
    steps: list[Step] = []
    arguments: dict[str, str] = {}
    for message in messages:
        if isinstance(message, ChatMessageAssistant):
            calls = [
                ToolCall(name=c.function, arguments=json.dumps(c.arguments), id=c.id)
                for c in message.tool_calls or []
            ]
            arguments.update({c.id: c.arguments for c in calls})
            output = (
                Content(
                    messages=[Message(role="assistant", content=message.text, tool_calls=calls)]
                )
                if calls
                else Content(text=message.text)
            )
            steps.append(Step(type="llm", name=message.model or "", output=output))
        elif isinstance(message, ChatMessageTool):
            steps.append(
                Step(
                    type="tool",
                    name=message.function or "",
                    input=Content(text=arguments.get(message.tool_call_id or "", "")),
                    output=Content(text=message.text),
                    error=message.error.message if message.error else "",
                )
            )
    return Trajectory(trace_id=trace_id, steps=steps)


def state_to_record(state: TaskState, target: Target) -> Record:
    """The record an Evals.si evaluator sees for an Inspect sample being scored."""
    completion = state.output.completion if state.output else ""
    return Record(
        id=str(state.sample_id),
        input=Content(text=state.input_text),
        output=Content(text=completion) if completion else None,
        reference=_reference(list(target.target)),
        metadata=dict(state.metadata or {}),
        trajectory=messages_to_trajectory(state.messages),
    )


def _value(score: Score) -> str | int | float | bool:
    if score.number is not None:
        return score.number
    if score.passed is not None:
        return 1.0 if score.passed else 0.0
    if score.label is not None:
        return score.label
    return json.dumps(score.structured, default=str)


def evalsi_scorer(
    evaluator: str,
    params: Mapping[str, Any] | None = None,
    *,
    judge: JudgeConfig | JudgeClient | None = None,
    cache: bool = True,
) -> Scorer:
    """An Inspect scorer that runs the Evals.si evaluator ``evaluator``.

    Single-metric evaluators score a number (``passed`` becomes 1 or 0) with
    mean and stderr; multi-metric ones score a dict keyed by metric. Records
    the evaluator cannot score (missing context, say) get no score rather
    than a zero.
    """
    bound = default_registry().resolve(evaluator).bind(params)
    outputs = [o.name for o in bound.spec.outputs]
    client: JudgeClient | None = None

    def context() -> EvalContext:
        nonlocal client
        if client is None and judge is not None:
            client = (
                judge
                if isinstance(judge, JudgeClient)
                else create_judge(judge, cache=JudgeCache() if cache else None)
            )
        return EvalContext(judge=client)

    metrics: Any = {"*": [mean(), stderr()]} if len(outputs) > 1 else [mean(), stderr()]

    @scorer(metrics=metrics, name=f"evalsi_{bound.name.replace('-', '_')}")
    def build() -> Scorer:
        async def score(state: TaskState, target: Target) -> InspectScore | None:
            record = state_to_record(state, target)
            if bound.spec.requires.missing(record) is not None:
                return None
            try:
                scores = await bound.run(record, context())
            except SkipRecord:
                return None
            explanation = "; ".join(s.explanation for s in scores if s.explanation)
            metadata = {"evalsi_evaluator": bound.spec.ref}
            if len(outputs) > 1:
                return InspectScore(
                    value={s.name: _value(s) for s in scores},
                    explanation=explanation or None,
                    metadata=metadata,
                )
            return InspectScore(
                value=_value(scores[0]), explanation=explanation or None, metadata=metadata
            )

        return score

    result: Scorer = build()
    return result


__all__ = [
    "evalsi_scorer",
    "messages_to_trajectory",
    "read_log",
    "sample_to_record",
    "state_to_record",
]

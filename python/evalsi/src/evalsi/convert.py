"""Conversions between ``evalsi.types`` and the protobuf messages in ``evalsi.v1alpha1``.

Needs the ``server`` extra (protobuf). JSON values travel as
``google.protobuf.Value``, so integers come back as floats; ``coerce_params``
restores integer params where the evaluator's default is an integer.
"""

from __future__ import annotations

import inspect
from collections.abc import Mapping
from typing import Any

from google.protobuf import duration_pb2, json_format, struct_pb2

from evalsi.evaluator import EvaluatorSpec, Scope, ScoreType
from evalsi.types import (
    Content,
    EvaluationResult,
    Message,
    Outcome,
    Record,
    Score,
    Step,
    ToolCall,
    Trajectory,
    Usage,
)
from evalsi.v1alpha1 import evaluator_pb2, record_pb2, score_pb2


def to_value(value: Any) -> struct_pb2.Value:
    return json_format.ParseDict(value, struct_pb2.Value())


def from_value(value: struct_pb2.Value) -> Any:
    return json_format.MessageToDict(value)


def to_struct(value: Mapping[str, Any]) -> struct_pb2.Struct:
    return json_format.ParseDict(dict(value), struct_pb2.Struct())


def from_struct(value: struct_pb2.Struct) -> dict[str, Any]:
    out: dict[str, Any] = json_format.MessageToDict(value)
    return out


# --- content and records ---


def content_to_proto(content: Content) -> record_pb2.Content:
    if content.text is not None:
        return record_pb2.Content(text=content.text)
    if content.messages is not None:
        return record_pb2.Content(
            messages=record_pb2.Messages(
                messages=[
                    record_pb2.Message(
                        role=m.role,
                        content=m.content,
                        tool_calls=[
                            record_pb2.ToolCall(id=c.id, name=c.name, arguments=c.arguments)
                            for c in m.tool_calls
                        ],
                        tool_call_id=m.tool_call_id,
                        name=m.name,
                    )
                    for m in content.messages
                ]
            )
        )
    return record_pb2.Content(json=to_value(content.json))


def content_from_proto(content: record_pb2.Content) -> Content:
    kind = content.WhichOneof("kind")
    if kind == "text":
        return Content(text=content.text)
    if kind == "messages":
        return Content(
            messages=[
                Message(
                    role=m.role,
                    content=m.content,
                    tool_calls=[
                        ToolCall(name=c.name, arguments=c.arguments, id=c.id) for c in m.tool_calls
                    ],
                    tool_call_id=m.tool_call_id,
                    name=m.name,
                )
                for m in content.messages.messages
            ]
        )
    if kind == "json":
        return Content(json=from_value(content.json), is_json=True)
    raise ValueError(f"content kind {kind!r} is not supported by this worker yet")


def usage_to_proto(usage: Usage) -> record_pb2.Usage:
    msg = record_pb2.Usage()
    if usage.input_tokens is not None:
        msg.input_tokens = usage.input_tokens
    if usage.output_tokens is not None:
        msg.output_tokens = usage.output_tokens
    if usage.cost_usd is not None:
        msg.cost_usd = usage.cost_usd
    if usage.latency_ms is not None:
        msg.latency.CopyFrom(_duration(usage.latency_ms))
    return msg


def usage_from_proto(usage: record_pb2.Usage) -> Usage:
    return Usage(
        input_tokens=usage.input_tokens if usage.HasField("input_tokens") else None,
        output_tokens=usage.output_tokens if usage.HasField("output_tokens") else None,
        cost_usd=usage.cost_usd if usage.HasField("cost_usd") else None,
        latency_ms=usage.latency.ToNanoseconds() / 1e6 if usage.HasField("latency") else None,
    )


def _duration(ms: float) -> duration_pb2.Duration:
    d = duration_pb2.Duration()
    d.FromNanoseconds(round(ms * 1_000_000))
    return d


def _step_enum(kind: str) -> Any:
    return record_pb2.StepType.Value(f"STEP_TYPE_{kind.upper()}")


def step_to_proto(step: Step) -> record_pb2.Step:
    msg = record_pb2.Step(
        span_id=step.span_id,
        parent_span_id=step.parent_span_id,
        type=_step_enum(step.type),
        name=step.name,
        error=step.error,
    )
    if step.input is not None:
        msg.input.CopyFrom(content_to_proto(step.input))
    if step.output is not None:
        msg.output.CopyFrom(content_to_proto(step.output))
    usage = step.usage or (
        Usage(latency_ms=step.duration_ms) if step.duration_ms is not None else None
    )
    if usage is not None:
        msg.usage.CopyFrom(usage_to_proto(usage))
    for key, value in step.attributes.items():
        msg.attributes[key].CopyFrom(to_value(value))
    return msg


def step_from_proto(msg: record_pb2.Step) -> Step:
    usage = usage_from_proto(msg.usage) if msg.HasField("usage") else None
    duration = usage.latency_ms if usage is not None else None
    if duration is None and msg.HasField("start_time") and msg.HasField("end_time"):
        duration = (msg.end_time.ToNanoseconds() - msg.start_time.ToNanoseconds()) / 1e6
    return Step(
        type=record_pb2.StepType.Name(msg.type).removeprefix("STEP_TYPE_").lower()
        if msg.type
        else "generic",
        name=msg.name,
        input=content_from_proto(msg.input) if msg.HasField("input") else None,
        output=content_from_proto(msg.output) if msg.HasField("output") else None,
        span_id=msg.span_id,
        parent_span_id=msg.parent_span_id,
        error=msg.error,
        duration_ms=duration,
        usage=usage,
        attributes={k: from_value(v) for k, v in msg.attributes.items()},
    )


def trajectory_to_proto(trajectory: Trajectory) -> record_pb2.Trajectory:
    return record_pb2.Trajectory(
        trace_id=trajectory.trace_id,
        session_id=trajectory.session_id,
        steps=[step_to_proto(s) for s in trajectory.steps],
    )


def trajectory_from_proto(msg: record_pb2.Trajectory) -> Trajectory:
    return Trajectory(
        trace_id=msg.trace_id,
        session_id=msg.session_id,
        steps=[step_from_proto(s) for s in msg.steps],
    )


def record_to_proto(record: Record) -> record_pb2.Record:
    msg = record_pb2.Record(id=record.id)
    for name in ("input", "output", "reference"):
        value: Content | None = getattr(record, name)
        if value is not None:
            getattr(msg, name).CopyFrom(content_to_proto(value))
    msg.context.extend(content_to_proto(c) for c in record.context)
    if record.usage is not None:
        msg.usage.CopyFrom(usage_to_proto(record.usage))
    for key, value in record.metadata.items():
        msg.metadata[key].CopyFrom(to_value(value))
    if record.trajectory is not None:
        msg.trajectory.CopyFrom(trajectory_to_proto(record.trajectory))
    return msg


def record_from_proto(msg: record_pb2.Record) -> Record:
    return Record(
        id=msg.id,
        input=content_from_proto(msg.input) if msg.HasField("input") else None,
        output=content_from_proto(msg.output) if msg.HasField("output") else None,
        reference=content_from_proto(msg.reference) if msg.HasField("reference") else None,
        context=[content_from_proto(c) for c in msg.context],
        usage=usage_from_proto(msg.usage) if msg.HasField("usage") else None,
        metadata={k: from_value(v) for k, v in msg.metadata.items()},
        trajectory=trajectory_from_proto(msg.trajectory) if msg.HasField("trajectory") else None,
    )


# --- scores and results ---


def score_to_proto(score: Score) -> score_pb2.Score:
    msg = score_pb2.Score(name=score.name, explanation=score.explanation)
    if score.number is not None:
        msg.number = score.number
    elif score.passed is not None:
        msg.passed = score.passed
    elif score.label is not None:
        msg.label = score.label
    else:
        msg.structured.CopyFrom(to_value(score.structured))
    if score.confidence is not None:
        msg.confidence = score.confidence
    if score.cost is not None:
        msg.cost.CopyFrom(usage_to_proto(score.cost))
    for key, value in score.metadata.items():
        msg.metadata[key].CopyFrom(to_value(value))
    return msg


def score_from_proto(msg: score_pb2.Score) -> Score:
    kind = msg.WhichOneof("value")
    values: dict[str, Any] = {}
    if kind == "structured":
        values["structured"] = from_value(msg.structured)
    elif kind is not None:
        values[kind] = getattr(msg, kind)
    return Score(
        **values,
        name=msg.name,
        explanation=msg.explanation,
        confidence=msg.confidence if msg.HasField("confidence") else None,
        cost=usage_from_proto(msg.cost) if msg.HasField("cost") else None,
        metadata={k: from_value(v) for k, v in msg.metadata.items()},
    )


_OUTCOMES = {
    Outcome.SCORED: score_pb2.OUTCOME_SCORED,
    Outcome.SKIPPED: score_pb2.OUTCOME_SKIPPED,
    Outcome.ERROR: score_pb2.OUTCOME_ERROR,
    Outcome.CANCELLED: score_pb2.OUTCOME_CANCELLED,
}
_OUTCOMES_BACK = {v: k for k, v in _OUTCOMES.items()}


def result_to_proto(result: EvaluationResult) -> score_pb2.EvaluationResult:
    return score_pb2.EvaluationResult(
        record_id=result.record_id,
        evaluator=result.evaluator,
        evaluator_ref=result.evaluator_ref,
        outcome=_OUTCOMES[result.outcome],
        scores=[score_to_proto(s) for s in result.scores],
        reason=result.reason,
        duration=_duration(result.duration_ms),
        trial=result.trial,
    )


def result_from_proto(msg: score_pb2.EvaluationResult) -> EvaluationResult:
    return EvaluationResult(
        record_id=msg.record_id,
        evaluator=msg.evaluator,
        evaluator_ref=msg.evaluator_ref,
        outcome=_OUTCOMES_BACK[msg.outcome],
        scores=[score_from_proto(s) for s in msg.scores],
        reason=msg.reason,
        duration_ms=msg.duration.ToNanoseconds() / 1e6,
        trial=msg.trial,
    )


# --- manifests ---

_SCOPES = {Scope.RECORD: evaluator_pb2.SCOPE_RECORD, Scope.DATASET: evaluator_pb2.SCOPE_DATASET}
_SCORE_TYPES = {
    ScoreType.NUMBER: evaluator_pb2.SCORE_TYPE_NUMBER,
    ScoreType.PASSED: evaluator_pb2.SCORE_TYPE_PASSED,
    ScoreType.LABEL: evaluator_pb2.SCORE_TYPE_LABEL,
    ScoreType.STRUCTURED: evaluator_pb2.SCORE_TYPE_STRUCTURED,
}


def params_schema(spec: EvaluatorSpec) -> dict[str, Any]:
    """A minimal JSON Schema for the params: names, defaults and which are required."""
    properties: dict[str, Any] = {}
    required = []
    for name, default in spec.params.items():
        if default is inspect.Parameter.empty:
            properties[name] = {}
            required.append(name)
        else:
            properties[name] = {"default": default}
    schema: dict[str, Any] = {"type": "object", "properties": properties}
    if required:
        schema["required"] = required
    return schema


def manifest_to_proto(spec: EvaluatorSpec) -> evaluator_pb2.EvaluatorManifest:
    req = spec.requires
    msg = evaluator_pb2.EvaluatorManifest(
        name=spec.name,
        version=spec.version,
        description=spec.description,
        pack=spec.pack,
        scope=_SCOPES[spec.scope],
        modalities=["text"],
        requires=evaluator_pb2.Requirements(
            input=req.input,
            output=req.output,
            reference=req.reference,
            context=req.context,
            trajectory=req.trajectory,
            judge=req.judge,
            isolation=record_pb2.ISOLATION_LEVEL_NONE,
        ),
        scheduling=evaluator_pb2.Scheduling(pool="judge" if req.judge else "cpu"),
    )
    for output in spec.outputs:
        out = evaluator_pb2.MetricSpec(
            name=output.name, type=_SCORE_TYPES[output.type], description=output.description
        )
        if output.min is not None:
            out.min = output.min
        if output.max is not None:
            out.max = output.max
        if output.higher_is_better is not None:
            out.higher_is_better = output.higher_is_better
        msg.outputs.append(out)
    msg.params_schema.CopyFrom(to_struct(params_schema(spec)))
    return msg


def coerce_params(spec: EvaluatorSpec, params: Mapping[str, Any]) -> dict[str, Any]:
    """Undo protobuf's int-to-float widening for params whose default is an int."""
    out = dict(params)
    for name, value in params.items():
        default = spec.params.get(name)
        if (
            isinstance(default, int)
            and not isinstance(default, bool)
            and isinstance(value, float)
            and value.is_integer()
        ):
            out[name] = int(value)
    return out

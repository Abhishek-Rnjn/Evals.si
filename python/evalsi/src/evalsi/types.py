"""Core data types.

These mirror the protobuf messages in ``proto/evalsi/v1alpha1`` (``Record``,
``Content``, ``Score``, ``EvaluationResult`` and friends) so that a record
means the same thing in the embedded library, on the server and in plugins.
Field names match the proto field names.
"""

from __future__ import annotations

import json
from collections.abc import Mapping
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any


@dataclass
class ToolCall:
    name: str
    # Raw JSON exactly as the model produced it; it may be malformed.
    arguments: str = ""
    id: str = ""


@dataclass
class Message:
    role: str
    content: str = ""
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_call_id: str = ""
    name: str = ""


@dataclass
class Content:
    """One of ``text``, ``messages`` or ``json``.

    Tabular rows, tensors and media references exist in the proto and arrive
    with the packs that need them.
    """

    text: str | None = None
    messages: list[Message] | None = None
    json: Any = None
    is_json: bool = False

    def __post_init__(self) -> None:
        if self.json is not None:
            self.is_json = True
        set_fields = sum([self.text is not None, self.messages is not None, self.is_json])
        if set_fields != 1:
            raise ValueError("Content needs exactly one of text, messages or json")

    @classmethod
    def from_value(cls, value: Any) -> Content:
        """Build content from a plain Python value, as found in a JSONL row.

        Strings become text; a list of objects with a ``role`` becomes chat
        messages; ``{"text": ...}``, ``{"messages": ...}`` and ``{"json": ...}``
        are taken literally; anything else is JSON.
        """
        if isinstance(value, Content):
            return value
        if isinstance(value, str):
            return cls(text=value)
        if isinstance(value, Mapping) and len(value) == 1:
            ((key, inner),) = value.items()
            if key == "text" and isinstance(inner, str):
                return cls(text=inner)
            if key == "messages" and isinstance(inner, Mapping) and "messages" in inner:
                inner = inner["messages"]  # protojson form: {"messages": {"messages": [...]}}
            if key == "messages" and _looks_like_messages(inner):
                return cls(messages=[_message_from_dict(m) for m in inner])
            if key == "json":
                return cls(json=inner, is_json=True)
        if _looks_like_messages(value):
            return cls(messages=[_message_from_dict(m) for m in value])
        return cls(json=value, is_json=True)

    def as_text(self) -> str:
        """A plain-text view: the text, the last assistant message, or compact JSON."""
        if self.text is not None:
            return self.text
        if self.messages is not None:
            for message in reversed(self.messages):
                if message.role == "assistant":
                    return message.content
            return self.messages[-1].content if self.messages else ""
        if isinstance(self.json, str):
            return self.json
        return json.dumps(self.json, ensure_ascii=False, separators=(",", ":"))

    def to_dict(self) -> dict[str, Any]:
        if self.text is not None:
            return {"text": self.text}
        if self.messages is not None:
            return {"messages": [_message_to_dict(m) for m in self.messages]}
        return {"json": self.json}


def _looks_like_messages(value: Any) -> bool:
    return (
        isinstance(value, list)
        and len(value) > 0
        and all(isinstance(m, Mapping) and "role" in m for m in value)
    )


def _message_from_dict(data: Mapping[str, Any]) -> Message:
    calls = [
        ToolCall(
            name=str(c.get("name", "")),
            arguments=c["arguments"]
            if isinstance(c.get("arguments"), str)
            else json.dumps(c.get("arguments", {})),
            id=str(c.get("id", "")),
        )
        for c in data.get("tool_calls") or []
    ]
    return Message(
        role=str(data["role"]),
        content=str(data.get("content") or ""),
        tool_calls=calls,
        tool_call_id=str(data.get("tool_call_id", "")),
        name=str(data.get("name", "")),
    )


def _message_to_dict(message: Message) -> dict[str, Any]:
    out: dict[str, Any] = {"role": message.role, "content": message.content}
    if message.tool_calls:
        out["tool_calls"] = [
            {"id": c.id, "name": c.name, "arguments": c.arguments} for c in message.tool_calls
        ]
    if message.tool_call_id:
        out["tool_call_id"] = message.tool_call_id
    if message.name:
        out["name"] = message.name
    return out


@dataclass
class Usage:
    """Cost of producing an output (on a record) or a score (on a score)."""

    input_tokens: int | None = None
    output_tokens: int | None = None
    cost_usd: float | None = None
    latency_ms: float | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Usage:
        latency = _opt_float(data.get("latency_ms"))
        raw = data.get("latency")  # protojson Duration, e.g. "0.290s"
        if latency is None and isinstance(raw, str) and raw.endswith("s"):
            latency = float(raw[:-1]) * 1000
        return cls(
            input_tokens=_opt_int(data.get("input_tokens")),
            output_tokens=_opt_int(data.get("output_tokens")),
            cost_usd=_opt_float(data.get("cost_usd")),
            latency_ms=latency,
        )

    def to_dict(self) -> dict[str, Any]:
        return {k: v for k, v in vars(self).items() if v is not None}


def _opt_int(value: Any) -> int | None:
    return None if value is None else int(value)


def _opt_float(value: Any) -> float | None:
    return None if value is None else float(value)


STEP_TYPES = (
    "llm",
    "tool",
    "retrieval",
    "agent",
    "handoff",
    "user",
    "guardrail",
    "embedding",
    "generic",
)


@dataclass
class Step:
    """One normalized span of an execution (see proto ``evalsi.v1alpha1.Step``)."""

    type: str = "generic"
    name: str = ""
    input: Content | None = None
    output: Content | None = None
    span_id: str = ""
    parent_span_id: str = ""
    # Empty when the step succeeded.
    error: str = ""
    duration_ms: float | None = None
    usage: Usage | None = None
    attributes: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Step:
        kind = str(data.get("type", "generic")).lower().removeprefix("step_type_")
        usage = Usage.from_dict(data["usage"]) if isinstance(data.get("usage"), Mapping) else None
        return cls(
            type=kind if kind in STEP_TYPES else "generic",
            name=str(data.get("name", "")),
            input=None if data.get("input") is None else Content.from_value(data["input"]),
            output=None if data.get("output") is None else Content.from_value(data["output"]),
            span_id=str(data.get("span_id", "")),
            parent_span_id=str(data.get("parent_span_id", "")),
            error=str(data.get("error", "")),
            duration_ms=usage.latency_ms
            if usage is not None
            else _opt_float(data.get("duration_ms")),
            usage=usage,
            attributes=dict(data.get("attributes") or {}),
        )

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"type": self.type, "name": self.name}
        for key in ("input", "output"):
            value: Content | None = getattr(self, key)
            if value is not None:
                out[key] = value.to_dict()
        for key in ("span_id", "parent_span_id", "error"):
            if getattr(self, key):
                out[key] = getattr(self, key)
        if self.duration_ms is not None:
            out["duration_ms"] = self.duration_ms
        if self.usage is not None:
            out["usage"] = self.usage.to_dict()
        return out


@dataclass
class ToolUse:
    """A tool the agent called, as seen in its trajectory."""

    name: str
    arguments: str = ""
    result: str = ""
    error: str = ""


@dataclass
class Trajectory:
    trace_id: str = ""
    session_id: str = ""
    steps: list[Step] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Trajectory:
        return cls(
            trace_id=str(data.get("trace_id", "")),
            session_id=str(data.get("session_id", "")),
            steps=[Step.from_dict(s) for s in data.get("steps") or []],
        )

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"steps": [s.to_dict() for s in self.steps]}
        if self.trace_id:
            out["trace_id"] = self.trace_id
        if self.session_id:
            out["session_id"] = self.session_id
        return out

    def tool_uses(self) -> list[ToolUse]:
        """Tools called, in order. Tool steps win; when a trace has none, tool
        calls requested in LLM outputs are used instead."""
        tools = [
            ToolUse(
                name=s.name,
                arguments=s.input.as_text() if s.input is not None else "",
                result=s.output.as_text() if s.output is not None else "",
                error=s.error,
            )
            for s in self.steps
            if s.type == "tool"
        ]
        if tools:
            return tools
        for step in self.steps:
            if step.type == "llm" and step.output is not None and step.output.messages:
                for message in step.output.messages:
                    tools += [
                        ToolUse(name=c.name, arguments=c.arguments) for c in message.tool_calls
                    ]
        return tools


@dataclass
class Record:
    """The unit every evaluator consumes."""

    id: str
    input: Content | None = None
    output: Content | None = None
    reference: Content | None = None
    context: list[Content] = field(default_factory=list)
    usage: Usage | None = None
    metadata: dict[str, Any] = field(default_factory=dict)
    trajectory: Trajectory | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"id": self.id}
        if self.trajectory is not None:
            out["trajectory"] = self.trajectory.to_dict()
        for name in ("input", "output", "reference"):
            value: Content | None = getattr(self, name)
            if value is not None:
                out[name] = value.to_dict()
        if self.context:
            out["context"] = [c.to_dict() for c in self.context]
        if self.usage is not None:
            out["usage"] = self.usage.to_dict()
        if self.metadata:
            out["metadata"] = self.metadata
        return out


@dataclass
class Score:
    """One metric value. Exactly one of ``number``, ``passed``, ``label`` or
    ``structured`` is set, matching the proto ``oneof``."""

    number: float | None = None
    passed: bool | None = None
    label: str | None = None
    structured: Any = None
    name: str = ""
    explanation: str = ""
    confidence: float | None = None
    cost: Usage | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        set_fields = sum(
            v is not None for v in (self.number, self.passed, self.label, self.structured)
        )
        if set_fields != 1:
            raise ValueError("Score needs exactly one of number, passed, label or structured")
        if self.number is not None:
            self.number = float(self.number)

    @property
    def value(self) -> float | bool | str | Any:
        for v in (self.number, self.passed, self.label):
            if v is not None:
                return v
        return self.structured

    def numeric(self) -> float | None:
        """The value used for aggregation; ``None`` for labels and structured values."""
        if self.number is not None:
            return self.number
        if self.passed is not None:
            return 1.0 if self.passed else 0.0
        return None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"name": self.name}
        for key in ("number", "passed", "label", "structured"):
            if getattr(self, key) is not None:
                out[key] = getattr(self, key)
        if self.explanation:
            out["explanation"] = self.explanation
        if self.confidence is not None:
            out["confidence"] = self.confidence
        if self.cost is not None:
            out["cost"] = self.cost.to_dict()
        if self.metadata:
            out["metadata"] = self.metadata
        return out


class Outcome(StrEnum):
    """How one evaluator fared on one record. Only ``SCORED`` counts toward metrics."""

    SCORED = "scored"
    SKIPPED = "skipped"
    ERROR = "error"
    CANCELLED = "cancelled"


@dataclass
class EvaluationResult:
    record_id: str
    evaluator: str
    evaluator_ref: str
    outcome: Outcome
    scores: list[Score] = field(default_factory=list)
    reason: str = ""
    duration_ms: float = 0.0
    trial: int = 0

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "record_id": self.record_id,
            "evaluator": self.evaluator,
            "evaluator_ref": self.evaluator_ref,
            "outcome": self.outcome.value,
            "duration_ms": round(self.duration_ms, 3),
        }
        if self.scores:
            out["scores"] = [s.to_dict() for s in self.scores]
        if self.reason:
            out["reason"] = self.reason
        if self.trial:
            out["trial"] = self.trial
        return out

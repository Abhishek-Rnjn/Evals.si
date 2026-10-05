"""What a harness reports while it drives an agent: steps, sandbox policy
events and a final result. They mirror ``TrajectoryEvent`` in the harness
protocol and become the record's trajectory."""

from __future__ import annotations

import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content, Step, Usage


def span_id() -> str:
    return secrets.token_hex(8)


@dataclass
class StepEvent:
    step: Step
    # Wall-clock start, for OTel export.
    start_ns: int = field(default_factory=time.time_ns)


@dataclass
class PolicyEvent:
    # "denial", "egress_denied" or "escalation_request".
    kind: str
    detail: str
    granted: bool = False
    span_id: str = ""

    def as_step(self) -> Step:
        """Policy events live in the trajectory as guardrail steps."""
        return Step(
            type="guardrail",
            name=f"sandbox.{self.kind}",
            output=Content(text=self.detail),
            span_id=span_id(),
            parent_span_id=self.span_id,
            attributes={
                "evalsi.policy.kind": self.kind,
                "evalsi.policy.granted": self.granted,
            },
        )


@dataclass
class FinalEvent:
    output: Content | None
    usage: Usage = field(default_factory=Usage)
    # completed, max_steps, budget, timeout, error or user_stopped.
    stop_reason: str = "completed"
    error: str = ""
    extra: dict[str, Any] = field(default_factory=dict)


Event = StepEvent | PolicyEvent | FinalEvent
Emit = Callable[[Event], None]

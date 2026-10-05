"""Running one agent task: setup, run, check, teardown, and the record.

``run_task`` is what both the embedded runner (``evalsi run``) and the
server's worker (``RunTask``) call, so one spec behaves the same in both.
The result is the record with the agent's output, its trajectory (steps plus
sandbox policy events), usage, the checker's verdict and, in its metadata,
how the run stopped and the sandbox isolation it ran under.
"""

from __future__ import annotations

import dataclasses
import importlib
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Record, Step, TaskCheck, Trajectory
from evalsi_harness import otel
from evalsi_harness.events import Event, FinalEvent, PolicyEvent, StepEvent
from evalsi_harness.harness import BuiltinHarness, Harness, HarnessContext
from evalsi_harness.task import Task, TaskError

OnEvent = Callable[[Event], None]


@dataclass
class TaskOutcome:
    """A finished task: the record to grade, or the error that stopped it."""

    record: Record | None
    error: str = ""
    events: list[Event] = field(default_factory=list)


def load_harness(task_spec: Any, ctx: HarnessContext) -> Harness:
    """The harness a run spec asks for; the built-in one by default."""
    harness = task_spec.harness
    kind = harness.WhichOneof("kind")
    if kind in (None, "builtin"):
        return BuiltinHarness(ctx, harness.builtin if kind == "builtin" else None)
    external = harness.external
    from google.protobuf import json_format

    config = json_format.MessageToDict(external.config) if external.HasField("config") else {}
    which = external.WhichOneof("kind")
    if which == "python":
        ctx.trust.check_python(external.python, "harness")
        module, _, name = external.python.partition(":")
        if not module or not name:
            raise TaskError(f"harness.external.python {external.python!r} must be module:Class")
        try:
            cls = getattr(importlib.import_module(module), name)
        except (ImportError, AttributeError) as exc:
            raise TaskError(f"cannot load harness {external.python}: {exc}") from exc
        harness_obj: Harness = cls(ctx, config)
        return harness_obj
    if which in ("address", "command"):
        from evalsi_harness.service import GrpcHarness

        if which == "address":
            return GrpcHarness(address=external.address, config=config)
        ctx.trust.check_command(list(external.command.argv), "harness command")
        return GrpcHarness(command=list(external.command.argv), config=config)
    raise TaskError("harness.external needs one of address, command or python")


async def run_task(task: Task, harness: Harness, *, on_event: OnEvent | None = None) -> TaskOutcome:
    events: list[Event] = []
    started = time.time_ns()
    try:
        handle = await harness.setup(task)
    except TaskError as exc:
        return TaskOutcome(None, error=str(exc))
    final: FinalEvent | None = None
    check: TaskCheck | None = None
    isolation: dict[str, Any] = {}
    try:
        async for event in harness.run(handle):
            events.append(event)
            if on_event is not None:
                on_event(event)
            if isinstance(event, FinalEvent):
                final = event
        if final is not None and final.stop_reason != "error":
            check = await harness.check(handle)
        isolation_of = getattr(harness, "isolation", None)
        if callable(isolation_of):
            isolation = isolation_of(handle)
    except TaskError as exc:
        return TaskOutcome(None, error=str(exc), events=events)
    finally:
        await harness.teardown(handle)
    if final is None:
        return TaskOutcome(None, error="the harness ended without a final result", events=events)
    if final.stop_reason == "error":
        return TaskOutcome(None, error=final.error or "the agent failed", events=events)
    if check is not None and check.error:
        return TaskOutcome(None, error=f"checker: {check.error}", events=events)

    steps: list[Step] = []
    for event in events:
        if isinstance(event, StepEvent):
            steps.append(event.step)
        elif isinstance(event, PolicyEvent):
            steps.append(event.as_step())
    agent = task.spec.target.model or (task.spec.target.agent.WhichOneof("kind") or "agent")
    trace_id = await otel.export(
        steps,
        task_id=task.id,
        trial=task.trial,
        run_id=task.run_id,
        agent=agent,
        started_ns=started,
    )
    metadata = dict(task.record.metadata)
    metadata["agent"] = {"stop_reason": final.stop_reason, **final.extra}
    if final.error:
        metadata["agent"]["error"] = final.error
    if isolation:
        metadata["isolation"] = isolation
    policy = [e for e in events if isinstance(e, PolicyEvent)]
    if policy:
        metadata["policy_events"] = [
            {"kind": p.kind, "detail": p.detail, "granted": p.granted} for p in policy
        ]
    record = dataclasses.replace(
        task.record,
        output=final.output,
        usage=final.usage,
        trajectory=Trajectory(trace_id=trace_id, steps=steps),
        check=check,
        metadata=metadata,
    )
    return TaskOutcome(record, events=events)

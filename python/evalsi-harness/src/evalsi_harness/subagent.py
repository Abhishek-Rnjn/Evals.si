"""``delegate``: hand a subtask to a sub-agent and keep only its report.

The sub-agent starts from a clean context with the parent's tools (but cannot
delegate itself, so there is one level), runs on the parent's model or the
target's ``subagent_model`` (named on its step, priced by its own name), and its
budget is taken out of what the parent has left. Everything it reads and
tries stays in its own conversation; the parent's context grows by the
report alone, which is what lets a long task get through token-heavy
subtasks. This is the ``offload`` context strategy, and the same mechanism is
a multi-agent trajectory: its steps are on the run, under a ``subagent`` step
that hangs from the ``delegate`` call.

The sub-agent's model calls and tool calls go through the same recording as
the parent's, in order, so a replay reproduces them. Calls to ``delegate`` in
one turn therefore run one after another, never in parallel.
"""

from __future__ import annotations

import time
from dataclasses import replace
from typing import TYPE_CHECKING, Any

from evalsi.types import Content, Step, Usage
from evalsi_harness.events import StepEvent, span_id
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import ToolResult, _clip

if TYPE_CHECKING:
    from evalsi_harness.loop import AgentLoop

DEFAULT_SUBAGENT_STEPS = 20
MAX_TASK_CHARS = 20_000

SUBAGENT_SYSTEM = (
    "You are a sub-agent working for another agent. Do only the task you are given, using the "
    "tools available, and check your work. The other agent cannot see anything you do except "
    "your final reply, so end with a concise report: what you found or did, exact paths, "
    "commands and values it needs, and anything you could not finish. Do not ask questions."
)


class DelegateTool:
    def __init__(self, loop: AgentLoop, max_steps: int = DEFAULT_SUBAGENT_STEPS) -> None:
        self.loop = loop
        self.max_steps = max_steps
        self.spec = ToolSpec(
            name="delegate",
            description=(
                "Give a self-contained subtask to a sub-agent with its own fresh context and the "
                "same tools, and get back only its report. Use it for work that would fill your "
                "context with output you do not need to keep: reading many files, searching, "
                "running and debugging a long command, exploring. State the task fully: the "
                "sub-agent knows nothing of this conversation."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "task": {"type": "string", "description": "Everything the sub-agent needs."},
                    "max_steps": {
                        "type": "integer",
                        "description": f"Model calls it may make (default and limit {max_steps}).",
                    },
                },
                "required": ["task"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        loop = self.loop
        task = args.get("task")
        if not isinstance(task, str) or not task.strip():
            return ToolResult("delegate needs a task", is_error=True)
        steps = args.get("max_steps")
        steps = (
            min(steps, self.max_steps) if isinstance(steps, int) and steps > 0 else self.max_steps
        )
        budget = loop.remaining_budget()
        if budget is None:
            return ToolResult("there is no budget left to delegate", is_error=True)
        # Imported here: the loop module imports this one.
        from evalsi_harness.loop import AgentLoop

        model = loop.subagent_model or loop.model
        sub = AgentLoop(
            model,
            loop.sub_tools(),
            replace(
                loop.config,
                max_steps=steps,
                max_tokens=budget["tokens"],
                max_usd=budget["usd"],
                wall_clock_s=budget["wall_clock_s"],
                system=SUBAGENT_SYSTEM,
                delegate_steps=0,
                # The user is talking to the agent they hired, not to its helpers.
                interjections=[],
                # A sub-agent has one job and one reply. fresh-rounds needs a progress
                # reader and a completion marker it was never told about.
                context=replace(loop.config.context, strategy="none")
                if loop.config.context.strategy == "fresh-rounds"
                else loop.config.context,
            ),
            loop.emit,
            hooks=loop.hooks,
            compaction_model=loop.compaction_model,
        )
        started = time.perf_counter()
        handoff = span_id()
        loop.emit(
            StepEvent(
                Step(
                    type="handoff",
                    name="delegate:handoff",
                    span_id=handoff,
                    parent_span_id=span or loop.root,
                )
            )
        )
        final = await sub.run([{"role": "user", "content": task[:MAX_TASK_CHARS]}])
        loop.bill_subagent(final, sub)
        report = final.output.as_text() if final.output is not None else ""
        if final.stop_reason != "completed":
            report = f"[the sub-agent stopped early: {final.stop_reason}]\n{report}".strip()
        loop.emit(
            StepEvent(
                Step(
                    type="agent",
                    name="subagent",
                    input=Content(text=task[:MAX_TASK_CHARS]),
                    output=Content(text=report),
                    span_id=sub.root,
                    parent_span_id=handoff,
                    duration_ms=(time.perf_counter() - started) * 1000,
                    usage=Usage(
                        input_tokens=final.usage.input_tokens if final.usage else None,
                        output_tokens=final.usage.output_tokens if final.usage else None,
                    ),
                    attributes={
                        "evalsi.subagent.steps": sub.state.steps,
                        "evalsi.subagent.tool_calls": sub.state.tool_calls,
                        "evalsi.subagent.stop_reason": final.stop_reason,
                        "evalsi.subagent.model": model.name,
                        # Its spend by model, as the parent's cost counts it.
                        "evalsi.subagent.spend": {
                            name: tokens for name, tokens in sorted(sub.state.spend.items())
                        },
                    },
                )
            )
        )
        return ToolResult(
            _clip(report or "(the sub-agent returned no report)"),
            is_error=final.stop_reason == "error",
            metadata={"subagent_steps": sub.state.steps, "stop_reason": final.stop_reason},
        )

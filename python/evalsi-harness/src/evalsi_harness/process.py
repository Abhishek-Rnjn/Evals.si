"""Step-level rewards: evaluator scores for the part of an episode so far.

An episode's outcome reward is its checker's verdict at the end
(:mod:`evalsi_harness.gym`). A process reward scores the steps taken so far,
so a trainer gets a signal before the end. Pass any number to ``TaskEnv`` and
read their sum from ``StepResult.process_reward`` (the checker's reward stays
in ``reward``, separate)::

    from evalsi_harness.process import EvaluatorReward, ToolErrorPenalty

    env = TaskEnv(process_rewards=[
        ToolErrorPenalty(0.1),
        EvaluatorReward("builtin/loop-detection", weight=0.5),
    ])

:class:`EvaluatorReward` runs a registered record-scope evaluator on a record
whose trajectory is the steps so far. A judge-backed evaluator makes one
judge call per step: that is its cost, so use it for short episodes or a
sample of steps. The reward for a step is the *change* in the evaluator's
score (``mode="delta"``, so a step that does not move the score earns 0 and a
trainer is not paid repeatedly for a state it is already in) or the score
itself (``mode="level"``). A metric where lower is better counts negatively.

A score that cannot be computed is ``None``, not 0: the evaluator skipped the
prefix (no tool calls yet), or it errored. The step's ``info["process_errors"]``
names the errors, and the sum of the rewards is ``None`` when any is.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi.types import Content, Outcome, Record, Score, Step, Trajectory


@dataclass
class ProcessContext:
    """What a process reward sees: the task and the episode so far."""

    record: Record
    steps: Sequence[Step]
    # The step just taken (the last of ``steps``) and the observation it produced.
    action: str = ""
    observation: str = ""
    is_error: bool = False
    # State a reward keeps for this episode only, keyed by the reward itself.
    memory: dict[int, Any] = field(default_factory=dict)
    errors: list[str] = field(default_factory=list)


class ProcessReward(Protocol):
    """A step's reward, or ``None`` when it cannot be computed."""

    async def __call__(self, context: ProcessContext) -> float | None: ...


@dataclass
class ToolErrorPenalty:
    """A fixed penalty for a step whose tool call failed (an unknown tool, a
    command that could not run, a file that is not there)."""

    penalty: float = 0.1

    def __post_init__(self) -> None:
        if self.penalty < 0:
            raise ValueError("penalty is a positive amount that is subtracted")

    async def __call__(self, context: ProcessContext) -> float | None:
        return -self.penalty if context.is_error else 0.0


def _number(score: Score) -> float | None:
    if score.number is not None:
        return float(score.number)
    if score.passed is not None:
        return 1.0 if score.passed else 0.0
    return None


class EvaluatorReward:
    """A registered evaluator's score of the trajectory so far, as a reward.

    ``metric`` picks one of the evaluator's scores when it emits several.
    ``baseline`` is the score assumed before the first scored step in
    ``delta`` mode. ``judge`` goes to :func:`evalsi.aevaluate`.
    """

    def __init__(
        self,
        ref: str,
        *,
        metric: str = "",
        params: Mapping[str, Any] | None = None,
        weight: float = 1.0,
        mode: str = "delta",
        baseline: float = 0.0,
        judge: Any = None,
    ) -> None:
        if mode not in ("delta", "level"):
            raise ValueError(f"mode must be delta or level, not {mode!r}")
        from evalsi.registry import default_registry

        self.ref, self.metric, self.params = ref, metric, dict(params or {})
        self.weight, self.mode, self.baseline = weight, mode, baseline
        self._judge = judge
        spec = default_registry().resolve(ref).spec
        if spec.scope.value != "record":
            raise ValueError(f"{ref} is a dataset-level evaluator; a step reward needs a record's")
        if len(spec.outputs) > 1 and not metric:
            raise ValueError(f"{ref} emits several metrics; name one with metric=")
        outputs = [m for m in spec.outputs if not metric or m.name == metric]
        if not outputs:
            raise ValueError(f"{ref} has no metric {metric!r}")
        self._sign = -1.0 if outputs[0].higher_is_better is False else 1.0

    async def __call__(self, context: ProcessContext) -> float | None:
        from evalsi import aevaluate

        record = Record(
            id=context.record.id,
            input=context.record.input,
            output=Content(text=context.observation),
            reference=context.record.reference,
            context=context.record.context,
            metadata=context.record.metadata,
            trajectory=Trajectory(steps=list(context.steps)),
        )
        spec: dict[str, Any] = {"ref": self.ref}
        if self.params:
            spec["params"] = self.params
        result = await aevaluate([record], [spec], judge=self._judge, cache=False)
        value: float | None = None
        for item in result.results:
            if item.outcome is Outcome.ERROR:
                context.errors.append(f"{self.ref}: {item.reason}")
            for score in item.scores:
                if not self.metric or score.name == self.metric:
                    value = _number(score)
        if value is None:
            return None
        before = context.memory.get(id(self), self.baseline)
        context.memory[id(self)] = value
        raw = value if self.mode == "level" else value - before
        return self.weight * self._sign * raw

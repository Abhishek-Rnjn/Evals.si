"""Rewards for fine-tuning and RL, built from Evals.si evaluators.

::

    import evalsi.rewards

    reward = evalsi.rewards.load("reward.yaml")

    # TRL: GRPOTrainer(reward_funcs=[reward], ...)
    reward(prompts=prompts, completions=completions, answer=answers)  # -> list[float]

    # verl: custom_reward_function.path=evalsi/rewards/verl.py
    reward.compute_score("gsm8k", solution_str, ground_truth)  # -> {"score": ..., ...}

    # Anything else: totals with a per-component breakdown
    results = reward.score([{"prompt": p, "completion": c, "answer": a}])

The spec format and the scoring rules are in :mod:`evalsi.rewards.spec`.
Rewards run in-process by default; with ``server=`` (or ``EVALSI_SERVER``)
they are scored by the server's Reward Service instead.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import math
from collections import OrderedDict
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from evalsi.evaluator import BoundEvaluator, EvalContext, EvaluatorConfigError
from evalsi.judges import JudgeClient, JudgeConfig, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.registry import Registry, default_registry
from evalsi.rewards.spec import (
    REFERENCE_COLUMNS,
    Component,
    RewardSpec,
    RewardSpecError,
    load_spec,
)
from evalsi.runner import _evaluate_one, _run_sync
from evalsi.types import Content, Outcome, Record, Usage

__all__ = [
    "Component",
    "ComponentResult",
    "Reward",
    "RewardError",
    "RewardResult",
    "RewardSpec",
    "RewardSpecError",
    "compose",
    "load",
    "load_spec",
    "to_record",
]


class RewardError(RuntimeError):
    """A component failed and the spec says ``onError: raise``."""


@dataclass
class ComponentResult:
    # scored, skipped or error
    status: str
    value: float | None = None
    reason: str = ""
    cached: bool = False

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"status": self.status}
        if self.value is not None:
            out["value"] = self.value
        if self.reason:
            out["reason"] = self.reason
        return out


@dataclass
class RewardResult:
    total: float | None
    components: dict[str, ComponentResult] = field(default_factory=dict)
    # True when a gate failed and the total is the floor.
    gated: bool = False

    def breakdown(self) -> dict[str, float | None]:
        return {name: c.value for name, c in self.components.items()}

    def to_dict(self) -> dict[str, Any]:
        return {
            "total": self.total,
            "gated": self.gated,
            "components": {k: v.to_dict() for k, v in self.components.items()},
        }


def compose(spec: RewardSpec, components: Mapping[str, ComponentResult]) -> RewardResult:
    """The total for one rollout from its component results (the rules in
    :mod:`evalsi.rewards.spec`). Raises :class:`RewardError` for errors under
    ``onError: raise``."""
    errors = [k for k, c in components.items() if c.status == "error"]
    if errors and spec.on_error == "raise":
        reasons = "; ".join(f"{k}: {components[k].reason}" for k in errors)
        raise RewardError(f"reward components failed: {reasons}")
    result = RewardResult(total=None, components=dict(components))
    for c in spec.components:
        got = components[c.key]
        if c.gate and (got.status != "scored" or got.value is None or got.value < c.threshold):
            result.gated = True
            result.total = spec.floor
            return result
    if errors and spec.on_error == "none":
        return result
    total = 0.0
    for c in spec.components:
        got = components[c.key]
        if got.status == "scored" and got.value is not None:
            total += c.weight * got.value
    if spec.clip is not None:
        total = min(max(total, spec.clip[0]), spec.clip[1])
    result.total = total
    return result


def to_record(
    rollout: Mapping[str, Any] | Record, *, index: int = 0, reference_field: str = ""
) -> Record:
    """A record from a rollout: ``prompt`` (or ``input``), ``completion`` (or
    ``output``), the reference from ``reference_field`` or the first of
    reference/ground_truth/answer/solution/label, and every other key as
    metadata. ``completion_tokens`` (or a ``completion_ids`` list) fills
    ``usage.output_tokens``."""
    if isinstance(rollout, Record):
        return rollout
    data = dict(rollout)
    prompt = data.pop("prompt", data.pop("input", None))
    completion = data.pop("completion", data.pop("output", None))
    if completion is None:
        raise RewardSpecError(f"rollout {index} has no completion")
    ref_key = reference_field or next((k for k in REFERENCE_COLUMNS if k in data), "")
    if reference_field and reference_field not in data:
        raise RewardSpecError(f"rollout {index} has no {reference_field!r}")
    reference = data.pop(ref_key, None) if ref_key else None
    tokens = data.pop("completion_tokens", None)
    ids = data.pop("completion_ids", None)
    if tokens is None and isinstance(ids, list):
        tokens = len(ids)
    record_id = str(data.pop("id", index))
    return Record(
        id=record_id,
        input=None if prompt is None else Content.from_value(prompt),
        output=Content.from_value(completion),
        reference=None if reference is None else Content.from_value(_plain(reference)),
        usage=None if tokens is None else Usage(output_tokens=int(tokens)),
        metadata={k: _plain(v) for k, v in data.items()},
    )


def _plain(value: Any) -> Any:
    """Numbers and other scalars as JSON-friendly values (numpy and torch included)."""
    if hasattr(value, "tolist"):
        return value.tolist()
    if hasattr(value, "item") and callable(value.item):
        return value.item()
    return value


class _LRU:
    def __init__(self, size: int) -> None:
        self.size = size
        self.items: OrderedDict[str, ComponentResult] = OrderedDict()

    def get(self, key: str) -> ComponentResult | None:
        hit = self.items.get(key)
        if hit is not None:
            self.items.move_to_end(key)
        return hit

    def put(self, key: str, value: ComponentResult) -> None:
        self.items[key] = value
        self.items.move_to_end(key)
        while len(self.items) > self.size:
            self.items.popitem(last=False)


class Reward:
    """A loaded reward: callable as a TRL reward function, with
    :meth:`compute_score` for verl, and :meth:`score` / :meth:`ascore` for
    totals with breakdowns."""

    def __init__(
        self,
        spec: RewardSpec,
        *,
        registry: Registry | None = None,
        judge: JudgeConfig | JudgeClient | None = None,
        concurrency: int | None = None,
    ) -> None:
        self.spec = spec
        self.concurrency = concurrency or spec.concurrency
        self._registry = registry or default_registry()
        self._instances = [_bind(c, self._registry) for c in spec.components]
        self._needs_judge = any(i.spec.requires.judge for i in self._instances)
        self._judge = judge
        if self._needs_judge and judge is None:
            self._judge = JudgeConfig(**spec.judge) if spec.judge else JudgeConfig.from_env()
            if self._judge is None:
                raise EvaluatorConfigError(
                    "this reward uses a judge: set 'judge' in the spec, pass judge=, or set "
                    "EVALSI_JUDGE_*"
                )
        self._cache = _LRU(spec.cache_size) if spec.cache else None
        self._prefixes = [
            json.dumps(
                {"ref": i.spec.ref, "params": i.params, "metric": c.metric},
                sort_keys=True,
                default=str,
            )
            for c, i in zip(spec.components, self._instances, strict=True)
        ]
        # The most recent batch's results, for logging (see evalsi.rewards.trl).
        self.last_results: list[RewardResult] = []
        self.__name__ = spec.name

    async def ascore(self, rollouts: Sequence[Mapping[str, Any] | Record]) -> list[RewardResult]:
        records = [
            to_record(r, index=i, reference_field=self.spec.reference_field)
            for i, r in enumerate(rollouts)
        ]
        owned: JudgeClient | None = None
        judge = self._judge if isinstance(self._judge, JudgeClient) else None
        if self._needs_judge and judge is None:
            assert isinstance(self._judge, JudgeConfig)
            owned = judge = create_judge(self._judge, cache=JudgeCache())
        ctx = EvalContext(judge=judge)
        slots: list[dict[str, ComponentResult]] = [{} for _ in records]
        semaphore = asyncio.Semaphore(self.concurrency)

        async def one(ri: int, ci: int) -> None:
            component, inst = self.spec.components[ci], self._instances[ci]
            record = records[ri]
            key = ""
            if self._cache is not None:
                body = json.dumps(record.to_dict() | {"id": ""}, sort_keys=True, default=str)
                key = hashlib.sha256((self._prefixes[ci] + "\0" + body).encode()).hexdigest()
                hit = self._cache.get(key)
                if hit is not None:
                    slots[ri][component.key] = ComponentResult(
                        hit.status, hit.value, hit.reason, cached=True
                    )
                    return
            async with semaphore:
                result = await _evaluate_one(inst, record, record.id, ctx)
            got = _component_result(component, result.outcome, result.scores, result.reason)
            slots[ri][component.key] = got
            if self._cache is not None and got.status != "error":
                self._cache.put(key, got)

        try:
            async with asyncio.TaskGroup() as group:
                for ri in range(len(records)):
                    for ci in range(len(self._instances)):
                        group.create_task(one(ri, ci))
        finally:
            if owned is not None:
                await owned.aclose()
        results = [
            compose(self.spec, {c.key: slot[c.key] for c in self.spec.components}) for slot in slots
        ]
        self.last_results = results
        return results

    def score(self, rollouts: Sequence[Mapping[str, Any] | Record]) -> list[RewardResult]:
        return _run_sync(self.ascore(rollouts))

    def __call__(
        self,
        prompts: Sequence[Any] | None = None,
        completions: Sequence[Any] | None = None,
        **kwargs: Any,
    ) -> list[float | None]:
        """TRL's reward-function signature: dataset columns arrive as keyword
        lists aligned with ``completions``."""
        if completions is None:
            raise TypeError("a reward function needs completions")
        rollouts = _rollouts(prompts, completions, kwargs)
        return [r.total for r in self.score(rollouts)]

    def compute_score(
        self,
        data_source: Any,
        solution_str: str,
        ground_truth: Any,
        extra_info: Mapping[str, Any] | None = None,
        **kwargs: Any,
    ) -> dict[str, Any]:
        """verl's ``compute_score`` signature. Returns ``{"score": total}`` plus
        one key per component, which verl logs."""
        rollout = {
            **dict(extra_info or {}),
            "completion": solution_str,
            "ground_truth": ground_truth,
            "data_source": data_source,
        }
        (result,) = self.score([rollout])
        return _verl_dict(result)

    def compute_score_batch(
        self,
        data_sources: Sequence[Any],
        solution_strs: Sequence[str],
        ground_truths: Sequence[Any],
        extra_infos: Sequence[Mapping[str, Any] | None] | None = None,
        **kwargs: Any,
    ) -> list[dict[str, Any]]:
        """verl's batch reward manager signature."""
        extras = list(extra_infos) if extra_infos is not None else [None] * len(solution_strs)
        rollouts = [
            {**dict(extra or {}), "completion": s, "ground_truth": g, "data_source": d}
            for d, s, g, extra in zip(
                data_sources, solution_strs, ground_truths, extras, strict=True
            )
        ]
        return [_verl_dict(r) for r in self.score(rollouts)]


def _verl_dict(result: RewardResult) -> dict[str, Any]:
    out: dict[str, Any] = {"score": result.total if result.total is not None else math.nan}
    for name, value in result.breakdown().items():
        out[name] = value if value is not None else math.nan
    return out


def _rollouts(
    prompts: Sequence[Any] | None, completions: Sequence[Any], columns: Mapping[str, Any]
) -> list[dict[str, Any]]:
    n = len(completions)
    rollouts: list[dict[str, Any]] = [{"completion": c} for c in completions]
    if prompts is not None:
        for rollout, prompt in zip(rollouts, prompts, strict=True):
            rollout["prompt"] = prompt
    for key, values in columns.items():
        # Per-sample columns only; TRL also passes things like trainer_state.
        if isinstance(values, list | tuple) and len(values) == n:
            for rollout, value in zip(rollouts, values, strict=True):
                rollout[key] = value
    return rollouts


def _component_result(
    component: Component, outcome: Outcome, scores: list[Any], reason: str
) -> ComponentResult:
    if outcome is Outcome.SKIPPED:
        return ComponentResult("skipped", reason=reason)
    if outcome is not Outcome.SCORED:
        return ComponentResult("error", reason=reason or outcome.value)
    score = scores[0]
    if component.metric:
        score = next((s for s in scores if s.name == component.metric), None)
        if score is None:
            return ComponentResult("error", reason=f"no metric {component.metric!r}")
    value = score.numeric()
    if value is None or math.isnan(value):
        return ComponentResult("error", reason=f"metric {score.name!r} is not numeric")
    return ComponentResult("scored", value=value)


def _bind(component: Component, registry: Registry) -> BoundEvaluator:
    definition = registry.resolve(component.ref)
    params = dict(component.params)
    for key, value in (("min_isolation", component.min_isolation), ("network", component.network)):
        if not value:
            continue
        if key not in definition.spec.params:
            raise RewardSpecError(
                f"component {component.key!r}: {definition.spec.name} does not run code in a "
                f"sandbox, so sandbox.{'minIsolation' if key == 'min_isolation' else key} "
                "does not apply"
            )
        params.setdefault(key, value)
    if component.metric and definition.spec.output(component.metric) is None:
        raise RewardSpecError(
            f"component {component.key!r}: {definition.spec.name} has no metric "
            f"{component.metric!r}"
        )
    return definition.bind(params, alias=component.key)


def load(
    source: str | Path | Mapping[str, Any] | RewardSpec,
    *,
    registry: Registry | None = None,
    judge: JudgeConfig | JudgeClient | None = None,
    concurrency: int | None = None,
) -> Reward:
    """Load a reward from a spec file, mapping or :class:`RewardSpec`."""
    return Reward(load_spec(source), registry=registry, judge=judge, concurrency=concurrency)

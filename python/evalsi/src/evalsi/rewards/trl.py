"""Evals.si rewards in TRL.

A loaded reward is already a TRL reward function. :func:`reward_funcs`
adds one function per component, weighted 0, so TRL logs each component's
mean next to the total (``rewards/<name>/mean``) without changing what is
trained on::

    from trl import GRPOConfig, GRPOTrainer
    from evalsi.rewards.trl import reward_funcs

    funcs, weights = reward_funcs("reward.yaml")  # or server="https://evalsi..."
    trainer = GRPOTrainer(
        model=model,
        reward_funcs=funcs,
        args=GRPOConfig(..., reward_weights=weights),
        train_dataset=dataset,
    )

The component functions reuse the batch the total just scored; called on
their own they score it (cheaply, through the cache).
"""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from pathlib import Path
from typing import Any

from evalsi.rewards import Reward, RewardSpec, call_key, load

RewardFunc = Callable[..., list[float | None]]


def component_func(reward: Reward, key: str) -> RewardFunc:
    """A reward function returning component ``key``'s values (``None`` where
    it was skipped or errored)."""
    if key not in {c.key for c in reward.spec.components}:
        raise KeyError(f"{reward.spec.name} has no component {key!r}")

    def func(
        prompts: Sequence[Any] | None = None,
        completions: Sequence[Any] | None = None,
        **kwargs: Any,
    ) -> list[float | None]:
        if completions is None:
            raise TypeError("a reward function needs completions")
        if reward.last_call != call_key(prompts, completions):
            reward(prompts=prompts, completions=completions, **kwargs)
        return [
            r.components[key].value if r.components[key].status == "scored" else None
            for r in reward.last_results
        ]

    func.__name__ = f"{reward.spec.name}/{key}"
    return func


def reward_funcs(
    source: str | Path | Mapping[str, Any] | RewardSpec | Reward,
    *,
    breakdown: bool = True,
    **load_options: Any,
) -> tuple[list[RewardFunc], list[float]]:
    """Reward functions and ``reward_weights`` for TRL: the total (weight 1)
    and, with ``breakdown``, each component (weight 0, logged only).
    ``load_options`` go to :func:`evalsi.rewards.load` (``server=``, ...)."""
    reward = source if isinstance(source, Reward) else load(source, **load_options)
    funcs: list[RewardFunc] = [reward]
    weights = [1.0]
    if breakdown and reward.spec.breakdown:
        for c in reward.spec.components:
            funcs.append(component_func(reward, c.key))
            weights.append(0.0)
    return funcs, weights

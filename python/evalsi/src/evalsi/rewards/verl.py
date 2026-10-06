"""Evals.si rewards in verl.

Point verl's custom reward function at this file::

    custom_reward_function:
      path: <output of: python -c "import evalsi.rewards.verl as m; print(m.__file__)">
      name: compute_score            # or compute_score_batch with the batch reward manager
      reward_kwargs:
        spec: /path/to/reward.yaml
        server: https://evalsi.example.com   # optional: score on the Reward Service

``spec`` and ``server`` may also come from ``EVALSI_REWARD_SPEC`` and
``EVALSI_REWARD_SERVER``. The return value is ``{"score": total, <component>:
value, ...}``; verl trains on ``score`` and logs the rest.
"""

from __future__ import annotations

import os
import threading
from collections.abc import Mapping, Sequence
from typing import Any

from evalsi.rewards import Reward, load

_lock = threading.Lock()
_rewards: dict[tuple[str, str], Reward] = {}


def get_reward(spec: str | None = None, server: str | None = None) -> Reward:
    """The reward for ``spec`` (and ``server``), loaded once per process."""
    spec = spec or os.environ.get("EVALSI_REWARD_SPEC", "")
    server = server if server is not None else os.environ.get("EVALSI_REWARD_SERVER", "")
    if not spec:
        raise ValueError(
            "no reward spec: set custom_reward_function.reward_kwargs.spec or EVALSI_REWARD_SPEC"
        )
    key = (spec, server)
    with _lock:
        if key not in _rewards:
            _rewards[key] = load(spec, server=server or None)
        return _rewards[key]


def compute_score(
    data_source: Any,
    solution_str: str,
    ground_truth: Any,
    extra_info: Mapping[str, Any] | None = None,
    *,
    spec: str | None = None,
    server: str | None = None,
    **kwargs: Any,
) -> dict[str, Any]:
    """verl's per-sample signature."""
    return get_reward(spec, server).compute_score(
        data_source, solution_str, ground_truth, extra_info
    )


def compute_score_batch(
    data_sources: Sequence[Any],
    solution_strs: Sequence[str],
    ground_truths: Sequence[Any],
    extra_infos: Sequence[Mapping[str, Any] | None] | None = None,
    *,
    spec: str | None = None,
    server: str | None = None,
    **kwargs: Any,
) -> list[dict[str, Any]]:
    """verl's batch reward manager signature."""
    return get_reward(spec, server).compute_score_batch(
        data_sources, solution_strs, ground_truths, extra_infos
    )

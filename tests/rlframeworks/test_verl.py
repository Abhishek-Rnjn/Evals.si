"""Evals.si rewards inside verl itself: verl loads ``evalsi.rewards.verl`` the
way ``custom_reward_function`` in its trainer config does, and its own reward
managers score a batch of rollouts with it.

No training runs (that needs GPUs and an inference engine); what is tested is
everything between a finished rollout batch and the reward tensor verl trains
on: the file loader, the kwargs merge, decoding the response tokens, and where
the score lands. Needs verl (see the rlframeworks job in .github/workflows/ci.yml).
"""

from __future__ import annotations

from typing import Any

import pytest

pytest.importorskip("verl")

import numpy as np  # noqa: E402
import torch  # noqa: E402
from omegaconf import OmegaConf  # noqa: E402
from verl import DataProto  # noqa: E402
from verl.trainer.ppo.reward import get_custom_reward_fn  # noqa: E402
from verl.workers.reward_manager.batch import BatchRewardManager  # noqa: E402
from verl.workers.reward_manager.naive import NaiveRewardManager  # noqa: E402

import evalsi.rewards.verl as entry  # noqa: E402

PROMPT = "What is 2 + 2? "
# Two rollouts: one right, one wrong, of different lengths.
RESPONSES = ["4", "5 or so"]
TRUTHS = ["4", "4"]


def rollouts(tokenizer: Any) -> DataProto:
    """A batch as verl's rollout leaves it: left-padded prompts, right-padded
    responses and one attention mask over both."""
    prompt_ids = [tokenizer(PROMPT, add_special_tokens=False)["input_ids"]] * len(RESPONSES)
    response_ids = [tokenizer(r, add_special_tokens=False)["input_ids"] for r in RESPONSES]
    p_len = max(len(p) for p in prompt_ids)
    r_len = max(len(r) for r in response_ids)
    pad = tokenizer.pad_token_id
    prompts = torch.tensor([[pad] * (p_len - len(p)) + p for p in prompt_ids])
    responses = torch.tensor([r + [pad] * (r_len - len(r)) for r in response_ids])
    mask = torch.tensor(
        [
            [0] * (p_len - len(p)) + [1] * len(p) + [1] * len(r) + [0] * (r_len - len(r))
            for p, r in zip(prompt_ids, response_ids, strict=True)
        ]
    )
    return DataProto.from_dict(
        tensors={"prompts": prompts, "responses": responses, "attention_mask": mask},
        non_tensors={
            "data_source": np.array(["evalsi-test"] * len(RESPONSES), dtype=object),
            "reward_model": np.array([{"ground_truth": t} for t in TRUTHS], dtype=object),
            "extra_info": np.array([{"index": i} for i in range(len(RESPONSES))], dtype=object),
        },
    )


def custom_reward(name: str, reward_spec: str) -> Any:
    """What verl builds from ``custom_reward_function`` in its config."""
    config = OmegaConf.create(
        {
            "reward": {
                "custom_reward_function": {
                    "path": entry.__file__,
                    "name": name,
                    "reward_kwargs": {"spec": reward_spec},
                }
            }
        }
    )
    return get_custom_reward_fn(config)


def check(out: dict[str, Any], data: DataProto) -> None:
    rewards = out["reward_tensor"]
    # The score lands on each response's last real token, as verl expects.
    lengths = data.batch["attention_mask"][:, data.batch["prompts"].shape[-1] :].sum(-1)
    assert rewards[0, lengths[0] - 1].item() == 1.0
    assert rewards[1, lengths[1] - 1].item() == 0.0
    assert rewards.sum().item() == 1.0
    # Each component is logged beside the total.
    assert out["reward_extra_info"]["correct"] == [1.0, 0.0]


def test_naive_reward_manager(tokenizer: Any, reward_spec: str) -> None:
    data = rollouts(tokenizer)
    manager = NaiveRewardManager(
        tokenizer, num_examine=0, compute_score=custom_reward("compute_score", reward_spec)
    )
    check(manager(data, return_dict=True), data)


def test_batch_reward_manager(tokenizer: Any, reward_spec: str) -> None:
    data = rollouts(tokenizer)
    manager = BatchRewardManager(
        tokenizer, num_examine=0, compute_score=custom_reward("compute_score_batch", reward_spec)
    )
    check(manager(data, return_dict=True), data)

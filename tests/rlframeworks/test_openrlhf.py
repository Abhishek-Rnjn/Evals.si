"""Evals.si rewards inside OpenRLHF itself: OpenRLHF's single-turn agent
executor (what its PPO and GRPO rollouts call after each generation) scores a
rollout through both of Evals.si's ways in: ``--remote_rm_url`` set to the
``evalsi.rewards.openrlhf`` file, and set to the URL of ``evalsi rewards
serve``.

The generation engine is a stand-in that returns a fixed completion (vLLM
needs a GPU); everything after generation is OpenRLHF's own code: decoding
prompt plus response into the query, loading the reward file, the HTTP
request, and reading the rewards back. OpenRLHF installs without its GPU
dependencies (``--no-deps``, see the rlframeworks job in
.github/workflows/ci.yml). It logs a failed reward instead of raising, so each
test checks the reward is there.
"""

from __future__ import annotations

import asyncio
import threading
from dataclasses import dataclass, field
from typing import Any

import pytest

pytest.importorskip("openrlhf")

from openrlhf.utils.agent import SingleTurnAgentExecutor  # noqa: E402

import evalsi.rewards.openrlhf as entry  # noqa: E402
from evalsi.rewards.verl import get_reward  # noqa: E402

PROMPT = "What is 2 + 2? "


@dataclass
class Params:
    """The two sampling parameters the executor reads."""

    max_tokens: int | None = 16
    logprobs: int | None = None


@dataclass
class Generated:
    token_ids: list[int]
    finish_reason: str = "stop"
    logprobs: Any = None


@dataclass
class Output:
    outputs: list[Generated] = field(default_factory=list)


class Engine:
    """Generates ``completion`` whatever it is asked."""

    def __init__(self, tokenizer: Any, completion: str) -> None:
        self.ids = tokenizer(completion, add_special_tokens=False)["input_ids"]

    async def generate(
        self, prompt_token_ids: Any, params: Any, multi_modal_data: Any = None
    ) -> Output:
        return Output([Generated(list(self.ids))])


def rollout(tokenizer: Any, remote_rm_url: str, completion: str) -> dict[str, Any]:
    executor = SingleTurnAgentExecutor(remote_rm_url=remote_rm_url)
    out: dict[str, Any] = asyncio.run(
        executor.execute(PROMPT, "4", Params(), 64, tokenizer, Engine(tokenizer, completion))
    )
    assert out["reward"] is not None, "OpenRLHF got no reward (it logs the failure)"
    return out


def _value(reward: Any) -> float:
    return float(reward[0] if isinstance(reward, list) else reward.flatten()[0])


def test_reward_function_file(tokenizer: Any) -> None:
    right = rollout(tokenizer, entry.__file__, "4")
    assert _value(right["reward"]) == 1.0
    assert _value(right["scores"]) == 1.0
    assert _value(right["extra_logs"]["correct"]) == 1.0
    assert _value(rollout(tokenizer, entry.__file__, "5")["reward"]) == 0.0


def test_remote_reward_model(tokenizer: Any) -> None:
    server = entry.make_server(get_reward(), port=0)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_address[1]}/get_reward"
        right = rollout(tokenizer, url, "4")
        assert _value(right["reward"]) == 1.0
        assert _value(right["extra_logs"]["correct"]) == 1.0
        assert _value(rollout(tokenizer, url, "5")["reward"]) == 0.0
    finally:
        server.shutdown()

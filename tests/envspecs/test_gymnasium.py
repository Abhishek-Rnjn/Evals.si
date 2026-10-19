"""The Gymnasium adapter, against gymnasium itself."""

from __future__ import annotations

import json

import pytest

pytest.importorskip("gymnasium")

import gymnasium
from gymnasium.utils.env_checker import check_env

from conftest import task
from evalsi_harness.envspecs.gymnasium import InfrastructureError, TaskGymEnv, make
from evalsi_harness.process import ToolErrorPenalty
from evalsi_harness.testing import LocalSandboxClient


def bash(command: str) -> dict[str, str]:
    return {"tool": "bash", "arguments": json.dumps({"command": command})}


def test_is_a_gymnasium_env_that_passes_the_checker() -> None:
    env = make([task()], client=LocalSandboxClient())
    try:
        assert isinstance(env, gymnasium.Env)
        # check_env resets, steps with sampled actions, and tests the spaces.
        check_env(env, skip_render_check=True)
    finally:
        env.close()


def test_episode_is_rewarded_by_the_checked_end_state() -> None:
    env = TaskGymEnv([task()], client=LocalSandboxClient())
    try:
        obs, info = env.reset(seed=0)
        assert obs == "Mark the notes done."
        assert {t["name"] for t in info["tools"]} == {"bash", "read_file", "write_file", "submit"}
        obs, reward, terminated, truncated, info = env.step(bash("cat setup.log notes.txt"))
        assert "ready" in obs
        assert "draft" in obs
        assert (reward, terminated, truncated) == (0.0, False, False)
        env.step(
            {
                "tool": "write_file",
                "arguments": {"path": "notes.txt", "content": "done\n"},  # a dict works too
            }
        )
        obs, reward, terminated, truncated, info = env.step({"tool": "submit", "arguments": "{}"})
        assert (reward, terminated, truncated) == (1.0, True, False)
        assert info["outcome_reward"] == 1.0
        assert info["check"]["passed"] is True
        with pytest.raises(RuntimeError, match="reset"):
            env.step(bash("true"))
        # A new episode starts from the set-up environment again.
        env.reset()
        assert env.step({"tool": "submit", "arguments": ""})[1] == 0.0
    finally:
        env.close()


def test_reaching_the_step_limit_truncates_and_still_grades() -> None:
    env = TaskGymEnv([task()], client=LocalSandboxClient(), max_steps=2)
    try:
        env.reset()
        env.step(bash("true"))
        _, reward, terminated, truncated, info = env.step(bash("echo done > notes.txt"))
        assert (reward, terminated, truncated) == (1.0, False, True)
        assert info["ended"] == "max_steps"
    finally:
        env.close()


def test_a_bad_action_is_a_failed_call_not_a_crash() -> None:
    env = TaskGymEnv([task()], client=LocalSandboxClient(), process_rewards=[ToolErrorPenalty(0.5)])
    try:
        env.reset()
        obs, reward, *_ = env.step({"tool": "bash", "arguments": "{not json"})
        assert "not valid JSON" in obs
        assert reward == -0.5  # the penalty; the outcome reward is still 0
        obs, reward, _, _, info = env.step({"tool": "write_file", "arguments": "[1]"})
        assert "JSON object" in obs
        assert reward == -0.5
        assert info["outcome_reward"] == 0.0
        assert info["process_reward"] == -0.5
    finally:
        env.close()


def test_a_checker_that_cannot_run_raises_instead_of_rewarding_zero() -> None:
    broken = {"command": ["sh", "-c", "exit 0"], "parser": "json"}
    env = TaskGymEnv([task(checker=broken)], client=LocalSandboxClient())
    try:
        env.reset()
        with pytest.raises(InfrastructureError, match="could not read"):
            env.step({"tool": "submit", "arguments": "{}"})
    finally:
        env.close()


def test_reset_picks_a_task_by_seed_or_option() -> None:
    env = TaskGymEnv([task("one"), task("two"), task("three")], client=LocalSandboxClient())
    try:
        assert env.reset(options={"task_index": 2})[0] == "three"
        first = [env.reset(seed=7)[1]["task_index"] for _ in range(1)]
        assert env.reset(seed=7)[1]["task_index"] == first[0]
        with pytest.raises(ValueError, match="task_index"):
            env.reset(options={"task_index": 3})
    finally:
        env.close()

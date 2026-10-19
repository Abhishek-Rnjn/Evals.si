from __future__ import annotations

import asyncio
from typing import Any

import pytest

from evalsi_harness.gym import Action, TaskEnv
from evalsi_harness.task import TaskError
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, spec_of

ENVIRONMENT: dict[str, Any] = {
    "files": {"notes.txt": "draft\n"},
    "setup": ["echo ready > setup.log"],
    "checker": {"command": ["sh", "-c", "grep -q done notes.txt"]},
}


def run(coro: Any) -> Any:
    return asyncio.run(coro)


def test_episode_rewards_the_checked_end_state() -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        async with TaskEnv(client) as env:
            spec = spec_of({"environment": ENVIRONMENT})
            obs = await env.reset(record("Mark the notes done."), spec)
            assert obs == "Mark the notes done."
            assert {t["name"] for t in env.tools} == {"bash", "read_file", "write_file", "submit"}
            step = await env.step(Action("bash", {"command": "cat setup.log notes.txt"}))
            assert "ready" in step.observation
            assert "draft" in step.observation
            assert (step.reward, step.done) == (0.0, False)
            step = await env.step(Action("write_file", {"path": "notes.txt", "content": "done\n"}))
            assert not step.done
            step = await env.step(Action("submit"))
            assert (step.reward, step.done) == (1.0, True)
            assert step.info["check"]["passed"] is True
            assert step.info["ended"] == "submitted"
            # Each reset is a fresh copy of the set-up environment.
            await env.reset(record("again"), spec)
            step = await env.step(Action("submit"))
            assert step.reward == 0.0

    run(go())


def test_max_steps_ends_the_episode() -> None:
    async def go() -> None:
        async with TaskEnv(LocalSandboxClient(), max_steps=2) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            first = await env.step(Action("nope"))
            assert "unknown tool" in first.observation
            assert first.info["tool_error"] is True
            last = await env.step(Action("bash", {"command": "echo done > notes.txt"}))
            assert last.done
            assert last.reward == 1.0
            assert last.info["ended"] == "max_steps"
            with pytest.raises(RuntimeError, match="reset"):
                await env.step(Action("submit"))

    run(go())


def test_a_checker_that_cannot_run_is_not_a_zero_reward() -> None:
    async def go() -> None:
        broken = {**ENVIRONMENT, "checker": {"command": ["sh", "-c", "exit 0"], "parser": "json"}}
        async with TaskEnv(LocalSandboxClient()) as env:
            await env.reset(record(), spec_of({"environment": broken}))
            step = await env.step(Action("submit"))
            assert step.done
            assert step.reward is None
            assert "could not read" in step.info["error"]

    run(go())


def test_tasks_need_an_environment_with_a_checker() -> None:
    async def go() -> None:
        async with TaskEnv(LocalSandboxClient()) as env:
            with pytest.raises(TaskError, match="no environment"):
                await env.reset(record(), spec_of({}))
            with pytest.raises(TaskError, match="no checker"):
                await env.reset(record(), spec_of({"environment": {"files": {"a": "b"}}}))

    run(go())


def test_environments_on_one_pool_pay_setup_once() -> None:
    from evalsi_harness.environment import EnvironmentPool

    async def go() -> None:
        client = LocalSandboxClient()
        pool = EnvironmentPool(client)
        spec = spec_of({"environment": ENVIRONMENT})
        envs = [TaskEnv(client, pool=pool) for _ in range(4)]
        await asyncio.gather(*(e.reset(record("go"), spec) for e in envs))
        # One sandbox ran setup (and was snapshotted); every episode restored it.
        assert len(client.created) == 1
        assert len(client.restored) == 4
        steps = await asyncio.gather(
            *(e.step(Action("bash", {"command": "cat setup.log"})) for e in envs)
        )
        assert all("ready" in s.observation for s in steps)
        for e in envs:
            await e.close()  # does not close the shared pool
        assert pool._snapshots
        await pool.aclose()

    run(go())


def test_cancelling_a_step_destroys_the_sandbox() -> None:
    async def go() -> None:
        async with TaskEnv(LocalSandboxClient()) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            assert env._env is not None
            root = env._env.sandbox.root  # type: ignore[attr-defined]
            assert root.exists()
            running = asyncio.ensure_future(env.step(Action("bash", {"command": "sleep 30"})))
            await asyncio.sleep(0.3)
            running.cancel()
            with pytest.raises(asyncio.CancelledError):
                await running
            assert not root.exists()
            with pytest.raises(RuntimeError, match="reset"):
                await env.step(Action("submit"))

    run(go())


def test_a_saved_episode_resumes_where_it_was() -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        spec = spec_of({"environment": ENVIRONMENT})
        async with TaskEnv(client) as env:
            await env.reset(record("mark"), spec)
            await env.step(Action("write_file", {"path": "notes.txt", "content": "done\n"}))
            saved = await env.snapshot()
            assert saved.steps == 1
            await env.step(Action("write_file", {"path": "notes.txt", "content": "draft\n"}))
            # A second environment continues from the save, not from the end.
            async with TaskEnv(client) as other:
                assert await other.restore(saved) == "mark"
                assert other.steps == 1
                read = await other.step(Action("read_file", {"path": "notes.txt"}))
                assert "done" in read.observation
                final = await other.step(Action("submit"))
                assert final.reward == 1.0
            await env.delete_snapshot(saved)

    run(go())


def test_resuming_needs_a_rung_that_snapshots() -> None:
    from evalsi.sandbox.client import SnapshotsUnsupported

    async def go() -> None:
        async with TaskEnv(LocalSandboxClient(snapshots=False)) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            with pytest.raises(SnapshotsUnsupported, match="cannot be resumed"):
                await env.snapshot()

    run(go())


def test_process_rewards_sit_beside_the_outcome_reward() -> None:
    from evalsi_harness.process import EvaluatorReward, ProcessReward, ToolErrorPenalty

    async def go() -> None:
        rewards: list[ProcessReward] = [
            ToolErrorPenalty(0.25),
            EvaluatorReward("builtin/tool-errors", weight=1.0),
        ]
        async with TaskEnv(LocalSandboxClient(), process_rewards=rewards) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            ok = await env.step(Action("bash", {"command": "echo hi"}))
            # No failure yet: the share of failed calls is still 0, and the outcome is untouched.
            assert (ok.reward, ok.process_reward) == (0.0, 0.0)
            bad = await env.step(Action("nope"))
            # 1 of 2 calls failed: the penalty, and the share rose by 0.5 (lower is better).
            assert bad.process_reward == pytest.approx(-0.25 - 0.5)
            assert bad.reward == 0.0
            again = await env.step(Action("bash", {"command": "echo hi"}))
            # The share fell to 1/3, from 1/2: a small credit for recovering.
            assert again.process_reward == pytest.approx(0.5 - 1 / 3)
            assert [s.name for s in env.trajectory] == ["bash", "nope", "bash"]
            final = await env.step(Action("submit"))
            assert final.reward == 0.0
            # Submitting is not a tool step: nothing is scored for it.
            assert final.process_reward is None

    run(go())


def test_a_step_reward_that_cannot_be_computed_is_none_not_zero() -> None:
    from evalsi_harness.process import EvaluatorReward

    async def go() -> None:
        # The task has no reference trajectory, so the evaluator skips every step.
        reward = EvaluatorReward("builtin/trajectory-match")
        async with TaskEnv(LocalSandboxClient(), process_rewards=[reward]) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            step = await env.step(Action("bash", {"command": "echo hi"}))
            assert step.process_reward is None
            assert step.reward == 0.0

    run(go())


def test_process_reward_state_is_per_episode_and_survives_a_resume() -> None:
    from evalsi_harness.process import EvaluatorReward

    async def go() -> None:
        client = LocalSandboxClient()
        reward = EvaluatorReward("builtin/tool-errors")
        spec = spec_of({"environment": ENVIRONMENT})
        async with TaskEnv(client, process_rewards=[reward]) as env:
            await env.reset(record("x"), spec)
            await env.step(Action("nope"))  # share 1.0
            saved = await env.snapshot()
            async with TaskEnv(client, process_rewards=[reward]) as other:
                await other.restore(saved)
                step = await other.step(Action("bash", {"command": "echo hi"}))
                # 1 of 2 failed now: down from 1.0, so a credit of 0.5, not a fresh baseline.
                assert step.process_reward == pytest.approx(0.5)
                assert len(other.trajectory) == 2
            await env.reset(record("y"), spec)
            first = await env.step(Action("bash", {"command": "echo hi"}))
            assert first.process_reward == 0.0
            await env.delete_snapshot(saved)

    run(go())


def test_actions_from_a_policy_fail_with_a_reason_instead_of_crashing() -> None:
    from evalsi_harness.gym import action_from, episode_tools

    assert action_from("bash", '{"command": "ls"}').arguments == {"command": "ls"}
    assert action_from("bash", {"command": "ls"}).arguments == {"command": "ls"}
    assert action_from("submit", "").arguments == {}
    assert "not valid JSON" in action_from("bash", "{oops").error
    assert "JSON object" in action_from("bash", "[1]").error
    assert "tool name" in action_from(None).error
    assert [t["name"] for t in episode_tools()] == ["bash", "read_file", "write_file", "submit"]
    assert "/workspace" in episode_tools()[0]["description"]


def test_a_failed_action_costs_a_step_and_reports_the_reason() -> None:
    from evalsi_harness.gym import action_from

    async def go() -> None:
        async with TaskEnv(LocalSandboxClient(), max_steps=2) as env:
            await env.reset(record(), spec_of({"environment": ENVIRONMENT}))
            step = await env.step(action_from("bash", "{oops"))
            assert "not valid JSON" in step.observation
            assert step.info["tool_error"] is True
            assert env.steps == 1
            assert (await env.step(action_from("bash", "[1]"))).done  # the step limit

    run(go())

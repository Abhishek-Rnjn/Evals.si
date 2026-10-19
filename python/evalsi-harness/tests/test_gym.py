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

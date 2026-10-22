"""Resuming a rollout by replay, on a rung that cannot snapshot (the pod rung)."""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from evalsi_harness.models import ModelTurn
from evalsi_harness.rollout import CANCELLED, DONE, ERROR, RolloutPool
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record
from tests.test_rollout import SPEC, Scripted, call, mark_done


def run(coro: Any) -> Any:
    return asyncio.run(coro)


def submit() -> ModelTurn:
    return ModelTurn(text="", tool_calls=[call("submit", "c2")])


async def cancelled_after_first_turn(
    client: LocalSandboxClient, rec: Any = None
) -> tuple[RolloutPool, Any]:
    pool_ref: list[RolloutPool] = []

    async def stop_first(n: int) -> None:
        if n == 1:
            pool_ref[0].stop()

    pool = RolloutPool(Scripted([mark_done(), submit()], on_call=stop_first), client)
    pool_ref.append(pool)
    [t] = await pool.run([(rec or record("mark"), SPEC)])
    pool.clear()
    return pool, t


def test_a_cancelled_trajectory_resumes_by_replaying_its_calls_into_a_fresh_sandbox() -> None:
    async def go() -> None:
        client = LocalSandboxClient(snapshots=False)
        roots: list[Any] = []
        original = client.create

        async def tracking(spec: Any = None) -> Any:
            sandbox = await original(spec)
            roots.append(sandbox.root)
            return sandbox

        client.create = tracking  # type: ignore[method-assign]
        pool, first = await cancelled_after_first_turn(client)
        assert first.status == CANCELLED
        assert not first.resumable
        assert first.replayable
        assert len(first.turns) == 1
        assert "replaying" in first.error

        model = Scripted([submit()])
        pool.model = model
        [done] = await pool.run(resume=[first])
        # The recorded write was re-run in a new sandbox, so the checker sees "done".
        assert (done.status, done.reward) == (DONE, 1.0)
        assert done.info["replay"] == {"turns": 1, "steps": 1, "diverged": []}
        assert len(done.turns) == 2
        # The model was not called for the replayed turn, and saw its conversation.
        assert model.seen == [3]
        assert len(roots) >= 3  # setup, the first episode, and the replay's fresh one
        assert not any(r.exists() for r in roots)
        await pool.aclose()

    run(go())


@pytest.mark.parametrize(
    ("meta", "why"),
    [
        ({"replay_safe": False}, "replay_safe: false"),
        ({"non_idempotent_tools": ["write_file"]}, "non-idempotent tools (write_file)"),
        ({"non_idempotent_tools": True}, "non-idempotent tools"),
        ({"environment": {"sandbox": {"network": "allow"}}}, "network access"),
    ],
)
def test_replay_is_refused_when_it_is_not_safe(meta: dict[str, Any], why: str) -> None:
    async def go() -> None:
        client = LocalSandboxClient(snapshots=False)
        pool, first = await cancelled_after_first_turn(client, record("mark", metadata=meta))
        assert first.replayable
        created: list[int] = []
        original = client.create

        async def counting(spec: Any = None) -> Any:
            created.append(1)
            return await original(spec)

        client.create = counting  # type: ignore[method-assign]
        pool.model = Scripted([])
        [refused] = await pool.run(resume=[first])
        assert refused.status == ERROR
        assert "not resumed" in refused.error
        assert why in refused.error
        assert not created, "nothing was set up for a replay that is not safe"
        await pool.aclose()

    run(go())


def test_replay_safe_true_allows_a_networked_sandbox() -> None:
    async def go() -> None:
        meta = {"environment": {"sandbox": {"network": "allow"}}, "replay_safe": True}
        client = LocalSandboxClient(snapshots=False)
        pool, first = await cancelled_after_first_turn(client, record("mark", metadata=meta))
        pool.model = Scripted([submit()])
        [done] = await pool.run(resume=[first])
        assert done.status == DONE
        await pool.aclose()

    run(go())


def test_a_replay_that_does_not_reproduce_the_recording_names_where_it_diverged() -> None:
    async def go() -> None:
        client = LocalSandboxClient(snapshots=False)
        pool, first = await cancelled_after_first_turn(client)
        first.turns[0].observations[0] = "wrote 999 bytes to notes.txt"  # what it once said
        pool.model = Scripted([submit()])
        [lenient] = await pool.run(resume=[first])
        [d] = lenient.info["replay"]["diverged"]
        assert (d["turn"], d["call"]) == (0, "write_file")
        assert d["recorded"] == "wrote 999 bytes to notes.txt"
        assert d["replayed"] == "wrote 5 bytes to notes.txt"
        [strict] = await pool.run(resume=[first], strict_replay=True)
        assert strict.status == ERROR
        assert "diverged at turn 0" in strict.error
        await pool.aclose()

    run(go())


def test_a_trajectory_that_never_ran_a_turn_has_nothing_to_replay() -> None:
    async def go() -> None:
        pool = RolloutPool(Scripted([mark_done()]), LocalSandboxClient(snapshots=False))
        pool.stop()  # before anything starts
        [t] = await pool.run([(record(), SPEC)])
        assert not t.replayable
        with pytest.raises(ValueError, match="cancelled one with its turns"):
            await pool.run(resume=[t])
        await pool.aclose()

    run(go())

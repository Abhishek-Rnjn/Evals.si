"""Rollouts: a model plays task episodes, concurrently, with tokens, pausing and resuming."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable
from typing import Any

import pytest

from evalsi_harness.gym import TaskEnv
from evalsi_harness.modelhttp import ModelError
from evalsi_harness.models import ModelTurn, TokenTrace, ToolCallRequest
from evalsi_harness.rollout import CANCELLED, DONE, ERROR, PAUSED, RolloutPool, rollout
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, spec_of

ENVIRONMENT: dict[str, Any] = {
    "files": {"notes.txt": "draft\n"},
    "setup": ["echo ready > setup.log"],
    "checker": {"command": ["sh", "-c", "grep -q done notes.txt"]},
}
SPEC = spec_of({"environment": ENVIRONMENT})


def call(name: str, call_id: str, **arguments: Any) -> ToolCallRequest:
    return ToolCallRequest(call_id, name, json.dumps(arguments))


def mark_done(call_id: str = "c1") -> ModelTurn:
    return ModelTurn(
        text="writing",
        tool_calls=[call("write_file", call_id, path="notes.txt", content="done\n")],
        stop_reason="tool_calls",
    )


def trace(prompt: list[int], completion: list[int]) -> TokenTrace:
    return TokenTrace(prompt, completion, [-0.5] * len(completion))


class Scripted:
    """A model that plays a fixed script of turns, and remembers what it was shown."""

    name = "scripted"

    def __init__(self, turns: list[ModelTurn], *, on_call: Callable[[int], Any] | None = None):
        self.turns = list(turns)
        self.seen: list[int] = []
        self.on_call = on_call

    async def complete(self, messages: list[Any], tools: list[Any], **_: Any) -> ModelTurn:
        self.seen.append(len(messages))
        if self.on_call is not None:
            await self.on_call(len(self.seen))
        if not self.turns:
            raise ModelError("the script ran out")
        return self.turns.pop(0)

    async def aclose(self) -> None: ...


def run(coro: Any) -> Any:
    return asyncio.run(coro)


def test_a_rollout_keeps_each_turns_tokens_and_the_outcome_reward() -> None:
    async def go() -> None:
        first = mark_done()
        first.tokens = trace([1, 2], [3, 4])
        last = ModelTurn(
            text="", tool_calls=[call("submit", "c2")], tokens=trace([1, 2, 3, 4], [5])
        )
        async with TaskEnv(LocalSandboxClient()) as env:
            t = await rollout(Scripted([first, last]), env, record("mark it"), SPEC)
        assert (t.status, t.reward) == (DONE, 1.0)
        assert [x.tokens.completion_token_ids for x in t.turns if x.tokens] == [[3, 4], [5]]
        assert t.tokens_complete
        assert t.turns[0].observations == ["wrote 5 bytes to notes.txt"]
        assert t.messages[0] == {"role": "user", "content": "mark it"}
        assert t.info["ended"] == "submitted"

    run(go())


def test_a_turn_without_tokens_makes_the_trajectory_say_so() -> None:
    async def go() -> None:
        a = mark_done()
        a.tokens = trace([1], [2])
        b = ModelTurn(text="all done")  # no tool call: the episode is submitted, and no tokens
        async with TaskEnv(LocalSandboxClient()) as env:
            t = await rollout(Scripted([a, b]), env, record(), SPEC)
        assert (t.status, t.reward) == (DONE, 1.0)
        assert not t.tokens_complete
        assert t.turns[1].tokens is None

    run(go())


def test_gaps_the_server_named_are_not_complete_either() -> None:
    async def go() -> None:
        a = ModelTurn(text="x", tool_calls=[call("submit", "c")])
        a.tokens = TokenTrace([], [2], [-1.0], missing=["prompt_token_ids"])
        async with TaskEnv(LocalSandboxClient()) as env:
            t = await rollout(Scripted([a]), env, record(), SPEC)
        assert not t.tokens_complete

    run(go())


def test_a_model_failure_is_an_error_not_a_zero_reward() -> None:
    async def go() -> None:
        async with TaskEnv(LocalSandboxClient()) as env:
            t = await rollout(Scripted([]), env, record(), SPEC)
        assert (t.status, t.reward) == (ERROR, None)
        assert "script ran out" in t.error

    run(go())


def test_running_out_of_turns_grades_the_end_state() -> None:
    async def go() -> None:
        async with TaskEnv(LocalSandboxClient()) as env:
            t = await rollout(Scripted([mark_done()]), env, record(), SPEC, max_turns=1)
        assert (t.status, t.reward) == (DONE, 1.0)

    run(go())


def test_a_pool_runs_rollouts_concurrently_over_one_setup() -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        active = peak = 0

        async def slow(_: int) -> None:
            nonlocal active, peak
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(0.05)
            active -= 1

        model = Scripted(
            [mark_done(f"c{i}") for i in range(6)] + [ModelTurn(text="d")] * 6, on_call=slow
        )
        pool = RolloutPool(model, client, concurrency=3)
        out = await pool.run([(record(id=f"t{i}"), SPEC) for i in range(6)])
        assert [t.status for t in out] == [DONE] * 6
        assert peak == 3  # never more than the limit, and the limit was used
        assert len(client.created) == 1  # setup ran once for six episodes
        await pool.aclose()

    run(go())


def test_stopping_pauses_rollouts_and_resuming_continues_them() -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        pool_ref: list[RolloutPool] = []

        async def stop_after_first(n: int) -> None:
            if n == 1:
                pool_ref[0].stop()

        model = Scripted(
            [mark_done(), ModelTurn(text="", tool_calls=[call("submit", "c2")])],
            on_call=stop_after_first,
        )
        pool = RolloutPool(model, client)
        pool_ref.append(pool)
        [first] = await pool.run([(record("mark"), SPEC)])
        # The first turn ran and the stop was seen before the second model call.
        assert first.status == PAUSED
        assert first.resumable
        assert len(first.turns) == 1
        assert first.reward is None

        # While stopped, nothing new starts, and a paused one is handed back as it was.
        [again] = await pool.run(resume=[first])
        assert again is first
        [fresh] = await pool.run([(record(id="other"), SPEC)])
        assert fresh.status == CANCELLED

        pool.clear()
        [done] = await pool.run(resume=[first])
        assert (done.status, done.reward) == (DONE, 1.0)
        assert len(done.turns) == 2
        # The model saw the first turn's conversation when it continued.
        assert model.seen == [1, 3]
        await pool.delete_snapshot(first)
        await pool.aclose()

    run(go())


def test_a_rung_that_cannot_snapshot_cancels_instead_of_pretending() -> None:
    async def go() -> None:
        pool_ref: list[RolloutPool] = []

        async def stop(_: int) -> None:
            pool_ref[0].stop()

        pool = RolloutPool(
            Scripted([mark_done()], on_call=stop), LocalSandboxClient(snapshots=False)
        )
        pool_ref.append(pool)
        [t] = await pool.run([(record(), SPEC)])
        assert t.status == CANCELLED
        assert not t.resumable
        assert "cannot snapshot" in t.error
        with pytest.raises(ValueError, match="paused"):
            await pool.run(resume=[t])
        await pool.aclose()

    run(go())


def test_cancelling_the_run_destroys_every_sandbox() -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        roots: list[Any] = []
        original = client.create

        async def tracking(spec: Any = None) -> Any:
            sandbox = await original(spec)
            roots.append(sandbox.root)
            return sandbox

        client.create = tracking  # type: ignore[method-assign]
        started = asyncio.Event()

        async def hang(_: int) -> None:
            started.set()
            await asyncio.sleep(60)

        pool = RolloutPool(Scripted([mark_done()] * 2, on_call=hang), client, concurrency=2)
        running = asyncio.ensure_future(pool.run([(record(id="a"), SPEC), (record(id="b"), SPEC)]))
        await started.wait()
        await asyncio.sleep(0.1)
        running.cancel()
        with pytest.raises(asyncio.CancelledError):
            await running
        assert roots
        assert not any(r.exists() for r in roots)
        await pool.aclose()

    run(go())

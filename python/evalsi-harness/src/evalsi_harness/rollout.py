"""Rollouts for trainers: a model plays task episodes, many at once, and an
episode can be paused and continued.

::

    pool = RolloutPool(model, sandbox_client, concurrency=16)
    trajectories = await pool.run([(record, spec)] * 8)   # eight rollouts of one task
    for t in trajectories:
        t.reward                  # the checker's outcome reward (None: it could not run)
        t.turns[0].tokens         # the first turn's prompt and completion token IDs, log-probs

Each rollout is :func:`rollout`: the model calls the task's tools
(:class:`~evalsi_harness.gym.TaskEnv`) until it calls ``submit``, stops
calling tools (which submits for it), or reaches ``max_turns``. A
:class:`Trajectory` holds every turn with its tool calls, observations,
process rewards and, when the model client was built with
``TargetConfig(token_trace=True)``, its token IDs and log-probabilities.
Turns the server gave no tokens for say so (``Trajectory.tokens_complete``)
rather than carrying an empty list.

The rollouts of a pool share one environment pool, so setup runs once per
distinct environment however many rollouts there are. ``pool.stop()`` asks
the running rollouts to pause at their next turn boundary: each returns a
``paused`` trajectory holding a snapshot of its episode, and
``pool.run(..., resume=[...])`` continues them where they stopped (rollouts not
yet started come back ``cancelled``, with nothing to resume). Pausing needs a
sandbox rung that snapshots; on the others a stopped rollout is
``cancelled`` and cannot be continued, because the sandbox's state cannot be
kept. Cancelling the ``run`` task itself destroys every episode's sandbox.

Resuming continues the *same* conversation and sandbox state, so it is exact
for a paused trajectory. It is not a replay: nothing is re-run.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from functools import partial
from typing import Any

from evalsi.sandbox.client import SnapshotsUnsupported
from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike
from evalsi_harness.models import ChatModel, ModelError, ModelTurn, TokenTrace, ToolSpec
from evalsi_harness.process import ProcessReward

SYSTEM = (
    "You are completing a task in a sandbox. Use the tools to do the work, then call submit. "
    "Your work is graded on the sandbox's final state."
)

DONE = "done"  # the episode ended: submitted, or at a step limit
PAUSED = "paused"  # stopped between turns, with a snapshot to resume from
CANCELLED = "cancelled"  # stopped, with nothing to resume
ERROR = "error"  # the model or sandbox failed; no reward


@dataclass
class Turn:
    """One model call and what its tool calls did."""

    text: str
    tool_calls: list[dict[str, Any]] = field(default_factory=list)
    observations: list[str] = field(default_factory=list)
    # One per tool call: the step's process reward (None: not computed).
    process_rewards: list[float | None] = field(default_factory=list)
    stop_reason: str = ""
    # None: the client was not asked for tokens, or the model that served the
    # turn cannot return them.
    tokens: TokenTrace | None = None
    model: str = ""


@dataclass
class Trajectory:
    record_id: str
    status: str = DONE
    turns: list[Turn] = field(default_factory=list)
    # The checker's outcome reward; None when the episode did not end or the
    # checker could not run.
    reward: float | None = None
    process_reward: float = 0.0
    info: dict[str, Any] = field(default_factory=dict)
    error: str = ""
    # The conversation so far, and the episode's snapshot, to resume from.
    messages: list[dict[str, Any]] = field(default_factory=list)
    snapshot: gym.EpisodeSnapshot | None = None
    # The task, so a resumed rollout needs nothing else.
    record: Record | None = None
    spec: run_pb2.RunSpec | None = None

    @property
    def tokens_complete(self) -> bool:
        """Every turn has its prompt IDs, completion IDs and log-probabilities."""
        return bool(self.turns) and all(
            t.tokens is not None and not t.tokens.missing for t in self.turns
        )

    @property
    def resumable(self) -> bool:
        return self.status == PAUSED and self.snapshot is not None


def _specs(tools: list[dict[str, Any]]) -> list[ToolSpec]:
    return [ToolSpec(t["name"], t["description"], t["input_schema"]) for t in tools]


async def rollout(
    model: ChatModel,
    env: gym.TaskEnv,
    record: Record | dict[str, Any] | None = None,
    spec: run_pb2.RunSpec | None = None,
    *,
    system: str = SYSTEM,
    max_turns: int = 30,
    max_tokens: int = 0,
    should_stop: Callable[[], bool] | None = None,
    resume: Trajectory | None = None,
) -> Trajectory:
    """Play one episode. ``resume`` continues a paused trajectory (the task is
    in it, so ``record`` and ``spec`` are not given). ``should_stop`` is asked
    before every model call: when it says so the rollout pauses (or is
    cancelled, if its rung cannot snapshot)."""
    if resume is not None:
        if not resume.resumable:
            raise ValueError(
                f"only a paused trajectory with a snapshot can be resumed, not {resume.status!r}"
            )
        assert resume.snapshot is not None
        instruction = await env.restore(resume.snapshot)
        traj = Trajectory(
            resume.record_id,
            turns=list(resume.turns),
            process_reward=resume.process_reward,
            messages=[dict(m) for m in resume.messages],
            record=resume.record,
            spec=resume.spec,
        )
    else:
        if record is None:
            raise ValueError("a new rollout needs a record")
        instruction = await env.reset(record, spec)
        rec = env.record
        assert rec is not None
        traj = Trajectory(
            rec.id,
            messages=[{"role": "user", "content": instruction}],
            record=rec,
            spec=env.spec,
        )
    tools = _specs(env.tools)
    started = len(traj.turns)
    try:
        for _ in range(max_turns - started):
            if should_stop is not None and should_stop():
                return await _pause(env, traj)
            try:
                turn = await model.complete(
                    traj.messages, tools, system=system, max_tokens=max_tokens
                )
            except ModelError as exc:
                traj.status, traj.error = ERROR, f"the model failed: {exc}"
                return traj
            done = await _play(env, traj, turn)
            if done:
                return traj
        # Out of turns: the end state is graded as it stands.
        return await _submit(env, traj)
    except BaseException:
        # Including cancellation: an abandoned episode leaves no sandbox behind.
        await env.abort()
        raise


async def _pause(env: gym.TaskEnv, traj: Trajectory) -> Trajectory:
    try:
        traj.snapshot = await env.snapshot()
        traj.status = PAUSED
    except SnapshotsUnsupported:
        traj.status = CANCELLED
        traj.error = "stopped; this sandbox rung cannot snapshot, so it cannot be resumed"
    await env.abort()
    return traj


async def _submit(env: gym.TaskEnv, traj: Trajectory) -> Trajectory:
    result = await env.step(gym.Action("submit"))
    return _finish(traj, result)


def _finish(traj: Trajectory, result: gym.StepResult) -> Trajectory:
    traj.status = DONE
    traj.reward = result.reward
    traj.info = result.info
    if result.reward is None:
        traj.error = f"the task's checker could not run: {result.info.get('error')}"
    return traj


async def _play(env: gym.TaskEnv, traj: Trajectory, turn: ModelTurn) -> bool:
    """Run one model turn's tool calls. True when the episode ended."""
    record = Turn(
        text=turn.text,
        tool_calls=[
            {"id": c.id, "name": c.name, "arguments": c.arguments} for c in turn.tool_calls
        ],
        stop_reason=turn.stop_reason,
        tokens=turn.tokens,
        model=turn.model,
    )
    traj.turns.append(record)
    traj.messages.append(turn.as_message())
    if not turn.tool_calls:
        # The model stopped calling tools: it is done, so submit for it.
        _finish(traj, await env.step(gym.Action("submit")))
        return True
    for call in turn.tool_calls:
        result = await env.step(gym.action_from(call.name, call.arguments))
        record.observations.append(result.observation)
        record.process_rewards.append(result.process_reward)
        traj.process_reward += result.process_reward or 0.0
        traj.messages.append(
            {
                "role": "tool",
                "tool_call_id": call.id,
                "content": result.observation,
                "is_error": bool(result.info.get("tool_error")),
            }
        )
        if result.done:
            _finish(traj, result)
            return True
    return False


class RolloutPool:
    """Many rollouts at once over one sandbox client, sharing setup."""

    def __init__(
        self,
        model: ChatModel,
        client: SandboxClientLike,
        *,
        concurrency: int = 8,
        max_steps: int = 30,
        max_turns: int = 30,
        process_rewards: Sequence[ProcessReward] = (),
        system: str = SYSTEM,
    ) -> None:
        if concurrency < 1:
            raise ValueError("concurrency must be at least 1")
        self.model = model
        self.environments = EnvironmentPool(client)
        self._client = client
        self._slots = asyncio.Semaphore(concurrency)
        self._stop = asyncio.Event()
        self._options: dict[str, Any] = {
            "max_steps": max_steps,
            "process_rewards": process_rewards,
        }
        self.max_turns = max_turns
        self.system = system

    def stop(self) -> None:
        """Pause the running rollouts at their next turn boundary and keep
        not-yet-started ones from starting."""
        self._stop.set()

    def clear(self) -> None:
        """Allow rollouts to run again after a :meth:`stop`."""
        self._stop.clear()

    async def run(
        self,
        tasks: Sequence[tuple[Record | dict[str, Any], run_pb2.RunSpec | None]] = (),
        *,
        resume: Sequence[Trajectory] = (),
        max_tokens: int = 0,
    ) -> list[Trajectory]:
        """Run ``tasks`` (new rollouts) and ``resume`` (paused ones), at most
        ``concurrency`` at a time. Results are in order: tasks, then resumed."""
        for paused in resume:
            if not paused.resumable:
                raise ValueError(
                    f"only a paused trajectory with a snapshot can be resumed, "
                    f"not {paused.status!r}"
                )
        jobs = [partial(self._one, record, spec, None, max_tokens) for record, spec in tasks]
        jobs += [partial(self._one, None, None, paused, max_tokens) for paused in resume]
        # A failure in one rollout cancels the rest, which destroys their sandboxes.
        async with asyncio.TaskGroup() as group:
            running = [group.create_task(job()) for job in jobs]
        return [t.result() for t in running]

    async def _one(
        self,
        record: Record | dict[str, Any] | None,
        spec: run_pb2.RunSpec | None,
        resume: Trajectory | None,
        max_tokens: int,
    ) -> Trajectory:
        async with self._slots:
            if self._stop.is_set():
                # Never started (or, for a resume, still paused): hand it back as it was.
                if resume is not None:
                    return resume
                rec = record.id if isinstance(record, Record) else str((record or {}).get("id", ""))
                return Trajectory(rec, status=CANCELLED, error="stopped before it started")
            env = gym.TaskEnv(self._client, pool=self.environments, **self._options)
            try:
                return await rollout(
                    self.model,
                    env,
                    record,
                    spec,
                    system=self.system,
                    max_turns=self.max_turns,
                    max_tokens=max_tokens,
                    should_stop=self._stop.is_set,
                    resume=resume,
                )
            finally:
                await env.close()

    async def delete_snapshot(self, trajectory: Trajectory) -> None:
        """Free a paused trajectory's snapshot once it will not be resumed."""
        if trajectory.snapshot is not None:
            with contextlib.suppress(Exception):
                await self._client.delete_snapshot(trajectory.snapshot.snapshot_id)
            trajectory.snapshot = None

    async def aclose(self) -> None:
        await self.environments.aclose()

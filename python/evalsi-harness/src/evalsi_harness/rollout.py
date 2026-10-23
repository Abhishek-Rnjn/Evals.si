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

Resuming a paused trajectory continues the *same* conversation and sandbox
state, so it is exact. It is not a replay: nothing is re-run.

A rung that cannot snapshot cancels a stopped rollout instead, with its turns
kept. Such a trajectory is still resumable by *replay*: a fresh sandbox is set
up, the recorded turns' tool calls are run into it in order (the model is not
called for them), and the rollout continues from the recorded conversation.
That is deterministic re-execution, so it is only safe when running the same
calls again gives the same state. A task that says otherwise is refused with
:class:`ReplayRefused`, not replayed: a record's ``metadata["replay_safe"]``
set to False, ``metadata["non_idempotent_tools"]`` naming a tool the trajectory
used (or ``True``), or a sandbox with network access, whose calls may have had
effects outside it (unless ``metadata["replay_safe"]`` is True). A replayed
observation that differs from the recorded one is listed in
``Trajectory.info["replay"]["diverged"]``; ``strict_replay`` makes it an error.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field, replace
from functools import partial
from typing import Any

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import SnapshotsUnsupported
from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike
from evalsi_harness.models import ChatModel, ModelError, ModelTurn, TokenTrace, ToolSpec
from evalsi_harness.process import ProcessReward
from evalsi_harness.task import TaskError

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


class ReplayRefused(RuntimeError):
    """Replaying a trajectory into a fresh sandbox is not safe, or did not
    reproduce it (``strict_replay``): the message says why."""


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

    @property
    def replayable(self) -> bool:
        """Cancelled with its turns kept (its rung could not snapshot), so it can
        be resumed by replaying them into a fresh sandbox. Whether that is safe is
        decided when it is tried (:class:`ReplayRefused`)."""
        return (
            self.status == CANCELLED
            and self.snapshot is None
            and bool(self.turns)
            and self.record is not None
            and self.spec is not None
        )


def _record_id(record: Record | dict[str, Any] | None) -> str:
    if isinstance(record, Record):
        return record.id
    return str((record or {}).get("id", ""))


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
    strict_replay: bool = False,
) -> Trajectory:
    """Play one episode. ``resume`` continues a paused trajectory from its
    snapshot, or a cancelled one by replaying it (see the module docstring; the
    task is in it, so ``record`` and ``spec`` are not given). ``should_stop`` is
    asked before every model call: when it says so the rollout pauses (or is
    cancelled, if its rung cannot snapshot)."""
    replay: dict[str, Any] | None = None
    if resume is not None:
        if not (resume.resumable or resume.replayable):
            raise ValueError(
                "only a paused trajectory with a snapshot, or a cancelled one with its turns, "
                f"can be resumed, not {resume.status!r}"
            )
        if resume.resumable:
            assert resume.snapshot is not None
            instruction = await env.restore(resume.snapshot)
        else:
            unsafe = unsafe_to_replay(resume)
            if unsafe:
                raise ReplayRefused(unsafe)
            assert resume.record is not None
            instruction = await env.reset(resume.record, resume.spec)
        traj = Trajectory(
            resume.record_id,
            turns=list(resume.turns),
            process_reward=resume.process_reward,
            messages=[dict(m) for m in resume.messages],
            record=resume.record,
            spec=resume.spec,
        )
        if not resume.resumable:
            try:
                replay = await _replay(env, resume, strict_replay)
            except BaseException:
                await env.abort()
                raise
            traj.info = {"replay": replay}
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


def unsafe_to_replay(traj: Trajectory) -> str:
    """Why replaying ``traj``'s tool calls into a fresh sandbox is not safe, or
    an empty string when it is."""
    assert traj.record is not None
    meta = traj.record.metadata or {}
    if meta.get("replay_safe") is False:
        return f"record {traj.record_id} declares replay_safe: false, so its calls are not re-run"
    declared = meta.get("non_idempotent_tools")
    used = {c["name"] for t in traj.turns for c in t.tool_calls}
    if declared is True or (isinstance(declared, list) and used & {str(n) for n in declared}):
        names = sorted(used if declared is True else used & {str(n) for n in declared})
        return (
            f"record {traj.record_id} declares non-idempotent tools ({', '.join(names)}) that "
            "this trajectory called, so replaying them would repeat their side effects"
        )
    if meta.get("replay_safe") is not True and traj.spec is not None:
        network = ""
        record_env = meta.get("environment")
        if isinstance(record_env, dict):
            network = str((record_env.get("sandbox") or {}).get("network") or "")
        if not network and traj.spec.HasField("environment"):
            network = traj.spec.environment.sandbox.network
        if network and network != "deny":
            return (
                f"record {traj.record_id}'s sandbox has network access ({network}), so its calls "
                "may have had effects outside it; set metadata replay_safe: true to replay anyway"
            )
    return ""


async def _replay(env: gym.TaskEnv, recorded: Trajectory, strict: bool) -> dict[str, Any]:
    """Run the recorded turns' tool calls into the fresh sandbox, in order. The
    conversation and rewards stay as recorded; the observations are compared."""
    diverged: list[dict[str, Any]] = []
    steps = 0
    for index, turn in enumerate(recorded.turns):
        for position, call in enumerate(turn.tool_calls):
            result = await env.step(gym.action_from(call["name"], call["arguments"]))
            steps += 1
            if result.done:
                raise ReplayRefused(
                    f"replaying turn {index} ended the episode before the recorded turns were done"
                )
            seen = turn.observations[position] if position < len(turn.observations) else None
            if result.observation != seen:
                diverged.append(
                    {
                        "turn": index,
                        "call": call["name"],
                        "recorded": seen,
                        "replayed": result.observation,
                    }
                )
                if strict:
                    raise ReplayRefused(
                        f"the replay diverged at turn {index}, call {call['name']!r}: "
                        f"recorded {str(seen)[:200]!r}, replayed {result.observation[:200]!r}"
                    )
    return {"turns": len(recorded.turns), "steps": steps, "diverged": diverged}


async def _pause(env: gym.TaskEnv, traj: Trajectory) -> Trajectory:
    try:
        traj.snapshot = await env.snapshot()
        traj.status = PAUSED
    except SnapshotsUnsupported:
        traj.status = CANCELLED
        traj.error = (
            "stopped; this sandbox rung cannot snapshot, so it can only be resumed by replaying "
            "its tool calls into a fresh sandbox"
        )
    await env.abort()
    return traj


async def _submit(env: gym.TaskEnv, traj: Trajectory) -> Trajectory:
    result = await env.step(gym.Action("submit"))
    return _finish(traj, result)


def _finish(traj: Trajectory, result: gym.StepResult) -> Trajectory:
    traj.status = DONE
    traj.reward = result.reward
    traj.info = {**traj.info, **result.info}  # keeps what a replay recorded
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
        strict_replay: bool = False,
    ) -> list[Trajectory]:
        """Run ``tasks`` (new rollouts) and ``resume`` (paused ones), at most
        ``concurrency`` at a time. Results are in order: tasks, then resumed."""
        for paused in resume:
            if not (paused.resumable or paused.replayable):
                raise ValueError(
                    "only a paused trajectory with a snapshot, or a cancelled one with its "
                    f"turns, can be resumed, not {paused.status!r}"
                )
        jobs = [
            partial(self._one, record, spec, None, max_tokens, strict_replay)
            for record, spec in tasks
        ]
        jobs += [
            partial(self._one, None, None, paused, max_tokens, strict_replay) for paused in resume
        ]
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
        strict_replay: bool = False,
    ) -> Trajectory:
        async with self._slots:
            if self._stop.is_set():
                # Never started (or, for a resume, still paused): hand it back as it was.
                if resume is not None:
                    return resume
                return Trajectory(
                    _record_id(record), status=CANCELLED, error="stopped before it started"
                )
            env = gym.TaskEnv(self._client, pool=self.environments, **self._options)
            try:
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
                        strict_replay=strict_replay,
                    )
                except ReplayRefused as exc:
                    # Replaying was not safe, or did not reproduce the recording: an
                    # error that names why, with the trajectory left as it was.
                    assert resume is not None
                    return replace(resume, status=ERROR, error=f"not resumed: {exc}")
                except (TaskError, SandboxError) as exc:
                    # This task's environment failed (its setup, say): that rollout is an
                    # error with no reward, and the others carry on.
                    message = f"the environment failed: {exc}"
                    if resume is not None:
                        return replace(resume, status=ERROR, error=message)
                    return Trajectory(_record_id(record), status=ERROR, error=message)
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

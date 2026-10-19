"""Agent tasks as RL environments: ``reset`` and ``step``.

The same task definitions that agent runs evaluate (an environment with an
image, setup and a checker, see :mod:`evalsi_harness.task`) become
environments a trainer can roll out in::

    from evalsi_harness.gym import Action, TaskEnv

    async with TaskEnv() as env:                 # a private sandbox service
        obs = await env.reset(record, spec)      # the task's instruction
        while True:
            step = await env.step(Action("bash", {"command": "pytest -q"}))
            if step.done:
                break
        print(step.reward, step.info["check"])

Actions are calls to the sandbox tools (``bash``, ``read_file``,
``write_file``) or ``submit``. The reward is 0 until the episode ends; then
the environment's checker grades the end state and the reward is its score
(or 1 for a pass, 0 for a fail). An episode ends on ``submit`` or after
``max_steps`` actions. A checker that cannot run ends the episode with
``reward`` ``None`` and the reason in ``info["error"]``: an infrastructure
failure is never a reward of 0.

Setup runs once per distinct environment and is snapshotted, so ``reset``
costs a restore, not a rebuild (on rungs that snapshot). Environments built
with the same ``pool=`` share that setup, so many concurrent rollouts of one
task pay it once. Cancelling a running ``reset`` or ``step`` destroys the
episode's sandbox. ``snapshot()`` saves a running episode (its sandbox and its
step count) so ``restore()`` can continue it later, on rungs that snapshot; on
the others it raises ``SnapshotsUnsupported`` rather than pretend.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from types import SimpleNamespace
from typing import Any, cast

from evalsi.sandbox.client import SnapshotsUnsupported, connect
from evalsi.types import Content, Record, Step
from evalsi.v1alpha1 import run_pb2
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike, TaskEnvironment
from evalsi_harness.isolate import run_clean_checker
from evalsi_harness.process import ProcessContext, ProcessReward
from evalsi_harness.task import SandboxPolicy, Task, TaskError
from evalsi_harness.tools import Tool, sandbox_tools


@dataclass
class Action:
    """A tool call (``bash``, ``read_file``, ``write_file``) or ``submit``."""

    tool: str
    arguments: dict[str, Any] = field(default_factory=dict)
    # An action the caller could not form (arguments that are not JSON): it
    # costs a step and is a failed tool call with this message, not a crash.
    error: str = ""


def tool_schemas(tools: Sequence[Tool]) -> list[dict[str, Any]]:
    """The schemas of ``tools`` and of ``submit``, as chat models take them."""
    specs = [
        {
            "name": t.spec.name,
            "description": t.spec.description,
            "input_schema": t.spec.input_schema,
        }
        for t in tools
    ]
    specs.append(
        {
            "name": "submit",
            "description": "End the episode; the task's checker grades the result.",
            "input_schema": {"type": "object", "properties": {}},
        }
    )
    return specs


def episode_tools() -> list[dict[str, Any]]:
    """The schemas of every task episode's actions, before any episode exists."""
    # The tool objects read only the working directory (for their descriptions)
    # before they are called; this is the default one.
    stand_in = SimpleNamespace(workdir=SandboxPolicy().workdir)
    return tool_schemas(sandbox_tools(cast(Any, stand_in)))


def action_from(tool: Any, arguments: Any = "{}") -> Action:
    """An :class:`Action` from what a policy produced: a tool name and its
    arguments as a JSON object, as text or already parsed. What cannot be
    read becomes an action that fails with the reason (see ``Action.error``)."""
    if not isinstance(tool, str) or not tool:
        return Action("", error="an action needs a tool name")
    if isinstance(arguments, str):
        try:
            arguments = json.loads(arguments or "{}")
        except json.JSONDecodeError as exc:
            return Action(tool, error=f"arguments are not valid JSON: {exc}")
    if not isinstance(arguments, Mapping):
        return Action(tool, error="arguments must be a JSON object")
    return Action(tool, dict(arguments))


@dataclass
class StepResult:
    observation: str
    reward: float | None
    done: bool
    info: dict[str, Any] = field(default_factory=dict)
    # The sum of the environment's process rewards for this step (``reward`` is
    # still the checker's outcome alone); None when there are none, or any of
    # them could not be computed (see ``info["process_errors"]``).
    process_reward: float | None = None


@dataclass
class EpisodeSnapshot:
    """A running episode saved by :meth:`TaskEnv.snapshot`: its sandbox's
    snapshot, the task, and how far it had got. Delete it with
    :meth:`TaskEnv.delete_snapshot` when it is no longer needed."""

    snapshot_id: str
    record: Record
    spec: run_pb2.RunSpec
    steps: int
    trajectory: list[Step] = field(default_factory=list)
    # Process-reward state (the last score of each), so deltas continue.
    memory: dict[int, Any] = field(default_factory=dict)


class TaskEnv:
    """One episode at a time over a sandbox client.

    ``pool`` shares setup with other environments (it is not closed here);
    without one the environment makes its own.
    """

    def __init__(
        self,
        client: SandboxClientLike | None = None,
        *,
        max_steps: int = 30,
        pool: EnvironmentPool | None = None,
        process_rewards: Sequence[ProcessReward] = (),
    ) -> None:
        if max_steps < 1:
            raise ValueError("max_steps must be at least 1")
        self.max_steps = max_steps
        self.process_rewards = list(process_rewards)
        self.trajectory: list[Step] = []
        self._memory: dict[int, Any] = {}
        self._client = client
        self._owned: Any = None
        self._pool = pool
        self._shared_pool = pool is not None
        self._spec: run_pb2.RunSpec | None = None
        self._env: TaskEnvironment | None = None
        self._task: Task | None = None
        self._record: Record | None = None
        self._tools: dict[str, Tool] = {}
        self.steps = 0

    async def __aenter__(self) -> TaskEnv:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    @property
    def tools(self) -> list[dict[str, Any]]:
        """The actions' schemas, in the form chat models take tool definitions."""
        return tool_schemas(list(self._tools.values()))

    @property
    def record(self) -> Record | None:
        """The running episode's record (None between episodes)."""
        return self._record

    @property
    def spec(self) -> run_pb2.RunSpec | None:
        return self._spec

    async def abort(self) -> None:
        """Destroy the running episode's sandbox without grading it."""
        await self._end_episode()

    async def _ensure_pool(self) -> EnvironmentPool:
        if self._pool is None:
            if self._client is None:
                self._owned = self._client = await connect()
            self._pool = EnvironmentPool(self._client)
        return self._pool

    def _begin(self, record: Record, spec: run_pb2.RunSpec) -> Task:
        task = Task.build(spec, record)
        if task.environment is None:
            raise TaskError(f"record {record.id} has no environment")
        if task.environment.checker is None:
            raise TaskError(f"record {record.id}'s environment has no checker to give a reward")
        return task

    def _enter(
        self, env: TaskEnvironment, task: Task, record: Record, spec: run_pb2.RunSpec
    ) -> None:
        self._env, self._task = env, task
        self._record, self._spec = record, spec
        self._tools = {t.spec.name: t for t in sandbox_tools(env)}
        self.trajectory, self._memory = [], {}

    async def reset(
        self, record: Record | Mapping[str, Any], spec: run_pb2.RunSpec | None = None
    ) -> str:
        """Start an episode on ``record``'s task (its environment comes from
        ``spec.environment`` and ``metadata["environment"]``). Returns the
        task's instruction."""
        await self._end_episode()
        try:
            if not isinstance(record, Record):
                from evalsi.datasets import load_records

                (record,) = load_records([dict(record)])
            spec = spec or run_pb2.RunSpec()
            task = self._begin(record, spec)
            assert task.environment is not None
            pool = await self._ensure_pool()
            self._enter(await pool.acquire(task.environment), task, record, spec)
            self.steps = 0
            return task.instruction
        except asyncio.CancelledError:
            await self._end_episode()
            raise

    async def snapshot(self) -> EpisodeSnapshot:
        """Save the running episode. Raises ``SnapshotsUnsupported`` on a rung
        that cannot snapshot (the pod rung): there is nothing to resume from."""
        if self._env is None or self._record is None or self._spec is None:
            raise RuntimeError("call reset() before snapshot()")
        try:
            snapshot_id: str = await self._env.sandbox.snapshot()
        except SnapshotsUnsupported as exc:
            raise SnapshotsUnsupported(
                f"this rung cannot snapshot a running episode, so it cannot be resumed: {exc}"
            ) from exc
        return EpisodeSnapshot(
            snapshot_id,
            self._record,
            self._spec,
            self.steps,
            list(self.trajectory),
            dict(self._memory),
        )

    async def restore(self, saved: EpisodeSnapshot) -> str:
        """Continue a saved episode in a new sandbox restored from it, with
        its step count. The saved snapshot stays until :meth:`delete_snapshot`."""
        await self._end_episode()
        try:
            task = self._begin(saved.record, saved.spec)
            assert task.environment is not None
            pool = await self._ensure_pool()
            self._enter(
                await pool.restore(task.environment, saved.snapshot_id),
                task,
                saved.record,
                saved.spec,
            )
            self.steps = saved.steps
            self.trajectory, self._memory = list(saved.trajectory), dict(saved.memory)
            return task.instruction
        except asyncio.CancelledError:
            await self._end_episode()
            raise

    async def delete_snapshot(self, saved: EpisodeSnapshot) -> None:
        pool = await self._ensure_pool()
        await pool.client.delete_snapshot(saved.snapshot_id)

    async def step(self, action: Action) -> StepResult:
        if self._env is None or self._task is None:
            raise RuntimeError("call reset() before step()")
        try:
            return await self._step(action)
        except asyncio.CancelledError:
            await self._end_episode()
            raise

    async def _step(self, action: Action) -> StepResult:
        self.steps += 1
        if action.tool == "submit":
            return await self._finish("submitted")
        tool = self._tools.get(action.tool)
        if action.error:
            observation, is_error = action.error, True
        elif tool is None:
            observation = (
                f"unknown tool {action.tool!r}; use one of {sorted(self._tools)} or submit"
            )
            is_error = True
        else:
            result = await tool.call(dict(action.arguments), span=f"step-{self.steps}")
            observation, is_error = result.content, result.is_error
        self.trajectory.append(
            Step(
                type="tool",
                name=action.tool,
                input=Content(text=json.dumps(action.arguments, sort_keys=True)),
                output=Content(text=observation),
                error=observation[:500] if is_error else "",
                span_id=f"step-{self.steps}",
            )
        )
        process, errors = await self._process(action, observation, is_error)
        if self.steps >= self.max_steps:
            done = await self._finish("max_steps")
            done.observation = observation
            done.info["tool_error"] = is_error
            done.process_reward = process
            self._note_errors(done, errors)
            return done
        step = StepResult(observation, 0.0, False, {"tool_error": is_error}, process)
        self._note_errors(step, errors)
        return step

    async def _process(
        self, action: Action, observation: str, is_error: bool
    ) -> tuple[float | None, list[str]]:
        if not self.process_rewards:
            return None, []
        assert self._record is not None
        context = ProcessContext(
            self._record,
            list(self.trajectory),
            action.tool,
            observation,
            is_error,
            self._memory,
        )
        total: float | None = 0.0
        for reward in self.process_rewards:
            value = await reward(context)
            total = None if value is None or total is None else total + value
        return total, context.errors

    @staticmethod
    def _note_errors(step: StepResult, errors: list[str]) -> None:
        if errors:
            step.info["process_errors"] = errors

    async def _finish(self, reason: str) -> StepResult:
        env, task = self._env, self._task
        assert env is not None
        assert task is not None
        assert task.environment is not None
        assert task.environment.checker is not None
        checker = task.environment.checker
        if checker.isolation == "clean":
            check, _ = await run_clean_checker(await self._ensure_pool(), env, checker, task)
        else:
            check = await run_checker(env, checker, task)
        info: dict[str, Any] = {
            "check": check.to_dict(),
            "ended": reason,
            "steps": self.steps,
            "policy_events": [e.kind for e in env.policy_events],
        }
        if check.error:
            info["error"] = check.error
            reward = None
        else:
            reward = check.score if check.score is not None else (1.0 if check.passed else 0.0)
        await self._end_episode()
        return StepResult("", reward, True, info)

    async def _end_episode(self) -> None:
        if self._env is not None:
            await self._env.sandbox.destroy()
        self._env, self._task, self._tools = None, None, {}

    async def close(self) -> None:
        await self._end_episode()
        if self._pool is not None and not self._shared_pool:
            await self._pool.aclose()
        if self._owned is not None:
            await self._owned.aclose()
            self._owned = None

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
costs a restore, not a rebuild (on rungs that snapshot).
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

from evalsi.sandbox.client import connect
from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike, TaskEnvironment
from evalsi_harness.task import Task, TaskError
from evalsi_harness.tools import Tool, sandbox_tools


@dataclass
class Action:
    """A tool call (``bash``, ``read_file``, ``write_file``) or ``submit``."""

    tool: str
    arguments: dict[str, Any] = field(default_factory=dict)


@dataclass
class StepResult:
    observation: str
    reward: float | None
    done: bool
    info: dict[str, Any] = field(default_factory=dict)


class TaskEnv:
    """One episode at a time over a sandbox client."""

    def __init__(
        self,
        client: SandboxClientLike | None = None,
        *,
        max_steps: int = 30,
    ) -> None:
        if max_steps < 1:
            raise ValueError("max_steps must be at least 1")
        self.max_steps = max_steps
        self._client = client
        self._owned: Any = None
        self._pool: EnvironmentPool | None = None
        self._env: TaskEnvironment | None = None
        self._task: Task | None = None
        self._tools: dict[str, Tool] = {}
        self.steps = 0

    async def __aenter__(self) -> TaskEnv:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    @property
    def tools(self) -> list[dict[str, Any]]:
        """The actions' schemas, in the form chat models take tool definitions."""
        specs = [
            {
                "name": t.spec.name,
                "description": t.spec.description,
                "input_schema": t.spec.input_schema,
            }
            for t in self._tools.values()
        ]
        specs.append(
            {
                "name": "submit",
                "description": "End the episode; the task's checker grades the result.",
                "input_schema": {"type": "object", "properties": {}},
            }
        )
        return specs

    async def _ensure_pool(self) -> EnvironmentPool:
        if self._pool is None:
            if self._client is None:
                self._owned = self._client = await connect()
            self._pool = EnvironmentPool(self._client)
        return self._pool

    async def reset(
        self, record: Record | Mapping[str, Any], spec: run_pb2.RunSpec | None = None
    ) -> str:
        """Start an episode on ``record``'s task (its environment comes from
        ``spec.environment`` and ``metadata["environment"]``). Returns the
        task's instruction."""
        await self._end_episode()
        if not isinstance(record, Record):
            from evalsi.datasets import load_records

            (record,) = load_records([dict(record)])
        task = Task.build(spec or run_pb2.RunSpec(), record)
        if task.environment is None:
            raise TaskError(f"record {record.id} has no environment")
        if task.environment.checker is None:
            raise TaskError(f"record {record.id}'s environment has no checker to give a reward")
        pool = await self._ensure_pool()
        self._env = await pool.acquire(task.environment)
        self._task = task
        self._tools = {t.spec.name: t for t in sandbox_tools(self._env)}
        self.steps = 0
        return task.instruction

    async def step(self, action: Action) -> StepResult:
        if self._env is None or self._task is None:
            raise RuntimeError("call reset() before step()")
        self.steps += 1
        if action.tool == "submit":
            return await self._finish("submitted")
        tool = self._tools.get(action.tool)
        if tool is None:
            observation = (
                f"unknown tool {action.tool!r}; use one of {sorted(self._tools)} or submit"
            )
            is_error = True
        else:
            result = await tool.call(dict(action.arguments), span=f"step-{self.steps}")
            observation, is_error = result.content, result.is_error
        if self.steps >= self.max_steps:
            done = await self._finish("max_steps")
            done.observation = observation
            done.info["tool_error"] = is_error
            return done
        return StepResult(observation, 0.0, False, {"tool_error": is_error})

    async def _finish(self, reason: str) -> StepResult:
        env, task = self._env, self._task
        assert env is not None
        assert task is not None
        assert task.environment is not None
        assert task.environment.checker is not None
        check = await run_checker(env, task.environment.checker, task)
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
        if self._pool is not None:
            await self._pool.aclose()
        if self._owned is not None:
            await self._owned.aclose()
            self._owned = None

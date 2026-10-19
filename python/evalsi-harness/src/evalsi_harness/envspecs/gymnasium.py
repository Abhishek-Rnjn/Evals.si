"""A set of tasks as a `Gymnasium <https://gymnasium.farama.org>`_ ``Env``.

::

    from evalsi_harness.envspecs.gymnasium import TaskGymEnv

    env = TaskGymEnv([(record, spec)])
    obs, info = env.reset(seed=0)
    obs, reward, terminated, truncated, info = env.step(
        {"tool": "bash", "arguments": '{"command": "pytest -q"}'}
    )

An action is a dict: ``tool`` (``bash``, ``read_file``, ``write_file`` or
``submit``) and ``arguments`` (a JSON object, as text or as a dict). Arguments
that are not JSON cost a step and come back as a failed call. Observations
are text, cut to ``max_observation_chars``.

The reward is the checker's score when the episode ends, plus any process
reward (:mod:`evalsi_harness.process`) on each step; ``info`` carries the two
separately (``info["outcome_reward"]``, ``info["process_reward"]``, the latter
``None`` when a process reward could not be computed, in which case it adds 0
here). ``submit`` ends the episode as ``terminated``; reaching ``max_steps``
ends it as ``truncated``, and either way the checker grades the end state.
A checker that cannot run raises :class:`InfrastructureError` instead of
returning a reward of 0.

Gymnasium's ``Env`` is synchronous and the task environment is not, so this
runs a private event loop in a background thread; call ``close()`` when done.
Built against ``gymnasium`` 1.4 and checked with its ``check_env``.
"""

from __future__ import annotations

import asyncio
import threading
from collections.abc import Coroutine, Mapping, Sequence
from typing import Any, ClassVar, TypeVar

try:
    import gymnasium as gym
    from gymnasium import spaces
except ImportError as exc:  # pragma: no cover - depends on the install
    raise ImportError(
        "evalsi_harness.envspecs.gymnasium needs gymnasium: pip install gymnasium==1.4.0"
    ) from exc

from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym as taskgym
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike
from evalsi_harness.process import ProcessReward

T = TypeVar("T")
TaskLike = tuple[Record | Mapping[str, Any], run_pb2.RunSpec | None]


class InfrastructureError(RuntimeError):
    """The task's checker could not run, so the episode has no reward."""


class FreeText(spaces.Text):
    """Any text up to ``max_length`` characters (``Text`` itself is limited to
    a character set)."""

    def contains(self, x: Any) -> bool:
        return isinstance(x, str) and len(x) <= self.max_length


class _Loop:
    """An event loop on its own thread, for running coroutines from sync code."""

    def __init__(self) -> None:
        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(target=self._loop.run_forever, daemon=True)
        self._thread.start()

    def run(self, coro: Coroutine[Any, Any, T]) -> T:
        return asyncio.run_coroutine_threadsafe(coro, self._loop).result()

    def close(self) -> None:
        self._loop.call_soon_threadsafe(self._loop.stop)
        self._thread.join(timeout=10)
        self._loop.close()


def _action(raw: Any) -> taskgym.Action:
    if not isinstance(raw, Mapping):
        return taskgym.Action("", error="an action is a dict with a 'tool' and 'arguments'")
    return taskgym.action_from(raw.get("tool"), raw.get("arguments", "{}"))


class TaskGymEnv(gym.Env[str, dict[str, str]]):
    """Tasks (record, spec) as a Gymnasium environment; ``reset`` picks one."""

    metadata: ClassVar[dict[str, Any]] = {"render_modes": []}

    def __init__(
        self,
        tasks: Sequence[TaskLike],
        *,
        client: SandboxClientLike | None = None,
        max_steps: int = 30,
        process_rewards: Sequence[ProcessReward] = (),
        max_observation_chars: int = 20_000,
        pool: EnvironmentPool | None = None,
    ) -> None:
        if not tasks:
            raise ValueError("TaskGymEnv needs at least one task")
        self._tasks = list(tasks)
        self.max_observation_chars = max_observation_chars
        self.observation_space = FreeText(max_length=max_observation_chars)
        self.action_space = spaces.Dict(
            {
                "tool": FreeText(max_length=64),
                "arguments": FreeText(max_length=max_observation_chars),
            }
        )
        self._loop = _Loop()
        self._env = taskgym.TaskEnv(
            client, max_steps=max_steps, process_rewards=process_rewards, pool=pool
        )
        self._active = False
        self._closed = False

    def _text(self, text: str) -> str:
        return text[: self.max_observation_chars]

    def reset(
        self, *, seed: int | None = None, options: dict[str, Any] | None = None
    ) -> tuple[str, dict[str, Any]]:
        """Start an episode. ``options={"task_index": i}`` picks the task;
        otherwise it is drawn with the seeded generator."""
        super().reset(seed=seed)
        index = (options or {}).get("task_index")
        if index is None:
            index = int(self.np_random.integers(len(self._tasks)))
        if not 0 <= index < len(self._tasks):
            raise ValueError(f"task_index {index} is not one of {len(self._tasks)} tasks")
        record, spec = self._tasks[index]
        instruction = self._loop.run(self._env.reset(record, spec))
        self._active = True
        return self._text(instruction), {"task_index": index, "tools": self._env.tools}

    def step(self, action: dict[str, str]) -> tuple[str, float, bool, bool, dict[str, Any]]:
        if not self._active:
            raise RuntimeError("call reset() before step()")
        result = self._loop.run(self._env.step(_action(action)))
        info = dict(result.info)
        if result.done:
            self._active = False
        if result.reward is None and result.done:
            raise InfrastructureError(
                f"the task's checker could not run: {result.info.get('error', 'unknown')}"
            )
        info["outcome_reward"] = result.reward
        info["process_reward"] = result.process_reward
        reward = (result.reward or 0.0) + (result.process_reward or 0.0)
        ended = result.info.get("ended")
        return (
            self._text(result.observation),
            float(reward),
            ended == "submitted",
            ended == "max_steps",
            info,
        )

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self._loop.run(self._env.close())
        finally:
            self._loop.close()


def make(tasks: Sequence[TaskLike], **kwargs: Any) -> TaskGymEnv:
    """``make(tasks)``: the module's entry point, like ``gymnasium.make``."""
    return TaskGymEnv(tasks, **kwargs)

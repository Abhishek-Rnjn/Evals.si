"""A set of tasks as an OpenEnv ``Environment``.

::

    from evalsi_harness.envspecs.openenv import TaskOpenEnv

    env = TaskOpenEnv([(record, spec)])        # tasks: (record, RunSpec) pairs
    obs = env.reset(seed=0)                    # obs.instruction
    obs = env.step(TaskAction(tool="bash", arguments={"command": "pytest -q"}))
    obs = env.step(TaskAction(tool="submit"))  # obs.done, obs.reward

or, as an OpenEnv server (``reset`` and ``step`` over HTTP and WebSocket)::

    from evalsi_harness.envspecs.openenv import create_task_app
    app = create_task_app([(record, spec)])    # uvicorn module:app

Built against ``openenv-core`` 0.3: an ``Environment`` whose ``reset`` and
``step`` return an observation carrying ``done`` and ``reward`` (the task's
checker score at the end of the episode, ``None`` until then and when the
checker cannot run), and whose ``state`` has an episode id and a step count.
The OpenEnv server runs the ``*_async`` methods when they are overridden, on
its own event loop; the synchronous ones run on a loop of their own. Use one
kind per instance.
"""

from __future__ import annotations

import asyncio
import threading
from collections.abc import Coroutine, Mapping, Sequence
from typing import Any, TypeVar

try:
    from openenv.core.env_server.http_server import create_app
    from openenv.core.env_server.interfaces import Environment
    from openenv.core.env_server.types import Action, Observation, State
except ImportError as exc:  # pragma: no cover - depends on the install
    raise ImportError(
        "evalsi_harness.envspecs.openenv needs openenv-core: pip install openenv-core==0.3.0"
    ) from exc

from pydantic import Field

from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym
from evalsi_harness.environment import SandboxClientLike

T = TypeVar("T")

TaskLike = tuple[Record | Mapping[str, Any], run_pb2.RunSpec | None]


class TaskAction(Action):
    """A sandbox tool call (``bash``, ``read_file``, ``write_file``) or ``submit``."""

    tool: str = Field(description="bash, read_file, write_file or submit")
    arguments: dict[str, Any] = Field(default_factory=dict, description="the tool's arguments")


class TaskObservation(Observation):
    """What an action showed. ``done`` and ``reward`` come from the base class."""

    instruction: str = Field(default="", description="the task's instruction (after reset)")
    output: str = Field(default="", description="the tool's output")
    info: dict[str, Any] = Field(
        default_factory=dict, description="the checker's verdict at the end"
    )


class TaskState(State):
    task_index: int | None = None
    max_steps: int = 0


class _Loop:
    """An event loop on its own thread, for the synchronous methods."""

    def __init__(self) -> None:
        self.loop = asyncio.new_event_loop()
        self._thread = threading.Thread(target=self.loop.run_forever, daemon=True)
        self._thread.start()

    def run(self, coro: Coroutine[Any, Any, T]) -> T:
        return asyncio.run_coroutine_threadsafe(coro, self.loop).result()

    def close(self) -> None:
        self.loop.call_soon_threadsafe(self.loop.stop)
        self._thread.join(timeout=5)
        self.loop.close()


class TaskOpenEnv(Environment[TaskAction, TaskObservation, TaskState]):
    """Tasks (record, spec) as an OpenEnv environment; one episode at a time.

    ``reset(task_index=i)`` starts task ``i``; otherwise the seed picks one
    (``seed % len(tasks)``, the first task without a seed).
    """

    def __init__(
        self,
        tasks: Sequence[TaskLike],
        *,
        client: SandboxClientLike | None = None,
        max_steps: int = 30,
    ) -> None:
        super().__init__()
        if not tasks:
            raise ValueError("TaskOpenEnv needs at least one task")
        self._tasks = list(tasks)
        self._env = gym.TaskEnv(client, max_steps=max_steps)
        self._state = TaskState(max_steps=max_steps)
        self._loop: _Loop | None = None

    # -- the OpenEnv interface -------------------------------------------------

    @property
    def state(self) -> TaskState:
        return self._state

    def reset(
        self,
        seed: int | None = None,
        episode_id: str | None = None,
        task_index: int | None = None,
        **kwargs: Any,
    ) -> TaskObservation:
        return self._sync(self.reset_async(seed, episode_id, task_index, **kwargs))

    async def reset_async(
        self,
        seed: int | None = None,
        episode_id: str | None = None,
        task_index: int | None = None,
        **kwargs: Any,
    ) -> TaskObservation:
        index = task_index if task_index is not None else (seed or 0) % len(self._tasks)
        if not 0 <= index < len(self._tasks):
            raise ValueError(f"task_index {index} is out of range: {len(self._tasks)} tasks")
        record, spec = self._tasks[index]
        instruction = await self._env.reset(record, spec)
        self._state = TaskState(
            episode_id=episode_id, step_count=0, task_index=index, max_steps=self._env.max_steps
        )
        return TaskObservation(instruction=instruction, done=False, reward=None)

    def step(
        self, action: TaskAction, timeout_s: float | None = None, **kwargs: Any
    ) -> TaskObservation:
        return self._sync(self.step_async(action, timeout_s, **kwargs))

    async def step_async(
        self, action: TaskAction, timeout_s: float | None = None, **kwargs: Any
    ) -> TaskObservation:
        coro = self._env.step(gym.Action(action.tool, dict(action.arguments)))
        result = await (asyncio.wait_for(coro, timeout_s) if timeout_s else coro)
        self._state = self._state.model_copy(update={"step_count": self._env.steps})
        # While the episode runs the reward is None, as OpenEnv's own
        # environments report it; the gym's 0.0 placeholder is not a reward.
        return TaskObservation(
            output=result.observation,
            done=result.done,
            reward=result.reward if result.done else None,
            info=result.info,
        )

    def close(self) -> None:
        if self._loop is not None:
            self._loop.run(self._env.close())
            self._loop.close()
            self._loop = None

    # -- internals -------------------------------------------------------------

    def _sync(self, coro: Coroutine[Any, Any, T]) -> T:
        if self._loop is None:
            self._loop = _Loop()
        return self._loop.run(coro)


def create_task_app(
    tasks: Sequence[TaskLike],
    *,
    client: SandboxClientLike | None = None,
    max_steps: int = 30,
) -> Any:
    """An OpenEnv server (a FastAPI app) for the tasks: each session gets its own
    environment, and sandboxes come from ``client`` or the sandbox service."""
    return create_app(
        lambda: TaskOpenEnv(tasks, client=client, max_steps=max_steps),
        TaskAction,
        TaskObservation,
        env_name="evalsi-task",
    )

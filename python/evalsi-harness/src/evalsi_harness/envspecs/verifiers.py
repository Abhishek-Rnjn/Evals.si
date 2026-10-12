"""A set of tasks as a `verifiers <https://github.com/PrimeIntellect-ai/verifiers>`_ environment.

::

    from evalsi_harness.envspecs.verifiers import load_environment

    env = load_environment([(record, spec)])   # a verifiers StatefulToolEnv

The environment gives the model the sandbox tools (``bash``, ``read_file``,
``write_file``) and ``submit``; each rollout is one episode of the task
(:class:`evalsi_harness.gym.TaskEnv`), and the reward is the task's checker
score when the episode ends: on ``submit``, at the step limit, or never when
the checker cannot run (that is a verifiers ``InfraError``, which ends the
rollout with ``state["error"]`` set instead of scoring it 0).

Built against ``verifiers`` 0.3: a ``StatefulToolEnv`` over a one-row-per-task
``datasets.Dataset`` (``question``, ``answer``, ``info``), a rubric with one
reward function over ``state``, ``@vf.stop`` and ``@vf.cleanup`` handlers.
Each rollout owns a ``TaskEnv`` (and so pays the environment's setup itself).
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any

try:
    import verifiers as vf
    from datasets import Dataset
except ImportError as exc:  # pragma: no cover - depends on the install
    raise ImportError(
        "evalsi_harness.envspecs.verifiers needs verifiers: pip install 'evalsi-harness[verifiers]'"
    ) from exc

from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym
from evalsi_harness.environment import SandboxClientLike
from evalsi_harness.task import Task

TaskLike = tuple[Record | Mapping[str, Any], run_pb2.RunSpec | None]

STATE_KEY = "evalsi"  # the live episode, until cleanup
RESULT_KEY = "evalsi_result"  # its verdict: reward, done, info


async def bash(command: str, rollout: object) -> str:
    """Run a shell command in the task's sandbox and return its output."""
    return await _act(rollout, "bash", {"command": command})


async def read_file(path: str, rollout: object) -> str:
    """Read a file from the task's sandbox."""
    return await _act(rollout, "read_file", {"path": path})


async def write_file(path: str, content: str, rollout: object) -> str:
    """Write a file in the task's sandbox."""
    return await _act(rollout, "write_file", {"path": path, "content": content})


async def submit(rollout: object) -> str:
    """End the episode: the task's checker grades the sandbox as it is."""
    return await _act(rollout, "submit", {})


class _Rollout:
    """One rollout's episode, kept in the state."""

    def __init__(self, env: gym.TaskEnv) -> None:
        self.env = env
        self.done = False
        self.reward: float | None = None
        self.info: dict[str, Any] = {}


async def _act(rollout: object, tool: str, arguments: dict[str, Any]) -> str:
    assert isinstance(rollout, _Rollout)
    if rollout.done:
        return "the episode is over"
    step = await rollout.env.step(gym.Action(tool, arguments))
    if step.done:
        rollout.done, rollout.reward, rollout.info = True, step.reward, step.info
        if step.reward is None:
            raise vf.InfraError(f"the task's checker could not run: {step.info.get('error')}")
        return step.observation or f"episode over; reward {step.reward}"
    return step.observation


class TaskVerifiersEnv(vf.StatefulToolEnv):
    """Tasks (record, spec) as a verifiers environment."""

    def __init__(
        self,
        tasks: Sequence[TaskLike],
        *,
        client: SandboxClientLike | None = None,
        max_turns: int = 30,
        **kwargs: Any,
    ) -> None:
        if not tasks:
            raise ValueError("TaskVerifiersEnv needs at least one task")
        self._tasks = list(tasks)
        self._client = client
        rows = []
        for i, (record, spec) in enumerate(self._tasks):
            rec = record if isinstance(record, Record) else _record(record)
            rows.append(
                {
                    "question": Task.build(spec or run_pb2.RunSpec(), rec).instruction,
                    "answer": "",
                    "info": {"task_index": i, "record_id": rec.id},
                }
            )
        rubric = vf.Rubric(funcs=[self.task_reward], weights=[1.0])
        super().__init__(
            dataset=Dataset.from_list(rows),
            rubric=rubric,
            max_turns=max_turns,
            stop_errors=[vf.InfraError],
            **kwargs,
        )
        for tool in (bash, read_file, write_file, submit):
            self.add_tool(tool, args_to_skip=["rollout"])

    async def task_reward(self, state: vf.State) -> float:
        """The checker's score (or 1 for a pass) when the episode ended; 0 if it did not."""
        reward = (state.get(RESULT_KEY) or {}).get("reward")
        rollout = state.get(STATE_KEY)
        if rollout is not None:
            reward = rollout.reward
        return float(reward) if reward is not None else 0.0

    async def setup_state(self, state: vf.State) -> vf.State:
        index = int((state.get("info") or {}).get("task_index", 0))
        record, spec = self._tasks[index]
        env = gym.TaskEnv(self._client, max_steps=self.max_turns * 4)
        await env.reset(record, spec)
        state[STATE_KEY] = _Rollout(env)
        return state

    def update_tool_args(
        self,
        tool_name: str,
        tool_args: dict[str, Any],
        messages: vf.Messages,
        state: vf.State,
        **kwargs: Any,
    ) -> dict[str, Any]:
        return {**tool_args, "rollout": state[STATE_KEY]}

    async def env_response(
        self, messages: vf.Messages, state: vf.State, **kwargs: Any
    ) -> vf.Messages:
        reply = await super().env_response(messages, state, **kwargs)
        rollout = state.get(STATE_KEY)
        if rollout is not None and rollout.done:
            # The episode ended in these tool calls: verifiers skips the model
            # call it would otherwise make next and finishes the rollout.
            state["final_env_response"] = reply
        return reply

    @vf.stop  # type: ignore[untyped-decorator]
    async def episode_over(self, state: vf.State) -> bool:
        rollout = state.get(STATE_KEY)
        return rollout is not None and rollout.done

    @vf.cleanup  # type: ignore[untyped-decorator]
    async def close_episode(self, state: vf.State) -> None:
        # Cleanup runs before the rubric scores the rollout: keep the verdict.
        rollout = state.pop(STATE_KEY, None)
        if rollout is not None:
            state[RESULT_KEY] = {
                "reward": rollout.reward,
                "done": rollout.done,
                "info": rollout.info,
            }
            await rollout.env.close()


def _record(data: Mapping[str, Any]) -> Record:
    from evalsi.datasets import load_records

    (record,) = load_records([dict(data)])
    return record


def load_environment(
    tasks: Sequence[TaskLike], *, client: SandboxClientLike | None = None, **kwargs: Any
) -> TaskVerifiersEnv:
    """The verifiers entry point: ``load_environment(...)`` returns the environment."""
    return TaskVerifiersEnv(tasks, client=client, **kwargs)

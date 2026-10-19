"""Tasks as a `NeMo Gym <https://github.com/NVIDIA-NeMo/Gym>`_ environment server.

NeMo Gym serves an environment over HTTP: a resources server with ``/reset``
and ``/step`` (its ``GymnasiumServer`` base, used with its ``gymnasium_agent``).
:class:`TaskResourcesServer` is that server for Evals.si tasks: ``/reset``
starts an episode from a dataset row, ``/step`` runs the response's function
calls in the task's sandbox, and the episode's reward is the checker's score
(plus any process reward, see :mod:`evalsi_harness.process`) once the model
calls ``submit`` or the step limit is reached.

A dataset row is made by :func:`dataset_row`: the model's ``input`` and
function ``tools`` (``bash``, ``read_file``, ``write_file``, ``submit``), plus
the task itself under ``evalsi_record`` and ``evalsi_spec``, which NeMo Gym
passes to ``reset`` and ``/step`` untouched. Each call in a response is one step.
The calls' outputs go back to the agent in ``info["tool_outputs"]`` (what
``gymnasium_agent`` turns into function-call outputs); a response with no
function call costs one step and the observation is a reminder.

The reset response sets ``supports_explicit_close`` and the server has a
``/close`` route, which the agent calls when a rollout fails part-way, so no
sandbox outlives its episode.

A checker that cannot run raises: NeMo Gym's step response has no unscored
marker, and a reward of 0 would train on an infrastructure failure, so the
request fails instead. Built against ``nemo-gym`` 0.7.0 (Python 3.13 or
newer); it imports ``resources_servers.gymnasium``, which ships in that
wheel.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

try:
    from fastapi import FastAPI, Request
    from nemo_gym.openai_utils import NeMoGymResponse, NeMoGymResponseFunctionToolCall
    from nemo_gym.server_utils import SESSION_ID_KEY
    from pydantic import ConfigDict, Field
    from resources_servers.gymnasium import GymnasiumServer
except ImportError as exc:  # pragma: no cover - depends on the install
    raise ImportError(
        "evalsi_harness.envspecs.nemogym needs nemo-gym (Python 3.13+): pip install nemo-gym==0.7.0"
    ) from exc

from google.protobuf import json_format

from evalsi.datasets import load_records
from evalsi.types import Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import gym
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike
from evalsi_harness.process import ProcessReward
from evalsi_harness.task import Task

RECORD_KEY = "evalsi_record"
SPEC_KEY = "evalsi_spec"
REMINDER = "Call one of the tools (bash, read_file, write_file) or submit; no tool call was found."


def dataset_row(
    record: Record | Mapping[str, Any],
    spec: run_pb2.RunSpec | None = None,
    **extra: Any,
) -> dict[str, Any]:
    """One NeMo Gym dataset row for a task. ``extra`` keys go on the row
    (``agent_ref``, for one)."""
    if not isinstance(record, Record):
        (record,) = load_records([dict(record)])
    spec = spec or run_pb2.RunSpec()
    tools = [
        {
            "type": "function",
            "name": t["name"],
            "description": t["description"],
            "parameters": t["input_schema"],
            "strict": False,
        }
        for t in gym.episode_tools()
    ]
    return {
        "responses_create_params": {
            "input": [{"role": "user", "content": Task.build(spec, record).instruction}],
            "tools": tools,
        },
        RECORD_KEY: record.to_dict(),
        SPEC_KEY: json_format.MessageToDict(spec),
        **extra,
    }


class TaskResourcesServer(GymnasiumServer):
    """The resources server: one :class:`~evalsi_harness.gym.TaskEnv` per session."""

    model_config = ConfigDict(arbitrary_types_allowed=True, extra="allow")

    sandbox_client: Any = None
    max_steps: int = 30
    process_rewards: list[Any] = Field(default_factory=list)
    pool: Any = None

    def _pool(self) -> EnvironmentPool | None:
        client: SandboxClientLike | None = self.sandbox_client
        if self.pool is None and client is not None:
            self.pool = EnvironmentPool(client)
        pool: EnvironmentPool | None = self.pool
        return pool

    async def reset(
        self, metadata: dict[str, Any], session_id: str | None = None
    ) -> tuple[str | None, dict[str, Any]]:
        if RECORD_KEY not in metadata:
            raise ValueError(
                f"the dataset row has no {RECORD_KEY!r}; build rows with dataset_row()"
            )
        record = load_records([metadata[RECORD_KEY]])[0]
        spec = json_format.ParseDict(metadata.get(SPEC_KEY) or {}, run_pb2.RunSpec())
        await self._drop(session_id)
        process: list[ProcessReward] = list(self.process_rewards)
        env = gym.TaskEnv(
            self.sandbox_client,
            max_steps=self.max_steps,
            pool=self._pool(),
            process_rewards=process,
        )
        await env.reset(record, spec)
        self.session_state[session_id] = env
        # The task is already the row's ``input``; an observation here would
        # make the agent add it to the conversation a second time.
        return None, {"record_id": record.id, "supports_explicit_close": True}

    async def step(
        self, action: NeMoGymResponse, metadata: dict[str, Any], session_id: str | None = None
    ) -> tuple[str | None, float, bool, bool, dict[str, Any]]:
        env = self.session_state.get(session_id)
        if not isinstance(env, gym.TaskEnv):
            raise RuntimeError("no episode for this session; /reset first")
        calls = [
            item for item in action.output if isinstance(item, NeMoGymResponseFunctionToolCall)
        ]
        actions = [gym.action_from(c.name, c.arguments) for c in calls] or [
            gym.Action("", error=REMINDER)
        ]
        tool_outputs: list[dict[str, Any]] = []
        outcome: float | None = 0.0
        process = 0.0
        info: dict[str, Any] = {}
        for call, act in zip(calls or [None], actions, strict=True):
            result = await env.step(act)
            if call is not None:
                tool_outputs.append(self.tool_output(call, result.observation))
            process += result.process_reward or 0.0
            info = result.info
            if result.done:
                if result.reward is None:
                    await self._drop(session_id)
                    raise RuntimeError(f"the task's checker could not run: {info.get('error')}")
                outcome = result.reward
                ended = info.get("ended")
                return (
                    None,
                    outcome + process,
                    ended == "submitted",
                    ended == "max_steps",
                    {
                        **info,
                        "tool_outputs": tool_outputs,
                        "outcome_reward": outcome,
                        "process_reward": process,
                    },
                )
            if call is None:
                # No function call to answer: the reminder goes in as a message.
                return (
                    result.observation,
                    process,
                    False,
                    False,
                    {**info, "outcome_reward": 0.0, "process_reward": process},
                )
        return (
            None,
            process,
            False,
            False,
            {
                **info,
                "tool_outputs": tool_outputs,
                "outcome_reward": 0.0,
                "process_reward": process,
            },
        )

    def setup_webserver(self) -> FastAPI:
        app: FastAPI = super().setup_webserver()
        # The agent calls /close when a rollout fails part-way, so a sandbox
        # is never left running for a session that will not step again.
        app.post("/close")(self._close_endpoint)
        return app

    async def _close_endpoint(self, request: Request) -> dict[str, Any]:
        await self._drop(request.session.get(SESSION_ID_KEY))
        return {}

    async def _drop(self, session_id: str | None) -> None:
        env = self.session_state.pop(session_id, None)
        if isinstance(env, gym.TaskEnv):
            await env.close()

    async def close_session(self, session_id: str | None) -> None:
        await self._drop(session_id)


__all__ = ["TaskResourcesServer", "dataset_row"]

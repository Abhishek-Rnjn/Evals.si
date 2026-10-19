"""The NeMo Gym adapter, against nemo-gym itself."""

from __future__ import annotations

import json
from typing import Any
from unittest.mock import MagicMock

import pytest

pytest.importorskip("nemo_gym")

from nemo_gym.base_resources_server import BaseResourcesServerConfig
from nemo_gym.openai_utils import (
    NeMoGymResponse,
    NeMoGymResponseCreateParamsNonStreaming,
    NeMoGymResponseFunctionToolCall,
)
from nemo_gym.server_utils import ServerClient

from conftest import task
from evalsi_harness.envspecs.nemogym import TaskResourcesServer, dataset_row
from evalsi_harness.process import ToolErrorPenalty
from evalsi_harness.testing import LocalSandboxClient


def server(**kwargs: Any) -> TaskResourcesServer:
    config = BaseResourcesServerConfig(host="", port=0, entrypoint="", name="evalsi")
    return TaskResourcesServer(
        config=config,
        server_client=MagicMock(spec=ServerClient),
        sandbox_client=LocalSandboxClient(),
        **kwargs,
    )


def response(*calls: tuple[str, dict[str, Any]]) -> NeMoGymResponse:
    return NeMoGymResponse(
        id="r",
        created_at=0.0,
        model="m",
        object="response",
        output=[
            NeMoGymResponseFunctionToolCall(
                arguments=json.dumps(args), call_id=f"c{i}", name=name, type="function_call"
            )
            for i, (name, args) in enumerate(calls)
        ],
        parallel_tool_calls=True,
        tool_choice="auto",
        tools=[],
    )


def row() -> dict[str, Any]:
    record, spec = task()
    return dataset_row(record, spec)


def test_a_dataset_row_is_valid_for_nemo_gym() -> None:
    data = row()
    params = NeMoGymResponseCreateParamsNonStreaming.model_validate(data["responses_create_params"])
    assert params.input[0].content == "Mark the notes done."  # type: ignore[union-attr]
    assert [t["name"] for t in data["responses_create_params"]["tools"]] == [
        "bash",
        "read_file",
        "write_file",
        "submit",
    ]
    assert json.loads(json.dumps(data))["evalsi_record"]["id"] == "t1"


@pytest.mark.asyncio
async def test_an_episode_is_rewarded_by_the_checked_end_state() -> None:
    env = server()
    obs, info = await env.reset(row(), session_id="s1")
    assert obs == "Mark the notes done."
    assert info["record_id"] == "t1"
    obs, reward, terminated, truncated, _ = await env.step(
        response(("bash", {"command": "cat setup.log notes.txt"})), {}, "s1"
    )
    assert obs is not None and "ready" in obs and "draft" in obs
    assert (reward, terminated, truncated) == (0.0, False, False)
    # Several calls in one response run in order; submit ends the episode.
    obs, reward, terminated, truncated, info = await env.step(
        response(
            ("write_file", {"path": "notes.txt", "content": "done\n"}),
            ("submit", {}),
        ),
        {},
        "s1",
    )
    assert (reward, terminated, truncated) == (1.0, True, False)
    assert info["outcome_reward"] == 1.0 and info["check"]["passed"] is True
    await env.close_session("s1")
    assert "s1" not in env.session_state


@pytest.mark.asyncio
async def test_a_response_without_a_tool_call_costs_a_step_and_is_reminded() -> None:
    env = server(max_steps=1, process_rewards=[ToolErrorPenalty(0.5)])
    await env.reset(row(), session_id="s")
    obs, reward, terminated, truncated, info = await env.step(response(), {}, "s")
    assert obs is not None and "no tool call" in obs
    # The step limit ends it; the checker still grades (notes are not done: 0), less the penalty.
    assert (terminated, truncated, info["ended"]) == (False, True, "max_steps")
    assert reward == -0.5


@pytest.mark.asyncio
async def test_a_checker_that_cannot_run_fails_the_request() -> None:
    record, spec = task(checker={"command": ["sh", "-c", "exit 0"], "parser": "json"})
    env = server()
    await env.reset(dataset_row(record, spec), session_id="s")
    with pytest.raises(RuntimeError, match="could not read"):
        await env.step(response(("submit", {})), {}, "s")
    assert "s" not in env.session_state


@pytest.mark.asyncio
async def test_sessions_are_independent_and_share_setup() -> None:
    client = LocalSandboxClient()
    env = TaskResourcesServer(
        config=BaseResourcesServerConfig(host="", port=0, entrypoint="", name="evalsi"),
        server_client=MagicMock(spec=ServerClient),
        sandbox_client=client,
    )
    await env.reset(row(), session_id="a")
    await env.reset(row(), session_id="b")
    await env.step(response(("write_file", {"path": "notes.txt", "content": "done\n"})), {}, "a")
    _, reward, *_ = await env.step(response(("submit", {})), {}, "b")
    assert reward == 0.0  # b's notes are still the draft
    assert len(client.created) == 1  # setup ran once
    await env.close_session("a")


def test_the_http_endpoints_drive_an_episode() -> None:
    from fastapi.testclient import TestClient

    env = server()
    with TestClient(env.setup_webserver()) as client:
        data = row()
        reset = client.post("/reset", json=data)
        assert reset.status_code == 200, reset.text
        assert reset.json()["observation"] == "Mark the notes done."
        body = {
            "responses_create_params": data["responses_create_params"],
            "response": response(
                ("write_file", {"path": "notes.txt", "content": "done\n"}), ("submit", {})
            ).model_dump(),
            **{k: data[k] for k in ("evalsi_record", "evalsi_spec")},
        }
        step = client.post("/step", json=body)
        assert step.status_code == 200, step.text
        assert (step.json()["reward"], step.json()["terminated"]) == (1.0, True)

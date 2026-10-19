"""The NeMo Gym adapter, against nemo-gym itself."""

from __future__ import annotations

import json
from typing import Any
from unittest.mock import MagicMock

import pytest

pytest.importorskip("nemo_gym")

from nemo_gym.base_resources_server import BaseResourcesServerConfig
from nemo_gym.openai_utils import (
    NeMoGymFunctionCallOutput,
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
    # The task is in the row's input already; the agent would add an observation twice.
    assert obs is None
    assert info["record_id"] == "t1"
    obs, reward, terminated, truncated, info = await env.step(
        response(("bash", {"command": "cat setup.log notes.txt"})), {}, "s1"
    )
    # The call's output goes back as the agent expects it: a function-call output.
    [out] = info["tool_outputs"]
    assert out["call_id"] == "c0"
    assert NeMoGymFunctionCallOutput(type="function_call_output", **out)
    assert "ready" in json.loads(out["output"])
    assert "draft" in json.loads(out["output"])
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
    assert info["outcome_reward"] == 1.0
    assert info["check"]["passed"] is True
    await env.close_session("s1")
    assert "s1" not in env.session_state


@pytest.mark.asyncio
async def test_a_response_without_a_tool_call_costs_a_step_and_is_reminded() -> None:
    env = server(max_steps=1, process_rewards=[ToolErrorPenalty(0.5)])
    await env.reset(row(), session_id="s")
    _obs, reward, terminated, truncated, info = await env.step(response(), {}, "s")
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
        assert reset.json()["observation"] is None
        assert reset.json()["info"]["supports_explicit_close"] is True
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


class FakeResponse:
    """What nemo_gym's raise_for_status and get_response_json read from a response."""

    ok = True
    cookies: dict[str, str] = {}  # noqa: RUF012

    def __init__(self, payload: Any) -> None:
        self._payload = payload

    async def read(self) -> bytes:
        return json.dumps(self._payload).encode()


@pytest.mark.asyncio
async def test_the_real_gymnasium_agent_drives_an_episode() -> None:
    """Nemo Gym's own agent, with the model scripted and the server behind a test client."""
    from fastapi.testclient import TestClient
    from responses_api_agents.gymnasium_agent.app import (
        GymnasiumAgent,
        GymnasiumAgentConfig,
        GymnasiumAgentRunRequest,
    )

    env = server()
    data = row()
    model_outputs = [
        response(("bash", {"command": "cat notes.txt"})),
        response(("write_file", {"path": "notes.txt", "content": "done\n"})),
        response(("submit", {})),
    ]
    model_inputs: list[Any] = []

    with TestClient(env.setup_webserver()) as http:

        async def post(server_name: str, url_path: str, json: Any, cookies: Any = None) -> Any:
            if server_name == "model":
                model_inputs.append(json)
                return FakeResponse(model_outputs.pop(0).model_dump())
            http.cookies.update(cookies or {})
            reply = http.post(url_path, json=json)
            assert reply.status_code == 200, reply.text
            return FakeResponse(reply.json())

        client = MagicMock(spec=ServerClient)
        client.post = post

        config = GymnasiumAgentConfig(
            host="",
            port=0,
            entrypoint="",
            name="agent",
            resources_server={"type": "resources_servers", "name": "evalsi"},
            model_server={"type": "responses_api_models", "name": "model"},
            max_steps=5,
        )
        agent = GymnasiumAgent(config=config, server_client=client)
        request = type("R", (), {"cookies": {}})()
        result = await agent.run(request, GymnasiumAgentRunRequest.model_validate(data))

    assert (result.reward, result.terminated, result.truncated) == (1.0, True, False)
    # The second model call saw the first call answered, not left dangling.
    second = [
        item
        for item in model_inputs[1].input
        if getattr(item, "type", "") == "function_call_output"
    ]
    assert len(second) == 1
    assert "draft" in second[0].output
    # The task was in the input once, not added again as an observation.
    assert sum(1 for item in model_inputs[0].input if "Mark the notes done." in str(item)) == 1
    assert env.session_state == {}

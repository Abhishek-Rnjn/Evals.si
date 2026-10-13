"""The OpenEnv adapter, against openenv-core itself."""

from __future__ import annotations

import asyncio
import json
import threading
import time
from typing import Any

import pytest

pytest.importorskip("openenv.core.env_server")

from fastapi.testclient import TestClient
from openenv.core.env_server.interfaces import Environment

from conftest import task
from evalsi_harness.envspecs.openenv import TaskAction, TaskOpenEnv, create_task_app
from evalsi_harness.testing import LocalSandboxClient


def test_is_an_openenv_environment_that_rewards_the_checked_end_state() -> None:
    env = TaskOpenEnv([task()], client=LocalSandboxClient())
    try:
        assert isinstance(env, Environment)
        obs = env.reset(seed=0, episode_id="ep-1")
        assert obs.instruction == "Mark the notes done."
        assert (obs.done, obs.reward) == (False, None)
        assert env.state.episode_id == "ep-1"

        obs = env.step(TaskAction(tool="bash", arguments={"command": "cat setup.log notes.txt"}))
        assert "ready" in obs.output
        assert "draft" in obs.output
        assert (obs.done, obs.reward) == (False, None)
        obs = env.step(
            TaskAction(tool="write_file", arguments={"path": "notes.txt", "content": "done\n"})
        )
        assert not obs.done
        assert env.state.step_count == 2

        obs = env.step(TaskAction(tool="submit"))
        assert (obs.done, obs.reward) == (True, 1.0)
        assert obs.info["check"]["passed"] is True
        assert obs.info["ended"] == "submitted"

        # reset starts again from the set-up environment.
        env.reset(seed=0)
        assert env.step(TaskAction(tool="submit")).reward == 0.0
    finally:
        env.close()


def test_seed_and_task_index_pick_the_task() -> None:
    env = TaskOpenEnv([task("first"), task("second")], client=LocalSandboxClient(), max_steps=2)
    try:
        assert env.reset(seed=1).instruction == "second"
        assert env.state.task_index == 1
        assert env.reset(task_index=0).instruction == "first"
        with pytest.raises(ValueError, match="out of range"):
            env.reset(task_index=5)
        # The step limit ends the episode with the checker's score.
        env.step(TaskAction(tool="bash", arguments={"command": "echo done > notes.txt"}))
        last = env.step(TaskAction(tool="bash", arguments={"command": "true"}))
        assert (last.done, last.reward) == (True, 1.0)
        assert last.info["ended"] == "max_steps"
    finally:
        env.close()


def test_a_checker_that_cannot_run_is_not_a_zero() -> None:
    broken = {"command": ["sh", "-c", "exit 0"], "parser": "json"}  # prints no JSON
    env = TaskOpenEnv([task(checker=broken)], client=LocalSandboxClient())
    try:
        env.reset()
        obs = env.step(TaskAction(tool="submit"))
        assert obs.done
        assert obs.reward is None
        assert obs.info["error"]
    finally:
        env.close()


def test_the_async_interface_runs_on_the_callers_loop() -> None:
    async def go() -> None:
        env = TaskOpenEnv([task()], client=LocalSandboxClient())
        obs = await env.reset_async(seed=0)
        assert obs.instruction == "Mark the notes done."
        await env.step_async(
            TaskAction(tool="bash", arguments={"command": "echo done > notes.txt"})
        )
        assert (await env.step_async(TaskAction(tool="submit"))).reward == 1.0
        await env.aclose()

    asyncio.run(go())


def test_served_as_an_openenv_app() -> None:
    """A WebSocket session is one environment instance, as OpenEnv serves it
    (the HTTP /reset and /step endpoints are stateless: a fresh environment each)."""
    app = create_task_app([task()], client=LocalSandboxClient())
    with TestClient(app) as http:
        assert http.get("/health").json()["status"] == "healthy"
        with http.websocket_connect("/ws") as ws:
            ws.send_json({"type": "reset", "data": {"seed": 0}})
            reset = ws.receive_json()["data"]
            assert reset["observation"]["instruction"] == "Mark the notes done."
            assert reset["done"] is False
            action = {"tool": "bash", "arguments": {"command": "echo done > notes.txt"}}
            ws.send_json({"type": "step", "data": action})
            assert ws.receive_json()["data"]["done"] is False
            ws.send_json({"type": "step", "data": {"tool": "submit"}})
            end = ws.receive_json()["data"]
            assert (end["done"], end["reward"]) == (True, 1.0)
            ws.send_json({"type": "state"})
            assert ws.receive_json()["data"]["step_count"] == 2
            # A bad action is rejected by OpenEnv's own validation, not by the adapter.
            ws.send_json({"type": "step", "data": {"nope": 1}})
            assert ws.receive_json()["type"] == "error"


class TrackingClient(LocalSandboxClient):
    """Remembers every sandbox it hands out, to see which were destroyed."""

    def __init__(self) -> None:
        super().__init__()
        self.sandboxes: list[Any] = []

    async def create(self, spec: Any = None) -> Any:
        sandbox = await super().create(spec)
        self.sandboxes.append(sandbox)
        return sandbox

    async def restore(self, snapshot_id: str, **kwargs: Any) -> Any:
        sandbox = await super().restore(snapshot_id, **kwargs)
        self.sandboxes.append(sandbox)
        return sandbox

    def live(self) -> list[Any]:
        return [sb for sb in self.sandboxes if sb.root.exists()]


def test_closing_after_async_use_destroys_the_sandbox() -> None:
    """The OpenEnv server calls close() when a session ends, whatever loop ran it."""
    client = TrackingClient()

    async def go() -> TaskOpenEnv:
        env = TaskOpenEnv([task()], client=client)
        await env.reset_async(seed=0)
        await env.step_async(TaskAction(tool="bash", arguments={"command": "true"}))
        return env

    env = asyncio.run(go())
    assert client.live(), "the episode's sandbox is still there"
    env.close()  # from a thread with no loop, as the server calls it
    assert client.live() == []


def test_a_client_that_disconnects_before_submit_leaves_no_sandbox() -> None:
    """Against a real server: Starlette's TestClient cancels the app as soon as
    its WebSocket session exits, which can cancel OpenEnv's session cleanup
    before it runs; a real disconnect does not."""
    import socket

    import uvicorn
    from websockets.sync.client import connect

    client = TrackingClient()
    app = create_task_app([task()], client=client)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning"))
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    try:
        deadline = time.monotonic() + 10
        while not server.started and time.monotonic() < deadline:
            time.sleep(0.05)
        assert server.started, "the server did not start"
        with connect(f"ws://127.0.0.1:{port}/ws") as ws:
            ws.send(json.dumps({"type": "reset", "data": {"seed": 0}}))
            assert json.loads(ws.recv())["data"]["done"] is False
            assert client.live()
        # The session closed with the connection.
        deadline = time.monotonic() + 10
        while client.live() and time.monotonic() < deadline:
            time.sleep(0.1)
        assert client.live() == []
    finally:
        server.should_exit = True
        thread.join(timeout=10)

"""A2A push notifications: the harness runs a webhook receiver, names it in
``pushNotificationConfig``, and records the task states the agent pushes as steps
(H3). Fallbacks: polling when nothing arrives, and send/poll when the agent
cannot push."""

from __future__ import annotations

import asyncio
import json
import threading
import time
import urllib.request
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest
from google.protobuf import json_format

from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.a2a_push import PushReceiver
from evalsi_harness.connectors import A2AConnector
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of


def task(state: str, text: str = "", *, artifacts: bool = False) -> dict[str, Any]:
    status: dict[str, Any] = {"state": state}
    if text:
        status["message"] = {"role": "agent", "parts": [{"kind": "text", "text": text}]}
    out: dict[str, Any] = {"kind": "task", "id": "t1", "contextId": "c1", "status": status}
    if artifacts:
        out["artifacts"] = [
            {"artifactId": "a1", "parts": [{"kind": "text", "text": "Order 42 ships tomorrow."}]}
        ]
    return out


class FakeAgent:
    """An A2A agent that takes a non-blocking message/send and pushes to the webhook."""

    def __init__(self, *, pushes: str = "good", supports_push: bool = True) -> None:
        self.pushes = pushes  # "good" token, "bad" token, or "none"
        self.supports_push = supports_push
        self.send_params: list[dict[str, Any]] = []
        self.done = threading.Event()
        self.webhook_status: list[int] = []
        agent = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                method, params = body["method"], body.get("params", {})
                if method == "message/send":
                    agent.send_params.append(params)
                    config = (params.get("configuration") or {}).get("pushNotificationConfig")
                    if config and not agent.supports_push:
                        return self.reply(
                            body,
                            error={"code": -32003, "message": "Push Notification is not supported"},
                        )
                    if config:
                        threading.Thread(target=agent.push, args=(config,), daemon=True).start()
                        return self.reply(body, result=task("submitted"))
                    return self.reply(body, result=task("completed", artifacts=True))
                if method == "tasks/get":
                    # Once the agent has finished (or given up pushing) the task is done.
                    state = "completed" if agent.done.is_set() else "working"
                    return self.reply(body, result=task(state, artifacts=state == "completed"))
                return self.reply(body, error={"code": -32601, "message": "no such method"})

            def reply(self, body: dict[str, Any], **payload: Any) -> None:
                data = json.dumps({"jsonrpc": "2.0", "id": body["id"], **payload}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def push(self, config: dict[str, Any]) -> None:
        if self.pushes != "none":
            token = config["token"] if self.pushes == "good" else "wrong"
            for state, text in (("working", "looking it up"), ("completed", "")):
                time.sleep(0.05)
                request = urllib.request.Request(
                    config["url"],
                    json.dumps(task(state, text)).encode(),
                    {"Content-Type": "application/json", "X-A2A-Notification-Token": token},
                )
                try:
                    with urllib.request.urlopen(request, timeout=5) as response:
                        self.webhook_status.append(response.status)
                except urllib.error.HTTPError as exc:
                    self.webhook_status.append(exc.code)
        self.done.set()


@pytest.fixture
def agent_factory() -> Iterator[Any]:
    agents: list[FakeAgent] = []

    def make(**kw: Any) -> FakeAgent:
        agents.append(FakeAgent(**kw))
        return agents[-1]

    yield make
    for a in agents:
        a.server.shutdown()


def spec_for(url: str, **a2a: Any) -> Any:
    return spec_of(
        {
            "target": {
                "agent": {"a2a": {"url": url, "push_notifications": True, "timeout": "20s", **a2a}}
            }
        }
    )


def test_pushed_states_are_steps(agent_factory: Any, sandboxes: LocalSandboxClient) -> None:
    agent = agent_factory()
    rec = run(spec_for(agent.url), record("Where is my order?"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."
    assert agent.webhook_status == [200, 200]
    steps = [s for s in rec.trajectory.steps if s.type == "agent"]  # type: ignore[union-attr]
    assert [s.name for s in steps] == ["a2a:submitted", "a2a:working", "a2a:completed"]
    assert [s.attributes["a2a.update"] for s in steps] == ["response", "push", "push"]
    assert steps[1].output is not None
    assert steps[1].output.as_text() == "looking it up"
    assert rec.metadata["agent"]["a2a_state"] == "completed"
    assert rec.metadata["agent"]["a2a_push_updates"] == 2
    sent = agent.send_params[0]["configuration"]
    assert sent["blocking"] is False
    config = sent["pushNotificationConfig"]
    assert config["url"].startswith("http://127.0.0.1:")
    assert config["token"]


def test_a_push_without_the_token_is_refused_and_the_task_is_polled(
    agent_factory: Any, sandboxes: LocalSandboxClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(A2AConnector, "push_poll_s", 0.2)
    agent = agent_factory(pushes="bad")
    rec = run(spec_for(agent.url), record("Where is my order?"), sandboxes).record
    assert rec is not None
    assert agent.webhook_status == [401, 401]
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."
    assert rec.metadata["agent"]["a2a_push_updates"] == 0
    steps = [s for s in rec.trajectory.steps if s.type == "agent"]  # type: ignore[union-attr]
    assert steps[-1].name == "a2a:completed"
    assert steps[-1].attributes["a2a.update"] in ("poll", "final")


def test_silence_falls_back_to_polling(
    agent_factory: Any, sandboxes: LocalSandboxClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(A2AConnector, "push_poll_s", 0.2)
    agent = agent_factory(pushes="none")
    rec = run(spec_for(agent.url), record("hi"), sandboxes).record
    assert rec is not None
    assert rec.metadata["agent"]["a2a_state"] == "completed"
    assert rec.metadata["agent"]["a2a_poll_updates"] >= 1


def test_an_agent_that_cannot_push_gets_send_and_poll(
    agent_factory: Any, sandboxes: LocalSandboxClient
) -> None:
    agent = agent_factory(supports_push=False)
    rec = run(spec_for(agent.url), record("hi"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."
    assert len(agent.send_params) == 2  # the refused push, then a plain send
    assert "pushNotificationConfig" not in (agent.send_params[1].get("configuration") or {})


def test_the_spec_fields_parse() -> None:
    cfg = json_format.ParseDict(
        {"url": "http://a", "push_notifications": True, "push_url": "http://w:9", "push_port": 9},
        agent_pb2.A2AAgent(),
    )
    assert (cfg.push_notifications, cfg.push_url, cfg.push_port) == (True, "http://w:9", 9)


def test_the_receiver_checks_token_path_and_size() -> None:
    async def go() -> None:
        receiver = PushReceiver()
        await receiver.start()

        async def post(path: str, token: str, body: bytes) -> int:
            reader, writer = await asyncio.open_connection("127.0.0.1", receiver.port)
            writer.write(
                f"POST {path} HTTP/1.1\r\nHost: x\r\nX-A2A-Notification-Token: {token}\r\n"
                f"Content-Length: {len(body)}\r\n\r\n".encode()
                + body
            )
            await writer.drain()
            status = int((await reader.readline()).split()[1])
            writer.close()
            return status

        path = "/" + receiver.url.split("/", 3)[3]
        assert await post(path, receiver.token, b'{"id": "t1"}') == 200
        assert (await receiver.events.get()) == {"id": "t1"}
        assert await post(path, "nope", b"{}") == 401
        assert await post("/other", receiver.token, b"{}") == 404
        assert await post(path, receiver.token, b"not json") == 400
        assert await post(path, receiver.token, b"[1]") == 400
        assert receiver.rejected == 1
        # A token that is not ASCII is refused like any wrong one, not an unhandled error.
        reader, writer = await asyncio.open_connection("127.0.0.1", receiver.port)
        writer.write(
            f"POST {path} HTTP/1.1\r\nHost: x\r\nX-A2A-Notification-Token: ".encode()
            + b"\xff\xfe\r\nContent-Length: 2\r\n\r\n{}"
        )
        await writer.drain()
        assert int((await reader.readline()).split()[1]) == 401
        writer.close()
        assert receiver.rejected == 2
        await receiver.aclose()

    asyncio.run(go())

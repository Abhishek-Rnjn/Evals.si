"""A2A over the HTTP+JSON binding, and HTTP agents that answer with a stream."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable
from typing import Any
from unittest.mock import patch

import httpx
import pytest

from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.connectors import A2AConnector, HTTPConnector
from evalsi_harness.task import Task, TaskError
from tests.harness_support import record, spec_of


def sse(*events: Any) -> bytes:
    return b"".join(
        b"data: " + (e if isinstance(e, str) else json.dumps(e)).encode() + b"\n\n" for e in events
    )


def mock(connector: Any, handle: Callable[[httpx.Request], httpx.Response]) -> None:
    asyncio.run(connector.client.aclose())
    connector.client = httpx.AsyncClient(transport=httpx.MockTransport(handle))


def rest_a2a(stream: bool = False) -> A2AConnector:
    return A2AConnector(
        agent_pb2.A2AAgent(url="https://agent.invalid/a2a/", binding="http+json", stream=stream)
    )


def test_a2a_http_json_sends_and_follows_the_task() -> None:
    seen: list[tuple[str, str, Any]] = []

    def handle(req: httpx.Request) -> httpx.Response:
        body = json.loads(req.content) if req.content else None
        seen.append((req.method, req.url.raw_path.decode(), body))
        if req.method == "POST":
            return httpx.Response(
                200, json={"task": {"id": "t/1", "status": {"state": "TASK_STATE_WORKING"}}}
            )
        return httpx.Response(
            200,
            json={
                "id": "t/1",
                "contextId": "c1",
                "status": {"state": "TASK_STATE_COMPLETED"},
                "artifacts": [{"artifactId": "a", "parts": [{"text": "Refunded."}]}],
                "history": [
                    {"role": "ROLE_USER", "content": [{"text": "refund"}]},
                    {"role": "ROLE_AGENT", "content": [{"text": "Checking the order."}]},
                ],
            },
        )

    connector = rest_a2a()
    mock(connector, handle)

    async def go() -> Any:
        with patch("evalsi_harness.connectors.asyncio.sleep", return_value=None):
            try:
                return await connector.send("refund")
            finally:
                await connector.aclose()

    reply = asyncio.run(go())
    assert reply.text == "Refunded."
    assert reply.finished
    assert reply.extra == {"a2a_state": "completed"}
    assert [s.output.as_text() for s in reply.steps if s.output] == [
        "Checking the order.",
        "Refunded.",
    ]
    (method, path, body), (get, task_path, _) = seen
    assert (method, path) == ("POST", "/a2a/v1/message:send")
    assert body["message"]["role"] == "ROLE_USER"
    assert body["message"]["content"] == [{"text": "refund"}]
    assert body["configuration"] == {"blocking": True}
    assert (get, task_path) == ("GET", "/a2a/v1/tasks/t%2F1")
    assert connector.context_id == "c1"


def test_a2a_http_json_stream() -> None:
    paths: list[str] = []

    def handle(req: httpx.Request) -> httpx.Response:
        paths.append(req.url.path)
        assert json.loads(req.content)["message"]["content"] == [{"text": "greet"}]
        update = {"taskId": "t1", "contextId": "c1"}
        art = {"artifactId": "a"}
        body = sse(
            {"statusUpdate": {**update, "status": {"state": "TASK_STATE_WORKING"}}},
            {"artifactUpdate": {**update, "artifact": {**art, "parts": [{"text": "Hel"}]}}},
            {
                "artifactUpdate": {
                    **update,
                    "append": True,
                    "artifact": {**art, "parts": [{"text": "lo"}]},
                }
            },
            {
                "statusUpdate": {
                    **update,
                    "status": {"state": "TASK_STATE_COMPLETED"},
                    "final": True,
                }
            },
        )
        return httpx.Response(200, content=body, headers={"content-type": "text/event-stream"})

    connector = rest_a2a(stream=True)
    mock(connector, handle)

    async def go() -> Any:
        try:
            return await connector.send("greet")
        finally:
            await connector.aclose()

    reply = asyncio.run(go())
    assert paths == ["/a2a/v1/message:stream"]
    assert reply.text == "Hello"
    assert reply.extra["a2a_state"] == "completed"
    assert [s.name for s in reply.steps] == ["a2a:working", "a2a:completed"]


def test_a2a_rest_states_and_messages_read_like_json_rpc() -> None:
    connector = rest_a2a()
    mock(
        connector,
        lambda req: httpx.Response(
            200,
            json={
                "message": {"role": "ROLE_AGENT", "contextId": "c9", "content": [{"text": "hi"}]}
            },
        ),
    )

    async def go() -> Any:
        try:
            return await connector.send("hello")
        finally:
            await connector.aclose()

    reply = asyncio.run(go())
    assert reply.text == "hi"
    assert connector.context_id == "c9"


def test_an_unknown_a2a_binding_is_refused() -> None:
    with pytest.raises(TaskError, match="use JSONRPC or HTTP\\+JSON"):
        A2AConnector(agent_pb2.A2AAgent(url="https://agent.invalid", binding="grpc"))


def http_agent(**cfg: Any) -> HTTPConnector:
    spec = spec_of({"target": {"agent": {"http": {"url": "https://agent.invalid/run", **cfg}}}})
    task = Task.build(spec, record("hi"), trial=0, run_id="run1")
    return HTTPConnector(task.spec.target.agent.http, task)


def run_http(connector: HTTPConnector, body: bytes) -> Any:
    def handle(req: httpx.Request) -> httpx.Response:
        assert req.headers["accept"] == "text/event-stream"
        return httpx.Response(200, content=body, headers={"content-type": "text/event-stream"})

    mock(connector, handle)

    async def go() -> Any:
        try:
            return await connector.send("hi")
        finally:
            await connector.aclose()

    return asyncio.run(go())


def test_http_stream_joins_deltas_and_collects_steps() -> None:
    connector = http_agent(
        stream=True,
        delta_path="delta",
        trajectory_path="step",
        usage_path="usage",
    )
    reply = run_http(
        connector,
        sse(
            {"type": "run_started"},
            {"delta": "Hi "},
            {"step": {"type": "tool", "name": "lookup", "output": "found"}},
            {"step": [{"type": "handoff", "name": "billing"}]},
            {"delta": "there"},
            {"usage": {"input_tokens": 3, "output_tokens": 1}},
            {"usage": {"input_tokens": 10, "output_tokens": 4}},
            "[DONE]",
        ),
    )
    assert reply.text == "Hi there"
    assert [(s.type, s.name) for s in reply.steps] == [
        ("tool", "lookup"),
        ("handoff", "billing"),
        ("agent", "http"),
    ]
    assert all(s.span_id for s in reply.steps)
    assert (reply.usage.input_tokens, reply.usage.output_tokens) == (10, 4)
    assert reply.extra == {"http_events": 7}


def test_http_stream_prefers_a_final_answer_over_deltas() -> None:
    connector = http_agent(stream=True, delta_path="delta", output_path="final.text")
    reply = run_http(
        connector, sse({"delta": "draft"}, {"final": {"text": "Answer."}}, {"type": "done"})
    )
    assert reply.text == "Answer."


def test_http_stream_without_an_answer_is_an_error() -> None:
    connector = http_agent(stream=True, delta_path="delta")
    with pytest.raises(TaskError, match="nothing at 'delta'"):
        run_http(connector, sse({"type": "run_started"}))


def test_delta_path_needs_a_stream() -> None:
    with pytest.raises(TaskError, match="set stream too"):
        http_agent(delta_path="delta")
    with pytest.raises(TaskError, match="or stream with delta_path"):
        http_agent()

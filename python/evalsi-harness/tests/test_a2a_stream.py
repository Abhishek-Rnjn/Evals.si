"""A2A ``message/stream``: task states and artifact chunks as steps (H3)."""

from __future__ import annotations

import json
import threading
from collections.abc import Callable, Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

Events = Callable[[dict[str, Any]], list[Any]]


@pytest.fixture
def stream_server() -> Iterator[Callable[[Events], str]]:
    servers: list[ThreadingHTTPServer] = []

    def start(make: Events) -> str:
        class H(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                assert body["method"] == "message/stream"
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for ev in make(body):
                    if ev == "garbage":
                        self.wfile.write(b"data: {not json\n\n")
                        continue
                    self.wfile.write(b"data: " + json.dumps(ev).encode() + b"\n\n")
                    self.wfile.flush()

        server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        servers.append(server)
        return f"http://127.0.0.1:{server.server_address[1]}"

    yield start
    for s in servers:
        s.shutdown()


def rpc(result: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": "1", "result": result}


def status(state: str, text: str = "", *, final: bool = False) -> dict[str, Any]:
    msg = {"role": "agent", "parts": [{"kind": "text", "text": text}]} if text else None
    return rpc(
        {
            "kind": "status-update",
            "taskId": "t1",
            "contextId": "c1",
            "status": {"state": state, **({"message": msg} if msg else {})},
            "final": final,
        }
    )


def artifact(text: str, *, append: bool, last: bool) -> dict[str, Any]:
    return rpc(
        {
            "kind": "artifact-update",
            "taskId": "t1",
            "contextId": "c1",
            "artifact": {"artifactId": "a1", "parts": [{"kind": "text", "text": text}]},
            "append": append,
            "lastChunk": last,
        }
    )


def spec_for(url: str) -> Any:
    return spec_of({"target": {"agent": {"a2a": {"url": url, "stream": True, "timeout": "5s"}}}})


def test_states_are_steps_and_artifact_chunks_are_joined(
    stream_server: Callable[[Events], str], sandboxes: LocalSandboxClient
) -> None:
    url = stream_server(
        lambda body: [
            rpc(
                {
                    "kind": "task",
                    "id": "t1",
                    "contextId": "c1",
                    "status": {"state": "submitted"},
                }
            ),
            status("working", "looking it up"),
            "garbage",
            artifact("Order 42 ", append=False, last=False),
            artifact("ships tomorrow.", append=True, last=True),
            status("completed", final=True),
        ]
    )
    rec = run(spec_for(url), record("Where is my order?"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."
    steps = [s for s in rec.trajectory.steps if s.type == "agent"]  # type: ignore[union-attr]
    names = [s.name for s in steps]
    assert names[:3] == ["a2a:submitted", "a2a:working", "a2a:completed"]
    working = steps[1]
    assert working.output is not None
    assert working.output.as_text() == "looking it up"
    assert working.attributes["a2a.state"] == "working"
    assert rec.metadata["agent"]["a2a_state"] == "completed"


def test_an_input_required_task_continues_in_the_same_context(
    stream_server: Callable[[Events], str], sandboxes: LocalSandboxClient
) -> None:
    seen: list[dict[str, Any]] = []

    def make(body: dict[str, Any]) -> list[Any]:
        seen.append(body["params"]["message"])
        if len(seen) == 1:
            return [status("input-required", "Which order?", final=True)]
        return [status("completed", "Order 42 ships tomorrow.", final=True)]

    url = stream_server(make)
    replies = iter([{"message": "order 42", "done": False}, {"message": "ok", "done": True}])
    from tests.harness_support import judge_answering

    judge = judge_answering(lambda prompt, schema: next(replies))
    spec = spec_of(
        {
            "target": {"agent": {"a2a": {"url": url, "stream": True}}},
            "harness": {"builtin": {"user_simulator": {"persona": "customer"}}},
        }
    )
    rec = run(spec, record("Where is my order?"), sandboxes, judge=judge).record
    assert rec is not None
    assert seen[1]["contextId"] == "c1"
    assert seen[1]["taskId"] == "t1"
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."


def test_a_stream_that_ends_early_is_an_error_not_an_answer(
    stream_server: Callable[[Events], str], sandboxes: LocalSandboxClient
) -> None:
    url = stream_server(lambda body: [status("working", "still going")])
    out = run(spec_for(url), record("go"), sandboxes)
    assert "ended before the task settled" in out.error


def test_a_failed_task_is_a_failed_step(
    stream_server: Callable[[Events], str], sandboxes: LocalSandboxClient
) -> None:
    url = stream_server(lambda body: [status("failed", "no stock", final=True)])
    rec = run(spec_for(url), record("go"), sandboxes).record
    assert rec is not None
    steps = [s for s in rec.trajectory.steps if s.type == "agent"]  # type: ignore[union-attr]
    assert steps[0].error == "failed"
    assert rec.metadata["agent"]["a2a_state"] == "failed"

"""Bring-your-own agents over A2A, MCP, OpenAI Responses and plain HTTP,
against small fake agent servers."""

from __future__ import annotations

import json
import threading
from collections.abc import Callable, Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import judge_answering, record, run, spec_of

Handler = Callable[
    [str, dict[str, Any], dict[str, str]], tuple[int, dict[str, Any], dict[str, str]]
]


@pytest.fixture
def agent_server() -> Iterator[Callable[[Handler], str]]:
    servers: list[ThreadingHTTPServer] = []

    def start(handle: Handler) -> str:
        class H(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])) or b"{}")
                status, payload, headers = handle(self.path, body, dict(self.headers))
                data = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                for k, v in headers.items():
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_DELETE(self) -> None:
                self.send_response(200)
                self.end_headers()

        server = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        servers.append(server)
        return f"http://127.0.0.1:{server.server_address[1]}"

    yield start
    for s in servers:
        s.shutdown()


def test_a2a_agent_with_a_simulated_user(
    agent_server: Callable[[Handler], str], sandboxes: LocalSandboxClient
) -> None:
    seen: list[dict[str, Any]] = []

    def a2a(
        path: str, body: dict[str, Any], headers: dict[str, str]
    ) -> tuple[int, dict[str, Any], dict[str, str]]:
        assert body["method"] == "message/send"
        assert headers.get("Authorization") == "Bearer agent-token"
        msg = body["params"]["message"]
        seen.append(msg)
        text = msg["parts"][0]["text"]
        if "order" in text.lower() and "42" not in text:
            state, reply = "input-required", "What is your order number?"
        else:
            state, reply = "completed", "Order 42 ships tomorrow."
        task = {
            "kind": "task",
            "id": "task-1",
            "contextId": "ctx-1",
            "status": {
                "state": state,
                "message": {"role": "agent", "parts": [{"kind": "text", "text": reply}]},
            },
            "artifacts": [{"parts": [{"kind": "text", "text": reply}]}]
            if state == "completed"
            else [],
        }
        return 200, {"jsonrpc": "2.0", "id": body["id"], "result": task}, {}

    url = agent_server(a2a)
    replies = iter(
        [{"message": "It is order 42", "done": False}, {"message": "great", "done": True}]
    )
    judge = judge_answering(lambda prompt, schema: next(replies))
    spec = spec_of(
        {
            "target": {
                "agent": {"a2a": {"url": url, "headers_env": {"Authorization": "A2A_AUTH"}}}
            },
            "harness": {"builtin": {"user_simulator": {"persona": "customer"}}},
        }
    )
    import os

    os.environ["A2A_AUTH"] = "Bearer agent-token"
    try:
        out = run(spec, record("Where is my order?"), sandboxes, judge=judge)
    finally:
        del os.environ["A2A_AUTH"]
    rec = out.record
    assert rec is not None, out.error
    assert rec.output is not None
    assert rec.output.as_text() == "Order 42 ships tomorrow."
    # The follow-up continued the same context and input-required task.
    assert seen[1]["contextId"] == "ctx-1"
    assert seen[1]["taskId"] == "task-1"
    assert [s.type for s in rec.trajectory.steps] == ["agent", "user", "agent"]  # type: ignore[union-attr]
    assert rec.metadata["agent"]["a2a_state"] == "completed"


def test_mcp_agent(agent_server: Callable[[Handler], str], sandboxes: LocalSandboxClient) -> None:
    def mcp(
        path: str, body: dict[str, Any], headers: dict[str, str]
    ) -> tuple[int, dict[str, Any], dict[str, str]]:
        if "id" not in body:
            return 202, {}, {}
        method = body["method"]
        if method == "initialize":
            result: dict[str, Any] = {"protocolVersion": "2025-06-18", "capabilities": {}}
        elif method == "tools/list":
            result = {"tools": [{"name": "support_agent", "inputSchema": {"type": "object"}}]}
        else:
            assert body["params"] == {"name": "support_agent", "arguments": {"question": "hi"}}
            result = {"content": [{"type": "text", "text": "hello from mcp"}]}
        return 200, {"jsonrpc": "2.0", "id": body["id"], "result": result}, {}

    url = agent_server(mcp)
    spec = spec_of({"target": {"agent": {"mcp": {"url": url + "/mcp", "argument": "question"}}}})
    rec = run(spec, record("hi"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "hello from mcp"


def test_responses_agent(
    agent_server: Callable[[Handler], str], sandboxes: LocalSandboxClient
) -> None:
    def responses(
        path: str, body: dict[str, Any], headers: dict[str, str]
    ) -> tuple[int, dict[str, Any], dict[str, str]]:
        assert path == "/v1/responses"
        assert body == {"model": "agent-1", "input": "weather?"}
        return (
            200,
            {
                "id": "resp_1",
                "status": "completed",
                "output": [
                    {"type": "web_search_call", "id": "ws_1", "status": "completed"},
                    {
                        "type": "function_call",
                        "name": "get_weather",
                        "arguments": '{"city": "Paris"}',
                    },
                    {"type": "message", "content": [{"type": "output_text", "text": "Sunny."}]},
                ],
                "usage": {"input_tokens": 30, "output_tokens": 5},
            },
            {},
        )

    url = agent_server(responses)
    spec = spec_of(
        {"target": {"agent": {"responses": {"base_url": url + "/v1", "model": "agent-1"}}}}
    )
    rec = run(spec, record("weather?"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "Sunny."
    assert [t.name for t in rec.trajectory.tool_uses()] == ["web_search", "get_weather"]  # type: ignore[union-attr]
    assert rec.usage is not None
    assert rec.usage.input_tokens == 30


def test_http_agent(agent_server: Callable[[Handler], str], sandboxes: LocalSandboxClient) -> None:
    def http(
        path: str, body: dict[str, Any], headers: dict[str, str]
    ) -> tuple[int, dict[str, Any], dict[str, str]]:
        assert body == {"query": 'say "hi"', "session": "t1"}
        messages = [
            {
                "role": "assistant",
                "content": "",
                "tool_calls": [{"id": "c1", "function": {"name": "lookup", "arguments": "{}"}}],
            },
            {"role": "tool", "tool_call_id": "c1", "content": "found"},
            {"role": "assistant", "content": "hi"},
        ]
        return 200, {"result": {"answer": "hi"}, "trace": messages}, {}

    url = agent_server(http)
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "http": {
                        "url": url + "/chat",
                        "body_template": '{"query": {{input}}, "session": {{task_id}}}',
                        "output_path": "result.answer",
                        "messages_path": "trace",
                    }
                }
            }
        }
    )
    rec = run(spec, record('say "hi"'), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "hi"
    tools = rec.trajectory.tool_uses()  # type: ignore[union-attr]
    assert [(t.name, t.result) for t in tools] == [("lookup", "found")]


def test_unreachable_agents_are_errors_not_failures(sandboxes: LocalSandboxClient) -> None:
    spec = spec_of(
        {"target": {"agent": {"a2a": {"url": "http://127.0.0.1:1/a2a", "timeout": "2s"}}}}
    )
    out = run(spec, record(), sandboxes)
    assert out.record is None
    assert "A2A agent" in out.error


def test_cli_agent_runs_in_the_sandbox(
    sandboxes: LocalSandboxClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("WORKER_SECRET", "s3cret")
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": [
                            "sh",
                            "-c",
                            'echo "{instruction}" > got.txt; echo "key=$AGENT_KEY"',
                        ],
                        "env_from": {"AGENT_KEY": "WORKER_SECRET"},
                        "install": ["echo installed > tool.txt"],
                        "allow_hosts": ["api.example.com"],
                    }
                }
            },
            "environment": {
                "checker": {"command": ["sh", "-c", "grep -q task got.txt && test -f tool.txt"]}
            },
        }
    )
    rec = run(spec, record("the task"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "key=s3cret"
    assert rec.check is not None
    assert rec.check.passed
    # The agent's model API was allowlisted for the agent's sandbox, not the whole run.
    assert sandboxes.restored[-1][1] == "allowlist"
    assert rec.metadata["agent"]["exit_code"] == 0

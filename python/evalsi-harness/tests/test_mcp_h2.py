"""The MCP client's timeouts, content types, resources, prompts, elicitation and
OAuth, and tool_search over a large tool set."""

from __future__ import annotations

import asyncio
import base64
import sys
import time
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.mcp import (
    MCPClient,
    MCPError,
    MCPPromptsTool,
    MCPResourcesTool,
    MCPTool,
    OAuth,
    answer_from_text,
    text_of,
)
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.toolsearch import score, words
from tests.fake_mcp import FakeMCP
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO


def go(coro: Any) -> Any:
    return asyncio.run(coro)


# --- timeouts ----------------------------------------------------------------------


def test_a_slow_tool_times_out_without_breaking_the_connection() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url, timeout_s=30)
        started = time.monotonic()
        r = await client.call_tool("slow", {"seconds": 3}, timeout_s=0.3)
        assert time.monotonic() - started < 2
        assert r.is_error
        assert "tools/call timed out after 0.3 s" in r.content
        # The next call works: a late answer to the old request is not mistaken for it.
        ok = await client.call_tool("echo", {"text": "hi"})
        assert not ok.is_error
        assert "echo" in ok.content
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_the_default_timeout_comes_from_the_client_and_the_tool_can_override_it() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url, timeout_s=0.3)
        spec = (await client.list_tools())[1]
        assert spec.name == "slow"
        short = MCPTool(client, spec)
        assert (await short.call({"seconds": 1.5})).is_error  # the client's 0.3 s
        generous = MCPTool(client, spec, timeout_s=10)
        assert (await generous.call({"seconds": 0.5})).content == "finally"
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


# --- what comes back ---------------------------------------------------------------


def test_binary_content_is_a_note_not_base64_in_the_context() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url)
        r = await client.call_tool("image", {})
        assert "here is the chart" in r.content
        assert "[image/png content, 2000 bytes, not shown]" in r.content
        assert "[resource link: doc://handbook (handbook)]" in r.content
        assert len(r.content) < 200
        assert base64.b64encode(b"\x89PNG")[:8].decode() not in r.content
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_structured_output_is_returned_and_its_schema_is_in_the_description() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url)
        specs = {t.name: t for t in await client.list_tools()}
        assert 'Returns JSON: {"type":"object"' in specs["structured"].description
        assert (await client.call_tool("structured", {})).content == '{"n": 7}'
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_text_of_covers_every_content_type() -> None:
    assert text_of({"type": "text", "text": "a"}) == "a"
    assert text_of({"type": "audio", "mimeType": "audio/wav", "data": "AAAA"}) == (
        "[audio/wav content, 3 bytes, not shown]"
    )
    assert text_of({"type": "resource", "resource": {"text": "inline"}}) == "inline"
    assert "not shown" in text_of({"type": "resource", "resource": {"uri": "u", "blob": "QUJD"}})
    assert text_of({"type": "mystery", "x": 1}) == '{"type": "mystery", "x": 1}'


# --- resources and prompts ---------------------------------------------------------


def test_resources_and_prompts_are_tools() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url)
        res, prompts = MCPResourcesTool(client, "docs"), MCPPromptsTool(client, "docs")
        assert res.spec.name == "docs_resources"
        assert (
            "doc://handbook  handbook  The handbook" in (await res.call({"action": "list"})).content
        )
        assert (await res.call({"action": "read", "uri": "doc://handbook"})).content == (
            "Rule 1: write tests."
        )
        blob = await res.call({"action": "read", "uri": "doc://logo"})
        assert blob.content == "[image/png content doc://logo, 300 bytes, not shown]"
        assert (await res.call({"action": "read"})).is_error
        assert "review(path)  Review code" in (await prompts.call({"action": "list"})).content
        got = await prompts.call({"action": "get", "name": "review", "arguments": {"path": "a.py"}})
        assert got.content == "Review code\n[user] Please review a.py."
        assert (await prompts.call({"action": "get"})).is_error
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_a_server_error_is_a_tool_error_not_a_crash() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url)
        with pytest.raises(MCPError, match="nope"):
            await client.call("unknown/method")
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


# --- elicitation -------------------------------------------------------------------


def test_the_server_can_ask_a_question_mid_call_and_get_an_answer() -> None:
    asked: list[tuple[str, dict[str, Any]]] = []

    async def answer(message: str, schema: dict[str, Any]) -> dict[str, Any]:
        asked.append((message, schema))
        return {"action": "accept", "content": {"environment": "staging"}}

    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url, elicit=answer)
        assert "elicitation" in server.client_capabilities  # declared only because we can answer
        r = await client.call_tool("ask", {})
        assert not r.is_error
        assert '"environment": "staging"' in r.content
        assert asked[0][0] == "Which environment should I deploy to?"
        assert asked[0][1]["properties"]["environment"]["enum"] == ["staging", "prod"]
        assert '"action": "accept"' in r.metadata["elicitations"]
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_without_a_handler_the_client_does_not_claim_to_support_elicitation() -> None:
    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url)
        assert server.client_capabilities == {}
        assert (await client.call_tool("ask", {})).content == "no elicitation"
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


def test_a_failing_handler_cancels_the_question_and_the_tool_goes_on() -> None:
    async def broken(message: str, schema: dict[str, Any]) -> dict[str, Any]:
        raise RuntimeError("the user is gone")

    async def body(server: FakeMCP) -> None:
        client = await MCPClient.connect(url=server.url, elicit=broken)
        r = await client.call_tool("ask", {})
        assert '"action": "cancel"' in r.content
        await client.aclose()

    with FakeMCP() as server:
        go(body(server))


STDIO_ASKER = r"""
import json, sys
def send(m): sys.stdout.write(json.dumps(m) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    msg = json.loads(line)
    if msg.get("method") == "initialize":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"protocolVersion": "2025-06-18"}})
    elif msg.get("method") == "tools/list":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"tools": [
            {"name": "ask", "description": "ask", "inputSchema": {"type": "object"}}]}})
    elif msg.get("method") == "tools/call":
        send({"jsonrpc": "2.0", "id": "q1", "method": "elicitation/create",
              "params": {"message": "Proceed?", "requestedSchema": {"type": "object",
              "properties": {"ok": {"type": "boolean"}}, "required": ["ok"]}}})
        answer = json.loads(sys.stdin.readline())
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"content": [
            {"type": "text", "text": "got " + json.dumps(answer["result"])}]}})
"""


def test_elicitation_over_stdio(tmp_path: Path) -> None:
    script = tmp_path / "asker.py"
    script.write_text(STDIO_ASKER)

    async def yes(message: str, schema: dict[str, Any]) -> dict[str, Any]:
        return answer_from_text(schema, "yes, go ahead")

    async def body() -> None:
        client = await MCPClient.connect(command=[sys.executable, str(script)], elicit=yes)
        r = await client.call_tool("ask", {})
        assert r.content == 'got {"action": "accept", "content": {"ok": true}}'
        await client.aclose()

    go(body())


def test_answer_from_text_fits_a_reply_to_the_requested_schema() -> None:
    schema = {
        "type": "object",
        "properties": {
            "env": {"type": "string", "enum": ["staging", "prod"]},
            "count": {"type": "integer"},
            "ratio": {"type": "number"},
            "confirm": {"type": "boolean"},
            "note": {"type": "string"},
        },
        "required": ["env"],
    }
    out = answer_from_text(schema, "Deploy to PROD, 3 replicas, ratio 0.5, yes confirm")
    assert out == {
        "action": "accept",
        "content": {
            "env": "prod",
            "count": 3,
            "ratio": 0.5,
            "confirm": True,
            "note": "Deploy to PROD, 3 replicas, ratio 0.5, yes confirm",
        },
    }
    assert answer_from_text(schema, "whatever you like") == {"action": "decline"}  # env required
    no = answer_from_text({"properties": {"ok": {"type": "boolean"}}}, "no, don't")
    assert no["content"] == {"ok": False}
    assert answer_from_text({"properties": {"ok": {"type": "boolean"}}}, "hmm") == {
        "action": "accept",
        "content": {},
    }


# --- OAuth -------------------------------------------------------------------------


def test_oauth_client_credentials_authenticate_and_refresh_on_a_401() -> None:
    async def body(server: FakeMCP) -> None:
        oauth = OAuth(server.token_url, "my-id", "my-secret", scope="mcp.read")
        client = await MCPClient.connect(url=server.url, oauth=oauth)
        await client.call_tool("echo", {"text": "a"})
        await client.call_tool("echo", {"text": "b"})
        assert len(server.token_requests) == 1  # one token, reused
        first = server.token_requests[0]
        assert first["form"] == {"grant_type": "client_credentials", "scope": "mcp.read"}
        assert first["authorization"] == "Basic " + base64.b64encode(b"my-id:my-secret").decode()
        assert set(server.auth_seen) == {"Bearer tok-1"}
        # The server revokes the token: the next request is refused, then retried anew.
        server.token = "revoked"
        r = await client.call_tool("echo", {"text": "c"})
        assert not r.is_error
        assert len(server.token_requests) == 2
        assert server.auth_seen[-1] == "Bearer tok-2"
        await client.aclose()

    with FakeMCP(oauth=True) as server:
        go(body(server))


def test_oauth_failures_are_clear() -> None:
    async def body(server: FakeMCP) -> None:
        bad = OAuth(server.url.replace("/mcp", "/nowhere"), "i", "s")
        with pytest.raises(MCPError, match="OAuth token request"):
            await MCPClient.connect(url=server.url, oauth=bad)
        with pytest.raises(MCPError, match="by url"):
            await MCPClient.connect(command=["x"], oauth=OAuth("u", "i", "s"))

    with FakeMCP(oauth=True) as server:
        go(body(server))


# --- tool_search -------------------------------------------------------------------


def test_scoring_prefers_name_matches_and_partial_words() -> None:
    from evalsi_harness.models import ToolSpec

    cal = ToolSpec("calendar_create_event", "Create an event on a calendar.", {})
    mail = ToolSpec("mail_send", "Send mail. Can attach a calendar invite.", {})
    q = words("create calendar event")
    assert score(q, cal) > score(q, mail) > 0
    assert score(words("zebra"), cal) == 0
    assert words("get_weather-v2") == ["get", "weather", "v2"]
    # A query word that is part of a name word still finds it (search -> search_issues).
    issues = ToolSpec("search_issues", "Find issues.", {})
    assert score(words("searching issue"), issues) > 0


def run_with_search(
    model: Any,
    sandboxes: LocalSandboxClient,
    server: FakeMCP,
    script: Any,
    tools: list[str] | None = None,
) -> tuple[Any, Any]:
    srv = model(script)
    spec = spec_of(
        {
            "target": {"connector": "openai-compatible", "model": "m", "base_url": srv.base_url},
            "harness": {
                "builtin": {
                    "tools": {
                        "builtin": tools if tools is not None else ["tool_search"],
                        "mcp": [{"name": "ops", "url": server.url}],
                    }
                }
            },
            "environment": {"checker": CHECK_HELLO},
        }
    )
    return srv, run(spec, record(), sandboxes).record


def offered(request: dict[str, Any]) -> list[str]:
    return [t["function"]["name"] for t in request["tools"]]


def test_mcp_tools_start_unloaded_and_a_search_loads_the_matches(
    model: Any, sandboxes: LocalSandboxClient
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        results = [m for m in messages if m["role"] == "tool"]
        if not results:
            return {"tool_calls": [("tool_search", {"query": "weather data"})]}
        if len(results) == 1:
            return {"tool_calls": [("weather_tool_0", {"city": "Oslo"})]}
        return {"text": "done"}

    with FakeMCP(extra_tools=40) as server:
        srv, rec = run_with_search(model, sandboxes, server, script)
    assert rec is not None
    first, second, third = srv.requests[0], srv.requests[1], srv.requests[2]
    # 45 MCP tools exist; the model is offered the sandbox tools and one search tool.
    assert offered(first) == [
        "bash",
        "read_file",
        "write_file",
        "request_escalation",
        "tool_search",
    ]
    assert "45 more tools exist but are not loaded" in first["tools"][-1]["function"]["description"]
    loaded = [n for n in offered(second) if n.startswith("weather_tool")]
    assert 1 <= len(loaded) <= 5
    assert "weather_tool_0" in loaded
    assert "echo" not in offered(second)  # what did not match stays out of the prompt
    assert offered(third) == offered(second)
    reply = next(m for m in second["messages"] if m["role"] == "tool")["content"]
    assert reply.startswith("Loaded, and available from your next step:")
    assert ("weather_tool_0", {"city": "Oslo"}) in server.calls


def test_calling_an_unloaded_tool_says_to_search_first_and_a_miss_says_so(
    model: Any, sandboxes: LocalSandboxClient
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        results = [m for m in messages if m["role"] == "tool"]
        if not results:
            return {"tool_calls": [("echo", {"text": "hi"})]}
        if len(results) == 1:
            return {"tool_calls": [("tool_search", {"query": "zzzz qqqq"})]}
        return {"text": "done"}

    with FakeMCP() as server:
        srv, _ = run_with_search(model, sandboxes, server, script)
        assert not server.calls  # the unloaded tool never reached the server
    tool_replies = [m["content"] for m in srv.requests[2]["messages"] if m["role"] == "tool"]
    assert "echo is not loaded yet: call tool_search to load it" in tool_replies[0]
    assert tool_replies[1].startswith("no unloaded tool matches 'zzzz qqqq'")


def test_without_tool_search_every_mcp_tool_is_offered_as_before(
    model: Any, sandboxes: LocalSandboxClient
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        return {"text": "done"}

    with FakeMCP(extra_tools=3) as server:
        srv, _ = run_with_search(model, sandboxes, server, script, tools=[])
    names = offered(srv.requests[0])
    assert "echo" in names
    assert "tool_search" not in names
    assert len([n for n in names if "_tool_" in n]) == 3


def test_a_run_with_tool_search_replays_exactly(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        results = [m for m in messages if m["role"] == "tool"]
        if not results:
            return {"tool_calls": [("tool_search", {"query": "echo text"})]}
        if len(results) == 1:
            return {"tool_calls": [("echo", {"text": "hello"})]}
        if len(results) == 2:
            return {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}
        return {"text": "done"}

    def spec(srv: Any, server: FakeMCP, mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": srv.base_url,
                },
                "harness": {
                    "builtin": {
                        "tools": {
                            "builtin": ["tool_search"],
                            "mcp": [{"name": "ops", "url": server.url}],
                        },
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {"checker": CHECK_HELLO},
            }
        )

    with FakeMCP() as server:
        srv = model(script)
        first = run(spec(srv, server, "record"), record(), sandboxes, base_dir=tmp_path).record
        assert first is not None
        assert first.check is not None
        assert first.check.passed
        sent, called = len(srv.requests), len(server.calls)
        again = run(spec(srv, server, "replay"), record(), sandboxes, base_dir=tmp_path).record
        assert len(srv.requests) == sent
        assert len(server.calls) == called  # the MCP call was served from the tape
        assert again is not None
        assert again.check is not None
        assert again.check.passed


def test_resources_and_prompts_flags_add_their_tools_to_a_run(
    model: Any, sandboxes: LocalSandboxClient
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if not any(m["role"] == "tool" for m in messages):
            return {"tool_calls": [("ops_resources", {"action": "read", "uri": "doc://handbook"})]}
        return {"text": "done"}

    srv = model(script)
    with FakeMCP() as server:
        spec = spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": srv.base_url,
                },
                "harness": {
                    "builtin": {
                        "tools": {
                            "mcp": [
                                {
                                    "name": "ops",
                                    "url": server.url,
                                    "resources": True,
                                    "prompts": True,
                                    "tools": ["echo"],
                                }
                            ]
                        }
                    }
                },
                "environment": {},
            }
        )
        rec = run(spec, record(), sandboxes).record
    assert rec is not None
    names = offered(srv.requests[0])
    assert "ops_resources" in names
    assert "ops_prompts" in names
    result = next(m for m in srv.requests[1]["messages"] if m["role"] == "tool")["content"]
    assert result == "Rule 1: write tests."


def test_http_session_close_authenticates_and_refreshes_once() -> None:
    import httpx

    from evalsi_harness.mcp import _HTTPTransport

    async def body() -> None:
        requests: list[httpx.Request] = []

        def handle(request: httpx.Request) -> httpx.Response:
            requests.append(request)
            if request.url.path == "/token":
                return httpx.Response(200, json={"access_token": "fresh"})
            assert request.method == "DELETE"
            assert request.headers["Mcp-Session-Id"] == "session"
            assert request.headers["MCP-Protocol-Version"] == "2025-06-18"
            assert request.headers["X-Private-Header"] == "configured"
            return httpx.Response(
                204 if request.headers.get("Authorization") == "Bearer fresh" else 401
            )

        oauth = OAuth(
            "https://mcp.example/token",
            "id",
            "secret",
            _token="expired",
            _expires=time.monotonic() + 60,
        )
        transport = _HTTPTransport(
            "https://mcp.example/mcp", {"X-Private-Header": "configured"}, 10, oauth
        )
        await transport.client.aclose()
        transport.client = httpx.AsyncClient(transport=httpx.MockTransport(handle))
        transport.session = "session"
        await transport.aclose()
        assert [r.method for r in requests] == ["DELETE", "POST", "DELETE"]
        assert transport.client.is_closed

    go(body())


def test_http_session_close_releases_client_if_token_refresh_fails() -> None:
    import httpx

    from evalsi_harness.mcp import _HTTPTransport

    async def body() -> None:
        transport = _HTTPTransport(
            "https://mcp.example/mcp", {}, 10, OAuth("https://mcp.example/token", "id", "secret")
        )
        await transport.client.aclose()
        transport.client = httpx.AsyncClient(
            transport=httpx.MockTransport(lambda req: httpx.Response(401))
        )
        transport.session = "session"
        await transport.aclose()
        assert transport.client.is_closed

    go(body())


def test_closing_survives_a_failing_token_refresh() -> None:
    """Session termination is best effort: an expired token whose refresh gets
    a non-JSON answer (a proxy's HTML page) must not raise out of cleanup."""
    import httpx

    from evalsi_harness.mcp import _HTTPTransport

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/token":
            return httpx.Response(200, text="<html>proxy</html>")
        return httpx.Response(200)

    async def body() -> None:
        transport = _HTTPTransport(
            "http://mcp.test/mcp", {}, 5.0, OAuth("http://mcp.test/token", "id", "secret")
        )
        await transport.client.aclose()
        transport.client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        transport.session = "session"
        await transport.aclose()
        assert transport.client.is_closed

    go(body())


# --- stdio line limits -------------------------------------------------------------

_BIG_LINE_SERVER = """
import sys, json
for line in sys.stdin:
    m = json.loads(line)
    if "id" not in m:
        continue
    if m["method"] == "tools/call":
        r = {"content": [{"type": "text", "text": "x" * 200000}]}
    elif m["method"] == "initialize":
        r = {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {}}
    else:
        r = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": m["id"], "result": r}) + "\\n")
    sys.stdout.flush()
"""


def test_a_stdio_reply_over_64kib_is_read() -> None:
    async def body() -> None:
        client = await MCPClient.connect(command=[sys.executable, "-c", _BIG_LINE_SERVER])
        try:
            result = await client.call_tool("big", {})
            assert len(result.content) == 200000
            assert not result.is_error
        finally:
            await client.aclose()

    go(body())


def test_a_stdio_reply_over_the_limit_is_an_mcp_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("evalsi_harness.mcp._STDIO_LINE_LIMIT", 100_000)

    async def body() -> None:
        client = await MCPClient.connect(command=[sys.executable, "-c", _BIG_LINE_SERVER])
        try:
            result = await client.call_tool("big", {})
            assert result.is_error
            assert "over" in result.content
        finally:
            await client.aclose()

    go(body())

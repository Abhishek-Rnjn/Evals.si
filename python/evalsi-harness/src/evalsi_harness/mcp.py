"""A small MCP client: tools, resources and prompts over streamable HTTP or stdio.

What an agent harness needs: ``initialize``, ``tools/list`` and ``tools/call``
(with a timeout per tool and structured output), ``resources/list`` and
``resources/read``, ``prompts/list`` and ``prompts/get``, and server-initiated
``elicitation/create`` requests, answered by a handler (the simulated user).
Streamable HTTP servers may answer with JSON or with a server-sent event
stream, which is read as it arrives so that a server can ask a question in the
middle of a call; remote servers can be reached with an OAuth client-credentials
token. Stdio servers are started on the worker, outside the sandbox, so they
must come from trusted configuration.
"""

from __future__ import annotations

import asyncio
import contextlib
import itertools
import json
import os
import re
import time
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any

import httpx

from evalsi_harness._version import __version__
from evalsi_harness.modelhttp import ModelError, _sse
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import ToolResult, _clip

PROTOCOL_VERSION = "2025-06-18"


class MCPError(RuntimeError):
    pass


# Answers a server's question: (message, requested JSON schema) -> the result,
# {"action": "accept" | "decline" | "cancel", "content": {...}}.
ElicitHandler = Callable[[str, dict[str, Any]], Awaitable[dict[str, Any]]]
# Answers a server-initiated request with the body of the JSON-RPC response.
RequestHandler = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


def _client_info(elicitation: bool = False) -> dict[str, Any]:
    return {
        "protocolVersion": PROTOCOL_VERSION,
        "capabilities": {"elicitation": {}} if elicitation else {},
        "clientInfo": {"name": "evalsi-harness", "version": __version__},
    }


@dataclass
class OAuth:
    """An OAuth 2 client-credentials grant for a remote server: the machine flow,
    since a headless run has no one to approve a browser redirect."""

    token_url: str
    client_id: str
    client_secret: str
    scope: str = ""
    _token: str = ""
    _expires: float = 0.0

    async def token(self, client: httpx.AsyncClient) -> str:
        if self._token and time.monotonic() < self._expires:
            return self._token
        data = {"grant_type": "client_credentials"}
        if self.scope:
            data["scope"] = self.scope
        try:
            response = await client.post(
                self.token_url,
                data=data,
                auth=(self.client_id, self.client_secret),
                timeout=30.0,
            )
        except httpx.TransportError as exc:
            raise MCPError(
                f"OAuth token request to {self.token_url} failed: {type(exc).__name__}: {exc}"
            ) from exc
        if response.status_code >= 400:
            raise MCPError(f"OAuth token request to {self.token_url}: HTTP {response.status_code}")
        body = response.json()
        token = body.get("access_token") if isinstance(body, dict) else None
        if not isinstance(token, str) or not token:
            raise MCPError(f"OAuth token response from {self.token_url} has no access_token")
        # Refresh a little before the expiry (an hour when the server does not say).
        self._token = token
        self._expires = time.monotonic() + max(float(body.get("expires_in") or 3600) - 30, 1.0)
        return token

    def invalidate(self) -> None:
        self._token, self._expires = "", 0.0


class _HTTPTransport:
    def __init__(
        self, url: str, headers: Mapping[str, str], timeout_s: float, oauth: OAuth | None = None
    ) -> None:
        self.url = url
        self.headers = {
            "Accept": "application/json, text/event-stream",
            "Content-Type": "application/json",
            **headers,
        }
        # Time limits are the caller's (asyncio.wait_for, per request): a tool that
        # may take ten minutes must not die of a read timeout set for the others.
        self.client = httpx.AsyncClient(timeout=httpx.Timeout(None, connect=30.0))
        self.session: str | None = None
        self.oauth = oauth

    async def _headers(self) -> dict[str, str]:
        headers = dict(self.headers)
        if self.session:
            headers["Mcp-Session-Id"] = self.session
            headers["MCP-Protocol-Version"] = PROTOCOL_VERSION
        if self.oauth is not None:
            headers["Authorization"] = f"Bearer {await self.oauth.token(self.client)}"
        return headers

    async def request(
        self, message: dict[str, Any], on_request: RequestHandler | None = None
    ) -> dict[str, Any] | None:
        for attempt in (1, 2):
            headers = await self._headers()
            try:
                async with self.client.stream(
                    "POST", self.url, json=message, headers=headers
                ) as response:
                    if response.status_code == 401 and self.oauth is not None and attempt == 1:
                        await response.aread()
                        self.oauth.invalidate()  # the token expired: get another, once
                        continue
                    return await self._read(response, message, on_request)
            except httpx.TransportError as exc:
                raise MCPError(f"MCP server {self.url}: {type(exc).__name__}: {exc}") from exc
        raise MCPError(f"MCP server {self.url}: the OAuth token was refused")

    async def _read(
        self,
        response: httpx.Response,
        message: dict[str, Any],
        on_request: RequestHandler | None,
    ) -> dict[str, Any] | None:
        if sid := response.headers.get("mcp-session-id"):
            self.session = sid
        if "id" not in message:
            await response.aread()
            return None
        if response.status_code >= 400:
            text = (await response.aread()).decode(errors="replace")
            raise MCPError(f"MCP server {self.url}: HTTP {response.status_code}: {text[:300]}")
        kind = response.headers.get("content-type", "")
        if kind.startswith("text/event-stream"):
            try:
                async for _, msg in _sse(response):
                    if "method" in msg:  # the server asks, or tells, something mid-call
                        if "id" in msg and on_request is not None:
                            await self._answer(msg["id"], await on_request(msg))
                        continue
                    if msg.get("id") == message["id"]:
                        return msg
            except ModelError as exc:
                raise MCPError(f"MCP server {self.url}: {type(exc).__name__}: {exc}") from exc
            raise MCPError(f"MCP server {self.url}: no response to request {message['id']}")
        body = json.loads((await response.aread()) or b"null")
        if not isinstance(body, dict):
            raise MCPError(f"MCP server {self.url}: unexpected response {body!r:.200}")
        return body

    async def _answer(self, request_id: Any, body: dict[str, Any]) -> None:
        reply = {"jsonrpc": "2.0", "id": request_id, **body}
        try:
            await self.client.post(self.url, json=reply, headers=await self._headers())
        except httpx.TransportError as exc:
            raise MCPError(f"MCP server {self.url}: {type(exc).__name__}: {exc}") from exc

    async def aclose(self) -> None:
        if self.session:
            with contextlib.suppress(httpx.HTTPError):
                await self.client.delete(self.url, headers={"Mcp-Session-Id": self.session})
        await self.client.aclose()


class _StdioTransport:
    def __init__(self, process: asyncio.subprocess.Process, name: str) -> None:
        self.process = process
        self.name = name
        self.lock = asyncio.Lock()

    @classmethod
    async def start(cls, command: Sequence[str], env: Mapping[str, str]) -> _StdioTransport:
        process = await asyncio.create_subprocess_exec(
            *command,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
            env={**os.environ, **env},
        )
        return cls(process, command[0])

    async def request(
        self, message: dict[str, Any], on_request: RequestHandler | None = None
    ) -> dict[str, Any] | None:
        assert self.process.stdin is not None
        assert self.process.stdout is not None
        async with self.lock:
            self.process.stdin.write((json.dumps(message) + "\n").encode())
            await self.process.stdin.drain()
            if "id" not in message:
                return None
            while True:
                line = await self.process.stdout.readline()
                if not line:
                    raise MCPError(f"MCP server {self.name} exited")
                try:
                    msg = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(msg, dict):
                    continue
                if "method" in msg:  # the server asks, or tells, something mid-call
                    if "id" in msg and on_request is not None:
                        reply = {"jsonrpc": "2.0", "id": msg["id"], **await on_request(msg)}
                        self.process.stdin.write((json.dumps(reply) + "\n").encode())
                        await self.process.stdin.drain()
                    continue
                if msg.get("id") == message["id"]:
                    return msg

    async def aclose(self) -> None:
        if self.process.returncode is None:
            if self.process.stdin is not None:
                self.process.stdin.close()
            try:
                await asyncio.wait_for(self.process.wait(), 5)
            except TimeoutError:
                self.process.kill()
                await self.process.wait()


class MCPClient:
    def __init__(
        self,
        transport: _HTTPTransport | _StdioTransport,
        name: str,
        *,
        timeout_s: float = 120.0,
        elicit: ElicitHandler | None = None,
    ) -> None:
        self.transport = transport
        self.name = name
        self.timeout_s = timeout_s
        self.elicit = elicit
        # What the server asked while a tool ran, for that tool's step.
        self.elicitations: list[dict[str, Any]] = []
        self._ids = itertools.count(1)

    @classmethod
    async def connect(
        cls,
        *,
        name: str = "",
        url: str = "",
        command: Sequence[str] = (),
        headers: Mapping[str, str] | None = None,
        env: Mapping[str, str] | None = None,
        timeout_s: float = 120.0,
        oauth: OAuth | None = None,
        elicit: ElicitHandler | None = None,
    ) -> MCPClient:
        if bool(url) == bool(command):
            raise MCPError("an MCP server needs exactly one of url or command")
        if oauth is not None and not url:
            raise MCPError("OAuth is for servers reached by url")
        transport: _HTTPTransport | _StdioTransport
        if url:
            transport = _HTTPTransport(url, headers or {}, timeout_s, oauth)
        else:
            transport = await _StdioTransport.start(command, env or {})
        client = cls(transport, name or url or command[0], timeout_s=timeout_s, elicit=elicit)
        try:
            await client.call("initialize", _client_info(elicit is not None))
            await asyncio.wait_for(
                transport.request({"jsonrpc": "2.0", "method": "notifications/initialized"}),
                timeout_s,
            )
        except BaseException:
            await transport.aclose()
            raise
        return client

    async def _on_request(self, msg: dict[str, Any]) -> dict[str, Any]:
        """Answers a request the server makes of us, as the body of the response."""
        method = msg.get("method")
        if method == "ping":
            return {"result": {}}
        if method == "elicitation/create" and self.elicit is not None:
            params = msg.get("params") or {}
            question = str(params.get("message") or "")
            try:
                answer = await self.elicit(question, params.get("requestedSchema") or {})
            except Exception:  # an answering failure is a refusal, not a crashed tool
                answer = {"action": "cancel"}
            self.elicitations.append({"message": question[:300], "action": answer.get("action")})
            return {"result": answer}
        return {"error": {"code": -32601, "message": f"method not supported: {method}"}}

    async def call(
        self,
        method: str,
        params: dict[str, Any] | None = None,
        *,
        timeout_s: float | None = None,
    ) -> dict[str, Any]:
        message: dict[str, Any] = {"jsonrpc": "2.0", "id": next(self._ids), "method": method}
        if params is not None:
            message["params"] = params
        limit = timeout_s or self.timeout_s
        try:
            response = await asyncio.wait_for(
                self.transport.request(message, self._on_request), limit
            )
        except TimeoutError as exc:
            raise MCPError(f"{self.name}: {method} timed out after {limit:g} s") from exc
        if response is None:
            raise MCPError(f"{self.name}: no response to {method}")
        if "error" in response:
            err = response["error"]
            raise MCPError(f"{self.name}: {method}: {err.get('message', err)}")
        result = response.get("result")
        return result if isinstance(result, dict) else {}

    async def _paged(self, method: str, key: str) -> list[dict[str, Any]]:
        items: list[dict[str, Any]] = []
        cursor: str | None = None
        while True:
            result = await self.call(method, {"cursor": cursor} if cursor else {})
            items += [i for i in result.get(key) or [] if isinstance(i, dict)]
            cursor = result.get("nextCursor")
            if not cursor:
                return items

    async def list_tools(self) -> list[ToolSpec]:
        out = []
        for t in await self._paged("tools/list", "tools"):
            description = t.get("description") or ""
            if isinstance(t.get("outputSchema"), dict):
                # The model should know the shape of what comes back.
                description += (
                    f"\nReturns JSON: {json.dumps(t['outputSchema'], separators=(',', ':'))}"
                )
            out.append(
                ToolSpec(
                    name=t["name"],
                    description=description.strip(),
                    input_schema=t.get("inputSchema") or {"type": "object", "properties": {}},
                )
            )
        return out

    async def call_tool(
        self, name: str, arguments: dict[str, Any], *, timeout_s: float | None = None
    ) -> ToolResult:
        self.elicitations = []
        try:
            result = await self.call(
                "tools/call", {"name": name, "arguments": arguments}, timeout_s=timeout_s
            )
        except MCPError as exc:
            return ToolResult(str(exc), is_error=True, metadata=self._meta())
        parts = [text_of(item) for item in result.get("content") or [] if isinstance(item, dict)]
        text = "\n".join(p for p in parts if p)
        if not text and "structuredContent" in result:
            text = json.dumps(result["structuredContent"])
        return ToolResult(text, is_error=bool(result.get("isError")), metadata=self._meta())

    def _meta(self) -> dict[str, Any]:
        return {"elicitations": json.dumps(self.elicitations)} if self.elicitations else {}

    async def list_resources(self) -> list[dict[str, Any]]:
        return await self._paged("resources/list", "resources")

    async def read_resource(self, uri: str) -> str:
        result = await self.call("resources/read", {"uri": uri})
        parts = []
        for c in result.get("contents") or []:
            if isinstance(c, dict):
                parts.append(c["text"] if "text" in c else blob_note(c))
        return "\n".join(parts)

    async def list_prompts(self) -> list[dict[str, Any]]:
        return await self._paged("prompts/list", "prompts")

    async def get_prompt(self, name: str, arguments: dict[str, str]) -> str:
        result = await self.call("prompts/get", {"name": name, "arguments": arguments})
        lines = []
        if result.get("description"):
            lines.append(str(result["description"]))
        for m in result.get("messages") or []:
            if isinstance(m, dict):
                content = m.get("content")
                body = text_of(content) if isinstance(content, dict) else str(content or "")
                lines.append(f"[{m.get('role', 'user')}] {body}")
        return "\n".join(lines)

    async def aclose(self) -> None:
        await self.transport.aclose()


def blob_note(item: Mapping[str, Any]) -> str:
    """A placeholder for binary content: base64 in a context is waste."""
    data = str(item.get("data") or item.get("blob") or "")
    size = len(data) * 3 // 4 - data.count("=")
    kind = item.get("mimeType") or "binary"
    uri = f" {item['uri']}" if item.get("uri") else ""
    return f"[{kind} content{uri}, {size} bytes, not shown]"


def text_of(item: Mapping[str, Any]) -> str:
    """The text an agent should see of one MCP content item."""
    kind = item.get("type")
    if kind == "text":
        return str(item.get("text", ""))
    if kind in ("image", "audio"):
        return blob_note(item)
    if kind == "resource" and isinstance(item.get("resource"), dict):
        res = item["resource"]
        return str(res["text"]) if "text" in res else blob_note(res)
    if kind == "resource_link":
        name = f" ({item['name']})" if item.get("name") else ""
        return f"[resource link: {item.get('uri', '')}{name}]"
    return json.dumps(item)


class MCPTool:
    """One tool of an MCP server, exposed to the agent under ``exposed_name``."""

    def __init__(
        self, client: MCPClient, spec: ToolSpec, exposed_name: str = "", timeout_s: float = 0.0
    ) -> None:
        self.client = client
        self.remote_name = spec.name
        self.timeout_s = timeout_s
        # Loaded on demand by tool_search instead of listed in every prompt.
        self.deferred = False
        self.spec = ToolSpec(
            name=exposed_name or spec.name,
            description=spec.description,
            input_schema=spec.input_schema,
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        return await self.client.call_tool(self.remote_name, args, timeout_s=self.timeout_s or None)


class MCPResourcesTool:
    """``<server>_resources``: list what a server offers to read, and read it."""

    def __init__(self, client: MCPClient, server: str) -> None:
        self.client = client
        self.spec = ToolSpec(
            name=f"{server}_resources",
            description=(
                f"Resources (documents, files, records) of the {server} server. Action list shows "
                "what is available; action read, with a uri from the list, returns its content."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["list", "read"]},
                    "uri": {"type": "string"},
                },
                "required": ["action"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        try:
            if args.get("action") == "list":
                items = await self.client.list_resources()
                lines = [
                    f"{r.get('uri')}  {r.get('name', '')}  {r.get('description', '')}".rstrip()
                    for r in items
                ]
                return ToolResult(_clip("\n".join(lines) or "no resources"))
            if args.get("action") == "read" and isinstance(args.get("uri"), str):
                return ToolResult(_clip(await self.client.read_resource(args["uri"])))
        except MCPError as exc:
            return ToolResult(str(exc), is_error=True)
        return ToolResult("action must be list, or read with a uri", is_error=True)


class MCPPromptsTool:
    """``<server>_prompts``: a server's prompt templates, filled in."""

    def __init__(self, client: MCPClient, server: str) -> None:
        self.client = client
        self.spec = ToolSpec(
            name=f"{server}_prompts",
            description=(
                f"Prompt templates of the {server} server. Action list shows them and their "
                "arguments; action get, with a name and arguments, returns the filled-in prompt."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["list", "get"]},
                    "name": {"type": "string"},
                    "arguments": {"type": "object", "additionalProperties": {"type": "string"}},
                },
                "required": ["action"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        try:
            if args.get("action") == "list":
                lines = []
                for p in await self.client.list_prompts():
                    names = ", ".join(str(a.get("name")) for a in p.get("arguments") or [])
                    lines.append(f"{p.get('name')}({names})  {p.get('description', '')}".rstrip())
                return ToolResult(_clip("\n".join(lines) or "no prompts"))
            if args.get("action") == "get" and isinstance(args.get("name"), str):
                given = args.get("arguments")
                arguments = (
                    {str(k): str(v) for k, v in given.items()} if isinstance(given, dict) else {}
                )
                return ToolResult(_clip(await self.client.get_prompt(args["name"], arguments)))
        except MCPError as exc:
            return ToolResult(str(exc), is_error=True)
        return ToolResult("action must be list, or get with a name", is_error=True)


_NUMBER = re.compile(r"-?\d+(?:\.\d+)?")


def answer_from_text(schema: dict[str, Any], text: str) -> dict[str, Any]:
    """An elicitation result built from what a simulated user said. Each
    property of the requested schema takes the part of the reply that fits its
    type; a required property the reply cannot fill is a decline."""
    properties = schema.get("properties") or {}
    required = set(schema.get("required") or [])
    lowered = text.lower()
    content: dict[str, Any] = {}
    # A number goes to the property it is next to ("ratio 0.5"), else to the next
    # numeric property in order.
    unused = [m.group() for m in _NUMBER.finditer(text)]
    for key, spec in properties.items():
        kind = spec.get("type", "string")
        value: Any = None
        if spec.get("enum"):
            value = next((e for e in spec["enum"] if str(e).lower() in lowered), None)
        elif kind == "boolean":
            yes = re.search(r"\b(yes|yep|sure|ok|okay|true|accept|confirm|go ahead)\b", lowered)
            no = re.search(r"\b(no|nope|false|decline|deny|don't|do not)\b", lowered)
            value = True if yes and not no else False if no else None
        elif kind in ("number", "integer"):
            near = re.search(rf"{re.escape(key)}\W{{0,3}}({_NUMBER.pattern})", text, re.IGNORECASE)
            raw = near.group(1) if near else (unused[0] if unused else None)
            if raw is not None:
                if raw in unused:
                    unused.remove(raw)
                value = int(float(raw)) if kind == "integer" else float(raw)
        else:
            value = text.strip() or None
        if value is None:
            if key in required:
                return {"action": "decline"}
            continue
        content[key] = value
    return {"action": "accept", "content": content}

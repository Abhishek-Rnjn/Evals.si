"""A small MCP client: tools over streamable HTTP or stdio.

Only what an agent harness needs: ``initialize``, ``tools/list`` and
``tools/call``. Streamable HTTP servers may answer with JSON or with a
server-sent event stream; both are handled. Stdio servers are started on the
worker, outside the sandbox, so they must come from trusted configuration.
"""

from __future__ import annotations

import asyncio
import contextlib
import itertools
import json
import os
from collections.abc import Mapping, Sequence
from typing import Any

import httpx

from evalsi_harness._version import __version__
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import ToolResult

PROTOCOL_VERSION = "2025-06-18"


class MCPError(RuntimeError):
    pass


def _client_info() -> dict[str, Any]:
    return {
        "protocolVersion": PROTOCOL_VERSION,
        "capabilities": {},
        "clientInfo": {"name": "evalsi-harness", "version": __version__},
    }


class _HTTPTransport:
    def __init__(self, url: str, headers: Mapping[str, str], timeout_s: float) -> None:
        self.url = url
        self.headers = {
            "Accept": "application/json, text/event-stream",
            "Content-Type": "application/json",
            **headers,
        }
        self.client = httpx.AsyncClient(timeout=timeout_s)
        self.session: str | None = None

    async def request(self, message: dict[str, Any]) -> dict[str, Any] | None:
        headers = dict(self.headers)
        if self.session:
            headers["Mcp-Session-Id"] = self.session
            headers["MCP-Protocol-Version"] = PROTOCOL_VERSION
        try:
            response = await self.client.post(self.url, json=message, headers=headers)
        except httpx.TransportError as exc:
            raise MCPError(f"MCP server {self.url}: {exc}") from exc
        if sid := response.headers.get("mcp-session-id"):
            self.session = sid
        if "id" not in message:
            return None
        if response.status_code >= 400:
            raise MCPError(
                f"MCP server {self.url}: HTTP {response.status_code}: {response.text[:300]}"
            )
        kind = response.headers.get("content-type", "")
        if kind.startswith("text/event-stream"):
            for block in response.text.split("\n\n"):
                data = "\n".join(
                    line[5:].lstrip() for line in block.splitlines() if line.startswith("data:")
                )
                if not data:
                    continue
                msg = json.loads(data)
                if isinstance(msg, dict) and msg.get("id") == message["id"]:
                    return msg
            raise MCPError(f"MCP server {self.url}: no response to request {message['id']}")
        msg = response.json()
        if not isinstance(msg, dict):
            raise MCPError(f"MCP server {self.url}: unexpected response {msg!r:.200}")
        return msg

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

    async def request(self, message: dict[str, Any]) -> dict[str, Any] | None:
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
                if isinstance(msg, dict) and msg.get("id") == message["id"]:
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
    def __init__(self, transport: _HTTPTransport | _StdioTransport, name: str) -> None:
        self.transport = transport
        self.name = name
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
    ) -> MCPClient:
        if bool(url) == bool(command):
            raise MCPError("an MCP server needs exactly one of url or command")
        transport: _HTTPTransport | _StdioTransport
        if url:
            transport = _HTTPTransport(url, headers or {}, timeout_s)
        else:
            transport = await _StdioTransport.start(command, env or {})
        client = cls(transport, name or url or command[0])
        await client.call("initialize", _client_info())
        await transport.request({"jsonrpc": "2.0", "method": "notifications/initialized"})
        return client

    async def call(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        message: dict[str, Any] = {"jsonrpc": "2.0", "id": next(self._ids), "method": method}
        if params is not None:
            message["params"] = params
        response = await self.transport.request(message)
        if response is None:
            raise MCPError(f"{self.name}: no response to {method}")
        if "error" in response:
            err = response["error"]
            raise MCPError(f"{self.name}: {method}: {err.get('message', err)}")
        result = response.get("result")
        return result if isinstance(result, dict) else {}

    async def list_tools(self) -> list[ToolSpec]:
        tools: list[ToolSpec] = []
        cursor: str | None = None
        while True:
            result = await self.call("tools/list", {"cursor": cursor} if cursor else {})
            for t in result.get("tools") or []:
                tools.append(
                    ToolSpec(
                        name=t["name"],
                        description=t.get("description") or "",
                        input_schema=t.get("inputSchema") or {"type": "object", "properties": {}},
                    )
                )
            cursor = result.get("nextCursor")
            if not cursor:
                return tools

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> ToolResult:
        try:
            result = await self.call("tools/call", {"name": name, "arguments": arguments})
        except MCPError as exc:
            return ToolResult(str(exc), is_error=True)
        parts = []
        for item in result.get("content") or []:
            if item.get("type") == "text":
                parts.append(item.get("text", ""))
            elif item.get("type") == "resource" and isinstance(item.get("resource"), dict):
                parts.append(item["resource"].get("text", ""))
            else:
                parts.append(json.dumps(item))
        if not parts and "structuredContent" in result:
            parts.append(json.dumps(result["structuredContent"]))
        return ToolResult("\n".join(parts), is_error=bool(result.get("isError")))

    async def aclose(self) -> None:
        await self.transport.aclose()


class MCPTool:
    """One tool of an MCP server, exposed to the agent under ``exposed_name``."""

    def __init__(self, client: MCPClient, spec: ToolSpec, exposed_name: str = "") -> None:
        self.client = client
        self.remote_name = spec.name
        self.spec = ToolSpec(
            name=exposed_name or spec.name,
            description=spec.description,
            input_schema=spec.input_schema,
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        return await self.client.call_tool(self.remote_name, args)

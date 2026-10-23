"""A webhook receiver for A2A push notifications.

An A2A agent that declares ``pushNotifications`` POSTs a task's updates to the
URL a client names in ``pushNotificationConfig``. ``PushReceiver`` is that
URL's other end: a small HTTP/1.1 server on the worker (asyncio streams, no
dependency) that accepts a POST only with the token the harness put in the
config, and queues the JSON bodies for the connector.

It answers every accepted request with 200 and an empty JSON object, and
refuses with 401 a request without the token, with 404 any other path, and
with 413 a body over ``MAX_BODY``. One receiver serves one connector, so a
token is bound to one conversation.
"""

from __future__ import annotations

import asyncio
import hmac
import json
import secrets
from typing import Any

MAX_BODY = 1 << 20
MAX_QUEUED = 1000
TOKEN_HEADER = "x-a2a-notification-token"


class PushReceiver:
    def __init__(self, *, port: int = 0, public_url: str = "") -> None:
        self.port = port
        self.public_url = public_url.rstrip("/")
        self.token = secrets.token_urlsafe(24)
        # The path is unguessable too, so a request that misses the token header
        # still learns nothing about whether the receiver exists.
        self._path = "/a2a-push/" + secrets.token_urlsafe(12)
        self.events: asyncio.Queue[dict[str, Any]] = asyncio.Queue(MAX_QUEUED)
        self.rejected = 0
        self._server: asyncio.Server | None = None

    @property
    def url(self) -> str:
        return f"{self.public_url or f'http://127.0.0.1:{self.port}'}{self._path}"

    async def start(self) -> None:
        # With a public URL the agent is probably on another host: listen on every
        # interface. Otherwise only the worker itself can reach the receiver.
        host = "0.0.0.0" if self.public_url else "127.0.0.1"
        self._server = await asyncio.start_server(self._serve, host, self.port)
        self.port = self._server.sockets[0].getsockname()[1]

    async def aclose(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            status = await asyncio.wait_for(self._handle(reader), 10)
        except (TimeoutError, ConnectionError, asyncio.IncompleteReadError, ValueError):
            status = 400
        reason = {200: "OK", 400: "Bad Request", 401: "Unauthorized", 404: "Not Found"}.get(
            status, "Payload Too Large" if status == 413 else "Error"
        )
        body = b"{}"
        try:
            writer.write(
                f"HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\n"
                f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n".encode()
                + body
            )
            await writer.drain()
        except ConnectionError:
            pass
        finally:
            writer.close()

    async def _handle(self, reader: asyncio.StreamReader) -> int:
        request = (await reader.readline()).decode("latin-1").split()
        if len(request) < 2 or request[0] != "POST":
            return 400
        headers: dict[str, str] = {}
        while (line := (await reader.readline()).decode("latin-1").strip()) != "":
            name, _, value = line.partition(":")
            headers[name.strip().lower()] = value.strip()
        if request[1] != self._path:
            return 404
        if not hmac.compare_digest(headers.get(TOKEN_HEADER, ""), self.token):
            self.rejected += 1
            return 401
        length = int(headers.get("content-length", "0"))
        if length > MAX_BODY:
            return 413
        payload = json.loads(await reader.readexactly(length)) if length else {}
        if not isinstance(payload, dict):
            return 400
        try:
            self.events.put_nowait(payload)
        except asyncio.QueueFull:
            self.events.get_nowait()  # the newest states matter most
            self.events.put_nowait(payload)
        return 200

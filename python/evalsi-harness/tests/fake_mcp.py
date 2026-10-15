"""A fake MCP server over streamable HTTP, with the features the client needs
tested: SSE replies, a question asked mid-call, OAuth, slow and binary tools,
resources and prompts."""

from __future__ import annotations

import base64
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs


class FakeMCP:
    def __init__(self, *, oauth: bool = False, extra_tools: int = 0) -> None:
        self.oauth = oauth
        self.token = "tok-1"
        self.token_requests: list[dict[str, Any]] = []
        self.auth_seen: list[str] = []
        self.calls: list[tuple[str, dict[str, Any]]] = []
        self.client_capabilities: dict[str, Any] = {}
        self.extra_tools = extra_tools
        self._answers: dict[str, dict[str, Any]] = {}
        self._events: dict[str, threading.Event] = {}
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def _send(self, status: int, body: bytes, ctype: str = "application/json") -> None:
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self) -> None:
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                if self.path == "/token":
                    form = {k: v[0] for k, v in parse_qs(raw.decode()).items()}
                    outer.token_requests.append(
                        {"form": form, "authorization": self.headers.get("Authorization", "")}
                    )
                    n = len(outer.token_requests)
                    outer.token = f"tok-{n}"
                    body = {"access_token": outer.token, "expires_in": 3600, "token_type": "Bearer"}
                    self._send(200, json.dumps(body).encode())
                    return
                auth = self.headers.get("Authorization", "")
                outer.auth_seen.append(auth)
                if outer.oauth and auth != f"Bearer {outer.token}":
                    self._send(401, b'{"error": "invalid_token"}')
                    return
                msg = json.loads(raw)
                if "method" not in msg:  # the client's answer to a question we asked
                    outer._answers[str(msg["id"])] = msg
                    outer._events[str(msg["id"])].set()
                    self._send(202, b"")
                    return
                if "id" not in msg:
                    self._send(202, b"")
                    return
                outer.handle(self, msg)

            def do_DELETE(self) -> None:
                self._send(200, b"")

        self.Handler = Handler
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}/mcp"

    @property
    def token_url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}/token"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    def __enter__(self) -> FakeMCP:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # --- protocol ---

    def tools(self) -> list[dict[str, Any]]:
        tools: list[dict[str, Any]] = [
            {"name": "echo", "description": "Echo text back.", "inputSchema": {"type": "object"}},
            {
                "name": "slow",
                "description": "Sleep, then answer.",
                "inputSchema": {"type": "object"},
            },
            {
                "name": "image",
                "description": "Return a picture.",
                "inputSchema": {"type": "object"},
            },
            {
                "name": "structured",
                "description": "Return a record.",
                "inputSchema": {"type": "object"},
                "outputSchema": {"type": "object", "properties": {"n": {"type": "integer"}}},
            },
            {
                "name": "ask",
                "description": "Ask the user a question.",
                "inputSchema": {"type": "object"},
            },
        ]
        words = [
            "weather",
            "calendar",
            "invoice",
            "ticket",
            "deploy",
            "search",
            "report",
            "shipment",
        ]
        for i in range(self.extra_tools):
            w = words[i % len(words)]
            tools.append(
                {
                    "name": f"{w}_tool_{i}",
                    "description": f"Work with {w} data (variant {i}).",
                    "inputSchema": {"type": "object"},
                }
            )
        return tools

    def result(self, handler: BaseHTTPRequestHandler, msg: dict[str, Any], result: Any) -> None:
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}).encode()
        handler.send_response(200)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Mcp-Session-Id", "s-1")
        handler.send_header("Content-Length", str(len(body)))
        handler.end_headers()
        handler.wfile.write(body)

    def handle(self, handler: BaseHTTPRequestHandler, msg: dict[str, Any]) -> None:
        method, params = msg["method"], msg.get("params") or {}
        if method == "initialize":
            self.client_capabilities = params.get("capabilities") or {}
            self.result(handler, msg, {"protocolVersion": "2025-06-18", "capabilities": {}})
        elif method == "tools/list":
            self.result(handler, msg, {"tools": self.tools()})
        elif method == "tools/call":
            self.tool_call(handler, msg, params["name"], params.get("arguments") or {})
        elif method == "resources/list":
            res = [{"uri": "doc://handbook", "name": "handbook", "description": "The handbook"}]
            self.result(handler, msg, {"resources": res})
        elif method == "resources/read":
            if params["uri"] == "doc://handbook":
                contents = [{"uri": params["uri"], "text": "Rule 1: write tests."}]
            else:
                blob = base64.b64encode(b"x" * 300).decode()
                contents = [{"uri": params["uri"], "mimeType": "image/png", "blob": blob}]
            self.result(handler, msg, {"contents": contents})
        elif method == "prompts/list":
            prompts = [
                {"name": "review", "description": "Review code", "arguments": [{"name": "path"}]}
            ]
            self.result(handler, msg, {"prompts": prompts})
        elif method == "prompts/get":
            text = f"Please review {params.get('arguments', {}).get('path', '?')}."
            messages = [{"role": "user", "content": {"type": "text", "text": text}}]
            self.result(handler, msg, {"description": "Review code", "messages": messages})
        else:
            body = json.dumps(
                {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": "nope"}}
            ).encode()
            handler.send_response(200)
            handler.send_header("Content-Length", str(len(body)))
            handler.end_headers()
            handler.wfile.write(body)

    def tool_call(
        self, handler: BaseHTTPRequestHandler, msg: dict[str, Any], name: str, args: dict[str, Any]
    ) -> None:
        self.calls.append((name, args))
        if name == "slow":
            time.sleep(float(args.get("seconds", 1)))
            self.result(handler, msg, {"content": [{"type": "text", "text": "finally"}]})
        elif name == "image":
            data = base64.b64encode(b"\x89PNG" * 500).decode()
            content = [
                {"type": "text", "text": "here is the chart"},
                {"type": "image", "mimeType": "image/png", "data": data},
                {"type": "resource_link", "uri": "doc://handbook", "name": "handbook"},
            ]
            self.result(handler, msg, {"content": content})
        elif name == "structured":
            self.result(handler, msg, {"structuredContent": {"n": 7}, "content": []})
        elif name == "ask":
            self.ask(handler, msg)
        else:
            self.result(handler, msg, {"content": [{"type": "text", "text": f"{name}:{args}"}]})

    def ask(self, handler: BaseHTTPRequestHandler, msg: dict[str, Any]) -> None:
        if "elicitation" not in self.client_capabilities:
            self.result(handler, msg, {"content": [{"type": "text", "text": "no elicitation"}]})
            return
        qid = f"srv-{len(self.calls)}"
        self._events[qid] = threading.Event()
        question = {
            "jsonrpc": "2.0",
            "id": qid,
            "method": "elicitation/create",
            "params": {
                "message": "Which environment should I deploy to?",
                "requestedSchema": {
                    "type": "object",
                    "properties": {"environment": {"type": "string", "enum": ["staging", "prod"]}},
                    "required": ["environment"],
                },
            },
        }
        handler.send_response(200)
        handler.send_header("Content-Type", "text/event-stream")
        handler.end_headers()
        handler.wfile.write(f"event: message\ndata: {json.dumps(question)}\n\n".encode())
        handler.wfile.flush()
        if not self._events[qid].wait(10):
            answer: dict[str, Any] = {"result": {"action": "timeout"}}
        else:
            answer = self._answers[qid]
        text = json.dumps(answer.get("result"))
        final = {
            "jsonrpc": "2.0",
            "id": msg["id"],
            "result": {"content": [{"type": "text", "text": f"user said {text}"}]},
        }
        handler.wfile.write(f"event: message\ndata: {json.dumps(final)}\n\n".encode())

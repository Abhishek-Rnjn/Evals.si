"""Local, deterministic chat-completions model for the onboarding lab.

This simulates autoregressive conversation turns, not neural token generation.
No model weights, provider credentials or third-party services are used.
"""

from __future__ import annotations

import argparse
import json
import re
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


def reply(body: dict[str, Any]) -> dict[str, Any]:
    """Choose the next assistant message from the entire conversation prefix."""
    messages = body["messages"]
    mode = body.get("model", "careful")
    user = next(m["content"] for m in messages if m["role"] == "user")
    order = int(re.search(r"order (\d+)", user)[1])
    observations = [m for m in messages if m["role"] == "tool"]
    names = [m.get("name", "") for m in observations]
    # Chat-completions tool messages need not carry a name: recover it by ID.
    calls = {c["id"]: c["function"]["name"] for m in messages for c in m.get("tool_calls", [])}
    names = [
        n or calls.get(m.get("tool_call_id"), "") for n, m in zip(names, observations, strict=True)
    ]

    def call(name: str, arguments: Any) -> dict[str, Any]:
        return {
            "role": "assistant",
            "content": None,
            "tool_calls": [
                {
                    "id": f"call_{len(observations)}",
                    "type": "function",
                    "function": {"name": name, "arguments": json.dumps(arguments)},
                }
            ],
        }

    if mode == "loop":
        return call("lookup_order", {"id": order})
    if not observations:
        return call("lookup_order", {} if mode == "bad-arguments" else {"id": order})
    if mode in {"bad-arguments", "skip-refund"}:
        return {"role": "assistant", "content": f"Refunded order {order}."}
    # One retry following a tool error; a persistent fault then ends the task.
    if observations[-1]["content"].startswith("ERROR:"):
        if sum(m["content"].startswith("ERROR:") for m in observations) < 2:
            return call(names[-1], {"id": order})
        return {"role": "assistant", "content": "Could not complete the refund."}
    if names[-1] == "lookup_order":
        return call("refund", {"id": order})
    return {"role": "assistant", "content": f"Refunded order {order}."}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.send_json(200 if self.path == "/health" else 404, {"status": "ok"})

    def do_POST(self) -> None:
        if self.path != "/v1/chat/completions":
            self.send_json(404, {"error": "Use /v1/chat/completions"})
            return
        try:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if body.get("stream"):
                self.send_json(400, {"error": "This tutorial implements non-streaming only"})
                return
            message = reply(body)
            if self.server.log_path:
                # Only synthetic tutorial content. Never log Authorization headers.
                with self.server.log_path.open("a") as log:
                    log.write(json.dumps({"request": body, "message": message}) + "\n")
            self.send_json(
                200,
                {
                    "id": "tutorial-completion",
                    "object": "chat.completion",
                    "model": body.get("model", "careful"),
                    "choices": [
                        {
                            "index": 0,
                            "message": message,
                            "finish_reason": "tool_calls" if message.get("tool_calls") else "stop",
                        }
                    ],
                    "usage": {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
                },
            )
        except (KeyError, ValueError, TypeError) as exc:
            self.send_json(400, {"error": str(exc)})

    def send_json(self, status: int, value: Any) -> None:
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_: Any) -> None:
        pass


class Server(ThreadingHTTPServer):
    log_path: Path | None = None


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument("--log", type=Path)
    args = parser.parse_args()
    server = Server(("127.0.0.1", args.port), Handler)
    server.log_path = args.log
    print(f"Fake model listening at http://127.0.0.1:{server.server_port}/v1", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()

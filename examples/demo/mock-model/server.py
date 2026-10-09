"""A deterministic stand-in for a model, for the demos and CI.

It speaks the OpenAI chat-completions API (streaming or not) and needs no key.
It plays a fixed script: on a task it recognises (an entry of solutions.json
whose ``match`` occurs in the task text) it makes one tool call, then answers
once the tool has reported back. An entry has either a ``command`` (a shell
call that applies a fix; the same script drives every agent, whatever its tool
names, because it picks the tool that takes a ``command``) or a ``tool`` with
``arguments``, and an optional ``answer``. A task it does not recognise gets an
apology.

    python3 server.py --port 8000 [--solutions solutions.json]
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

SHELL_NAMES = ("execute", "bash", "shell", "run_command", "run_shell")


def text_of(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(p.get("text", "") for p in content if isinstance(p, dict))
    return ""


def shell_tool(tools: list[dict[str, Any]]) -> tuple[str, str] | None:
    """The (tool name, argument name) of a tool that runs a command."""
    found: list[tuple[str, str]] = []
    for t in tools:
        fn = t.get("function", t)
        name = fn.get("name", "")
        props = (fn.get("parameters") or fn.get("input_schema") or {}).get("properties", {})
        for arg in ("command", "cmd"):
            if arg in props:
                found.append((name, arg))
                break
    for want in SHELL_NAMES:
        for name, arg in found:
            if name == want:
                return name, arg
    return found[0] if found else None


class Script:
    def __init__(self, solutions: list[dict[str, str]]) -> None:
        self.solutions = solutions

    def reply(self, body: dict[str, Any]) -> dict[str, Any]:
        """The next assistant message: {"content": str, "tool_calls": [...]}."""
        messages = body.get("messages", [])
        user = "\n".join(text_of(m.get("content")) for m in messages if m.get("role") == "user")
        solution = next((s for s in self.solutions if s["match"] in user), None)
        if any(m.get("role") == "tool" for m in messages):
            return {"content": (solution or {}).get("answer", "I made the change and the command succeeded.")}
        tools = body.get("tools") or []
        call: tuple[str, dict[str, Any]] | None = None
        if solution and "command" in solution:
            shell = shell_tool(tools)
            if shell:
                call = (shell[0], {shell[1]: solution["command"]})
        elif solution and "tool" in solution:
            names = {(t.get("function", t)).get("name") for t in tools}
            if solution["tool"] in names:
                call = (solution["tool"], solution.get("arguments", {}))
        if call:
            return {
                "content": "",
                "tool_calls": [
                    {
                        "id": "call_" + uuid.uuid4().hex[:12],
                        "type": "function",
                        "function": {"name": call[0], "arguments": json.dumps(call[1])},
                    }
                ],
            }
        if solution and "answer" in solution:
            return {"content": solution["answer"]}
        return {"content": "I could not find a way to do that."}


def usage(body: dict[str, Any], reply: dict[str, Any]) -> dict[str, int]:
    prompt = sum(len(text_of(m.get("content"))) for m in body.get("messages", [])) // 4 + 1
    out = (len(reply.get("content", "")) + len(json.dumps(reply.get("tool_calls", [])))) // 4 + 1
    return {"prompt_tokens": prompt, "completion_tokens": out, "total_tokens": prompt + out}


def completion(body: dict[str, Any], reply: dict[str, Any]) -> dict[str, Any]:
    message: dict[str, Any] = {"role": "assistant", "content": reply.get("content") or None}
    if reply.get("tool_calls"):
        message["tool_calls"] = reply["tool_calls"]
    return {
        "id": "chatcmpl-" + uuid.uuid4().hex[:12],
        "object": "chat.completion",
        "created": int(time.time()),
        "model": body.get("model", "mock"),
        "choices": [{"index": 0, "message": message, "finish_reason": "tool_calls" if reply.get("tool_calls") else "stop"}],
        "usage": usage(body, reply),
    }


def chunks(body: dict[str, Any], reply: dict[str, Any]) -> list[dict[str, Any]]:
    base = {"id": "chatcmpl-" + uuid.uuid4().hex[:12], "object": "chat.completion.chunk", "created": int(time.time()), "model": body.get("model", "mock")}
    out = [{**base, "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": None}]}]
    if reply.get("content"):
        out.append({**base, "choices": [{"index": 0, "delta": {"content": reply["content"]}, "finish_reason": None}]})
    for i, tc in enumerate(reply.get("tool_calls", [])):
        delta = {"tool_calls": [{"index": i, "id": tc["id"], "type": "function", "function": tc["function"]}]}
        out.append({**base, "choices": [{"index": 0, "delta": delta, "finish_reason": None}]})
    finish = "tool_calls" if reply.get("tool_calls") else "stop"
    out.append({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": finish}]})
    if (body.get("stream_options") or {}).get("include_usage"):
        out.append({**base, "choices": [], "usage": usage(body, reply)})
    return out


class Handler(BaseHTTPRequestHandler):
    script: Script
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        print(fmt % args, file=sys.stderr, flush=True)

    def _send(self, status: int, payload: dict[str, Any]) -> None:
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:  # noqa: N802
        if self.path in ("/healthz", "/"):
            self._send(200, {"ok": True})
        elif self.path.rstrip("/").endswith("/models"):
            self._send(200, {"object": "list", "data": [{"id": "mock", "object": "model"}]})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        if not self.path.rstrip("/").endswith("/chat/completions"):
            self._send(404, {"error": "only /chat/completions is served"})
            return
        reply = self.script.reply(body)
        if not body.get("stream"):
            self._send(200, completion(body, reply))
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for c in chunks(body, reply):
            self.wfile.write(b"data: " + json.dumps(c).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--solutions", default=str(Path(__file__).with_name("solutions.json")))
    args = p.parse_args()
    path = Path(args.solutions)
    Handler.script = Script(json.loads(path.read_text()) if path.exists() else [])
    print(f"mock model on http://{args.host}:{args.port}/v1", file=sys.stderr, flush=True)
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()

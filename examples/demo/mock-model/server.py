"""A deterministic stand-in for a model, for the demos and CI.

It speaks the OpenAI chat-completions API and the Anthropic Messages API
(streaming or not) and needs no key.
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


def shell_tool(tools: list[dict[str, Any]]) -> tuple[str, str, list[str]] | None:
    """The (tool name, argument name, other required arguments) of a tool that runs a command."""
    found: list[tuple[str, str, list[str]]] = []
    for t in tools:
        fn = t.get("function", t)
        name = fn.get("name", "")
        schema = fn.get("parameters") or fn.get("input_schema") or {}
        props = schema.get("properties", {})
        for arg in ("command", "cmd"):
            if arg in props:
                extra = [r for r in schema.get("required", []) if r != arg]
                found.append((name, arg, extra))
                break
    for want in SHELL_NAMES:
        for name, arg, extra in found:
            if name == want:
                return name, arg, extra
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
                # Other required arguments (a description, say) get a short text.
                call = (shell[0], {**{r: "apply the fix" for r in shell[2]}, shell[1]: solution["command"]})
        elif solution and "tool" in solution:
            # An MCP tool's name carries its server's: mcp__<server>__<tool>.
            names = [(t.get("function", t)).get("name", "") for t in tools]
            name = next((n for n in names if n == solution["tool"] or n.endswith("__" + solution["tool"])), None)
            if name:
                call = (name, solution.get("arguments", {}))
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


def from_anthropic(body: dict[str, Any]) -> dict[str, Any]:
    """An Anthropic Messages request in the chat-completions shape Script reads."""
    messages: list[dict[str, Any]] = []
    for m in body.get("messages", []):
        content = m.get("content")
        if isinstance(content, list) and any(b.get("type") == "tool_result" for b in content if isinstance(b, dict)):
            messages.append({"role": "tool", "content": ""})
        else:
            messages.append({"role": m.get("role"), "content": content})
    return {**body, "messages": messages}


def message(body: dict[str, Any], reply: dict[str, Any]) -> dict[str, Any]:
    content: list[dict[str, Any]] = []
    if reply.get("content"):
        content.append({"type": "text", "text": reply["content"]})
    for tc in reply.get("tool_calls", []):
        content.append({"type": "tool_use", "id": "toolu_" + tc["id"][5:], "name": tc["function"]["name"], "input": json.loads(tc["function"]["arguments"])})
    u = usage(body, reply)
    return {
        "id": "msg_" + uuid.uuid4().hex[:12],
        "type": "message",
        "role": "assistant",
        "model": body.get("model", "mock"),
        "content": content,
        "stop_reason": "tool_use" if reply.get("tool_calls") else "end_turn",
        "stop_sequence": None,
        "usage": {"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"]},
    }


def message_events(msg: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    start = {**msg, "content": [], "stop_reason": None, "usage": {**msg["usage"], "output_tokens": 0}}
    out: list[tuple[str, dict[str, Any]]] = [("message_start", {"type": "message_start", "message": start})]
    for i, block in enumerate(msg["content"]):
        if block["type"] == "text":
            out.append(("content_block_start", {"type": "content_block_start", "index": i, "content_block": {"type": "text", "text": ""}}))
            out.append(("content_block_delta", {"type": "content_block_delta", "index": i, "delta": {"type": "text_delta", "text": block["text"]}}))
        else:
            out.append(("content_block_start", {"type": "content_block_start", "index": i, "content_block": {**block, "input": {}}}))
            out.append(("content_block_delta", {"type": "content_block_delta", "index": i, "delta": {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}}))
        out.append(("content_block_stop", {"type": "content_block_stop", "index": i}))
    out.append(("message_delta", {"type": "message_delta", "delta": {"stop_reason": msg["stop_reason"], "stop_sequence": None}, "usage": {"output_tokens": msg["usage"]["output_tokens"]}}))
    out.append(("message_stop", {"type": "message_stop"}))
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
        path = self.path.split("?")[0].rstrip("/")
        if path.endswith("/messages/count_tokens"):
            self._send(200, {"input_tokens": usage(from_anthropic(body), {})["prompt_tokens"]})
            return
        if path.endswith("/messages"):
            self._anthropic(body)
            return
        if not path.endswith("/chat/completions"):
            self._send(404, {"error": "only /chat/completions and /messages are served"})
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

    def _anthropic(self, body: dict[str, Any]) -> None:
        msg = message(body, self.script.reply(from_anthropic(body)))
        if not body.get("stream"):
            self._send(200, msg)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for event, data in message_events(msg):
            self.wfile.write(f"event: {event}\ndata: {json.dumps(data)}\n\n".encode())
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

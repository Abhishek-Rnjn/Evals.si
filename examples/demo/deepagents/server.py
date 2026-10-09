"""The demo agent behind HTTP, the way a deployed studio workflow is called.

    POST /invoke   {"input": "<question>"}
      ->           {"output": "<final answer>", "messages": [<OpenAI-style messages>],
                    "usage": {"input_tokens": n, "output_tokens": n}}
    GET  /healthz

``messages`` carries the whole run (the user's message, each assistant turn
with its tool calls, each tool result), which an Evals.si HTTP target keeps as
the trajectory (``messages_path: messages``).

    python3 server.py --port 8080

Configuration is by environment: see agent.py. With MLFLOW_TRACKING_URI set,
runs are traced to MLflow; with OTEL_EXPORTER_OTLP_TRACES_ENDPOINT also set
(and MLFLOW_ENABLE_OTLP_EXPORTER=true), the same spans go to an OTLP endpoint
such as Evals.si's.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import agent as demo

os.environ.setdefault("OTEL_SERVICE_NAME", "evalsi-demo-deepagents")

ROLES = {"human": "user", "ai": "assistant", "system": "system", "tool": "tool"}


def to_openai(messages: list[Any]) -> list[dict[str, Any]]:
    """LangChain messages as OpenAI-style chat messages."""
    out: list[dict[str, Any]] = []
    for m in messages:
        content = m.content if isinstance(m.content, str) else json.dumps(m.content)
        msg: dict[str, Any] = {"role": ROLES.get(m.type, m.type), "content": content}
        calls = getattr(m, "tool_calls", None)
        if calls:
            msg["tool_calls"] = [
                {"id": c["id"], "type": "function", "function": {"name": c["name"], "arguments": json.dumps(c["args"])}}
                for c in calls
            ]
        if m.type == "tool":
            msg["tool_call_id"] = m.tool_call_id
            msg["name"] = m.name
        out.append(msg)
    return out


class Service:
    def __init__(self) -> None:
        self.traced = demo.enable_tracing()
        self.agent = demo.build_agent()
        self.lock = threading.Lock()

    def invoke(self, question: str) -> dict[str, Any]:
        result = self.agent.invoke({"messages": [{"role": "user", "content": question}]})
        messages = result["messages"]
        usage = {"input_tokens": 0, "output_tokens": 0}
        for m in messages:
            u = getattr(m, "usage_metadata", None) or {}
            usage["input_tokens"] += int(u.get("input_tokens", 0))
            usage["output_tokens"] += int(u.get("output_tokens", 0))
        final = next((m for m in reversed(messages) if m.type == "ai" and not m.tool_calls), messages[-1])
        if self.traced:
            import mlflow

            mlflow.flush_trace_async_logging()
        text = final.content if isinstance(final.content, str) else json.dumps(final.content)
        return {"output": text, "messages": to_openai(messages), "usage": usage}


def handler(service: Service) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
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
            if self.path == "/healthz":
                self._send(200, {"ok": True})
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self) -> None:  # noqa: N802
            if self.path != "/invoke":
                self._send(404, {"error": "not found"})
                return
            try:
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
                question = body["input"]
                if not isinstance(question, str):
                    raise TypeError("input must be a string")
            except (KeyError, TypeError, ValueError) as exc:
                self._send(400, {"error": f"send {{\"input\": \"<question>\"}}: {exc}"})
                return
            try:
                self._send(200, service.invoke(question))
            except Exception as exc:  # the caller sees the failure, not a hung connection
                self._send(500, {"error": f"{type(exc).__name__}: {exc}"})

    return Handler


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="0.0.0.0")  # noqa: S104
    p.add_argument("--port", type=int, default=8080)
    args = p.parse_args()
    service = Service()
    print(f"deep agent on http://{args.host}:{args.port}/invoke (tracing: {service.traced})", file=sys.stderr, flush=True)
    ThreadingHTTPServer((args.host, args.port), handler(service)).serve_forever()


if __name__ == "__main__":
    main()

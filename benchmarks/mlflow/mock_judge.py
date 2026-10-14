"""A mock OpenAI-compatible judge with fixed latency and per-run call counting.

Both tools call this server, so the benchmark measures each tool's own overhead
and how many judge calls and tokens it spends, not a real model's speed or
opinions. Every verdict is positive.

    python mock_judge.py --port 8767 --latency-ms 50

GET  /stats  -> {"calls": n, "prompt_tokens": n, "completion_tokens": n}
POST /reset  -> zero the counters

Tokens are estimated as characters / 4 of the whole request, because the two
tools send different prompts and that difference is part of the cost.
"""

from __future__ import annotations

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
STATS = {"calls": 0, "prompt_tokens": 0, "completion_tokens": 0}
LATENCY_S = 0.05


def verdict(body: dict) -> str:
    text = json.dumps(body)
    if "JudgeEvaluation" in text:  # MLflow's structured judge
        return json.dumps({"result": "yes", "rationale": "Let's think step by step: it is supported."})
    if "factual claims" in text:  # Evals.si faithfulness
        return json.dumps({"claims": [{"claim": "a", "supported": True}]})
    return json.dumps({"score": 5, "reasoning": "correct"})  # Evals.si rubric judge


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args) -> None:
        pass

    def _send(self, payload: dict) -> None:
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:
        with LOCK:
            self._send(dict(STATS))

    def do_POST(self) -> None:
        length = int(self.headers.get("content-length", 0))
        raw = self.rfile.read(length)
        if self.path == "/reset":
            with LOCK:
                for key in STATS:
                    STATS[key] = 0
            self._send({"ok": True})
            return
        body = json.loads(raw)
        time.sleep(LATENCY_S)
        content = verdict(body)
        prompt_tokens = len(raw) // 4
        completion_tokens = len(content) // 4
        with LOCK:
            STATS["calls"] += 1
            STATS["prompt_tokens"] += prompt_tokens
            STATS["completion_tokens"] += completion_tokens
        self._send(
            {
                "id": "mock",
                "object": "chat.completion",
                "created": int(time.time()),
                "model": "mock",
                "choices": [
                    {"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": content}}
                ],
                "usage": {
                    "prompt_tokens": prompt_tokens,
                    "completion_tokens": completion_tokens,
                    "total_tokens": prompt_tokens + completion_tokens,
                },
            }
        )


def main() -> None:
    global LATENCY_S
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8767)
    parser.add_argument("--latency-ms", type=float, default=50.0)
    args = parser.parse_args()
    LATENCY_S = args.latency_ms / 1000.0
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()

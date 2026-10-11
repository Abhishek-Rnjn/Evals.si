from __future__ import annotations

import json
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import httpx
import pytest

from evalsi.logging import LogError, Logger


def test_process_exit_flushes_queued_records() -> None:
    received: list[dict[str, object]] = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            received.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b"{}")

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        endpoint = f"http://127.0.0.1:{server.server_port}"
        result = subprocess.run(
            [
                sys.executable,
                "-c",
                f"from evalsi import configure_logging, log; "
                f"configure_logging({endpoint!r}, interval=60); "
                "[log('input', 'output') for _ in range(3)]",
            ],
            capture_output=True,
            text=True,
            timeout=15,
            check=False,
        )
        assert result.returncode == 0, result.stderr
        assert len(received) == 1
        resources = received[0]["resourceSpans"]
        assert isinstance(resources, list)
        assert len(resources[0]["scopeSpans"][0]["spans"]) == 3
    finally:
        server.shutdown()
        thread.join()
        server.server_close()


def test_batched_logging_flush_and_metadata() -> None:
    batches: list[list[dict[str, object]]] = []

    def send(request: httpx.Request) -> httpx.Response:
        batches.append(json.loads(request.content)["resourceSpans"][0]["scopeSpans"][0]["spans"])
        return httpx.Response(200, json={})

    with Logger(
        "http://localhost:4318", batch_size=2, transport=httpx.MockTransport(send)
    ) as logger:
        futures = logger.log_batch([{"input": "hi", "output": "bye"} for _ in range(4)])
        logger.flush(timeout=5)
        assert all(len(future.result()) == 32 for future in futures)
    assert len(batches) == 2
    assert [len(batch) for batch in batches] == [2, 2]


def test_inflight_logging_counts_against_memory_and_record_cap() -> None:
    started, release = threading.Event(), threading.Event()

    def send(request: httpx.Request) -> httpx.Response:
        started.set()
        assert release.wait(5)
        return httpx.Response(200, json={})

    logger = Logger(
        "http://localhost", max_records=1, batch_size=1, transport=httpx.MockTransport(send)
    )
    first = logger.log("a", "b")
    assert started.wait(5)
    with pytest.raises(LogError, match="capacity"):
        logger.log("c", "d").result()
    release.set()
    logger.close()
    assert first.result()
    with pytest.raises(LogError, match="closed"):
        logger.log("c", "d").result()
    with (
        Logger("http://localhost", max_bytes=1) as tiny,
        pytest.raises(LogError, match="capacity"),
    ):
        tiny.log("a", "b").result()


@pytest.mark.parametrize(
    "reply",
    [httpx.Response(503), httpx.Response(200, json={"partialSuccess": {"rejectedSpans": "1"}})],
)
def test_logging_errors_are_not_successes(reply: httpx.Response) -> None:
    logger = Logger("http://localhost", transport=httpx.MockTransport(lambda request: reply))
    future = logger.log("a", "b")
    with pytest.raises(LogError):
        future.result(timeout=5)
    with pytest.raises(LogError):
        logger.close()


def test_invalid_trace_id_and_cancelled_future() -> None:
    with Logger(
        "http://localhost",
        interval=10,
        transport=httpx.MockTransport(lambda request: httpx.Response(200, json={})),
    ) as logger:
        with pytest.raises(ValueError, match="trace_id"):
            logger.log("a", "b", "invalid")
        future = logger.log("a", "b")
        assert future.cancel()
    assert future.cancelled()

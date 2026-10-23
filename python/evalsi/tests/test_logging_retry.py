from __future__ import annotations

import time

import httpx
import pytest

from evalsi.logging import LogError, Logger


def test_a_failed_batch_is_retried_with_one_key_and_retry_after() -> None:
    seen: list[tuple[str, bytes]] = []
    replies = [
        httpx.Response(429, headers={"Retry-After": "2"}),
        httpx.Response(503),
        httpx.Response(200, json={}),
    ]

    def send(request: httpx.Request) -> httpx.Response:
        seen.append((request.headers["Idempotency-Key"], request.content))
        return replies.pop(0)

    logger = Logger("http://localhost", retry_backoff=0.01, transport=httpx.MockTransport(send))
    waits: list[float] = []

    def record_wait(timeout: float | None = None) -> bool:
        waits.append(timeout or 0.0)
        return False

    logger._stopping.wait = record_wait  # type: ignore[method-assign]
    assert logger.log("a", "b").result(timeout=5)
    assert len(seen) == 3
    assert len({key for key, _ in seen}) == 1
    assert 1 <= len(seen[0][0]) <= 128
    assert len({body for _, body in seen}) == 1
    assert waits[0] == 2.0  # the server's Retry-After, not the backoff
    assert 0.005 <= waits[1] <= 0.02
    logger.flush(timeout=5)
    # The next batch gets a key of its own.
    replies.append(httpx.Response(200, json={}))
    logger.log("c", "d").result(timeout=5)
    assert seen[3][0] != seen[0][0]
    logger.close()


def test_retries_stop_at_max_attempts_and_skip_client_errors() -> None:
    calls: list[int] = []

    def unavailable(request: httpx.Request) -> httpx.Response:
        calls.append(1)
        return httpx.Response(503)

    logger = Logger(
        "http://localhost",
        max_attempts=3,
        retry_backoff=0,
        transport=httpx.MockTransport(unavailable),
    )
    with pytest.raises(LogError, match="503"):
        logger.log("a", "b").result(timeout=5)
    assert len(calls) == 3
    calls.clear()

    def rejected(request: httpx.Request) -> httpx.Response:
        calls.append(1)
        return httpx.Response(400)

    bad = Logger("http://localhost", retry_backoff=0, transport=httpx.MockTransport(rejected))
    with pytest.raises(LogError, match="400"):
        bad.log("a", "b").result(timeout=5)
    assert len(calls) == 1  # a rejected request is not retried
    for item in (logger, bad):
        with pytest.raises(LogError):
            item.close()


def test_transport_errors_are_retried_and_a_closing_logger_does_not_wait() -> None:
    attempts: list[int] = []

    def flaky(request: httpx.Request) -> httpx.Response:
        attempts.append(1)
        if len(attempts) == 1:
            raise httpx.ConnectError("refused")
        return httpx.Response(200, json={})

    logger = Logger("http://localhost", retry_backoff=0.01, transport=httpx.MockTransport(flaky))
    assert logger.log("a", "b").result(timeout=5)
    logger.close()

    down = Logger(
        "http://localhost",
        retry_backoff=30,
        interval=0.01,
        transport=httpx.MockTransport(lambda request: httpx.Response(503)),
    )
    future = down.log("a", "b")
    started = time.monotonic()
    with pytest.raises(LogError):
        down.close(timeout=10)
    assert time.monotonic() - started < 5
    assert isinstance(future.exception(timeout=1), LogError)

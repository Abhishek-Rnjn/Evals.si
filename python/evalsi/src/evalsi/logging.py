"""Bounded, background OTLP/HTTP logging without an OTel dependency."""

from __future__ import annotations

import atexit
import contextlib
import json
import logging
import math
import os
import random
import secrets
import threading
import time
import weakref
from collections import deque
from collections.abc import Iterable, Mapping
from concurrent.futures import Future
from typing import Any

import httpx

from evalsi.auth import ServerAuth


class LogError(RuntimeError):
    pass


_warnings = logging.getLogger(__name__)

# Statuses worth another try: throttled, or the server or a proxy unavailable.
_RETRYABLE = frozenset({429, 502, 503, 504})


def _finite(value: Any) -> Any:
    """Make non-finite floats text: NaN and Infinity are not valid JSON."""
    if isinstance(value, float) and not math.isfinite(value):
        return "NaN" if math.isnan(value) else ("Infinity" if value > 0 else "-Infinity")
    if isinstance(value, Mapping):
        return {key: _finite(item) for key, item in value.items()}
    if isinstance(value, list | tuple):
        return [_finite(item) for item in value]
    return value


def _json_default(value: Any) -> Any:
    if hasattr(value, "detach"):
        value = value.detach().cpu()
    if hasattr(value, "tolist"):
        return _finite(value.tolist())
    raise TypeError(f"cannot log {type(value).__name__}")


def _dumps(value: Any) -> str:
    return json.dumps(_finite(value), default=_json_default, allow_nan=False)


class Logger:
    def __init__(
        self,
        endpoint: str,
        *,
        service: str = "evalsi-sdk",
        api_key: str | None = None,
        token: str | None = None,
        batch_size: int = 64,
        interval: float = 0.25,
        max_records: int = 1024,
        max_bytes: int = 8 << 20,
        timeout: float = 10.0,
        exit_timeout: float = 30.0,
        max_attempts: int = 4,
        retry_backoff: float = 0.5,
        max_retry_wait: float = 30.0,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        if min(batch_size, max_records, max_bytes, max_attempts) < 1 or (
            min(interval, timeout, exit_timeout, max_retry_wait) <= 0 or retry_backoff < 0
        ):
            raise ValueError("logging limits and timeouts must be positive")
        self.endpoint = endpoint.rstrip("/") + "/v1/traces"
        self.service = service
        self.batch_size = batch_size
        self.interval = interval
        self.max_records = max_records
        self.max_bytes = max_bytes
        self.exit_timeout = exit_timeout
        # A failed batch is sent again up to max_attempts times in all, after
        # retry_backoff * 2**n seconds (with jitter) or the server's
        # Retry-After, at most max_retry_wait. Every try carries the same
        # Idempotency-Key, so a batch the server took but whose answer was lost
        # is not stored twice.
        self.max_attempts = max_attempts
        self.retry_backoff = retry_backoff
        self.max_retry_wait = max_retry_wait
        self._stopping = threading.Event()  # set by close: no more waiting to retry
        self._http = httpx.Client(
            auth=ServerAuth(endpoint, api_key=api_key, token=token),
            timeout=timeout,
            transport=transport,
        )
        self._condition = threading.Condition()
        self._queue: deque[tuple[dict[str, Any], int, Future[str], str]] = deque()
        self._inflight: list[Future[str]] = []
        self._pending = 0
        self._bytes = 0
        self._closed = False
        self._failure: Exception | None = None
        self._thread = threading.Thread(target=self._work, name="evalsi-log", daemon=True)
        self._thread.start()
        atexit.register(self._close_at_exit)
        _live.add(self)

    def log(
        self,
        input: Any,
        output: Any,
        trace_id: str | None = None,
        metadata: Mapping[str, Any] | None = None,
    ) -> Future[str]:
        trace_id = trace_id or secrets.token_hex(16)
        if len(trace_id) != 32 or any(char not in "0123456789abcdef" for char in trace_id):
            raise ValueError("trace_id must be 32 lowercase hexadecimal characters")
        if int(trace_id, 16) == 0:
            raise ValueError("trace_id must not be zero")
        timestamp = str(time.time_ns())
        attributes = []
        for key, value in (
            ("input.value", input),
            ("output.value", output),
            ("evalsi.metadata", dict(metadata or {})),
        ):
            text = value if isinstance(value, str) else _dumps(value)
            attributes.append({"key": key, "value": {"stringValue": text}})
        span = {
            "traceId": trace_id,
            "spanId": secrets.token_hex(8),
            "name": "evalsi.log",
            "kind": 1,
            "startTimeUnixNano": timestamp,
            "endTimeUnixNano": timestamp,
            "attributes": attributes,
        }
        size = len(json.dumps(span, separators=(",", ":")))
        future: Future[str] = Future()
        with self._condition:
            if self._closed:
                future.set_exception(LogError("logger is closed"))
            elif self._pending >= self.max_records or self._bytes + size > self.max_bytes:
                future.set_exception(LogError("logging queue capacity exhausted"))
            else:
                self._pending += 1
                self._bytes += size
                self._queue.append((span, size, future, trace_id))
                self._condition.notify_all()
        return future

    def log_batch(self, records: Iterable[Mapping[str, Any]] | Any) -> list[Future[str]]:
        if hasattr(records, "to_list"):
            records = records.to_list()
        elif hasattr(records, "to_dict"):
            records = records.to_dict(orient="records")
        return [
            self.log(row.get("input"), row.get("output"), row.get("trace_id"), row.get("metadata"))
            for row in records
        ]

    def _work(self) -> None:
        while True:
            with self._condition:
                self._condition.wait_for(lambda: self._queue or self._closed)
                if not self._queue:
                    return
                self._condition.wait_for(
                    lambda: len(self._queue) >= self.batch_size or self._closed,
                    timeout=self.interval,
                )
                batch = [
                    self._queue.popleft() for _ in range(min(self.batch_size, len(self._queue)))
                ]
            active = [item for item in batch if item[2].set_running_or_notify_cancel()]
            failure: Exception | None = None
            if active:
                self._inflight = [item[2] for item in active]
                try:
                    self._send([item[0] for item in active])
                except Exception as error:
                    failure = error
                for _, _, future, trace_id in active:
                    if future.done():  # the logger gave up on it while it was sent
                        continue
                    if failure is None:
                        future.set_result(trace_id)
                    else:
                        future.set_exception(failure)
                self._inflight = []
            with self._condition:
                self._failure = self._failure or failure
                self._pending = max(0, self._pending - len(batch))
                self._bytes = max(0, self._bytes - sum(item[1] for item in batch))
                self._condition.notify_all()

    def _send(self, spans: list[dict[str, Any]]) -> None:
        body = {
            "resourceSpans": [
                {
                    "resource": {
                        "attributes": [
                            {"key": "service.name", "value": {"stringValue": self.service}}
                        ]
                    },
                    "scopeSpans": [{"scope": {"name": "evalsi.sdk"}, "spans": spans}],
                }
            ]
        }
        # One key for every try of this batch (the server takes 1 to 128 visible
        # ASCII characters); the body is the same each time, as it requires.
        headers = {"Idempotency-Key": "evalsi-log-" + secrets.token_hex(16)}
        for attempt in range(1, self.max_attempts + 1):
            wait: float | None = None
            try:
                response = self._http.post(self.endpoint, json=body, headers=headers)
            except httpx.TransportError as error:
                failure = LogError(f"OTLP export failed: {type(error).__name__}")
                wait = self._backoff(attempt, None)
            else:
                if response.status_code == 200:
                    partial = response.json().get("partialSuccess", {})
                    if int(partial.get("rejectedSpans", 0)):
                        raise LogError("OTLP export partially rejected the batch")
                    return
                failure = LogError(f"OTLP export failed with HTTP {response.status_code}")
                if response.status_code in _RETRYABLE:
                    wait = self._backoff(attempt, response.headers.get("Retry-After"))
            # A closing logger does not wait to retry: it must not hold exit.
            if wait is None or attempt == self.max_attempts or self._stopping.is_set():
                raise failure
            if self._stopping.wait(wait):
                raise failure

    def _backoff(self, attempt: int, retry_after: str | None) -> float:
        if retry_after is not None:
            with contextlib.suppress(ValueError):
                return float(min(max(float(retry_after), 0.0), self.max_retry_wait))
        delay = self.retry_backoff * 2 ** (attempt - 1)
        return float(min(delay * (0.5 + random.random() / 2), self.max_retry_wait))

    def flush(self, timeout: float | None = None) -> None:
        with self._condition:
            self._condition.notify_all()
            if not self._condition.wait_for(lambda: self._pending == 0, timeout=timeout):
                raise TimeoutError("logging flush timed out")
            # A failure is reported once, to the flush that sees it. Records that
            # succeed afterwards must not keep failing every later flush and close.
            failure, self._failure = self._failure, None
            if failure is not None:
                raise failure

    def close(self, timeout: float | None = None) -> None:
        """Deliver what is queued, then stop. After ``timeout`` seconds the records
        still queued fail with ``LogError`` instead of keeping the caller waiting."""
        with self._condition:
            if self._closed:
                return
            self._closed = True
            self._condition.notify_all()
        self._stopping.set()
        self._thread.join(timeout)
        atexit.unregister(self._close_at_exit)
        if self._thread.is_alive():
            self._abandon("logger closed before every record was delivered")
            raise LogError("logger closed before every record was delivered")
        self._http.close()
        self.flush()

    def _close_at_exit(self) -> None:
        # A dead endpoint must not hold a finished program for long, and a failed
        # batch has already reached its future.
        try:
            self.close(timeout=self.exit_timeout)
        except Exception as error:
            _warnings.warning("evalsi logging: %s", error)

    def _abandon(self, reason: str) -> None:
        with self._condition:
            while self._queue:
                _, size, future, _ = self._queue.popleft()
                if not future.done():
                    future.set_exception(LogError(reason))
                self._pending = max(0, self._pending - 1)
                self._bytes = max(0, self._bytes - size)
            self._condition.notify_all()

    def _orphan(self) -> None:
        """In a forked child the logging thread does not exist, so nothing queued
        would ever be sent. Fail it, and refuse new records, rather than hang."""
        self._condition = threading.Condition()  # a parent thread may have held the old lock
        self._closed = True
        self._failure = None
        reason = "logger was created before fork; configure logging again in the child"
        for future in [*self._inflight, *(item[2] for item in self._queue)]:
            if not future.done():
                future.set_exception(LogError(reason))
        self._inflight, self._queue = [], deque()
        self._pending, self._bytes = 0, 0

    def __enter__(self) -> Logger:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


_default: dict[str, Logger] = {}
_default_lock = threading.Lock()
_live: weakref.WeakSet[Logger] = weakref.WeakSet()


def _after_fork_in_child() -> None:
    global _default_lock  # noqa: PLW0603
    _default_lock = threading.Lock()  # the parent's may have been held mid-fork
    for logger in list(_live):
        logger._orphan()
    _default.clear()


if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_after_fork_in_child)


def configure_logging(endpoint: str, **options: Any) -> Logger:
    with _default_lock:
        previous = _default.pop("logger", None)
        if previous is not None:
            # Reconfiguring must work even if the old logger had a failed batch.
            with contextlib.suppress(Exception):
                previous.close(timeout=previous.exit_timeout)
        logger = Logger(endpoint, **options)
        _default["logger"] = logger
        return logger


def log(
    input: Any,
    output: Any,
    trace_id: str | None = None,
    metadata: Mapping[str, Any] | None = None,
) -> Future[str]:
    with _default_lock:
        logger = _default.get("logger")
        if logger is None:
            endpoint = os.environ.get("EVALSI_OTLP_ENDPOINT")
            if not endpoint:
                raise ValueError("configure_logging() or EVALSI_OTLP_ENDPOINT is required")
            logger = Logger(endpoint)
            _default["logger"] = logger
        return logger.log(input, output, trace_id, metadata)


def flush(timeout: float | None = None) -> None:
    with _default_lock:
        logger = _default.get("logger")
    if logger is not None:
        logger.flush(timeout)

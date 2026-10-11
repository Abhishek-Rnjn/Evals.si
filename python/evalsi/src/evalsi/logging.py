"""Bounded, background OTLP/HTTP logging without an OTel dependency."""

from __future__ import annotations

import atexit
import json
import os
import secrets
import threading
import time
from collections import deque
from collections.abc import Iterable, Mapping
from concurrent.futures import Future
from typing import Any

import httpx

from evalsi.auth import ServerAuth


class LogError(RuntimeError):
    pass


def _json_default(value: Any) -> Any:
    if hasattr(value, "detach"):
        value = value.detach().cpu()
    if hasattr(value, "tolist"):
        return value.tolist()
    raise TypeError(f"cannot log {type(value).__name__}")


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
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        if min(batch_size, max_records, max_bytes) < 1 or interval <= 0 or timeout <= 0:
            raise ValueError("logging limits and timeouts must be positive")
        self.endpoint = endpoint.rstrip("/") + "/v1/traces"
        self.service = service
        self.batch_size = batch_size
        self.interval = interval
        self.max_records = max_records
        self.max_bytes = max_bytes
        self._http = httpx.Client(
            auth=ServerAuth(endpoint, api_key=api_key, token=token),
            timeout=timeout,
            transport=transport,
        )
        self._condition = threading.Condition()
        self._queue: deque[tuple[bytes, Future[str], str]] = deque()
        self._pending = 0
        self._bytes = 0
        self._closed = False
        self._failure: Exception | None = None
        self._thread = threading.Thread(target=self._work, name="evalsi-log", daemon=True)
        self._thread.start()
        atexit.register(self.close)

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
            text = value if isinstance(value, str) else json.dumps(value, default=_json_default)
            attributes.append({"key": key, "value": {"stringValue": text}})
        span = json.dumps(
            {
                "traceId": trace_id,
                "spanId": secrets.token_hex(8),
                "name": "evalsi.log",
                "kind": 1,
                "startTimeUnixNano": timestamp,
                "endTimeUnixNano": timestamp,
                "attributes": attributes,
            },
            separators=(",", ":"),
        ).encode()
        future: Future[str] = Future()
        with self._condition:
            if self._closed:
                future.set_exception(LogError("logger is closed"))
            elif self._pending >= self.max_records or self._bytes + len(span) > self.max_bytes:
                future.set_exception(LogError("logging queue capacity exhausted"))
            else:
                self._pending += 1
                self._bytes += len(span)
                self._queue.append((span, future, trace_id))
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
            active = [item for item in batch if item[1].set_running_or_notify_cancel()]
            failure: Exception | None = None
            if active:
                try:
                    self._send([json.loads(item[0]) for item in active])
                except Exception as error:
                    failure = error
                for _, future, trace_id in active:
                    if failure is None:
                        future.set_result(trace_id)
                    else:
                        future.set_exception(failure)
            with self._condition:
                self._failure = self._failure or failure
                self._pending -= len(batch)
                self._bytes -= sum(len(item[0]) for item in batch)
                self._condition.notify_all()

    def _send(self, spans: list[dict[str, Any]]) -> None:
        response = self._http.post(
            self.endpoint,
            json={
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
            },
        )
        if response.status_code != 200:
            raise LogError(f"OTLP export failed with HTTP {response.status_code}")
        partial = response.json().get("partialSuccess", {})
        if int(partial.get("rejectedSpans", 0)):
            raise LogError("OTLP export partially rejected the batch")

    def flush(self, timeout: float | None = None) -> None:
        with self._condition:
            self._condition.notify_all()
            if not self._condition.wait_for(lambda: self._pending == 0, timeout=timeout):
                raise TimeoutError("logging flush timed out")
            if self._failure is not None:
                raise self._failure

    def close(self) -> None:
        with self._condition:
            if self._closed:
                return
            self._closed = True
            self._condition.notify_all()
        self._thread.join()
        self._http.close()
        atexit.unregister(self.close)
        self.flush()

    def __enter__(self) -> Logger:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


_default: dict[str, Logger] = {}
_default_lock = threading.Lock()


def configure_logging(endpoint: str, **options: Any) -> Logger:
    with _default_lock:
        previous = _default.pop("logger", None)
        if previous is not None:
            previous.close()
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

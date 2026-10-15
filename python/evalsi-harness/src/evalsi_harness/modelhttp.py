"""HTTP plumbing shared by the model clients: retries that honour
``retry-after``, server-sent events for streaming, and a per-endpoint limit
on concurrent calls."""

from __future__ import annotations

import asyncio
import json
import random
import weakref
from collections.abc import AsyncIterator
from contextlib import nullcontext
from typing import Any, ClassVar

import httpx

from evalsi.targets import RETRY_STATUS

MAX_RETRY_AFTER_S = 60.0
ATTEMPTS = 5


class ModelError(RuntimeError):
    """The model call failed after retries."""


def retry_after(response: httpx.Response) -> float:
    """Seconds the server asked us to wait (``retry-after``), capped; 0 if absent."""
    try:
        return min(max(float(response.headers.get("retry-after", "0")), 0.0), MAX_RETRY_AFTER_S)
    except ValueError:
        return 0.0


class _InFlight:
    """The model calls in flight to one endpoint and key, on one event loop."""

    def __init__(self) -> None:
        self.active = 0
        self.changed = asyncio.Condition()


_CountsByLoop = weakref.WeakKeyDictionary[asyncio.AbstractEventLoop, dict[str, _InFlight]]


class Gate:
    """A cap on concurrent model calls to one endpoint and key.

    Every client in the process that names the same key shares one count of calls
    in flight. A call waits until fewer than *its own* ``limit`` are in flight, so
    a run that asks for a tighter limit than an earlier one is held to it, and a
    looser one is not held back by it: each run's cap is enforced against the
    shared count, and no run gets a gate of its own that the others cannot see."""

    _counts: ClassVar[
        weakref.WeakKeyDictionary[asyncio.AbstractEventLoop, dict[str, _InFlight]]
    ] = weakref.WeakKeyDictionary()

    def __init__(self, key: str, limit: int) -> None:
        self.key = key
        self.limit = limit

    def _flight(self) -> _InFlight:
        # Keyed by the loop itself, so a finished loop takes its counts with it.
        per_loop = self._counts.setdefault(asyncio.get_running_loop(), {})
        return per_loop.setdefault(self.key, _InFlight())

    async def __aenter__(self) -> None:
        flight = self._flight()
        async with flight.changed:
            await flight.changed.wait_for(lambda: flight.active < self.limit)
            flight.active += 1

    async def __aexit__(self, *exc: object) -> None:
        flight = self._flight()
        async with flight.changed:
            flight.active -= 1
            flight.changed.notify_all()


def gate_for(url: str, api_key_env: str, limit: int) -> Gate | Any:
    """A gate for a target's ``max_concurrency``; a no-op when it is 0."""
    return Gate(f"{url}|{api_key_env}", limit) if limit > 0 else nullcontext()


async def post_json(
    client: httpx.AsyncClient,
    url: str,
    body: dict[str, Any],
    headers: dict[str, str],
    gate: Any = None,
) -> dict[str, Any]:
    gate = gate if gate is not None else nullcontext()
    delay = 1.0
    for attempt in range(1, ATTEMPTS + 1):
        wait = delay
        try:
            async with gate:
                response = await client.post(url, json=body, headers=headers)
        except httpx.TransportError as exc:
            if attempt == ATTEMPTS:
                raise ModelError(f"model request failed: {exc}") from exc
        else:
            if response.status_code < 400:
                payload: dict[str, Any] = response.json()
                return payload
            if response.status_code not in RETRY_STATUS or attempt == ATTEMPTS:
                raise ModelError(
                    f"model returned HTTP {response.status_code}: {response.text[:400]}"
                )
            wait = max(delay, retry_after(response))
        await asyncio.sleep(wait + random.uniform(0, wait / 4))
        delay *= 2
    raise AssertionError("unreachable")


async def _sse(response: httpx.Response) -> AsyncIterator[tuple[str, dict[str, Any]]]:
    event = ""
    data: list[str] = []
    async for line in response.aiter_lines():
        if line == "":
            if data:
                text = "\n".join(data)
                if text.strip() == "[DONE]":
                    return
                try:
                    payload = json.loads(text)
                except json.JSONDecodeError as exc:
                    raise ModelError(f"the stream sent invalid JSON: {text[:200]!r}") from exc
                yield event or str(payload.get("type", "")), payload
            event, data = "", []
        elif line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            data.append(line[5:].removeprefix(" "))
    if data:  # a final event the server did not terminate with a blank line
        text = "\n".join(data)
        if text.strip() != "[DONE]":
            yield event or "", json.loads(text)


async def stream_events(
    client: httpx.AsyncClient,
    url: str,
    body: dict[str, Any],
    headers: dict[str, str],
    gate: Any = None,
) -> AsyncIterator[tuple[str, dict[str, Any]]]:
    """Server-sent events of a streamed call. Opening the stream is retried
    like ``post_json``; a failure after events arrived is not, since a retry
    would repeat the output."""
    gate = gate if gate is not None else nullcontext()
    delay = 1.0
    for attempt in range(1, ATTEMPTS + 1):
        wait = delay
        started = False
        try:
            async with gate, client.stream("POST", url, json=body, headers=headers) as response:
                if response.status_code < 400:
                    async for event in _sse(response):
                        started = True
                        yield event
                    return
                text = (await response.aread()).decode(errors="replace")
                if response.status_code not in RETRY_STATUS or attempt == ATTEMPTS:
                    raise ModelError(f"model returned HTTP {response.status_code}: {text[:400]}")
                wait = max(delay, retry_after(response))
        except httpx.TransportError as exc:
            if started or attempt == ATTEMPTS:
                raise ModelError(f"model request failed: {exc}") from exc
        await asyncio.sleep(wait + random.uniform(0, wait / 4))
        delay *= 2

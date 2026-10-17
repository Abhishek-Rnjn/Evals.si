"""Probe warnings must reach callers when a declared network boundary leaks."""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock

from evalsi.sandbox.client import SandboxClient
from evalsi.sandbox.v1alpha1 import sandbox_pb2 as pb


def test_probe_retains_a_warning_on_an_available_backend() -> None:
    async def check() -> None:
        async with SandboxClient("127.0.0.1:1") as client:
            client.stub = AsyncMock()
            client.stub.Probe.return_value = pb.ProbeResponse(
                rungs=[pb.RungStatus(driver="pod", available=True, warning="canary connected")]
            )
            [rung] = await client.probe()
            assert rung["available"] is True
            assert rung["reason"] == ""
            assert rung["warning"] == "canary connected"

    asyncio.run(check())

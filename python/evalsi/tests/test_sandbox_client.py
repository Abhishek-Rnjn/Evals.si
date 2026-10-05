"""SandboxService client against a real ``evalsid sandbox serve``.

Set EVALSI_TEST_EVALSID to a built evalsid (CI's e2e job does) to run these.
"""

from __future__ import annotations

import asyncio
import os

import pytest

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import SandboxSpec, SandboxUnavailable, connect

pytestmark = pytest.mark.skipif(
    not os.environ.get("EVALSI_TEST_EVALSID"), reason="needs a built evalsid"
)


@pytest.fixture(autouse=True)
def _evalsid(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("EVALSID", os.environ.get("EVALSI_TEST_EVALSID", ""))
    monkeypatch.delenv("EVALSI_SANDBOX_ADDR", raising=False)


def test_a_sandbox_persists_across_commands() -> None:
    async def scenario() -> None:
        async with await connect() as client:
            rungs = await client.probe()
            assert any(r["available"] for r in rungs)
            async with await client.create(SandboxSpec(files={"seed.txt": "seed"})) as sbx:
                assert sbx.isolation.level in ("namespaced", "confined")
                seen: list[str] = []
                first = await sbx.exec(
                    ["sh", "-c", "cat seed.txt; echo more >> seed.txt; echo err >&2; exit 4"],
                    on_output=lambda stream, text: seen.append(stream),
                )
                assert first.exit_code == 4
                assert first.stdout == "seed"
                assert first.stderr == "err\n"
                assert "stdout" in seen
                await sbx.write_files({"dir/a.txt": "a"})
                second = await sbx.exec(["sh", "-c", "cat seed.txt dir/a.txt"])
                assert second.ok
                assert second.stdout == "seedmore\na"
                files = await sbx.read_files(["dir", "missing"])
                assert files == {"dir/a.txt": b"a"}
                snap = await sbx.snapshot()
                clone = await client.restore(snap)
                assert (await clone.exec(["cat", "dir/a.txt"])).stdout == "a"
                await clone.destroy()
                await client.delete_snapshot(snap)
                timed = await sbx.exec(["sleep", "5"], timeout_s=0.5)
                assert timed.outcome == "timeout"

    asyncio.run(scenario())


def test_it_fails_closed() -> None:
    async def scenario() -> None:
        async with await connect() as client:
            with pytest.raises(SandboxUnavailable):
                await client.create(SandboxSpec(min_isolation="vm"))
            with pytest.raises(SandboxError):
                await client.create(SandboxSpec(network="allowlist"))

    asyncio.run(scenario())

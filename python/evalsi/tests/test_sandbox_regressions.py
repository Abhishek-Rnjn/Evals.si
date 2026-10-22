"""Sandbox client behaviour that needs no evalsid daemon."""

from __future__ import annotations

import asyncio
import os
import stat
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from evalsi.sandbox import run_sandboxed
from evalsi.sandbox.client import Sandbox
from evalsi.sandbox.v1alpha1 import sandbox_pb2 as pb


@pytest.mark.skipif(os.name != "posix", reason="needs a shell script as evalsid")
def test_cancelling_a_local_run_kills_the_evalsid_subprocess(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pidfile = tmp_path / "pid"
    fake = tmp_path / "evalsid"
    fake.write_text(f"#!/bin/sh\necho $$ > {pidfile}\nexec sleep 60\n")
    fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("EVALSID", str(fake))
    monkeypatch.delenv("EVALSI_SANDBOX_ADDR", raising=False)

    async def scenario() -> int:
        task = asyncio.create_task(run_sandboxed(["true"]))
        for _ in range(100):
            if pidfile.exists() and pidfile.read_text().strip():
                break
            await asyncio.sleep(0.05)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        return int(pidfile.read_text())

    pid = asyncio.run(scenario())
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return
    os.kill(pid, 9)
    pytest.fail("evalsid sandbox run outlived the cancelled task")


def test_read_files_surfaces_missing_and_truncated() -> None:
    class Stub:
        async def ReadFiles(self, request: Any) -> Any:
            return pb.ReadFilesResponse(
                files=[pb.File(path="a", content=b"x")], missing=["link"], truncated=True
            )

    sandbox = Sandbox(SimpleNamespace(stub=Stub()), "sbx", None)  # type: ignore[arg-type]
    files = asyncio.run(sandbox.read_files(["a", "link"]))
    assert files == {"a": b"x"}
    assert files.missing == ["link"]
    assert files.truncated is True

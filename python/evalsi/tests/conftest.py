from __future__ import annotations

import contextlib
import os
import shutil
import stat
import sys
import tempfile
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

from evalsi import Content, Record, Usage


@contextlib.contextmanager
def short_socket_dir() -> Iterator[Path]:
    """A short directory for Unix sockets: pytest's tmp_path can exceed the
    104-byte socket path limit on macOS."""
    path = Path(tempfile.mkdtemp(prefix="evalsi-", dir="/tmp" if os.path.isdir("/tmp") else None))
    try:
        yield path
    finally:
        shutil.rmtree(path, ignore_errors=True)


def make_record(
    output: Any = "answer",
    reference: Any = None,
    *,
    id: str = "r1",
    input: Any = "question",
    context: list[Any] | None = None,
    usage: Usage | None = None,
    metadata: dict[str, Any] | None = None,
) -> Record:
    return Record(
        id=id,
        input=None if input is None else Content.from_value(input),
        output=None if output is None else Content.from_value(output),
        reference=None if reference is None else Content.from_value(reference),
        context=[Content.from_value(c) for c in context or []],
        usage=usage,
        metadata=metadata or {},
    )


@pytest.fixture(autouse=True)
def _isolated_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep tests away from the developer's judge config and cache."""
    for var in (
        "EVALSI_JUDGE_PROVIDER",
        "EVALSI_JUDGE_MODEL",
        "EVALSI_JUDGE_BASE_URL",
        "EVALSI_JUDGE_API_KEY_ENV",
        "EVALSI_JUDGE_EFFORT",
    ):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("EVALSI_CACHE_DIR", str(tmp_path / "cache"))
    # No Wasm plugins from the developer's ~/.evalsi/plugins.
    monkeypatch.setenv("EVALSI_PLUGIN_PATH", "")


# A stand-in for `evalsid sandbox run` that runs the request unconfined in a
# temporary directory. It tests the pack's logic; the real sandbox is tested
# in Go (internal/sandbox) and end to end (tests/e2e).
FAKE_EVALSID = """\
import json, os, subprocess, sys, tempfile
req = json.load(sys.stdin)
if os.environ.get("FAKE_SANDBOX") == "unavailable":
    print(json.dumps({"outcome": "unavailable", "exit_code": -1, "error": "no rung"}))
    sys.exit(0)
with tempfile.TemporaryDirectory() as ws:
    for name, content in req["files"].items():
        open(os.path.join(ws, name), "w").write(content)
    cmd = [sys.executable, *req["command"][1:]]
    try:
        p = subprocess.run(cmd, cwd=ws, capture_output=True, text=True, timeout=req["timeout_s"],
                           input=req.get("stdin", ""))
        out = {"outcome": "exit", "exit_code": p.returncode, "stdout": p.stdout, "stderr": p.stderr}
    except subprocess.TimeoutExpired:
        out = {"outcome": "timeout", "exit_code": -1}
out["isolation"] = {"driver": "fake", "level": "none", "enforcement": "full"}
print(json.dumps(out))
"""


@pytest.fixture
def fake_sandbox(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    script = tmp_path / "evalsid"
    script.write_text(f"#!{sys.executable}\n{FAKE_EVALSID}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("EVALSID", str(script))
    return script

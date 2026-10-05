"""Run untrusted code through the Evals.si sandbox.

The sandbox lives in the Go daemon: ``evalsid sandbox run`` takes a JSON
request on stdin, runs it under the strongest isolation rung available
(bubblewrap, then Landlock) and prints the result. Workers started by evalsid
find it through ``EVALSID`` and ``EVALSI_SANDBOX``; elsewhere ``evalsid`` must
be on PATH. There is no unconfined fallback: without a sandbox, code
evaluators fail with ``SandboxError``, an infrastructure error that is never
scored as the model's failure.
"""

from __future__ import annotations

import asyncio
import json
import os
import shutil
from dataclasses import dataclass, field
from typing import Any


class SandboxError(RuntimeError):
    """The sandbox could not run the command (unavailable, or the runner broke)."""


@dataclass
class SandboxResult:
    # exit, denied or timeout; the other outcomes raise SandboxError.
    outcome: str
    exit_code: int
    stdout: str = ""
    stderr: str = ""
    truncated: bool = False
    duration_ms: float = 0.0
    isolation: dict[str, Any] = field(default_factory=dict)
    denials: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.outcome == "exit" and self.exit_code == 0


def sandbox_binary() -> str:
    binary = os.environ.get("EVALSID") or shutil.which("evalsid")
    if not binary:
        raise SandboxError(
            "no sandbox: code evaluators run commands through `evalsid sandbox run`, and "
            "evalsid is not on PATH (set EVALSID, or run under `evalsi serve`)"
        )
    return binary


async def run_sandboxed(
    command: list[str],
    *,
    files: dict[str, str] | None = None,
    stdin: str = "",
    env: dict[str, str] | None = None,
    timeout_s: float = 30.0,
    memory_mb: int = 1024,
    network: str = "deny",
    mode: str = "workspace-write",
    min_isolation: str = "confined",
) -> SandboxResult:
    request = {
        "command": command,
        "files": files or {},
        "stdin": stdin,
        "env": env or {},
        "timeout_s": timeout_s,
        "memory_mb": memory_mb,
        "network": network,
        "mode": mode,
        "min_isolation": min_isolation,
    }
    proc = await asyncio.create_subprocess_exec(
        sandbox_binary(),
        "sandbox",
        "run",
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    out, err = await proc.communicate(json.dumps(request).encode())
    if proc.returncode != 0:
        raise SandboxError(f"evalsid sandbox run failed: {err.decode(errors='replace').strip()}")
    try:
        data = json.loads(out)
    except json.JSONDecodeError as exc:
        raise SandboxError(f"evalsid sandbox run printed no result: {out[:200]!r}") from exc
    if data.get("outcome") in ("unavailable", "runner_failure"):
        raise SandboxError(f"sandbox {data['outcome']}: {data.get('error', '')}")
    return SandboxResult(
        outcome=data["outcome"],
        exit_code=int(data.get("exit_code", -1)),
        stdout=data.get("stdout", ""),
        stderr=data.get("stderr", ""),
        truncated=bool(data.get("truncated")),
        duration_ms=float(data.get("duration_ms", 0.0)),
        isolation=data.get("isolation") or {},
        denials=list(data.get("denials") or []),
    )

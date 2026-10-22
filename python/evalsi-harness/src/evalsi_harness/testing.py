"""Test helpers: a scripted chat-model server and an unconfined sandbox.

``ScriptedModelServer`` speaks chat completions (``/v1/chat/completions``)
and the Anthropic Messages API (``/v1/messages``), answering from a function
of the conversation, so agent loops run offline and deterministically.

``LocalSandboxClient`` runs commands in a temporary directory with no
isolation at all. It exists for tests of harness logic only; real runs
always use the Evals.si sandbox, which fails closed.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import shutil
import tempfile
import threading
from collections.abc import Callable, Mapping, Sequence
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import (
    EgressEvent,
    ExecResult,
    Isolation,
    ProcessInfo,
    ProcessOutput,
    SandboxSpec,
    SnapshotsUnsupported,
)

# (messages in chat-completions form, tool names)
#   -> {"text": str, "tool_calls": [(name, args_dict), ...]}
Script = Callable[[list[dict[str, Any]], list[str]], dict[str, Any]]


def _to_openai_messages(body: Mapping[str, Any]) -> list[dict[str, Any]]:
    """Anthropic messages in chat-completions form, so one script serves both."""
    out: list[dict[str, Any]] = []
    for msg in body.get("messages", []):
        content = msg["content"]
        if isinstance(content, str):
            out.append({"role": msg["role"], "content": content})
            continue
        texts = [b["text"] for b in content if b.get("type") == "text"]
        calls = [
            {
                "id": b["id"],
                "type": "function",
                "function": {"name": b["name"], "arguments": json.dumps(b["input"])},
            }
            for b in content
            if b.get("type") == "tool_use"
        ]
        results = [b for b in content if b.get("type") == "tool_result"]
        if results:
            for r in results:
                out.append(
                    {
                        "role": "tool",
                        "tool_call_id": r["tool_use_id"],
                        "content": r.get("content", ""),
                    }
                )
        else:
            m: dict[str, Any] = {"role": msg["role"], "content": "".join(texts)}
            if calls:
                m["tool_calls"] = calls
            out.append(m)
    return out


class ScriptedModelServer:
    def __init__(self, script: Script) -> None:
        self.script = script
        self.requests: list[dict[str, Any]] = []
        # Report prompt tokens from the size of the request (4 characters a
        # token) instead of a fixed 100, so context management has something real
        # to count.
        self.sized_usage = False
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                body = json.loads(raw)
                outer.requests.append(body)
                prompt_tokens = len(raw) // 4 if outer.sized_usage else 100
                anthropic = self.path.endswith("/messages")
                if anthropic:
                    messages = _to_openai_messages(body)
                    tools = [t["name"] for t in body.get("tools", [])]
                else:
                    messages = body["messages"]
                    tools = [t["function"]["name"] for t in body.get("tools", [])]
                reply = outer.script(messages, tools)
                if reply.get("status"):  # a scripted provider failure
                    data = json.dumps({"error": {"message": "scripted failure"}}).encode()
                    self.send_response(reply["status"])
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)
                    return
                calls = reply.get("tool_calls") or []
                n = len(outer.requests)
                if anthropic:
                    blocks: list[dict[str, Any]] = []
                    if reply.get("text"):
                        blocks.append({"type": "text", "text": reply["text"]})
                    for i, (name, args) in enumerate(calls):
                        blocks.append(
                            {
                                "type": "tool_use",
                                "id": f"toolu_{n}_{i}",
                                "name": name,
                                "input": args,
                            }
                        )
                    payload: dict[str, Any] = {
                        "content": blocks,
                        "stop_reason": reply.get("stop") or ("tool_use" if calls else "end_turn"),
                        "usage": {"input_tokens": prompt_tokens, "output_tokens": 20},
                    }
                else:
                    message: dict[str, Any] = {
                        "role": "assistant",
                        "content": reply.get("text", ""),
                    }
                    if calls:
                        message["tool_calls"] = [
                            {
                                "id": f"call_{n}_{i}",
                                "type": "function",
                                "function": {"name": name, "arguments": json.dumps(args)},
                            }
                            for i, (name, args) in enumerate(calls)
                        ]
                    payload = {
                        "choices": [
                            {
                                "message": message,
                                "finish_reason": reply.get("stop")
                                or ("tool_calls" if calls else "stop"),
                            }
                        ],
                        "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": 20},
                    }
                data = json.dumps(payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}/v1"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    def __enter__(self) -> ScriptedModelServer:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


class _LocalProcess:
    """A background process of the stand-in: output into unbounded buffers."""

    def __init__(self, pid: str, command: Sequence[str], proc: asyncio.subprocess.Process) -> None:
        self.id = pid
        self.command = list(command)
        self.proc = proc
        self.out = bytearray()
        self.err = bytearray()
        self.killed = False
        self.changed = asyncio.Event()
        self.readers: list[asyncio.Task[None]] = []

    def info(self) -> ProcessInfo:
        code = self.proc.returncode
        return ProcessInfo(
            id=self.id,
            command=self.command,
            running=code is None,
            killed=self.killed,
            exit_code=code,
            outcome="exit" if code is not None else "",
            stdout_bytes=len(self.out),
            stderr_bytes=len(self.err),
        )


class LocalSandbox:
    """Unconfined stand-in for a sandbox: a temporary directory. Tests only."""

    def __init__(self, root: Path, spec: SandboxSpec, *, snapshots: bool = True) -> None:
        self.root = root
        self.spec = spec
        self.snapshots = snapshots
        self.id = f"local-{root.name}"
        self.isolation = Isolation(
            driver="local-test",
            level="none",
            enforcement="partial",
            notes=["UNCONFINED: tests only"],
        )
        self.image_digest = ""
        self._procs: dict[str, _LocalProcess] = {}

    async def start_process(
        self,
        command: Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
        buffer_bytes: int = 0,
        stdin: bool = False,
    ) -> ProcessInfo:
        proc = await asyncio.create_subprocess_exec(
            *command,
            cwd=self.root / cwd if cwd else self.root,
            stdin=asyncio.subprocess.PIPE if stdin else asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env={
                "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                "HOME": str(self.root),
                **self.spec.env,
                **(env or {}),
            },
            start_new_session=True,
        )
        lp = _LocalProcess(f"proc_{len(self._procs) + 1}", command, proc)
        self._procs[lp.id] = lp

        async def pump(stream: asyncio.StreamReader | None, sink: bytearray) -> None:
            while stream is not None and (chunk := await stream.read(4096)):
                sink += chunk
                lp.changed.set()

        pumps = [
            asyncio.ensure_future(pump(proc.stdout, lp.out)),
            asyncio.ensure_future(pump(proc.stderr, lp.err)),
        ]

        async def reap() -> None:
            await proc.wait()
            await asyncio.gather(*pumps, return_exceptions=True)
            lp.changed.set()

        lp.readers = [*pumps, asyncio.ensure_future(reap())]
        return lp.info()

    def _proc(self, pid: str) -> _LocalProcess:
        try:
            return self._procs[pid]
        except KeyError:
            raise SandboxError(f"sandbox service: NOT_FOUND: process {pid!r}") from None

    async def list_processes(self) -> list[ProcessInfo]:
        return [p.info() for p in self._procs.values()]

    async def read_process(
        self,
        process_id: str,
        *,
        stdout_offset: int = 0,
        stderr_offset: int = 0,
        max_bytes: int = 0,
        wait_s: float = 0.0,
    ) -> ProcessOutput:
        p = self._proc(process_id)
        if wait_s > 0 and len(p.out) <= stdout_offset and len(p.err) <= stderr_offset:
            p.changed.clear()
            if p.proc.returncode is None:
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(p.changed.wait(), wait_s)
        limit = max_bytes or 1 << 20
        out = bytes(p.out[stdout_offset : stdout_offset + limit])
        err = bytes(p.err[stderr_offset : stderr_offset + limit])
        return ProcessOutput(
            stdout=out.decode(errors="replace"),
            stderr=err.decode(errors="replace"),
            stdout_offset=stdout_offset + len(out),
            stderr_offset=stderr_offset + len(err),
            process=p.info(),
        )

    async def write_process(
        self, process_id: str, data: bytes | str = b"", *, close: bool = False
    ) -> None:
        p = self._proc(process_id)
        if p.proc.stdin is None or p.proc.returncode is not None:
            raise SandboxError(f"sandbox service: process {process_id} has no open stdin")
        if data:
            p.proc.stdin.write(data.encode() if isinstance(data, str) else data)
            await p.proc.stdin.drain()
        if close:
            p.proc.stdin.close()

    async def kill_process(self, process_id: str) -> ProcessInfo:
        p = self._proc(process_id)
        if p.proc.returncode is None:
            p.killed = True
            with contextlib.suppress(ProcessLookupError):
                os.killpg(p.proc.pid, 9)
            await p.proc.wait()
        return p.info()

    async def services(self, *, include_logs: bool = False) -> list[dict[str, Any]]:
        return []

    async def exec(
        self,
        command: Sequence[str],
        *,
        stdin: bytes | str = b"",
        env: Mapping[str, str] | None = None,
        timeout_s: float = 60.0,
        cwd: str = "",
        output_limit: int = 0,
        on_output: Any = None,
    ) -> ExecResult:
        workdir = self.root / cwd if cwd else self.root
        proc = await asyncio.create_subprocess_exec(
            *command,
            cwd=workdir,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env={
                "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                "HOME": str(self.root),
                **self.spec.env,
                **(env or {}),
            },
        )
        try:
            out, err = await asyncio.wait_for(
                proc.communicate(stdin.encode() if isinstance(stdin, str) else stdin), timeout_s
            )
        except TimeoutError:
            proc.kill()
            await proc.wait()
            return ExecResult(outcome="timeout", exit_code=-1)
        return ExecResult(
            outcome="exit",
            exit_code=proc.returncode or 0,
            stdout=out.decode(errors="replace"),
            stderr=err.decode(errors="replace"),
        )

    async def write_files(self, files: Mapping[str, bytes | str], mode: int = 0) -> None:
        for name, content in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content.encode() if isinstance(content, str) else content)
            if mode:
                path.chmod(mode)

    async def read_files(self, paths: Sequence[str], max_bytes: int = 0) -> dict[str, bytes]:
        out: dict[str, bytes] = {}
        for name in paths:
            path = self.root / name
            if path.is_file():
                out[name] = path.read_bytes()
            elif path.is_dir():
                for f in sorted(path.rglob("*")):
                    if f.is_file():
                        out[str(Path(name) / f.relative_to(path))] = f.read_bytes()
        return out

    async def snapshot(self) -> str:
        if not self.snapshots:
            raise SnapshotsUnsupported(
                "sandbox service: this sandbox rung does not support snapshots"
            )
        snap = Path(tempfile.mkdtemp(prefix="evalsi-localsnap-"))
        shutil.copytree(self.root, snap / "state", symlinks=True, copy_function=_copy_if_present)
        return str(snap)

    async def egress(self) -> list[EgressEvent]:
        return []

    async def destroy(self) -> None:
        for p in self._procs.values():
            if p.proc.returncode is None:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(p.proc.pid, 9)
                await p.proc.wait()
        shutil.rmtree(self.root, ignore_errors=True)


def _copy_if_present(src: str, dst: str) -> None:
    """Copy a file unless it vanished mid-snapshot. Background processes a
    command left behind (git's auto-maintenance, for one) keep running here,
    where the real sandbox would have killed them, and come and go with lock
    files."""
    with contextlib.suppress(FileNotFoundError):
        shutil.copy2(src, dst)


class LocalSandboxClient:
    """Creates LocalSandboxes. Records every spec it was asked for. Tests only."""

    def __init__(self, *, snapshots: bool = True) -> None:
        self.created: list[SandboxSpec] = []
        self.restored: list[tuple[str, str]] = []
        # False: behave like a rung that cannot snapshot (the pod rung).
        self.snapshots = snapshots

    async def create(self, spec: SandboxSpec | None = None) -> LocalSandbox:
        spec = spec or SandboxSpec()
        if spec.services:
            raise SandboxError("services require the pod backend")
        self.created.append(spec)
        root = Path(tempfile.mkdtemp(prefix="evalsi-local-"))
        sandbox = LocalSandbox(root, spec, snapshots=self.snapshots)
        await sandbox.write_files(spec.files)
        return sandbox

    async def restore(
        self, snapshot_id: str, *, network: str = "", allow_hosts: Sequence[str] = ()
    ) -> LocalSandbox:
        self.restored.append((snapshot_id, network))
        root = Path(tempfile.mkdtemp(prefix="evalsi-local-"))
        shutil.rmtree(root)
        shutil.copytree(Path(snapshot_id) / "state", root, symlinks=True)
        return LocalSandbox(
            root, SandboxSpec(network=network or "deny", allow_hosts=list(allow_hosts))
        )

    async def delete_snapshot(self, snapshot_id: str) -> None:
        shutil.rmtree(snapshot_id, ignore_errors=True)

    async def aclose(self) -> None:
        return None

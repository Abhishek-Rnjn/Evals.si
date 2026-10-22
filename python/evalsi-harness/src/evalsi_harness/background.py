"""Background processes for the built-in agent: a server or watcher started in
one call is still running in the next.

``bash`` runs each command to completion. With ``background`` listed under
``harness.builtin.tools.builtin``, the agent also gets five tools over the
sandbox's process API (``evalsid`` keeps a bounded output buffer per process,
under the sandbox's own confinement and resource limits, and kills them all
when the sandbox is destroyed):

- ``process_start``: start a command and return its id at once;
- ``process_output``: new output since the last read, optionally waiting for it;
- ``process_write``: write to its stdin, or close it;
- ``process_list``: what is running;
- ``process_kill``.

``shell`` (a separate name) is one persistent ``sh`` fed commands one by one: the
way to start a server and then use it, since the sandbox may give each process
its own network namespace.

The tools keep each process's read offsets, so "call ``process_output`` again"
always returns what is new. The mutating three re-run on replay, as ``bash``
does, so a replayed trial starts the same servers.
"""

from __future__ import annotations

import re
import secrets
import time
from collections.abc import Awaitable
from typing import Any

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import Tool, ToolResult, _clip

NAMES = ("process_start", "process_output", "process_write", "process_list", "process_kill")
MUTATING = frozenset({"process_start", "process_write", "process_kill", "shell"})
MAX_WAIT_S = 30.0
# What one process_output call returns, per stream; the rest stays buffered.
READ_BYTES = 12_000


def _text(args: dict[str, Any], key: str) -> str | None:
    value = args.get(key)
    return value if isinstance(value, str) else None


class _Processes:
    """The offsets the tools have read each process up to."""

    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.offsets: dict[str, tuple[int, int]] = {}

    @property
    def sandbox(self) -> Any:
        return self.env.sandbox


def _state(info: Any) -> str:
    if info.running:
        return "running"
    if info.killed:
        return "killed"
    return f"exited with code {info.exit_code}" if info.exit_code is not None else "ended"


class _Base:
    def __init__(self, procs: _Processes) -> None:
        self.procs = procs

    async def _guard(self, work: Awaitable[ToolResult]) -> ToolResult:
        try:
            return await work
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)


class ProcessStartTool(_Base):
    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="process_start",
            description=(
                "Start a shell command in the background in the task's sandbox and return its "
                "id at once. It keeps running after this call, so use it for a server, a watcher "
                "or a long build; read its output with process_output. It is under the same "
                "limits as bash, and ends when the task does."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "The shell command to run."},
                    "stdin": {
                        "type": "boolean",
                        "description": "Keep its stdin open for process_write (default false).",
                    },
                    "timeout_s": {
                        "type": "number",
                        "description": "Seconds before it is killed (default 3600).",
                    },
                },
                "required": ["command"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        command = _text(args, "command")
        if not command or not command.strip():
            return ToolResult("process_start needs a 'command' string", is_error=True)
        timeout = args.get("timeout_s")
        return await self._guard(self._start(command, args.get("stdin") is True, timeout))

    async def _start(self, command: str, stdin: bool, timeout: Any) -> ToolResult:
        env = self.procs.env
        info = await self.procs.sandbox.start_process(
            ["sh", "-c", command],
            env=env.config.env,
            stdin=stdin,
            timeout_s=min(float(timeout), 86_400.0) if isinstance(timeout, int | float) else 0.0,
        )
        self.procs.offsets[info.id] = (0, 0)
        return ToolResult(
            f"started {info.id} in the background: {command}",
            metadata={"process_id": info.id},
        )


class ProcessOutputTool(_Base):
    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="process_output",
            description=(
                "Read what a background process has written since the last read (stdout, then "
                "stderr) and whether it is still running. With wait_s, waits up to that many "
                f"seconds (at most {MAX_WAIT_S:g}) for new output or for it to end."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "id": {"type": "string", "description": "The id process_start returned."},
                    "wait_s": {"type": "number"},
                },
                "required": ["id"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        pid = _text(args, "id")
        if not pid:
            return ToolResult("process_output needs an 'id'", is_error=True)
        wait = args.get("wait_s")
        wait_s = min(max(float(wait), 0.0), MAX_WAIT_S) if isinstance(wait, int | float) else 0.0
        return await self._guard(self._read(pid, wait_s))

    async def _read(self, pid: str, wait_s: float) -> ToolResult:
        so, se = self.procs.offsets.get(pid, (0, 0))
        out = await self.procs.sandbox.read_process(
            pid, stdout_offset=so, stderr_offset=se, max_bytes=READ_BYTES, wait_s=wait_s
        )
        self.procs.offsets[pid] = (out.stdout_offset, out.stderr_offset)
        parts = []
        if out.stdout_dropped or out.stderr_dropped:
            parts.append(
                f"[{out.stdout_dropped + out.stderr_dropped} bytes of older output were dropped "
                "by the buffer]"
            )
        if out.stdout:
            parts.append(out.stdout)
        if out.stderr:
            parts.append(f"[stderr]\n{out.stderr}")
        more = out.process.stdout_bytes > out.stdout_offset or (
            out.process.stderr_bytes > out.stderr_offset
        )
        if more:
            parts.append("[more output is waiting: call process_output again]")
        parts.append(f"[{pid} {_state(out.process)}]")
        return ToolResult(
            _clip("\n".join(parts)),
            metadata={"process_id": pid, "running": out.process.running},
        )


class ProcessWriteTool(_Base):
    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="process_write",
            description=(
                "Write text to a background process's stdin (it must have been started with "
                "stdin: true), and optionally close stdin afterwards. Add a trailing newline "
                "yourself where the program expects a line."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "text": {"type": "string"},
                    "close": {"type": "boolean", "description": "Close stdin after writing."},
                },
                "required": ["id"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        pid, text = _text(args, "id"), _text(args, "text") or ""
        if not pid:
            return ToolResult("process_write needs an 'id'", is_error=True)
        close = args.get("close") is True
        if not text and not close:
            return ToolResult("process_write needs 'text' or close: true", is_error=True)
        return await self._guard(self._write(pid, text, close))

    async def _write(self, pid: str, text: str, close: bool) -> ToolResult:
        await self.procs.sandbox.write_process(pid, text, close=close)
        what = f"wrote {len(text.encode())} bytes to {pid}" if text else f"closed stdin of {pid}"
        return ToolResult(what + (" and closed stdin" if text and close else ""))


class ProcessListTool(_Base):
    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="process_list",
            description="List the background processes started in this task, and their state.",
            input_schema={"type": "object", "properties": {}},
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        return await self._guard(self._list())

    async def _list(self) -> ToolResult:
        infos = await self.procs.sandbox.list_processes()
        if not infos:
            return ToolResult("no background processes")
        return ToolResult(
            "\n".join(f"{i.id}  {_state(i)}  {' '.join(i.command[2:] or i.command)}" for i in infos)
        )


class ProcessKillTool(_Base):
    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="process_kill",
            description="Kill a background process and everything it started.",
            input_schema={
                "type": "object",
                "properties": {"id": {"type": "string"}},
                "required": ["id"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        pid = _text(args, "id")
        if not pid:
            return ToolResult("process_kill needs an 'id'", is_error=True)
        return await self._guard(self._kill(pid))

    async def _kill(self, pid: str) -> ToolResult:
        info = await self.procs.sandbox.kill_process(pid)
        return ToolResult(f"{pid}: {_state(info)}")


SHELL_TIMEOUT_S = 120.0
SHELL_LIFETIME_S = 4 * 3600.0


class ShellTool(_Base):
    """One long-lived ``sh`` in the sandbox, fed commands one at a time.

    The sandbox confines every command separately, and on some rungs each gets
    its own network namespace, so a server started by ``process_start`` is not
    reachable from the next ``bash`` call. A single shell is: its working
    directory, variables and ``&`` jobs survive between calls, and so does its
    network, so ``python3 -m http.server & `` in one call can be fetched with
    ``curl`` in the next."""

    def __init__(self, procs: _Processes) -> None:
        super().__init__(procs)
        self.spec = ToolSpec(
            name="shell",
            description=(
                "Run a command in a persistent shell in the task's sandbox. Unlike bash, the "
                "shell stays alive between calls: cd, exported variables and background jobs "
                "(command &) survive, and a server you start with & can be reached by later "
                "calls. stderr is mixed into the output. A command that runs past timeout_s "
                "(default 120) ends the shell and its jobs; the next call starts a fresh one."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "What to run in the shell."},
                    "timeout_s": {"type": "number"},
                },
                "required": ["command"],
            },
        )
        self._pid = ""
        self._offset = 0
        self._count = 0

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        command = _text(args, "command")
        if not command or not command.strip():
            return ToolResult("shell needs a 'command' string", is_error=True)
        timeout = args.get("timeout_s")
        limit = min(float(timeout), 3600.0) if isinstance(timeout, int | float) else SHELL_TIMEOUT_S
        return await self._guard(self._run(command, max(limit, 1.0)))

    async def _session(self) -> str:
        sb = self.procs.sandbox
        if self._pid:
            alive = [p for p in await sb.list_processes() if p.id == self._pid and p.running]
            if alive:
                return self._pid
        info = await sb.start_process(
            ["sh", "-c", "exec 2>&1; exec sh"],
            env=self.procs.env.config.env,
            stdin=True,
            timeout_s=SHELL_LIFETIME_S,
        )
        self._pid, self._offset = info.id, 0
        await sb.write_process(info.id, "__W=$(pwd)\n")
        return str(info.id)

    async def _run(self, command: str, limit_s: float) -> ToolResult:
        sb = self.procs.sandbox
        pid = await self._session()
        self._count += 1
        nonce = secrets.token_hex(6)
        name = f".evalsi-shell-{self._count:04d}.sh"
        await sb.write_files({name: command + "\n"})
        done = f"__EVALSI_DONE_{nonce}__"
        script = (
            f'F="$__W/{name}"; if sh -n "$F"; then . "$F"; else false; fi; __s=$?; rm -f "$F"; '
            f"printf '\\n{done} %d\\n' $__s\n"
        )
        await sb.write_process(pid, script)
        marker = re.compile(rf"\n?{done} (\d+)\n")
        text, deadline = "", time.monotonic() + limit_s
        while True:
            remaining = deadline - time.monotonic()
            out = await sb.read_process(
                pid, stdout_offset=self._offset, max_bytes=1 << 20, wait_s=min(max(remaining, 0), 5)
            )
            self._offset = out.stdout_offset
            text += out.stdout
            if m := marker.search(text):
                body = text[: m.start()].removesuffix("\n")
                status = int(m.group(1))
                return ToolResult(
                    _clip(f"{body}\n[exit code {status}]" if body else f"[exit code {status}]"),
                    is_error=status != 0,
                    metadata={"exit_code": status},
                )
            if not out.process.running:
                self._pid = ""
                return ToolResult(
                    _clip(f"{text}\n[the shell exited; the next call starts a new one]"),
                    is_error=True,
                )
            if remaining <= 0:
                await sb.kill_process(pid)
                self._pid = ""
                return ToolResult(
                    _clip(
                        f"{text}\n[timed out after {limit_s:g}s; the shell and its background "
                        "jobs were ended, the next call starts a new one]"
                    ),
                    is_error=True,
                    metadata={"outcome": "timeout"},
                )


def shell_tools(env: TaskEnvironment) -> list[Tool]:
    return [ShellTool(_Processes(env))]


def process_tools(env: TaskEnvironment) -> list[Tool]:
    procs = _Processes(env)
    return [
        ProcessStartTool(procs),
        ProcessOutputTool(procs),
        ProcessWriteTool(procs),
        ProcessListTool(procs),
        ProcessKillTool(procs),
    ]

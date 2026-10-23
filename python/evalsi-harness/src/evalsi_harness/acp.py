"""The Agent Client Protocol (ACP) connector.

ACP is JSON-RPC 2.0 over the stdio of an agent process, one JSON object per
line. Here the agent is a command run inside the task's sandbox, and the
harness is the *client*: it starts the process with an open stdin (the
sandbox's background-process API), initializes, opens a session, sends the
instruction as ``session/prompt`` and serves what the agent asks of it until the
prompt's response arrives.

Pinned to ACP **protocol version 1** (the stable ``initialize`` /
``session/new`` / ``session/prompt`` / ``session/update`` /
``session/request_permission`` / ``fs/*`` / ``terminal/*`` surface of
agentclientprotocol.com). An agent answering ``initialize`` with another
version is refused rather than half-understood. Not implemented: authentication
methods, ``session/load`` and ``session/set_mode``, image/audio/resource prompt
content, MCP servers handed to the agent (``mcpServers`` is always empty), and
the unstable extensions.

What becomes of each message:

- ``session/update``: ``agent_message_chunk`` text is one ``llm`` step per run of
  chunks; ``agent_thought_chunk`` an ``acp:thought`` step; ``tool_call`` a ``tool``
  step, completed by its ``tool_call_update``\\ s (status, output); ``plan`` an
  ``acp:plan`` step. Other updates are ignored.
- ``session/request_permission``: decided by the spec's ``approval`` config, which
  has the shape and semantics of the built-in ``ApprovalGate`` hook (rules, an
  approver script, a default; a rule that matches nothing allows). The request and
  the answer are ``hitl_request`` and ``hitl_resolved`` steps when the gate asks,
  and every request is an ``acp_permission`` policy event.
- ``fs/read_text_file`` and ``fs/write_text_file``: served in the sandbox through
  its file API, under the workdir (or an absolute path inside a writable image root).
- ``terminal/*``: only with ``terminals: true``; each terminal is a background
  process in the sandbox, under the sandbox's limits.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import posixpath
from collections.abc import Mapping
from typing import Any

from google.protobuf import json_format

from evalsi.sandbox import SandboxError
from evalsi.types import Content, Message, Step, Usage
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.approval import ApprovalGate
from evalsi_harness.connectors import AgentReply, _timeout
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.events import PolicyEvent, span_id
from evalsi_harness.hooks import Deny, HookContext, Rewrite, event_step
from evalsi_harness.models import ToolCallRequest
from evalsi_harness.task import TaskError

PROTOCOL_VERSION = 1
DEFAULT_TIMEOUT_S = 600.0
# Bytes of one JSON line from the agent that are tolerated.
MAX_LINE = 8 << 20
# Of an agent's stderr kept for an error message.
STDERR_TAIL = 2000

# JSON-RPC error codes.
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
RESOURCE_NOT_FOUND = -32002
INTERNAL_ERROR = -32603

# ACP tool kinds and the built-in tool whose approval rules should cover them.
_TOOL_FOR_KIND = {
    "execute": "bash",
    "edit": "write_file",
    "delete": "write_file",
    "move": "write_file",
    "read": "read_file",
}


class RpcError(Exception):
    """A JSON-RPC error answered to the agent (or by it)."""

    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _content_text(content: Any) -> str:
    """Text of an ACP content block or tool-call content item."""
    if not isinstance(content, Mapping):
        return ""
    kind = content.get("type")
    if kind == "text":
        return str(content.get("text", ""))
    if kind == "content":
        return _content_text(content.get("content"))
    if kind == "diff":
        old, new = content.get("oldText") or "", content.get("newText", "")
        return f"diff {content.get('path', '')}\n--- old\n{old}\n+++ new\n{new}"
    if kind == "terminal":
        return f"[terminal {content.get('terminalId', '')}]"
    if kind == "resource_link":
        return str(content.get("uri", ""))
    return ""


def _json_text(value: Any) -> str:
    return value if isinstance(value, str) else json.dumps(value, sort_keys=True, default=str)


class ACPConnector:
    """An agent speaking ACP over the stdio of a process in the task's sandbox."""

    def __init__(self, cfg: agent_pb2.ACPAgent, env: TaskEnvironment | None) -> None:
        if not cfg.command:
            raise TaskError("the acp agent needs a command")
        if env is None:
            raise TaskError(
                "an ACP agent runs inside the task's sandbox, so the run needs an environment"
            )
        self.cfg = cfg
        self.env = env
        self.name = cfg.command[0]
        self.terminals = cfg.terminals
        approval = json_format.MessageToDict(cfg.approval) if cfg.HasField("approval") else {}
        self.gate = ApprovalGate(approval)
        self._ctx = HookContext(config=approval)
        self._pid = ""
        self._reader: asyncio.Task[None] | None = None
        self._pending: dict[int, asyncio.Future[Any]] = {}
        self._handlers: set[asyncio.Task[None]] = set()
        self._next_id = 0
        self._session = ""
        self._stderr = ""
        self._exit = ""
        self._steps: list[Step] = []
        self._tools: dict[str, Step] = {}
        self._text: dict[str, list[str]] = {"message": [], "thought": []}
        self._answer: list[str] = []
        self._terminal_offsets: dict[str, int] = {}
        self.agent_info: dict[str, Any] = {}

    # --- the process ---

    async def _start(self) -> None:
        extra: dict[str, str] = {}
        for inside, outside in self.cfg.env_from.items():
            value = os.environ.get(outside)
            if value is None:
                raise TaskError(f"the acp agent needs {outside} on the worker (env_from {inside})")
            extra[inside] = value
        try:
            info = await self.env.sandbox.start_process(
                list(self.cfg.command),
                env={**self.env.config.env, **extra},
                stdin=True,
                timeout_s=_timeout(self.cfg) + 60.0,
                buffer_bytes=1 << 20,
            )
        except SandboxError as exc:
            raise TaskError(f"acp agent: {exc}") from exc
        self._pid = info.id
        self._reader = asyncio.ensure_future(self._read_loop())

    async def _read_loop(self) -> None:
        """Reads the agent's stdout into messages until the process ends."""
        offsets = [0, 0]
        buffer = b""
        try:
            while True:
                out = await self.env.sandbox.read_process(
                    self._pid, stdout_offset=offsets[0], stderr_offset=offsets[1], wait_s=5.0
                )
                offsets = [out.stdout_offset, out.stderr_offset]
                if out.stderr_raw:
                    self._stderr = (self._stderr + out.stderr_raw.decode(errors="replace"))[
                        -STDERR_TAIL:
                    ]
                buffer += out.stdout_raw
                while (cut := buffer.find(b"\n")) >= 0:
                    line, buffer = buffer[:cut], buffer[cut + 1 :]
                    if line.strip():
                        self._dispatch(line)
                if len(buffer) > MAX_LINE:
                    raise TaskError("acp agent: a line of output over 8 MiB")
                if not out.process.running and not out.stdout_raw and not out.stderr_raw:
                    code = out.process.exit_code
                    tail = self._stderr.strip()
                    self._exit = f"the agent exited (code {code})" + (f": {tail}" if tail else "")
                    break
        except (SandboxError, TaskError) as exc:
            self._exit = f"acp agent: {exc}"
        finally:
            for future in self._pending.values():
                if not future.done():
                    future.set_exception(TaskError(f"acp agent: {self._exit or 'stopped'}"))

    def _dispatch(self, line: bytes) -> None:
        try:
            message = json.loads(line)
        except json.JSONDecodeError:
            return  # a program may print a stray line; the protocol is the JSON ones
        if not isinstance(message, dict):
            return
        method = message.get("method")
        if method is None:  # a response to one of our requests
            future = self._pending.pop(message.get("id"), None)  # type: ignore[arg-type]
            if future is not None and not future.done():
                if "error" in message:
                    err = message["error"] if isinstance(message["error"], Mapping) else {}
                    future.set_exception(
                        RpcError(int(err.get("code", INTERNAL_ERROR)), str(err.get("message", err)))
                    )
                else:
                    future.set_result(message.get("result"))
            return
        params = message.get("params")
        params = params if isinstance(params, dict) else {}
        if "id" not in message:  # a notification
            if method == "session/update":
                self._update(params.get("update"))
            return
        task = asyncio.ensure_future(self._serve(message["id"], str(method), params))
        self._handlers.add(task)
        task.add_done_callback(self._handlers.discard)

    async def _write(self, message: dict[str, Any]) -> None:
        try:
            await self.env.sandbox.write_process(
                self._pid, json.dumps({"jsonrpc": "2.0", **message}) + "\n"
            )
        except SandboxError as exc:
            raise TaskError(f"acp agent: {self._exit or exc}") from exc

    async def _request(self, method: str, params: dict[str, Any], timeout_s: float) -> Any:
        self._next_id += 1
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[self._next_id] = future
        await self._write({"id": self._next_id, "method": method, "params": params})
        try:
            return await asyncio.wait_for(future, timeout_s)
        except RpcError as exc:
            raise TaskError(f"acp agent: {method}: {exc.message} (code {exc.code})") from exc
        except TimeoutError:
            raise TaskError(f"acp agent: {method} timed out after {timeout_s:g}s") from None

    # --- the conversation ---

    async def _open(self) -> None:
        await self._start()
        timeout = _timeout(self.cfg)
        caps: dict[str, Any] = {"fs": {"readTextFile": True, "writeTextFile": True}}
        if self.terminals:
            caps["terminal"] = True
        init = await self._request(
            "initialize",
            {
                "protocolVersion": PROTOCOL_VERSION,
                "clientCapabilities": caps,
                "clientInfo": {"name": "evalsi-harness", "title": "Evals.si harness"},
            },
            min(timeout, 120.0),
        )
        version = init.get("protocolVersion") if isinstance(init, Mapping) else None
        if version != PROTOCOL_VERSION:
            raise TaskError(
                f"acp agent speaks protocol version {version!r}; this harness implements "
                f"version {PROTOCOL_VERSION}"
            )
        self.agent_info = dict(init.get("agentInfo") or {})
        created = await self._request(
            "session/new", {"cwd": self.env.workdir, "mcpServers": []}, min(timeout, 120.0)
        )
        self._session = str(created.get("sessionId", "")) if isinstance(created, Mapping) else ""
        if not self._session:
            raise TaskError("acp agent: session/new returned no sessionId")

    async def send(self, text: str) -> AgentReply:
        if not self._session:
            await self._open()
        timeout = _timeout(self.cfg)
        self._steps, self._answer = [], []
        self._text = {"message": [], "thought": []}
        try:
            result = await self._request(
                "session/prompt",
                {"sessionId": self._session, "prompt": [{"type": "text", "text": text}]},
                timeout,
            )
        except TaskError:
            await self._cancel()
            raise
        self._flush("message")
        self._flush("thought")
        stop = str(result.get("stopReason", "")) if isinstance(result, Mapping) else ""
        reply = "".join(self._answer).strip()
        steps = self._steps
        steps.append(
            Step(
                type="agent",
                name=self.name,
                input=Content(text=text),
                output=Content(text=reply),
                error="" if stop in ("end_turn", "max_turn_requests") else stop,
                span_id=span_id(),
                attributes={"acp.stop_reason": stop, "acp.session_id": self._session},
            )
        )
        return AgentReply(
            reply,
            steps=steps,
            usage=Usage(),
            # A session is open for more prompts after end_turn; a simulated user may continue it.
            finished=stop != "end_turn",
            stop_reason="" if stop == "end_turn" else stop,
            extra={"acp_stop_reason": stop, "acp_protocol_version": PROTOCOL_VERSION},
        )

    async def _cancel(self) -> None:
        """Tell the agent to stop the turn it was timed out of (best effort: the
        process is killed when the connector closes)."""
        with contextlib.suppress(TaskError):
            await self._write({"method": "session/cancel", "params": {"sessionId": self._session}})

    async def aclose(self) -> None:
        for task in [*self._handlers]:
            task.cancel()
        if self._pid:
            with contextlib.suppress(SandboxError):
                await self.env.sandbox.kill_process(self._pid)
            for tid in list(self._terminal_offsets):
                with contextlib.suppress(SandboxError):
                    await self.env.sandbox.kill_process(tid)
        if self._reader is not None:
            self._reader.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await self._reader

    # --- session/update ---

    def _flush(self, kind: str) -> None:
        chunks, self._text[kind] = self._text[kind], []
        text = "".join(chunks)
        if not text:
            return
        if kind == "message":
            self._answer.append(text)
            self._steps.append(
                Step(
                    type="llm",
                    name="agent",
                    output=Content(messages=[Message(role="assistant", content=text)]),
                    span_id=span_id(),
                )
            )
        else:
            self._steps.append(
                Step(
                    type="agent",
                    name="acp:thought",
                    output=Content(text=text),
                    span_id=span_id(),
                )
            )

    def _update(self, update: Any) -> None:
        if not isinstance(update, Mapping):
            return
        kind = str(update.get("sessionUpdate", ""))
        if kind in ("agent_message_chunk", "agent_thought_chunk"):
            which = "message" if kind == "agent_message_chunk" else "thought"
            other = "thought" if which == "message" else "message"
            self._flush(other)
            self._text[which].append(_content_text(update.get("content")))
            return
        self._flush("message")
        self._flush("thought")
        if kind == "tool_call":
            self._tool_call(update)
        elif kind == "tool_call_update":
            self._tool_update(update)
        elif kind == "plan":
            entries = update.get("entries") or []
            self._steps.append(
                Step(
                    type="agent",
                    name="acp:plan",
                    output=Content(text=json.dumps(entries, sort_keys=True)),
                    span_id=span_id(),
                    attributes={"acp.plan_entries": len(entries)},
                )
            )

    def _tool_call(self, update: Mapping[str, Any]) -> None:
        call_id = str(update.get("toolCallId", ""))
        step = Step(
            type="tool",
            name=str(update.get("title") or update.get("kind") or "tool"),
            input=Content(text=_json_text(update["rawInput"])) if "rawInput" in update else None,
            span_id=span_id(),
            attributes={
                "acp.tool_call_id": call_id,
                "acp.kind": str(update.get("kind", "other")),
                "acp.status": str(update.get("status", "pending")),
            },
        )
        self._tools[call_id] = step
        self._steps.append(step)
        self._tool_update(update)

    def _tool_update(self, update: Mapping[str, Any]) -> None:
        step = self._tools.get(str(update.get("toolCallId", "")))
        if step is None:
            return
        if update.get("status"):
            step.attributes["acp.status"] = str(update["status"])
            if update["status"] == "failed":
                step.error = "failed"
        if update.get("title"):
            step.name = str(update["title"])
        if "rawInput" in update and step.input is None:
            step.input = Content(text=_json_text(update["rawInput"]))
        parts = [_content_text(c) for c in update.get("content") or []]
        if "rawOutput" in update:
            parts.append(_json_text(update["rawOutput"]))
        text = "\n".join(p for p in parts if p)
        if text:
            step.output = Content(text=text)

    # --- requests from the agent ---

    async def _serve(self, rpc_id: Any, method: str, params: dict[str, Any]) -> None:
        try:
            result = await self._call(method, params)
        except RpcError as exc:
            await self._answer_error(rpc_id, exc.code, exc.message)
            return
        except (SandboxError, OSError) as exc:
            await self._answer_error(rpc_id, INTERNAL_ERROR, str(exc))
            return
        with contextlib.suppress(TaskError):
            await self._write({"id": rpc_id, "result": result})

    async def _answer_error(self, rpc_id: Any, code: int, message: str) -> None:
        with contextlib.suppress(TaskError):
            await self._write({"id": rpc_id, "error": {"code": code, "message": message}})

    async def _call(self, method: str, params: dict[str, Any]) -> Any:
        if method == "session/request_permission":
            return self._permission(params)
        if method == "fs/read_text_file":
            return await self._read_file(params)
        if method == "fs/write_text_file":
            return await self._write_file(params)
        if method.startswith("terminal/") and self.terminals:
            return await self._terminal(method.removeprefix("terminal/"), params)
        raise RpcError(METHOD_NOT_FOUND, f"the client does not implement {method}")

    # --- permissions ---

    def _permission(self, params: Mapping[str, Any]) -> dict[str, Any]:
        call = params.get("toolCall") if isinstance(params.get("toolCall"), Mapping) else {}
        assert isinstance(call, Mapping)
        options = [o for o in params.get("options") or [] if isinstance(o, Mapping)]
        kind = str(call.get("kind") or "other")
        raw = call.get("rawInput") if isinstance(call.get("rawInput"), Mapping) else {}
        assert isinstance(raw, Mapping)
        locations = [loc for loc in call.get("locations") or [] if isinstance(loc, Mapping)]
        args: dict[str, Any] = {}
        command = raw.get("command")
        if command:
            args["command"] = " ".join(map(str, command)) if isinstance(command, list) else command
        path = (
            raw.get("path")
            or raw.get("file_path")
            or (locations[0].get("path") if locations else "")
        )
        if path:
            args["path"] = str(path)
        tool = _TOOL_FOR_KIND.get(kind, kind)
        call_id = str(call.get("toolCallId", ""))
        title = str(call.get("title", ""))
        request = ToolCallRequest(call_id, tool, json.dumps(args))
        out = self.gate.before_tool(self._ctx, request)
        for name, attrs in self._ctx.events:
            step = event_step(name, "", {**attrs, "acp_tool_call_id": call_id, "title": title})
            self._steps.append(step)
        self._ctx.events = []
        if out is None:
            allowed, why = True, ""
        elif isinstance(out, Deny):
            allowed, why = False, out.reason
        else:
            assert isinstance(out, Rewrite)
            # An approver's edit changes the call, which the agent made; ACP has no way
            # to hand it back, so an edited call is not run.
            allowed, why = False, "an edited call cannot be returned to an ACP agent"
        chosen = self._option(options, allowed)
        detail = f"{tool}: {title or call_id} -> {'allow' if allowed else 'deny'}" + (
            f" ({why})" if why else ""
        )
        self.env.policy_events.append(PolicyEvent("acp_permission", detail, granted=allowed))
        if chosen is None:
            return {"outcome": {"outcome": "cancelled"}}
        return {"outcome": {"outcome": "selected", "optionId": chosen}}

    @staticmethod
    def _option(options: list[Mapping[str, Any]], allow: bool) -> str | None:
        order = ("allow_once", "allow_always") if allow else ("reject_once", "reject_always")
        for kind in order:
            for option in options:
                if option.get("kind") == kind:
                    return str(option.get("optionId", ""))
        return None

    # --- files ---

    def _sandbox_path(self, raw: Any) -> str:
        if not isinstance(raw, str) or not posixpath.isabs(raw):
            raise RpcError(INVALID_PARAMS, "path must be absolute")
        clean = posixpath.normpath(raw)
        workdir = posixpath.normpath(self.env.workdir)
        if clean == workdir:
            raise RpcError(INVALID_PARAMS, f"{raw} is the workdir, not a file")
        if clean.startswith(workdir + "/"):
            return clean[len(workdir) + 1 :]
        return clean  # absolute inside a writable image root; the sandbox decides

    async def _read_file(self, params: Mapping[str, Any]) -> dict[str, Any]:
        path = self._sandbox_path(params.get("path"))
        files = await self.env.sandbox.read_files([path], max_bytes=4 << 20)
        if path not in files:
            raise RpcError(RESOURCE_NOT_FOUND, f"no such file: {params.get('path')}")
        content = files[path].decode(errors="replace")
        line, limit = params.get("line"), params.get("limit")
        if isinstance(line, int) or isinstance(limit, int):
            lines = content.splitlines(keepends=True)
            start = max((line if isinstance(line, int) else 1) - 1, 0)
            end = start + limit if isinstance(limit, int) and limit >= 0 else None
            content = "".join(lines[start:end])
        return {"content": content}

    async def _write_file(self, params: Mapping[str, Any]) -> None:
        path = self._sandbox_path(params.get("path"))
        content = params.get("content")
        if not isinstance(content, str):
            raise RpcError(INVALID_PARAMS, "content must be a string")
        await self.env.sandbox.write_files({path: content})

    # --- terminals ---

    async def _terminal(self, method: str, params: Mapping[str, Any]) -> Any:
        sandbox = self.env.sandbox
        if method == "create":
            command = params.get("command")
            if not isinstance(command, str) or not command:
                raise RpcError(INVALID_PARAMS, "command is required")
            env = {
                str(v["name"]): str(v.get("value", ""))
                for v in params.get("env") or []
                if isinstance(v, Mapping) and "name" in v
            }
            cwd = ""
            if isinstance(params.get("cwd"), str):
                cwd = self._sandbox_path(params["cwd"]) if params["cwd"] != self.env.workdir else ""
            limit = params.get("outputByteLimit")
            info = await sandbox.start_process(
                [command, *map(str, params.get("args") or [])],
                env={**self.env.config.env, **env},
                cwd=cwd,
                buffer_bytes=limit if isinstance(limit, int) and limit > 0 else 0,
            )
            self._terminal_offsets[info.id] = 0
            return {"terminalId": info.id}
        terminal = str(params.get("terminalId", ""))
        if terminal not in self._terminal_offsets:
            raise RpcError(INVALID_PARAMS, f"no such terminal: {terminal}")
        if method == "wait_for_exit":
            while True:
                out = await sandbox.read_process(
                    terminal, stdout_offset=1 << 40, stderr_offset=1 << 40, wait_s=5.0
                )
                if not out.process.running:
                    return {
                        "exitCode": out.process.exit_code if not out.process.killed else None,
                        "signal": "SIGKILL" if out.process.killed else None,
                    }
        if method == "output":
            out = await sandbox.read_process(terminal)
            text = out.stdout + out.stderr
            dropped = out.stdout_dropped + out.stderr_dropped > 0
            result: dict[str, Any] = {"output": text, "truncated": dropped}
            if not out.process.running:
                result["exitStatus"] = {
                    "exitCode": out.process.exit_code if not out.process.killed else None,
                    "signal": "SIGKILL" if out.process.killed else None,
                }
            return result
        if method == "kill":
            await sandbox.kill_process(terminal)
            return None
        if method == "release":
            await sandbox.kill_process(terminal)
            del self._terminal_offsets[terminal]
            return None
        raise RpcError(METHOD_NOT_FOUND, f"the client does not implement terminal/{method}")

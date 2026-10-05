"""Tools the agent can call: the sandbox tools, MCP servers, mocks, and
fault injection for robustness tests."""

from __future__ import annotations

import asyncio
import inspect
import json
import random
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.models import ToolSpec

OUTPUT_LIMIT = 16_000


@dataclass
class ToolResult:
    content: str
    is_error: bool = False
    metadata: dict[str, Any] = field(default_factory=dict)


class Tool(Protocol):
    spec: ToolSpec

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult: ...


def _clip(text: str, limit: int = OUTPUT_LIMIT) -> str:
    if len(text) <= limit:
        return text
    half = limit // 2
    return f"{text[:half]}\n... [{len(text) - limit} characters omitted] ...\n{text[-half:]}"


class FunctionTool:
    """A Python function as a tool. It may be sync or async and returns a str
    or a ToolResult; exceptions become error results."""

    def __init__(
        self,
        name: str,
        fn: Callable[..., Any],
        *,
        description: str = "",
        input_schema: dict[str, Any] | None = None,
    ) -> None:
        self.spec = ToolSpec(
            name=name,
            description=description or (fn.__doc__ or "").strip(),
            input_schema=input_schema or {"type": "object", "properties": {}},
        )
        self._fn = fn

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        try:
            value = self._fn(**args)
            if inspect.isawaitable(value):
                value = await value
        except Exception as exc:  # the agent sees its tool failing, like a real one
            return ToolResult(f"{type(exc).__name__}: {exc}", is_error=True)
        if isinstance(value, ToolResult):
            return value
        return ToolResult(value if isinstance(value, str) else json.dumps(value))


# --- sandbox tools ---


class BashTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="bash",
            description=(
                f"Run a shell command in the task's sandbox, in {env.workdir}. Files persist "
                "between calls; background processes do not. Returns the exit code and output."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "The shell command to run."},
                    "timeout_s": {
                        "type": "number",
                        "description": "Seconds before the command is killed (optional).",
                    },
                },
                "required": ["command"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        command = args.get("command")
        if not isinstance(command, str) or not command.strip():
            return ToolResult("bash needs a 'command' string", is_error=True)
        timeout = args.get("timeout_s")
        timeout = min(float(timeout), 3600.0) if isinstance(timeout, int | float) else None
        try:
            result = await self.env.run(command, timeout_s=timeout, span=span)
        except SandboxError as exc:
            return ToolResult(f"the sandbox failed: {exc}", is_error=True)
        parts = []
        if result.stdout:
            parts.append(result.stdout)
        if result.stderr:
            parts.append(f"[stderr]\n{result.stderr}")
        if result.outcome == "timeout":
            parts.append("[timed out]")
        if result.denials:
            parts.append(f"[sandbox denied: {', '.join(result.denials)}]")
        parts.append(f"[exit code {result.exit_code}]")
        return ToolResult(
            _clip("\n".join(parts)),
            is_error=not result.ok,
            metadata={"exit_code": result.exit_code, "outcome": result.outcome},
        )


class ReadFileTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="read_file",
            description="Read a text file from the task's sandbox (path relative to the workdir).",
            input_schema={
                "type": "object",
                "properties": {"path": {"type": "string"}},
                "required": ["path"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        path = args.get("path")
        if not isinstance(path, str) or not path:
            return ToolResult("read_file needs a 'path'", is_error=True)
        try:
            files = await self.env.sandbox.read_files([path], max_bytes=1 << 20)
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        if path not in files:
            if any(f.startswith(path.rstrip("/") + "/") for f in files):
                return ToolResult(f"{path} is a directory; use bash to list it", is_error=True)
            return ToolResult(f"no such file: {path}", is_error=True)
        return ToolResult(_clip(files[path].decode(errors="replace")))


class WriteFileTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="write_file",
            description=(
                "Create or overwrite a file in the task's sandbox (path relative to the workdir)."
            ),
            input_schema={
                "type": "object",
                "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                "required": ["path", "content"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        path, content = args.get("path"), args.get("content")
        if not isinstance(path, str) or not isinstance(content, str):
            return ToolResult("write_file needs 'path' and 'content' strings", is_error=True)
        try:
            await self.env.sandbox.write_files({path: content})
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        return ToolResult(f"wrote {len(content.encode())} bytes to {path}")


def sandbox_tools(env: TaskEnvironment) -> list[Tool]:
    return [BashTool(env), ReadFileTool(env), WriteFileTool(env)]


# --- mocks and faults ---


def _key(args: Mapping[str, Any]) -> str:
    return json.dumps(args, sort_keys=True, separators=(",", ":"))


class MockTool:
    """A tool answered from the spec: one response, or responses by exact arguments."""

    def __init__(
        self,
        name: str,
        *,
        description: str = "",
        input_schema: dict[str, Any] | None = None,
        response: str = "",
        responses: Mapping[str, str] | None = None,
    ) -> None:
        self.spec = ToolSpec(
            name=name,
            description=description or f"The {name} tool.",
            input_schema=input_schema or {"type": "object", "properties": {}},
        )
        self.response = response
        self.responses: dict[str, str] = {}
        for raw, value in (responses or {}).items():
            try:
                self.responses[_key(json.loads(raw))] = value
            except json.JSONDecodeError:
                self.responses[raw] = value

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        hit = self.responses.get(_key(args))
        if hit is not None:
            return ToolResult(hit)
        if self.response:
            return ToolResult(self.response)
        return ToolResult(f"no mocked response for {self.spec.name}({_key(args)})", is_error=True)


@dataclass
class Fault:
    tool: str
    kind: str
    rate: float


class FaultyTool:
    """Wraps a tool and injects timeouts, errors or malformed results.

    The random stream is seeded by task, trial and tool, so a rerun injects
    exactly the same faults."""

    KINDS = ("timeout", "error", "malformed")

    def __init__(self, inner: Tool, faults: list[Fault], seed: str) -> None:
        self.inner = inner
        self.spec = inner.spec
        self.faults = [f for f in faults if f.tool in ("*", inner.spec.name)]
        for f in self.faults:
            if f.kind not in self.KINDS:
                raise ValueError(f"fault kind must be one of {self.KINDS}, not {f.kind!r}")
        self._rng = random.Random(f"{seed}:{inner.spec.name}")

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        for fault in self.faults:
            if self._rng.random() < fault.rate:
                meta = {"fault": fault.kind}
                if fault.kind == "timeout":
                    await asyncio.sleep(0)
                    return ToolResult("the tool timed out", is_error=True, metadata=meta)
                if fault.kind == "error":
                    return ToolResult(
                        "the tool failed: internal error (injected)", is_error=True, metadata=meta
                    )
                real = await self.inner.call(args, span=span)
                garbled = real.content[: max(1, len(real.content) // 2)] + '\x00{"truncat'
                return ToolResult(garbled, is_error=real.is_error, metadata=meta)
        return await self.inner.call(args, span=span)

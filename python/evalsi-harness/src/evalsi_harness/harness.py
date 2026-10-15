"""The harness protocol in Python, and the built-in harness.

A harness prepares a task's environment (``setup``), drives the agent
through it streaming events (``run``), grades the end state (``check``) and
cleans up (``teardown``). The built-in harness runs the reference agent loop
on the target's model, or a bring-your-own agent through a connector, in an
Evals.si sandbox. A harness you bring implements the same five methods as a
Python class (``ExternalHarness.python``) or as the gRPC ``HarnessService``.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import secrets
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from google.protobuf import json_format

from evalsi.judges import JudgeClient
from evalsi.run import target_config
from evalsi.sandbox import SandboxError
from evalsi.types import TaskCheck
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness._version import __version__
from evalsi_harness.cassette import Cassette, CassetteModel, CassetteTool
from evalsi_harness.checker import run_checker
from evalsi_harness.context import PROGRESS_NOTE, ROUND_DONE, ContextConfig, context_config
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike, TaskEnvironment
from evalsi_harness.events import Event, FinalEvent, PolicyEvent, StepEvent
from evalsi_harness.hooks import Hooks, load_hooks
from evalsi_harness.loop import DEFAULT_SYSTEM, AgentLoop, LoopConfig
from evalsi_harness.mcp import MCPClient, MCPTool
from evalsi_harness.models import ChatModel, create_model
from evalsi_harness.task import Task, TaskError
from evalsi_harness.tools import (
    Fault,
    FaultyTool,
    FunctionTool,
    MockTool,
    Tool,
    ToolResult,
    sandbox_tools,
)
from evalsi_harness.toolset import MEMORY_DIR, MUTATING, extra_tools
from evalsi_harness.trust import Trust
from evalsi_harness.user_sim import UserSimulator


@dataclass
class HarnessManifest:
    name: str
    version: str
    description: str = ""
    task_formats: list[str] = field(default_factory=lambda: ["evalsi"])
    uses_sandbox: bool = True
    capabilities: list[str] = field(default_factory=list)


class Harness(Protocol):
    def describe(self) -> HarnessManifest: ...

    async def setup(self, task: Task) -> str: ...

    def run(self, handle: str) -> AsyncIterator[Event]: ...

    async def check(self, handle: str) -> TaskCheck | None: ...

    async def teardown(self, handle: str) -> None: ...

    async def aclose(self) -> None: ...


@dataclass
class HarnessContext:
    """What the worker (or the embedded runner) gives a harness."""

    # Opens the sandbox client lazily: only tasks with an environment need one.
    sandboxes: Callable[[], Awaitable[SandboxClientLike]]
    # Judge clients by name ("" is the run's default), for the user simulator.
    judge: Callable[[str], JudgeClient] | None = None
    # Relative recording directories resolve against this.
    base_dir: Path = field(default_factory=Path.cwd)
    # What a spec may make the worker itself execute: commands (stdio MCP
    # servers, harness commands) and Python references (harness classes,
    # checker parsers). Embedded runs trust their own spec; a server's worker
    # only allows what the server config lists (see trust.py).
    trust: Trust = field(default_factory=Trust)


@dataclass
class LiveTask:
    task: Task
    env: TaskEnvironment | None
    isolation: dict[str, Any] = field(default_factory=dict)
    closers: list[Callable[[], Awaitable[None]]] = field(default_factory=list)
    cassette: Cassette | None = None
    # What the agent changed in the workdir (a git diff), for code review
    # evaluators; captured before the checker runs.
    diff: str | None = None


# The workdir's changes against HEAD, untracked files included, leaving out
# the files Evals.si itself places (.evalsi-*). Exit 3: not a git repository.
DIFF_SCRIPT = r"""git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 3
x=':(exclude).evalsi-*'
git -c core.quotepath=off diff --no-color --no-ext-diff HEAD -- . "$x" 2>/dev/null ||
  git -c core.quotepath=off diff --no-color --no-ext-diff -- . "$x"
git ls-files --others --exclude-standard -- . "$x" | while IFS= read -r f; do
  git diff --no-color --no-ext-diff --no-index /dev/null "$f"
done
exit 0"""
DIFF_LIMIT = 256 * 1024


async def capture_diff(env: TaskEnvironment) -> str | None:
    """What the agent changed, as a git diff, or None when the workdir is not
    a git repository (or git is missing)."""
    try:
        result = await env.sandbox.exec(["sh", "-c", DIFF_SCRIPT], timeout_s=60, env=env.config.env)
    except SandboxError:
        return None
    if result.exit_code != 0:
        return None
    diff = result.stdout
    if len(diff) > DIFF_LIMIT:
        diff = diff[:DIFF_LIMIT] + f"\n[diff truncated at {DIFF_LIMIT} bytes]\n"
    return diff


def _agent_kind(task: Task) -> str:
    if task.spec.HasField("target") and task.spec.target.HasField("agent"):
        return task.spec.target.agent.WhichOneof("kind") or ""
    return ""


class BuiltinHarness:
    """The light built-in harness (design §11)."""

    def __init__(self, ctx: HarnessContext, config: agent_pb2.BuiltinHarness | None = None) -> None:
        self.ctx = ctx
        self.config = config or agent_pb2.BuiltinHarness()
        self._pool: EnvironmentPool | None = None
        self._pool_lock = asyncio.Lock()
        self._live: dict[str, LiveTask] = {}

    def describe(self) -> HarnessManifest:
        return HarnessManifest(
            name="evalsi-harness",
            version=__version__,
            description=(
                "Tool loop over any chat model or a bring-your-own agent, in an Evals.si sandbox."
            ),
            capabilities=[
                "sandbox-tools",
                "mcp-tools",
                "user-simulator",
                "record-replay",
                "fault-injection",
            ],
        )

    async def _environments(self) -> EnvironmentPool:
        async with self._pool_lock:
            if self._pool is None:
                self._pool = EnvironmentPool(await self.ctx.sandboxes())
            return self._pool

    async def setup(self, task: Task) -> str:
        env = None
        if task.environment is not None:
            extra: list[str] = []
            if _agent_kind(task) == "cli":
                cli = task.spec.target.agent.cli
                extra = list(cli.allow_hosts)
                task.environment.setup = [*task.environment.setup, *cli.install]
            env = await (await self._environments()).acquire(
                task.environment, extra_allow_hosts=extra
            )
        elif _agent_kind(task) == "cli":
            raise TaskError(
                "a CLI agent runs inside the task's sandbox, so the run needs an environment"
            )
        handle = secrets.token_hex(8)
        live = LiveTask(task, env)
        if env is not None:
            live.isolation = env.isolation.to_dict()
            live.closers.append(env.sandbox.destroy)
        self._live[handle] = live
        return handle

    def _get(self, handle: str) -> LiveTask:
        try:
            return self._live[handle]
        except KeyError:
            raise TaskError(f"unknown harness handle {handle}") from None

    async def run(self, handle: str) -> AsyncIterator[Event]:
        live = self._get(handle)
        queue: asyncio.Queue[Event | None] = asyncio.Queue()

        async def drive() -> None:
            try:
                kind = _agent_kind(live.task)
                if kind:
                    from evalsi_harness.connectors import drive_agent

                    final = await drive_agent(
                        live.task,
                        live.env,
                        queue.put_nowait,
                        timeout_s=self._wall_clock(),
                        user=self._user_simulator(live.task),
                    )
                else:
                    final = await self._run_loop(live, queue.put_nowait)
            except TaskError as exc:
                final = FinalEvent(output=None, stop_reason="error", error=str(exc))
            except (SandboxError, OSError, RuntimeError) as exc:
                final = FinalEvent(
                    output=None, stop_reason="error", error=f"{type(exc).__name__}: {exc}"
                )
            if live.env is not None:
                for event in live.env.policy_events:
                    queue.put_nowait(event)
                live.env.policy_events.clear()
            queue.put_nowait(final)
            queue.put_nowait(None)

        runner = asyncio.create_task(drive())
        try:
            while (event := await queue.get()) is not None:
                yield event
        finally:
            if not runner.done():
                runner.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await runner

    def _wall_clock(self) -> float:
        b = self.config.budget
        return b.wall_clock.ToNanoseconds() / 1e9 if b.HasField("wall_clock") else 0.0

    def _system(self, live: LiveTask) -> str:
        """The agent's system prompt. Subclasses (benchmark harnesses) replace it."""
        c = self.config
        notes = []
        rounds = c.context.strategy == "fresh-rounds"
        if rounds:
            notes.append(
                "Your work may be split across several rounds, each starting with a clean "
                "context. Keep notes in your memory file progress.md: what is done, what you "
                f"verified, what is left. When the whole task is complete, end your reply with "
                f"{ROUND_DONE}."
            )
        if "memory" in c.tools.builtin and not rounds:
            notes.append(
                "You have a memory tool for notes that survive your context being shortened. "
                "Record what you decide, verify and still have to do, and read it back when "
                "earlier work is no longer in front of you."
            )
        if "session_search" in c.tools.builtin:
            notes.append(
                "Earlier parts of this task may be cleared or summarized. session_search finds "
                "and re-reads any of it."
            )
        if "delegate" in c.tools.builtin:
            notes.append(
                "For work that would fill your context with output you do not need to keep "
                "(reading many files, long searches, debugging a long command), use delegate: a "
                "sub-agent does it in a fresh context and you get only its report."
            )
        parts = [DEFAULT_SYSTEM, *notes, *([c.instructions] if c.instructions else [])]
        return "\n\n".join(parts)

    @staticmethod
    def _cache_prices(live: LiveTask) -> dict[str, float]:
        """Anthropic bills cache reads at a tenth of the input price and writes
        at 1.25 times it. For other providers we assume no discount, so a
        spend budget stays an upper bound; a chain with any other provider in
        it (a fallback) is priced that way too."""
        spec = live.task.spec
        if not spec.HasField("target"):
            return {}
        chain = [spec.target, *spec.target.fallbacks]
        if all(t.connector == "anthropic" for t in chain):
            return {"cache_read_mult": 0.1, "cache_write_mult": 1.25}
        return {}

    def _progress_reader(self, live: LiveTask) -> Callable[[], Awaitable[str]] | None:
        """For fresh-rounds: the notes the last round left in the memory file."""
        if self._context(live).strategy != "fresh-rounds" or live.env is None:
            return None
        env = live.env
        path = f"{MEMORY_DIR}/{PROGRESS_NOTE}"

        async def read() -> str:
            files = await env.sandbox.read_files([path], max_bytes=64 * 1024)
            return files.get(path, b"").decode(errors="replace")

        return read

    def _builtin_names(self, live: LiveTask) -> list[str]:
        """The optional tools a spec names, plus the memory tool that fresh-rounds
        needs for its notes."""
        names = list(self.config.tools.builtin)
        if self._context(live).strategy == "fresh-rounds" and "memory" not in names:
            names.append("memory")
        return names

    def _hooks(self, live: LiveTask) -> Hooks:
        refs = [
            (h.python, json_format.MessageToDict(h.config) if h.HasField("config") else {})
            for h in self.config.hooks
        ]
        hooks = load_hooks(refs, self.ctx.trust)
        hooks.bind(live.task.id, live.task.trial)
        return hooks

    def _context(self, live: LiveTask) -> ContextConfig:
        try:
            return context_config(self.config.context)
        except ValueError as exc:
            raise TaskError(f"harness.builtin.context: {exc}") from exc

    @staticmethod
    def _truncation_retry(live: LiveTask) -> int:
        """The limit to retry a truncated turn with: twice the target's, up to 128k."""
        spec = live.task.spec
        if not spec.HasField("target"):
            return 0
        current = spec.target.max_tokens or 16000
        return min(current * 2, 128_000) if current < 128_000 else 0

    def _loop_config(self, live: LiveTask) -> LoopConfig:
        c = self.config
        system = self._system(live)
        return LoopConfig(
            max_steps=c.max_steps or 30,
            max_tokens=c.budget.tokens,
            max_usd=c.budget.usd,
            wall_clock_s=self._wall_clock(),
            input_per_mtok=c.pricing.input_per_mtok,
            output_per_mtok=c.pricing.output_per_mtok,
            truncation_retry_tokens=self._truncation_retry(live),
            session_search="session_search" in self.config.tools.builtin,
            delegate_steps=(self.config.delegate_steps or 20)
            if "delegate" in self.config.tools.builtin
            else 0,
            context=self._context(live),
            **self._cache_prices(live),
            parallel=c.parallel_tool_calls if c.HasField("parallel_tool_calls") else True,
            system=system,
        )

    async def _tools(self, live: LiveTask) -> list[Tool]:
        tools: list[Tool] = []
        cfg = self.config.tools
        use_sandbox = cfg.sandbox if cfg.HasField("sandbox") else live.env is not None
        if use_sandbox:
            if live.env is None:
                raise TaskError("tools.sandbox is on but the task has no environment")
            # Always offered: asking is itself evidence for safety evaluators,
            # even when the policy (by default) denies it.
            tools += [
                *sandbox_tools(live.env, spill_outputs=self._context(live).spill_outputs),
                self._escalation_tool(live),
            ]
        builtin = self._builtin_names(live)
        if builtin:
            if not use_sandbox or live.env is None:
                raise TaskError("tools.builtin needs the sandbox tools and an environment")
            try:
                tools += extra_tools(live.env, builtin)
            except ValueError as exc:
                raise TaskError(f"tools.builtin: {exc}") from exc
        for server in cfg.mcp:
            if server.command:
                self.ctx.trust.check_command(list(server.command), f"MCP server {server.name}")
            headers = {h: os.environ.get(env, "") for h, env in server.headers_env.items()}
            client = await MCPClient.connect(
                name=server.name, url=server.url, command=list(server.command), headers=headers
            )
            live.closers.append(client.aclose)
            for spec in await client.list_tools():
                if server.tools and spec.name not in server.tools:
                    continue
                tools.append(MCPTool(client, spec))
        for mock in cfg.mocks:
            tools.append(
                MockTool(
                    mock.name,
                    description=mock.description,
                    input_schema=json_format.MessageToDict(mock.input_schema)
                    if mock.HasField("input_schema")
                    else None,
                    response=mock.response,
                    responses=dict(mock.responses),
                )
            )
        if cfg.faults:
            faults = [Fault(f.tool, f.kind, f.rate) for f in cfg.faults]
            seed = f"{live.task.id}:{live.task.trial}"
            tools = [FaultyTool(t, faults, seed) for t in tools]
        return tools

    def _escalation_tool(self, live: LiveTask) -> Tool:
        allowed = set(self.config.escalation.allow)

        async def request_escalation(kind: str, reason: str = "") -> ToolResult:
            env = live.env
            assert env is not None
            granted = kind in allowed and kind == "network"
            env.policy_events.append(
                PolicyEvent(
                    kind="escalation_request",
                    detail=f"{kind}: {reason}".strip(": "),
                    granted=granted,
                )
            )
            if not granted:
                return ToolResult(
                    f"escalation to {kind!r} denied by the harness policy", is_error=True
                )
            pool = await self._environments()
            snapshot = await env.sandbox.snapshot()
            try:
                wider = await pool.client.restore(snapshot, network="allow")
            finally:
                await pool.client.delete_snapshot(snapshot)
            old = env.sandbox
            env.sandbox = wider
            live.closers.append(wider.destroy)
            await old.destroy()
            return ToolResult("network access granted for the rest of this task")

        return FunctionTool(
            "request_escalation",
            request_escalation,
            description=(
                "Ask for a wider sandbox policy when the sandbox denied something you need. "
                "kind is 'network'. The harness policy decides; it denies by default."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "kind": {"type": "string", "enum": ["network"]},
                    "reason": {"type": "string"},
                },
                "required": ["kind"],
            },
        )

    def _user_simulator(self, task: Task) -> UserSimulator | None:
        us = self.config.user_simulator
        if not us.ByteSize():
            return None
        if self.ctx.judge is None:
            raise TaskError("the user simulator needs a judge model")
        try:
            judge = self.ctx.judge(us.judge)
        except ValueError as exc:
            raise TaskError(str(exc)) from exc
        return UserSimulator(
            judge,
            persona=us.persona,
            goal=us.goal or task.instruction,
            max_turns=us.max_turns,
            seed=f"{task.id}#{task.trial}",
        )

    def _cassette(self, live: LiveTask) -> Cassette | None:
        rec = self.config.recording
        if rec.mode in ("", "off"):
            return None
        base = Path(rec.dir or "cassettes")
        if not base.is_absolute():
            base = self.ctx.base_dir / base
        fork_to = None
        if rec.mode == "fork":
            # One log per task and trial, so a name given for the run is
            # prefixed with the task's own.
            if rec.fork_as:
                fork_to = base / f"{live.task.slug}.{rec.fork_as}.jsonl"
            else:
                n = 1
                while (base / f"{live.task.slug}.fork-{n}.jsonl").exists():
                    n += 1
                fork_to = base / f"{live.task.slug}.fork-{n}.jsonl"
        try:
            return Cassette(
                base / f"{live.task.slug}.jsonl",
                rec.mode,
                branch_at_step=rec.branch_at_step,
                context=self._context(live).describe(),
                hooks=[h.python for h in self.config.hooks],
                fork_to=fork_to,
            )
        except ValueError as exc:
            raise TaskError(f"harness.builtin.recording: {exc}") from exc

    async def _run_loop(self, live: LiveTask, emit: Callable[[Event], None]) -> FinalEvent:
        task = live.task
        no_model = not task.spec.HasField("target") or not task.spec.target.model
        if no_model and self.config.recording.mode != "replay":
            raise TaskError(
                "the built-in agent needs a target model (spec.target.connector and model)"
            )
        model: ChatModel | None = None
        if task.spec.HasField("target") and task.spec.target.model:
            try:
                model = create_model(target_config(task.spec.target))
            except ValueError as exc:
                raise TaskError(f"target: {exc}") from exc
            live.closers.append(model.aclose)
        tools = await self._tools(live)
        cassette = live.cassette = self._cassette(live)
        if cassette is not None:
            model = CassetteModel(cassette, model)
            sandbox_names = {"bash", "write_file", *MUTATING}
            tools = [
                CassetteTool(cassette, t, reexecute=t.spec.name in sandbox_names) for t in tools
            ]
        assert model is not None
        user = self._user_simulator(task)
        hooks = self._hooks(live)
        loop = AgentLoop(
            model,
            tools,
            self._loop_config(live),
            emit,
            user=user,
            hooks=hooks,
            progress=self._progress_reader(live),
        )
        conversation = await self._conversation(live, user, emit)
        try:
            return await loop.run(conversation)
        finally:
            if cassette is not None:
                cassette.save()

    async def _conversation(
        self, live: LiveTask, user: UserSimulator | None, emit: Callable[[Event], None]
    ) -> list[dict[str, Any]]:
        """The messages the agent starts from. Subclasses may open with the
        simulated user, or replay a task's history."""
        conversation = live.task.conversation
        if live.env is not None and conversation and conversation[-1]["role"] == "user":
            conversation[-1] = {
                **conversation[-1],
                "content": f"{conversation[-1]['content']}\n\n"
                f"(Your working directory in the sandbox is {live.env.workdir}.)",
            }
        return conversation

    async def check(self, handle: str) -> TaskCheck | None:
        live = self._get(handle)
        env_config = live.task.environment
        if live.env is not None:
            live.diff = await capture_diff(live.env)
        if live.env is None or env_config is None or env_config.checker is None:
            return None
        parser = env_config.checker.parser
        if ":" in parser and not parser.startswith("junit:"):
            self.ctx.trust.check_python(parser, "checker parser")
        return await run_checker(live.env, env_config.checker, live.task)

    def isolation(self, handle: str) -> dict[str, Any]:
        return self._get(handle).isolation

    def artifacts(self, handle: str) -> dict[str, Any]:
        """Extra record metadata: the agent's diff, when the workdir is a git repository."""
        diff = self._get(handle).diff
        return {"diff": diff} if diff is not None else {}

    async def teardown(self, handle: str) -> None:
        live = self._live.pop(handle, None)
        if live is None:
            return
        for close in reversed(live.closers):
            with contextlib.suppress(Exception):
                await close()

    async def aclose(self) -> None:
        for handle in list(self._live):
            await self.teardown(handle)
        if self._pool is not None:
            await self._pool.aclose()


__all__ = [
    "BuiltinHarness",
    "Harness",
    "HarnessContext",
    "HarnessManifest",
    "LiveTask",
    "StepEvent",
]

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
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike, TaskEnvironment
from evalsi_harness.events import Event, FinalEvent, PolicyEvent, StepEvent
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
        return DEFAULT_SYSTEM + (f"\n\n{c.instructions}" if c.instructions else "")

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
            tools += [*sandbox_tools(live.env), self._escalation_tool(live)]
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
        return Cassette(
            base / f"{live.task.slug}.jsonl", rec.mode, branch_at_step=rec.branch_at_step
        )

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
            sandbox_names = {"bash", "write_file"}
            tools = [
                CassetteTool(cassette, t, reexecute=t.spec.name in sandbox_names) for t in tools
            ]
        assert model is not None
        user = self._user_simulator(task)
        loop = AgentLoop(model, tools, self._loop_config(live), emit, user=user)
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
        if live.env is None or env_config is None or env_config.checker is None:
            return None
        parser = env_config.checker.parser
        if ":" in parser and not parser.startswith("junit:"):
            self.ctx.trust.check_python(parser, "checker parser")
        return await run_checker(live.env, env_config.checker, live.task)

    def isolation(self, handle: str) -> dict[str, Any]:
        return self._get(handle).isolation

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

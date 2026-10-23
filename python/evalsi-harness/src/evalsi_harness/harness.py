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
import re
import secrets
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Protocol

from google.protobuf import json_format

from evalsi.judges import JudgeClient
from evalsi.run import target_config
from evalsi.sandbox import SandboxError
from evalsi.types import TaskCheck
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness import determinism, workspace
from evalsi_harness._version import __version__
from evalsi_harness.browser import BrowserConfig, BrowserSession, browser_tools
from evalsi_harness.cassette import Cassette, CassetteModel, CassetteTool, CassetteUserSimulator
from evalsi_harness.checker import run_checker
from evalsi_harness.context import PROGRESS_NOTE, ROUND_DONE, ContextConfig, context_config
from evalsi_harness.desktop import Computer, DesktopConfig, XvfbDisplay, computer_tool
from evalsi_harness.environment import EnvironmentPool, SandboxClientLike, TaskEnvironment
from evalsi_harness.events import Event, FinalEvent, PolicyEvent, StepEvent
from evalsi_harness.hooks import Hooks, load_hooks
from evalsi_harness.isolate import run_clean_checker
from evalsi_harness.loop import DEFAULT_SYSTEM, AgentLoop, LoopConfig
from evalsi_harness.mcp import (
    MCPClient,
    MCPPromptsTool,
    MCPResourcesTool,
    MCPTool,
    OAuth,
    answer_from_text,
)
from evalsi_harness.models import ChatModel, NamedModel, create_model
from evalsi_harness.profiles import Profile, resolve
from evalsi_harness.protect import Baseline, ProtectError
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

MAX_ELICITATIONS = 10


def _elicitation(user: UserSimulator) -> Callable[[str, dict[str, Any]], Awaitable[dict[str, Any]]]:
    """Answers an MCP server's question with the simulated user."""

    async def answer(message: str, schema: dict[str, Any]) -> dict[str, Any]:
        user.elicitations = getattr(user, "elicitations", 0) + 1
        if user.elicitations > MAX_ELICITATIONS:
            return {"action": "cancel"}
        # The conversation's turn allowance is not spent here: a replay serves the
        # tool's recorded result without asking, so a turn taken now would be a turn
        # the replay (or a resume) does not take, and the two would drift apart.
        turns = user.turns
        try:
            elicit = getattr(user, "elicit", None)
            reply = (
                await elicit(message, schema)
                if elicit is not None
                else await user.reply([{"role": "assistant", "content": message}])
            )
        finally:
            user.turns = turns
        if not reply.message:
            return {"action": "cancel"}
        return answer_from_text(schema, reply.message)

    return answer


def _hook_identity(h: Any) -> Any:
    """What a recording depends on for a hook: its class, and its config when it has one
    (a changed ban list changes which calls are denied)."""
    config = json_format.MessageToDict(h.config) if h.HasField("config") else {}
    return {"python": h.python, "config": config} if config else h.python


def _all_models(target: Any) -> list[Any]:
    """Every model a target may call: its own, its fallbacks, and its compaction and
    sub-agent models, in that order."""
    roles = [
        getattr(target, r) for r in ("compaction_model", "subagent_model") if target.HasField(r)
    ]
    return [target, *target.fallbacks, *roles]


def _doubled(max_tokens: int) -> int:
    """Twice a target's output limit (16,000 when unset), up to 128k; 0 when already there."""
    current = max_tokens or 16000
    return min(current * 2, 128_000) if current < 128_000 else 0


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
    # The simulated user, when the task has one: MCP elicitation is answered by it.
    # (On the live task, not an argument, so a subclass's _tools(self, live) still fits.)
    user: UserSimulator | None = None
    # What the agent changed in the workdir (a git diff), for code review
    # evaluators; captured before the checker runs.
    diff: str | None = None
    # The checker's protected files as the trial began, and the policy events the
    # check produced (they arrive after the agent's stream has ended).
    protect: Baseline | None = None
    late_events: list[PolicyEvent] = field(default_factory=list)
    protect_result: dict[str, Any] = field(default_factory=dict)
    checker_isolation: dict[str, Any] = field(default_factory=dict)
    # Checker files that did not exist when the agent started (after the image and
    # setup): naming one of these is probing. One the sandbox already had may be run.
    checker_hidden: list[str] = field(default_factory=list)
    diff_baseline: str | None = None
    # The agent loop, for what it learned of the models that served the run, and the
    # MCP servers connected (name, client), for the determinism record.
    loop: Any = None
    mcp_servers: list[tuple[str, Any]] = field(default_factory=list)
    # The files under metadata.grading.workspace.paths, for a judge (workspace.py).
    workspace_files: dict[str, Any] | None = None


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


async def _absent(env: TaskEnvironment, paths: list[str]) -> list[str]:
    """The paths that do not exist in the prepared sandbox. A path that cannot be
    checked counts as present, so nothing is flagged on a guess."""
    out = []
    for path in paths:
        try:
            result = await env.sandbox.exec(["test", "-e", path], timeout_s=30, env=env.config.env)
        except SandboxError:
            continue
        if result.exit_code == 1:
            out.append(path)
    return out


def _agent_kind(task: Task) -> str:
    if task.spec.HasField("target") and task.spec.target.HasField("agent"):
        return task.spec.target.agent.WhichOneof("kind") or ""
    return ""


class BuiltinHarness:
    """The light built-in harness (design §11)."""

    def __init__(self, ctx: HarnessContext, config: agent_pb2.BuiltinHarness | None = None) -> None:
        self.ctx = ctx
        self.config = config or agent_pb2.BuiltinHarness()
        self.profile: Profile | None = None
        self._profile_error = ""
        try:
            self.profile = resolve(self.config.profile, list(self.config.tools.builtin))
        except ValueError as exc:
            self._profile_error = str(exc)
        if self.profile is not None:
            # The profile's toolset and default strategy are the spec's from here on.
            resolved = agent_pb2.BuiltinHarness()
            resolved.CopyFrom(self.config)
            resolved.tools.builtin.extend(self.profile.builtin)
            window = resolved.context
            if self.profile.compact_with_window and not window.strategy and window.window_tokens:
                window.strategy = "compact"
            self.config = resolved
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
            if _agent_kind(task) in ("cli", "acp"):
                cli = getattr(task.spec.target.agent, _agent_kind(task))
                extra = list(cli.allow_hosts)
                task.environment.setup = [*task.environment.setup, *cli.install]
            env = await (await self._environments()).acquire(
                task.environment, extra_allow_hosts=extra
            )
        elif _agent_kind(task) in ("cli", "acp"):
            raise TaskError(
                f"a {_agent_kind(task).upper()} agent runs inside the task's sandbox, "
                "so the run needs an environment"
            )
        handle = secrets.token_hex(8)
        live = LiveTask(task, env)
        if env is not None:
            live.isolation = env.isolation.to_dict()
            live.closers.append(env.sandbox.destroy)
        self._live[handle] = live
        checker = task.environment.checker if task.environment else None
        if env is not None and checker is not None and checker.protect:
            # Per trial, after the environment's files and installs are in place.
            try:
                live.protect = await Baseline.take(env, checker.protect, checker.on_change)
            except ProtectError as exc:
                await self.teardown(handle)
                raise TaskError(f"checker.protect: {exc}") from exc
        if env is not None:
            # What setup and the task's files changed against HEAD, so a trusted change
            # to a tracked test is not counted as the agent's.
            live.diff_baseline = await capture_diff(env)
        if env is not None and checker is not None and checker.files:
            live.checker_hidden = await _absent(env, list(checker.files))
        return handle

    def _get(self, handle: str) -> LiveTask:
        try:
            return self._live[handle]
        except KeyError:
            raise TaskError(f"unknown harness handle {handle}") from None

    async def run(self, handle: str) -> AsyncIterator[Event]:
        live = self._get(handle)
        queue: asyncio.Queue[Event | None] = asyncio.Queue()

        async def produce() -> None:
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
            if self.profile is not None and not _agent_kind(live.task):
                final.extra["profile"] = self.profile.ident
            queue.put_nowait(final)

        async def drive() -> None:
            try:
                await produce()
            except Exception as exc:
                queue.put_nowait(
                    FinalEvent(
                        output=None, stop_reason="error", error=f"{type(exc).__name__}: {exc}"
                    )
                )
            finally:
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
        if c.context.strategy == "evidence-state":
            notes.append(
                "Record what you find with the evidence tool, not in your head: each finding is a "
                "statement, its source and a quote copied verbatim from what you read. Correct a "
                "finding by superseding or retracting it. Keep the conclusions you are working "
                "toward, with the evidence that supports each and the gaps still open; your "
                "conversation will be cut down as it grows, and those lists are what remains."
            )
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
        if "code_mode" in c.tools.builtin:
            notes.append(
                "You act by writing scripts with run_code, which calls the task's tools as "
                "functions. Do a whole step of work in one script (look, filter, change, check) "
                "and print only what you need to see."
            )
        if "browser" in c.tools.builtin:
            start = BrowserConfig.from_metadata(live.task.record.metadata).start_url
            notes.append(
                "You have a web browser (the browser_* tools). Read pages with browser_snapshot "
                "and act on the refs it gives; use browser_screenshot when layout matters."
                + (f" It opens at {start}." if start else "")
            )
        if "computer" in c.tools.builtin:
            cfg = DesktopConfig.from_metadata(live.task.record.metadata)
            notes.append(
                f"You have a {cfg.width}x{cfg.height} desktop (the computer tool). Take a "
                "screenshot to see it, then click, type and press keys by pixel position."
            )
        if "delegate" in c.tools.builtin:
            notes.append(
                "For work that would fill your context with output you do not need to keep "
                "(reading many files, long searches, debugging a long command), use delegate: a "
                "sub-agent does it in a fresh context and you get only its report."
            )
        base = self.profile.system if self.profile and self.profile.system else DEFAULT_SYSTEM
        parts = [base, *notes, *([c.instructions] if c.instructions else [])]
        return "\n\n".join(parts)

    @staticmethod
    def _cache_prices(live: LiveTask) -> dict[str, float]:
        """Cache prices as multiples of the input price. Anthropic bills reads at a
        tenth and writes at 1.25 times; for other providers we assume no discount,
        so a spend budget stays an upper bound. In a chain that includes any other
        provider (a fallback) the read discount is off, but the write surcharge
        stays whenever Anthropic is in it: a write it makes costs 1.25 times."""
        spec = live.task.spec
        if not spec.HasField("target"):
            return {}
        chain = _all_models(spec.target)
        anthropic = [t.connector == "anthropic" for t in chain]
        if all(anthropic):
            return {"cache_read_mult": 0.1, "cache_write_mult": 1.25}
        if any(anthropic):
            return {"cache_write_mult": 1.25}
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
        return _doubled(spec.target.max_tokens)

    @staticmethod
    def _truncation_retry_by_model(live: LiveTask) -> dict[str, int]:
        """The same, for each model of a fallback chain by name, so a turn a fallback
        served is retried above the fallback's own limit, not the primary's."""
        spec = live.task.spec
        if not spec.HasField("target"):
            return {}
        chain = _all_models(spec.target)
        # The compaction and sub-agent models first, so the chain's own settings win a name.
        ordered = [
            *chain[1 + len(spec.target.fallbacks) :],
            *chain[: 1 + len(spec.target.fallbacks)],
        ]
        return {t.model: _doubled(t.max_tokens) for t in ordered}

    def _loop_config(self, live: LiveTask) -> LoopConfig:
        c = self.config
        system = self._system(live)
        cache = self._cache_prices(live)
        return LoopConfig(
            max_steps=c.max_steps or 30,
            max_tokens=c.budget.tokens,
            max_usd=c.budget.usd,
            wall_clock_s=self._wall_clock(),
            input_per_mtok=c.pricing.input_per_mtok,
            output_per_mtok=c.pricing.output_per_mtok,
            truncation_retry_tokens=self._truncation_retry(live),
            truncation_retry_by_model=self._truncation_retry_by_model(live),
            session_search="session_search" in self.config.tools.builtin,
            code_mode="code_mode" in self.config.tools.builtin,
            delegate_steps=(self.config.delegate_steps or 20)
            if "delegate" in self.config.tools.builtin
            else 0,
            context=self._context(live),
            interjections=sorted(
                ((j.at_step, j.message) for j in c.interjections), key=lambda j: j[0]
            ),
            cache_read_mult=cache.get("cache_read_mult", 1.0),
            cache_write_mult=cache.get("cache_write_mult", 1.0),
            model_prices={
                name: (p.input_per_mtok, p.output_per_mtok) for name, p in c.model_pricing.items()
            },
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
            tools += sandbox_tools(live.env, spill_outputs=self._context(live).spill_outputs)
            if self.profile is None or self.profile.escalation:
                tools.append(self._escalation_tool(live))
        builtin = self._builtin_names(live)
        if "browser" in builtin:
            tools += self._browser_tools(live)
        if "computer" in builtin:
            tools += self._computer_tools(live)
        builtin = [n for n in builtin if n not in ("browser", "computer")]
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
            oauth = None
            if server.HasField("oauth"):
                o = server.oauth
                oauth = OAuth(
                    o.token_url,
                    os.environ.get(o.client_id_env, ""),
                    os.environ.get(o.client_secret_env, ""),
                    o.scope,
                )
            elicit = (
                _elicitation(live.user) if server.elicitation and live.user is not None else None
            )
            client = await MCPClient.connect(
                name=server.name,
                url=server.url,
                command=list(server.command),
                headers=headers,
                timeout_s=server.timeout_s or 120.0,
                oauth=oauth,
                elicit=elicit,
            )
            live.closers.append(client.aclose)
            live.mcp_servers.append((server.name, client))
            for spec in await client.list_tools():
                if server.tools and spec.name not in server.tools:
                    continue
                tool = MCPTool(
                    client, spec, timeout_s=float(server.tool_timeout_s.get(spec.name, 0))
                )
                # tool_search: MCP tools start unloaded and are loaded as the agent needs them.
                tool.deferred = "tool_search" in cfg.builtin
                tools.append(tool)
            label = re.sub(r"[^A-Za-z0-9_]", "_", server.name) or f"mcp{len(tools)}"
            if server.resources:
                tools.append(MCPResourcesTool(client, label))
            if server.prompts:
                tools.append(MCPPromptsTool(client, label))
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
            faults = [Fault(f.tool, f.kind, f.rate, f.payload) for f in cfg.faults]
            seed = f"{live.task.id}:{live.task.trial}"
            tools = [FaultyTool(t, faults, seed) for t in tools]
        return tools

    def _browser_tools(self, live: LiveTask) -> list[Tool]:
        """The ``browser`` toolset, configured by the record's ``metadata.browser``. The
        browser opens on the first call and closes with the task."""
        try:
            config = BrowserConfig.from_metadata(live.task.record.metadata)
        except ValueError as exc:
            raise TaskError(f"tools.builtin: {exc}") from exc
        # The browser runs on the worker, outside the sandbox: a server must trust it.
        self.ctx.trust.check_command(["evalsi-browser"], "the browser toolset")
        session = BrowserSession(config)
        live.closers.append(session.aclose)
        return browser_tools(session)

    def _computer_tools(self, live: LiveTask) -> list[Tool]:
        """The ``computer`` toolset over an Xvfb display on the worker, configured by the
        record's ``metadata.desktop``. The display starts on the first call and stops with
        the task."""
        try:
            config = DesktopConfig.from_metadata(live.task.record.metadata)
        except ValueError as exc:
            raise TaskError(f"tools.builtin: {exc}") from exc
        self.ctx.trust.check_command(["evalsi-desktop"], "the computer toolset")
        for argv in config.launch:
            self.ctx.trust.check_command(argv, "metadata.desktop.launch")
        display = XvfbDisplay(config)
        live.closers.append(display.aclose)
        return [computer_tool(Computer(display, config))]

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

    def _user_simulator_identity(self, task: Task) -> dict[str, Any] | None:
        """Override for adapters that enable a simulator without builtin config."""
        us = self.config.user_simulator
        return json_format.MessageToDict(us) if us.ByteSize() else None

    def _user_judge(self, name: str) -> JudgeClient:
        if self.ctx.judge is None:
            raise TaskError("the user simulator needs a judge model")
        try:
            return self.ctx.judge(name)
        except ValueError as exc:
            raise TaskError(str(exc)) from exc

    def _user_simulator(self, task: Task) -> UserSimulator | None:
        us = self.config.user_simulator
        if self._user_simulator_identity(task) is None:
            return None
        return UserSimulator(
            lambda: self._user_judge(us.judge),
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
                hooks=[_hook_identity(h) for h in self.config.hooks],
                toolset={
                    "builtin": list(self.config.tools.builtin),
                    "delegate_steps": self.config.delegate_steps,
                    **(
                        {
                            "interjections": [
                                [j.at_step, j.message] for j in self.config.interjections
                            ]
                        }
                        if self.config.interjections
                        else {}
                    ),
                    **({"profile": self.profile.ident} if self.profile else {}),
                },
                fork_to=fork_to,
                user_simulator=self._user_simulator_identity(live.task),
            )
        except ValueError as exc:
            raise TaskError(f"harness.builtin.recording: {exc}") from exc

    def _role_model(self, live: LiveTask, role: str) -> ChatModel | None:
        """The target's ``compaction_model`` or ``subagent_model``, built like the main
        model (its own fallbacks ignored), or None when it is not set."""
        spec = live.task.spec
        if not spec.HasField("target") or not spec.target.HasField(role):  # type: ignore[arg-type]
            return None
        try:
            config = replace(target_config(getattr(spec.target, role)), fallbacks=())
            model = NamedModel(create_model(config))
        except ValueError as exc:
            raise TaskError(f"target.{role}: {exc}") from exc
        live.closers.append(model.aclose)
        return model

    def _reexecute_tool(self, live: LiveTask, tool: Tool) -> bool:
        return tool.spec.name in {"bash", "write_file", *MUTATING}

    async def _run_loop(self, live: LiveTask, emit: Callable[[Event], None]) -> FinalEvent:
        task = live.task
        if self._profile_error:
            raise TaskError(f"harness.builtin.profile: {self._profile_error}")
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
        cassette = live.cassette = self._cassette(live)

        def make_user() -> UserSimulator:
            result = self._user_simulator(task)
            assert result is not None
            return result

        user = live.user = (
            CassetteUserSimulator(cassette, make_user)
            if cassette is not None and self._user_simulator_identity(task) is not None
            else self._user_simulator(task)
        )
        tools = await self._tools(live)
        roles = {
            role: self._role_model(live, role) for role in ("compaction_model", "subagent_model")
        }
        if cassette is not None:
            model = CassetteModel(cassette, model)
            # One log for every model, in call order: a replay serves each its turns.
            roles = {
                role: CassetteModel(cassette, m) if m is not None else None
                for role, m in roles.items()
            }
            tools = [
                CassetteTool(cassette, t, reexecute=self._reexecute_tool(live, t)) for t in tools
            ]
        assert model is not None
        hooks = self._hooks(live)
        loop = AgentLoop(
            model,
            tools,
            self._loop_config(live),
            emit,
            user=user,
            hooks=hooks,
            progress=self._progress_reader(live),
            compaction_model=roles["compaction_model"],
            subagent_model=roles["subagent_model"],
        )
        live.loop = loop
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
            # Before the checker, which may change or remove the agent's files.
            try:
                wanted = workspace.paths_of(live.task.record.metadata)
            except ValueError as exc:
                raise TaskError(str(exc)) from exc
            if wanted:
                live.workspace_files = await workspace.collect(live.env, wanted)
        if live.env is None or env_config is None or env_config.checker is None:
            return None
        parser = env_config.checker.parser
        if ":" in parser and not parser.startswith("junit:"):
            self.ctx.trust.check_python(parser, "checker parser")
        if live.protect is not None:
            verdict = await self._protected(live)
            if verdict is not None:
                return verdict
        if env_config.checker.isolation == "clean":
            check, info = await run_clean_checker(
                await self._environments(), live.env, env_config.checker, live.task
            )
            live.checker_isolation = info
            return check
        return await run_checker(live.env, env_config.checker, live.task)

    async def _protected(self, live: LiveTask) -> TaskCheck | None:
        """Compare the protected files with the baseline. A failing check when the
        trial must fail, None to go on to the checker (nothing changed, or the
        originals were restored)."""
        assert live.protect is not None
        assert live.env is not None
        base = live.protect
        try:
            changes = await base.compare(live.env)
            live.protect_result = {
                "mode": base.mode,
                "files": len(base.hashes),
                "changed": changes.to_dict(),
            }
            if not changes:
                return None
            live.late_events.append(
                PolicyEvent(kind="protected_file_modified", detail=changes.describe())
            )
            if base.mode == "restore":
                await base.restore(live.env, changes)
                return None
        except ProtectError as exc:
            return TaskCheck(passed=False, error=f"protected files could not be checked: {exc}")
        return TaskCheck(
            passed=False,
            details=changes.describe(),
            tests={p: "modified" for p in changes.modified}
            | {p: "deleted" for p in changes.deleted}
            | {p: "created" for p in changes.created},
        )

    def late_events(self, handle: str) -> list[PolicyEvent]:
        """Policy events raised after the agent finished (by the check)."""
        return list(self._get(handle).late_events)

    def isolation(self, handle: str) -> dict[str, Any]:
        return self._get(handle).isolation

    def artifacts(self, handle: str) -> dict[str, Any]:
        """Extra record metadata: the agent's diff, when the workdir is a git repository."""
        live = self._get(handle)
        out: dict[str, Any] = {"diff": live.diff} if live.diff is not None else {}
        if live.diff_baseline:
            out["diff_baseline"] = live.diff_baseline
        if live.protect_result:
            out["protected_files"] = live.protect_result
        checker = live.task.environment.checker if live.task.environment else None
        if checker is not None and checker.files and live.checker_hidden:
            out["checker_files"] = sorted(live.checker_hidden)
        if live.checker_isolation:
            out["checker_isolation"] = live.checker_isolation
        out["determinism"] = self._determinism(live)
        if live.workspace_files is not None:
            out["workspace_files"] = live.workspace_files
        return out

    def _determinism(self, live: LiveTask) -> dict[str, Any]:
        """The run's determinism record (see determinism.py)."""
        task, c = live.task, self.config
        spec = task.spec
        target = spec.target if spec.HasField("target") else None
        temperature = (
            target.temperature if target is not None and target.HasField("temperature") else None
        )
        seeds: dict[str, Any] = {
            "temperature": temperature,
            "user_simulator": f"{task.id}#{task.trial}" if c.HasField("user_simulator") else None,
            "fault_injection": f"{task.id}:{task.trial}" if c.tools.faults else None,
        }
        state = live.loop.state if live.loop is not None else None
        harness: dict[str, Any] = {
            "name": "evalsi-harness",
            "version": __version__,
            "context": self._context(live).describe(),
            "tools": sorted(c.tools.builtin),
        }
        if self.profile is not None:
            harness["profile"] = self.profile.ident
        if c.recording.mode not in ("", "off"):
            harness["recording"] = c.recording.mode
        env, config = live.env, task.environment
        image: dict[str, Any] = {}
        if env is not None:
            image = {
                "name": config.image if config is not None else "",
                "digest": getattr(env.sandbox, "image_digest", "") or "",
                "isolation": env.isolation.to_dict(),
            }
        return determinism.build(
            seeds=seeds,
            models=determinism.served_models(
                state.served if state is not None else {},
                state.fingerprints if state is not None else {},
            ),
            harness=harness,
            mcp_servers={n: dict(m.server_info) for n, m in live.mcp_servers if m.server_info},
            image=image,
        )

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

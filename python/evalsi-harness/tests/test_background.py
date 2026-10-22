"""Background processes for the built-in agent (H2).

The tools run against the unconfined local stand-in, which implements the same
process API as the sandbox service, so the tool logic is tested offline. The
real rungs (bubblewrap, Landlock) are exercised in ``test_real_rungs.py``.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from pathlib import Path

from evalsi_harness import background
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.task import EnvironmentConfig
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.tools import Tool
from evalsi_harness.toolset import MUTATING, extra_tools


def with_tools(body: Callable[[dict[str, Tool], TaskEnvironment, Path], Awaitable[None]]) -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        sandbox = await client.create()
        env = TaskEnvironment(sandbox, EnvironmentConfig())
        tools = {t.spec.name: t for t in extra_tools(env, ["background"])}
        try:
            await body(tools, env, Path(sandbox.root))
        finally:
            await sandbox.destroy()

    asyncio.run(go())


def test_the_group_offers_five_tools_and_the_mutating_ones_replay() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        assert list(tools) == list(background.NAMES)
        assert background.MUTATING <= MUTATING
        assert "process_output" not in MUTATING

    with_tools(body)


def test_a_server_started_in_one_call_answers_in_the_next() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        started = await tools["process_start"].call(
            {"command": "echo hi > served.txt; echo listening; sleep 30"}
        )
        pid = started.metadata["process_id"]
        assert pid == "proc_1"
        assert not started.is_error
        out = await tools["process_output"].call({"id": pid, "wait_s": 5})
        assert "listening" in out.content
        assert "[proc_1 running]" in out.content
        # Nothing new: the next read is empty, not a repeat.
        again = await tools["process_output"].call({"id": pid})
        assert "listening" not in again.content
        # A later command in the same sandbox sees what it did.
        assert (root / "served.txt").read_text() == "hi\n"
        listed = await tools["process_list"].call({})
        assert "proc_1  running" in listed.content
        killed = await tools["process_kill"].call({"id": pid})
        assert "killed" in killed.content
        assert "killed" in (await tools["process_output"].call({"id": pid})).content

    with_tools(body)


def test_stdin_round_trip_and_exit_status() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        started = await tools["process_start"].call(
            {"command": "while read l; do echo got:$l; done; exit 4", "stdin": True}
        )
        pid = started.metadata["process_id"]
        await tools["process_write"].call({"id": pid, "text": "ping\n"})
        out = await tools["process_output"].call({"id": pid, "wait_s": 5})
        assert "got:ping" in out.content
        await tools["process_write"].call({"id": pid, "close": True})
        for _ in range(10):
            out = await tools["process_output"].call({"id": pid, "wait_s": 2})
            if "exited" in out.content:
                break
        assert "exited with code 4" in out.content
        late = await tools["process_write"].call({"id": pid, "text": "x"})
        assert late.is_error

    with_tools(body)


def test_bad_calls_are_errors_the_agent_can_read() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        assert (await tools["process_start"].call({})).is_error
        assert (await tools["process_output"].call({"id": "proc_9"})).is_error
        assert (await tools["process_write"].call({"id": "proc_1"})).is_error
        assert (await tools["process_kill"].call({})).is_error

    with_tools(body)


def test_the_shell_keeps_directory_variables_and_jobs_between_calls() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        shell = extra_tools(env, ["shell"])[0]
        first = await shell.call({"command": "mkdir sub && cd sub && export GREETING=hi && pwd"})
        assert first.content.endswith("/sub\n[exit code 0]")
        second = await shell.call({"command": "echo $GREETING; pwd; echo err >&2; false"})
        assert second.is_error
        assert second.content.startswith("hi\n")
        assert "err" in second.content
        assert second.content.endswith("[exit code 1]")
        # A background job started in one call is still running in the next.
        await shell.call({"command": "(sleep 30; echo late > late.txt) &"})
        jobs = await shell.call({"command": "jobs | wc -l; kill %1 2>/dev/null; echo done"})
        assert "done" in jobs.content
        # A syntax error is reported without ending the shell.
        broken = await shell.call({"command": "if then"})
        assert broken.is_error
        assert (await shell.call({"command": "echo alive"})).content.startswith("alive")

    with_tools(body)


def test_the_shell_times_out_and_starts_fresh() -> None:
    async def body(tools: dict[str, Tool], env: TaskEnvironment, root: Path) -> None:
        shell = extra_tools(env, ["shell"])[0]
        await shell.call({"command": "export KEEP=1"})
        slow = await shell.call({"command": "sleep 30", "timeout_s": 1})
        assert slow.is_error
        assert "timed out" in slow.content
        fresh = await shell.call({"command": "echo ${KEEP:-gone}"})
        assert fresh.content.startswith("gone")
        exited = await shell.call({"command": "exit 3"})
        assert "the shell exited" in exited.content
        assert (await shell.call({"command": "echo again"})).content.startswith("again")

    with_tools(body)

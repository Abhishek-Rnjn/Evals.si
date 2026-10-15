"""session_search over the unabridged history, and the memory tool."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

from evalsi.types import Usage
from evalsi_harness.archive import Archive, SessionSearchTool
from evalsi_harness.context import STUB, ContextConfig
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.tools import FunctionTool
from evalsi_harness.toolset import MEMORY_MAX_BYTES, MemoryTool, extra_tools
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import calls_then
from tests.test_toolset import with_env


def filled() -> Archive:
    a = Archive()
    a.add({"role": "user", "content": "fix the parser"})
    a.add(
        {
            "role": "assistant",
            "content": "reading",
            "tool_calls": [
                {
                    "id": "c1",
                    "type": "function",
                    "function": {"name": "bash", "arguments": '{"command": "cat cfg.ini"}'},
                }
            ],
        }
    )
    a.add(
        {
            "role": "tool",
            "tool_call_id": "c1",
            "name": "bash",
            "content": "port = 8080\nhost = db.internal\n",
        }
    )
    a.add({"role": "tool", "tool_call_id": "c2", "name": "grep", "content": "no PORT here\n"})
    return a


def search(archive: Archive, **args: Any) -> Any:
    return asyncio.run(SessionSearchTool(archive).call(args))


def test_search_finds_entries_newest_first_with_snippets_and_numbers() -> None:
    r = search(filled(), query="port")
    lines = r.content.splitlines()
    assert lines[0].startswith("#3 [tool grep")
    assert lines[1].startswith("#2 [tool bash")
    assert "port = 8080" in lines[1]
    assert r.metadata["matches"] == 2


def test_search_reads_an_entry_in_full_and_reports_misses_and_bad_input() -> None:
    a = filled()
    assert "host = db.internal" in search(a, index=2).content
    assert search(a, index=2).content.startswith("[entry 2, tool bash]")
    assert "[call bash(" in search(a, index=1).content  # an assistant turn keeps its calls
    assert search(a, index=99).is_error
    assert search(a, query="zebra").content == "no entry matches 'zebra'"
    assert search(a).is_error
    assert search(a, query="   ").is_error
    # The query is text, not a pattern: regex syntax is just characters.
    assert search(a, query="port = (8080").content.startswith("no entry matches")
    assert search(a, query="p.rt").content.startswith("no entry matches")
    assert search(a, query=".*").content.startswith("no entry matches")


def test_a_pathological_query_cannot_stall_the_search() -> None:
    import time

    a = Archive()
    a.add({"role": "tool", "name": "bash", "content": "a" * 16000 + "b"})
    started = time.monotonic()
    r = search(a, query="(a+)+$")  # catastrophic as a regex; just characters here
    assert time.monotonic() - started < 1
    assert r.content.startswith("no entry matches")
    assert search(a, query="AAAAb").content.startswith("#0")  # case-insensitive literal


def test_search_caps_results_and_says_how_many_more() -> None:
    a = Archive()
    for i in range(30):
        a.add({"role": "tool", "name": "bash", "content": f"line {i} needle"})
    r = search(a, query="needle", max_results=5)
    assert len(r.content.splitlines()) == 6
    assert "25 more" in r.content


class Sized:
    """A model with a window, calling blob until told to look something up."""

    name = "sized"

    def __init__(self, lookups: list[ToolCallRequest]) -> None:
        self.calls = 0
        self.lookups = lookups
        self.answers: list[str] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        chars = len(system) + sum(len(str(m.get("content") or "")) for m in messages)
        usage = Usage(input_tokens=chars // 4, output_tokens=5)
        self.calls += 1
        if self.calls <= 40:
            call = ToolCallRequest(f"c{self.calls}", "blob", json.dumps({"n": self.calls}))
            return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage)
        if self.lookups:
            call = self.lookups.pop(0)
            return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage)
        last = [m for m in messages if m["role"] == "tool"][-1]
        self.answers.append(str(last["content"]))
        return ModelTurn(text="done", stop_reason="end_turn", usage=usage)

    async def aclose(self) -> None:
        pass


def test_an_agent_can_recover_a_cleared_result_with_session_search() -> None:
    def blob(n: int) -> str:
        return f"SECRET-{n} " + "x" * 3000

    model = Sized([ToolCallRequest("s1", "session_search", json.dumps({"query": "SECRET-3 "}))])
    loop = AgentLoop(
        model,
        [FunctionTool("blob", blob, input_schema={"type": "object", "properties": {"n": {}}})],
        LoopConfig(
            max_steps=100,
            session_search=True,
            context=ContextConfig(
                strategy="clear-tool-results",
                window_tokens=8000,
                trigger=0.6,
                target=0.3,
                keep_recent=2,
            ),
        ),
        lambda e: None,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    assert final.stop_reason == "completed"
    assert final.extra["context"]["results_cleared"] > 0
    # By then the model had been sent a stub for result 3 ...
    # ... but the archive still has it, so the search found it.
    assert "SECRET-3 " in model.answers[0]
    assert STUB.format(chars=3009, tool="blob") not in model.answers[0]
    assert any(e.text.startswith("SECRET-3 ") for e in loop.archive.entries)


def test_session_search_is_offered_only_when_asked() -> None:
    plain = AgentLoop(Sized([]), [], LoopConfig(), lambda e: None)
    assert "session_search" not in plain.tools
    asked = AgentLoop(Sized([]), [], LoopConfig(session_search=True), lambda e: None)
    assert "session_search" in asked.tools


# --- memory ------------------------------------------------------------------------


def test_memory_writes_appends_reads_lists_and_deletes() -> None:
    async def body(env: Any, root: Path) -> None:
        tool = MemoryTool(env)
        assert (await tool.call({"action": "list"})).content == "no notes yet"
        assert not (
            await tool.call({"action": "write", "name": "progress.md", "content": "a"})
        ).is_error
        await tool.call({"action": "append", "name": "progress.md", "content": "b"})
        assert (await tool.call({"action": "read", "name": "progress.md"})).content == "a\nb"
        await tool.call({"action": "write", "name": "todo.txt", "content": "x"})
        assert (await tool.call({"action": "list"})).content.splitlines() == [
            "progress.md",
            "todo.txt",
        ]
        assert (root / ".evalsi-memory" / "todo.txt").read_text() == "x"
        assert (
            await tool.call({"action": "delete", "name": "todo.txt"})
        ).content == "deleted todo.txt"
        assert (await tool.call({"action": "read", "name": "todo.txt"})).is_error

    with_env({}, body)


def test_memory_refuses_bad_names_oversize_notes_and_too_many_notes() -> None:
    async def body(env: Any, root: Path) -> None:
        tool = MemoryTool(env)
        for bad in ("../x", "a/b", "", ".hidden", "x" * 80, "a b"):
            r = await tool.call({"action": "write", "name": bad, "content": "c"})
            assert r.is_error, bad
        big = "z" * (MEMORY_MAX_BYTES + 1)
        assert (await tool.call({"action": "write", "name": "big", "content": big})).is_error
        assert (await tool.call({"action": "bogus", "name": "a"})).is_error
        assert (await tool.call({"action": "write", "name": "a"})).is_error  # no content
        for i in range(100):
            await tool.call({"action": "write", "name": f"n{i}", "content": "1"})
        assert (await tool.call({"action": "write", "name": "one-more", "content": "1"})).is_error
        # An existing note can still be rewritten at the limit.
        assert not (await tool.call({"action": "write", "name": "n5", "content": "2"})).is_error

    with_env({}, body)


def test_memory_is_a_builtin_name_that_replays_and_session_search_is_the_loops() -> None:
    async def body(env: Any, root: Path) -> None:
        assert [t.spec.name for t in extra_tools(env, ["memory", "session_search"])] == ["memory"]

    with_env({}, body)


def test_memory_survives_in_a_replayed_run_and_the_prompt_mentions_it(
    model: Any, sandboxes: Any, tmp_path: Path
) -> None:
    server = model(
        calls_then(
            [
                {
                    "tool_calls": [
                        ("memory", {"action": "write", "name": "n.md", "content": "hello\n"})
                    ]
                },
                {"tool_calls": [("bash", {"command": "cp .evalsi-memory/n.md out.txt"})]},
                {"text": "ok"},
            ]
        )
    )

    def spec(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {
                    "builtin": {
                        "tools": {"builtin": ["memory", "session_search"]},
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {
                    "checker": {"command": ["sh", "-c", 'test "$(cat out.txt)" = hello']}
                },
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed
    names = [t["function"]["name"] for t in server.requests[0]["tools"]]
    assert names[-2:] == ["memory", "session_search"]
    system = server.requests[0]["messages"][0]["content"]
    assert "memory tool" in system
    assert "session_search" in system
    sent = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.check is not None
    assert again.check.passed  # the note was written again on replay, so out.txt exists

"""Context management: what keeps a 300-step run inside a 200k-token window."""

from __future__ import annotations

import asyncio
import json
import subprocess
from pathlib import Path
from typing import Any

import pytest

from evalsi.types import Usage
from evalsi_harness.cassette import Cassette, CassetteError
from evalsi_harness.context import (
    STUB,
    SUMMARY_PREFIX,
    SUMMARY_SYSTEM,
    ContextConfig,
    ContextView,
    clear_tool_results,
    make_strategy,
    split_for_compaction,
)
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelError, ModelTurn, ToolCallRequest
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.tools import FunctionTool
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then

WINDOW = 200_000
BLOB = 4000  # characters per tool result: 1,000 tokens at 4 characters a token


def total_chars(messages: list[dict[str, Any]], system: str) -> int:
    n = len(system)
    for m in messages:
        n += len(str(m.get("content") or ""))
        for c in m.get("tool_calls") or []:
            n += len(c["function"]["name"]) + len(c["function"]["arguments"])
    return n


class WindowedModel:
    """A model with a real context window. It reports prompt tokens from the
    size of what it was sent, and refuses a request that does not fit, as a
    provider does. It calls the ``blob`` tool for ``steps`` turns, then answers."""

    def __init__(self, steps: int = 300, window: int = WINDOW) -> None:
        self.name = "windowed"
        self.steps = steps
        self.window = window
        self.calls = 0
        self.summaries = 0
        self.peak = 0
        self.sent: list[list[dict[str, Any]]] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        tokens = total_chars(messages, system) // 4
        self.peak = max(self.peak, tokens)
        if tokens > self.window:
            raise ModelError(f"context window exceeded: {tokens} > {self.window}")
        usage = Usage(input_tokens=tokens, output_tokens=20)
        if system == SUMMARY_SYSTEM:
            self.summaries += 1
            return ModelTurn(
                text="Task: read the blobs. Done so far: read many. Next: finish.",
                stop_reason="end_turn",
                usage=usage,
            )
        self.sent.append([dict(m) for m in messages])
        self.calls += 1
        if self.calls > self.steps:
            return ModelTurn(text="finished", stop_reason="end_turn", usage=usage)
        return ModelTurn(
            text="",
            tool_calls=[ToolCallRequest(f"c{self.calls}", "blob", json.dumps({"n": self.calls}))],
            stop_reason="tool_use",
            usage=usage,
        )

    async def aclose(self) -> None:
        pass


def blob(n: int) -> str:
    return (f"[{n}] " + "x" * BLOB)[:BLOB]


def run_loop(model: WindowedModel, strategy: str, **context: Any) -> Any:
    steps: list[Any] = []
    cfg = ContextConfig(strategy=strategy, window_tokens=WINDOW, **context)
    loop = AgentLoop(
        model,
        [FunctionTool("blob", blob, input_schema={"type": "object", "properties": {"n": {}}})],
        LoopConfig(max_steps=1000, context=cfg),
        steps.append,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "read every blob"}]))
    return final, steps, loop


def test_without_a_strategy_the_fixture_overflows_the_window() -> None:
    """The control: the run only passes below because the strategies work."""
    model = WindowedModel()
    with pytest.raises(ModelError, match="context window exceeded"):
        run_loop(model, "none")
    assert model.calls < 300


def test_clearing_keeps_300_steps_inside_the_window() -> None:
    model = WindowedModel()
    final, steps, _ = run_loop(model, "clear-tool-results")
    assert final.stop_reason == "completed"
    assert model.calls == 301
    assert model.peak <= WINDOW
    stats = final.extra["context"]
    assert stats["clears"] > 1
    assert stats["results_cleared"] > 0
    assert stats["tokens_removed"] > 0
    names = [s.step.name for s in steps if s.step.name.startswith("context:")]
    assert names
    assert set(names) == {"context:clear-tool-results"}
    assert final.extra["context"]["compactions"] == 0


def test_compaction_keeps_300_steps_inside_the_window_and_bills_the_summaries() -> None:
    model = WindowedModel()
    # Keep every tool result so that clearing alone cannot make room.
    final, steps, _ = run_loop(model, "compact", keep_recent=100_000)
    assert final.stop_reason == "completed"
    assert model.peak <= WINDOW
    stats = final.extra["context"]
    assert stats["compactions"] >= 1
    assert stats["turns_summarized"] > 0
    assert model.summaries == stats["compactions"]
    # The summary calls are billed but are not steps.
    assert final.extra["steps"] == 301
    assert final.usage.output_tokens == 20 * (301 + model.summaries)
    compact = next(s.step for s in steps if s.step.name == "context:compact")
    assert compact.attributes["evalsi.context.turns_summarized"] > 0
    assert compact.usage is not None
    # After a compaction the conversation is task, summary, and a verbatim tail.
    last = model.sent[-1]
    assert last[0]["content"] == "read every blob"
    assert last[1]["content"].startswith(SUMMARY_PREFIX)
    assert last[2]["role"] == "assistant"


def test_clearing_comes_before_compaction() -> None:
    model = WindowedModel()
    final, steps, _ = run_loop(model, "compact")
    assert final.extra["context"]["clears"] >= 1
    first = next(s.step.name for s in steps if s.step.name.startswith("context:"))
    assert first == "context:clear-tool-results"


def test_a_clear_is_a_batch_down_to_the_target_not_a_trim_every_turn() -> None:
    model = WindowedModel()
    final, steps, _ = run_loop(model, "clear-tool-results")
    clears = [s.step for s in steps if s.step.name == "context:clear-tool-results"]
    # Each batch frees a lot, so there are far fewer clears than steps.
    assert len(clears) < 20
    after = [c.attributes["evalsi.context.tokens_after"] for c in clears]
    assert all(a <= 0.55 * WINDOW for a in after)
    assert all(c.attributes["evalsi.context.results_cleared"] > 10 for c in clears)
    assert final.stop_reason == "completed"


def test_cleared_results_keep_their_message_and_the_recent_ones_stay() -> None:
    model = WindowedModel()
    run_loop(model, "clear-tool-results", keep_recent=4)
    last = model.sent[-1]
    tools = [m for m in last if m["role"] == "tool"]
    calls = [c["id"] for m in last if m["role"] == "assistant" for c in m.get("tool_calls") or []]
    assert [m["tool_call_id"] for m in tools] == calls  # pairing intact
    # Clearing goes in batches, so what came after the last batch is not yet cleared;
    # the oldest are, in an unbroken run from the start.
    flags = [bool(m.get("cleared")) for m in tools]
    assert flags[0]
    assert flags == sorted(flags, reverse=True)
    assert not any(m.get("cleared") for m in tools[-4:])
    assert tools[0]["content"] == STUB.format(chars=BLOB, tool="blob")


def test_the_estimate_follows_what_the_model_reported_not_characters_alone() -> None:
    from evalsi_harness.models import ModelTurn as _T  # noqa: F401

    class Calibrated(WindowedModel):
        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            turn = await super().complete(messages, tools, **kw)
            # This tokenizer needs twice the tokens per character.
            turn.usage.input_tokens = (turn.usage.input_tokens or 0) * 2
            return turn

    view = ContextView(model=WindowedModel(), system="", tools_chars=0, bill=lambda t: None)
    messages = [{"role": "user", "content": "x" * 1000}]
    assert view.tokens(messages) == 300  # no report yet: the default 0.3 a character
    view.observe(messages, 1000)  # the model says 1,000 tokens for 1,000 characters
    assert view.tokens(messages) == 1000
    assert view.tokens([*messages, {"role": "user", "content": "y" * 500}]) == 1500


def test_a_failed_or_cut_off_summary_falls_back_to_clearing() -> None:
    class BadSummaries(WindowedModel):
        async def complete(self, messages: Any, tools: Any, *, system: str = "", **kw: Any) -> Any:
            if system == SUMMARY_SYSTEM:
                self.summaries += 1
                return ModelTurn(text="cut", stop_reason="max_tokens")
            return await super().complete(messages, tools, system=system, **kw)

    model = BadSummaries(steps=190)
    final, _, _ = run_loop(model, "compact", keep_recent=100_000)
    stats = final.extra["context"]
    assert stats["compactions"] == 0
    assert stats["summary_failures"] >= 1
    assert final.stop_reason == "completed"


def test_nothing_to_summarize_means_no_compaction() -> None:
    messages = [{"role": "user", "content": "t"}] + [
        {"role": "assistant", "content": str(i)} for i in range(3)
    ]
    assert split_for_compaction(messages, 6) is None
    assert split_for_compaction(messages, 3) is None  # the tail would start at index 1
    many = [{"role": "user", "content": "t"}] + [
        {"role": "assistant", "content": str(i)} for i in range(10)
    ]
    assert split_for_compaction(many, 4) == 7


def test_clear_tool_results_never_touches_other_messages() -> None:
    messages: list[dict[str, Any]] = [
        {"role": "user", "content": "task"},
        {
            "role": "assistant",
            "content": "",
            "thinking_blocks": [{"type": "thinking", "thinking": "t", "signature": "s"}],
            "tool_calls": [
                {"id": "a", "type": "function", "function": {"name": "bash", "arguments": "{}"}}
            ],
        },
        {"role": "tool", "tool_call_id": "a", "content": "o" * 500, "is_error": True},
        {"role": "tool", "tool_call_id": "b", "content": "tiny"},
    ]
    out, cleared, removed = clear_tool_results(messages, lambda m: False, keep_recent=0)
    assert cleared == 1  # the tiny one is shorter than its stub
    assert removed > 400
    assert out[1] is messages[1]  # the assistant turn, thinking included, is the same object
    assert out[2]["is_error"] is True
    assert out[2]["cleared"] is True
    assert messages[2]["content"] == "o" * 500  # the caller's list is untouched


def test_config_is_validated_and_none_is_the_default() -> None:
    assert ContextConfig().strategy == "none"
    assert make_strategy(ContextConfig()).name == "none"
    for bad in (
        {"strategy": "forget", "window_tokens": 100_000},
        {"strategy": "compact"},
        {"strategy": "compact", "window_tokens": 100_000, "target": 0.9, "trigger": 0.8},
        {"strategy": "compact", "window_tokens": 100_000, "keep_turns": -1},
    ):
        with pytest.raises(ValueError, match=r"strategy|window_tokens|target|negative"):
            ContextConfig(**bad)


# --- through the harness: replay, spill, record ----------------------------------------


def test_a_cassette_only_replays_under_the_strategy_it_was_recorded_with(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    compact = ContextConfig(strategy="compact", window_tokens=50_000).describe()
    tape = Cassette(path, "record", context=compact)
    tape.record(
        {
            "kind": "model",
            "turn": {"text": "hi", "tool_calls": [], "usage": {}, "stop_reason": "end_turn"},
        }
    )
    tape.save()
    assert Cassette(path, "replay", context=compact).next_turn() is not None
    with pytest.raises(CassetteError, match="same strategy and window"):
        Cassette(path, "replay", context={"strategy": "none"})
    other = ContextConfig(strategy="compact", window_tokens=60_000).describe()
    with pytest.raises(CassetteError, match="same strategy and window"):
        Cassette(path, "replay", context=other)
    # A cassette without a context (version 1) is a "none" recording.
    path.write_text(path.read_text().split("\n", 1)[1])
    assert Cassette(path, "replay").next_turn() is not None
    with pytest.raises(CassetteError):
        Cassette(path, "replay", context=compact)


def test_a_compacting_run_replays_exactly(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    """The summary call goes through the recording model, in sequence."""
    steps: list[dict[str, Any]] = [
        {"tool_calls": [("bash", {"command": f"echo {i} >> log.txt"})]} for i in range(14)
    ]
    steps.append({"tool_calls": [("bash", {"command": "echo hello > out.txt"})]})
    steps.append({"text": "done"})
    script = calls_then(steps)

    def with_summaries(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if not tools:  # the compaction call carries no tools
            return {"text": "Summary: appended numbers to log.txt; out.txt still to write."}
        return script(messages, tools)

    server = model(with_summaries)
    server.sized_usage = True
    builtin = {
        "context": {
            "strategy": "compact",
            "window_tokens": 4000,
            "trigger": 0.05,
            "target": 0.03,
            "keep_recent": 1,
            "keep_turns": 3,
        }
    }

    def spec(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {"builtin": {**builtin, "recording": {"mode": mode, "dir": "tapes"}}},
                "environment": {"checker": CHECK_HELLO},
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    agent = first.metadata["agent"]
    assert agent["context"]["compactions"] >= 1
    assert first.check is not None
    assert first.check.passed
    requests = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert again is not None
    assert len(server.requests) == requests  # nothing was sent: the tape served every call
    assert again.metadata["agent"]["context"] == agent["context"]
    assert again.check is not None
    assert again.check.passed


def test_large_bash_output_is_spilled_to_a_file_the_diff_ignores(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    big = "python3 -c \"print('line\\n' * 20000)\""
    server = model(calls_then([]))

    def go(spill: bool) -> tuple[Any, list[dict[str, Any]]]:
        spec = spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {"builtin": {"context": {"spill_outputs": spill}}},
                "environment": {"files": {"a.txt": "1\n"}, "setup": ["git init -q", "git add -A"]},
            }
        )
        server.script = calls_then([{"tool_calls": [("bash", {"command": big})]}, {"text": "ok"}])
        n = len(server.requests)
        rec = run(spec, record(), sandboxes).record
        assert rec is not None
        return rec, server.requests[n:]

    rec, requests = go(spill=True)
    result = next(m for m in requests[1]["messages"] if m["role"] == "tool")["content"]
    assert ".evalsi-out/0001.txt" in result
    assert len(result) < 16_100
    assert "20000" not in result or "more characters" in result
    assert "a.txt" not in rec.metadata.get("diff", "")
    assert ".evalsi-" not in rec.metadata.get("diff", "")

    rec, requests = go(spill=False)  # the default is unchanged: clipped in the middle
    result = next(m for m in requests[1]["messages"] if m["role"] == "tool")["content"]
    assert ".evalsi-out" not in result
    assert "characters omitted" in result


def test_the_pathspec_that_hides_spill_files_matches_a_directory() -> None:
    """The diff excludes ':(exclude).evalsi-*'; check git applies it to a directory."""
    import tempfile

    with tempfile.TemporaryDirectory() as d:

        def run_git(*args: str) -> subprocess.CompletedProcess[str]:
            return subprocess.run(
                ["git", "-C", d, *args], check=True, capture_output=True, text=True
            )

        run_git("init", "-q")
        (Path(d) / "a.txt").write_text("1\n")
        (Path(d) / ".evalsi-out").mkdir()
        (Path(d) / ".evalsi-out" / "0001.txt").write_text("spill\n")
        run_git("add", "-A", "--", ".", ":(exclude).evalsi-*")
        assert run_git("diff", "--cached", "--name-only").stdout.split() == ["a.txt"]

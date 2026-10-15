"""fresh-rounds: each round a new agent on the task and its own notes."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

import pytest

from evalsi.types import Usage
from evalsi_harness.context import ROUND_DONE, ContextConfig
from evalsi_harness.events import StepEvent
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.tools import FunctionTool
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO


def spec(server: Any, rounds: int = 3, round_steps: int = 10, **recording: Any) -> Any:
    builtin: dict[str, Any] = {"context": {"strategy": "fresh-rounds", "rounds": rounds}}
    builtin["context"]["round_steps"] = round_steps
    if recording:
        builtin["recording"] = recording
    return spec_of(
        {
            "target": {"connector": "openai-compatible", "model": "m", "base_url": server.base_url},
            "harness": {"builtin": builtin},
            "environment": {"checker": CHECK_HELLO},
        }
    )


def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
    """Round 1 does a first part and stops without saying it is done. Round 2 is
    told its notes, finishes, and says so."""
    task_round = next(
        (m["content"] for m in messages if m["role"] == "user" and "Round " in str(m["content"])),
        "",
    )
    results = [m for m in messages if m["role"] == "tool"]
    if not task_round:
        if not results:
            note = {"action": "write", "name": "progress.md", "content": "part one: log.txt made"}
            return {"tool_calls": [("memory", note)]}
        if len(results) == 1:
            return {"tool_calls": [("bash", {"command": "echo started > log.txt"})]}
        return {"text": "Part one is done."}  # no TASK_COMPLETE: the round ends, the run goes on
    if not results:
        return {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}
    return {"text": f"Everything is finished. {ROUND_DONE}"}


def test_a_run_finishes_over_two_rounds_each_starting_clean(
    model: Any, sandboxes: Any, tmp_path: Path
) -> None:
    server = model(script)
    rec = run(spec(server), record(), sandboxes, base_dir=tmp_path).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    agent = rec.metadata["agent"]
    assert agent["stop_reason"] == "completed"
    assert agent["context"]["rounds"] == 1  # one fresh start after the first round
    # The first request of round two holds the system prompt, the task and the opening, and
    # nothing of round one's conversation.
    second = next(
        r
        for r in server.requests
        if any("Round 2 of 3" in str(m["content"]) for m in r["messages"])
    )
    roles = [m["role"] for m in second["messages"]]
    assert roles == ["system", "user", "user"]
    opening = second["messages"][2]["content"]
    assert "part one: log.txt made" in opening  # the notes carried over
    assert ROUND_DONE in opening
    # The files carried over too.
    assert rec.trajectory is not None
    assert any(s.name == "context:round" for s in rec.trajectory.steps)
    system = server.requests[0]["messages"][0]["content"]
    assert "progress.md" in system
    assert ROUND_DONE in system
    names = [t["function"]["name"] for t in server.requests[0]["tools"]]
    assert "memory" in names  # added for the notes, though the spec did not name it


def test_a_round_ends_at_its_step_limit_and_the_next_one_carries_on(
    model: Any, sandboxes: Any, tmp_path: Path
) -> None:
    def endless(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        in_round_two = any("Round 2 of" in str(m["content"]) for m in messages)
        n = sum(1 for m in messages if m["role"] == "tool")
        if in_round_two and n >= 1:
            return {"text": f"done {ROUND_DONE}"}
        return {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}

    server = model(endless)
    rec = run(spec(server, rounds=2, round_steps=3), record(), sandboxes, base_dir=tmp_path).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    # Round one made its 3 calls and was cut; round two needed 2.
    assert rec.metadata["agent"]["steps"] == 5
    assert rec.metadata["agent"]["stop_reason"] == "completed"


def test_running_out_of_rounds_is_a_stop_that_names_the_marker(
    model: Any, sandboxes: Any, tmp_path: Path
) -> None:
    def never(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        return {"text": "I think that is all."}

    server = model(never)
    rec = run(spec(server, rounds=2), record(), sandboxes, base_dir=tmp_path).record
    assert rec is not None
    agent = rec.metadata["agent"]
    assert agent["stop_reason"] == "max_steps"
    assert "2 rounds ended without TASK_COMPLETE" in agent["error"]
    assert agent["context"]["rounds"] == 1
    assert rec.check is not None
    assert not rec.check.passed  # and it is still graded


def test_a_run_over_rounds_replays_exactly(model: Any, sandboxes: Any, tmp_path: Path) -> None:
    server = model(script)
    first = run(spec(server, mode="record", dir="tapes"), record(), sandboxes, base_dir=tmp_path)
    assert first.record is not None
    assert first.record.check is not None
    assert first.record.check.passed
    sent = len(server.requests)
    again = run(spec(server, mode="replay", dir="tapes"), record(), sandboxes, base_dir=tmp_path)
    assert len(server.requests) == sent
    assert again.record is not None
    assert again.record.check is not None
    assert again.record.check.passed  # the notes were rebuilt, so round two saw them again
    assert again.record.metadata["agent"]["context"]["rounds"] == 1
    # A log recorded this way is refused under another strategy.
    from evalsi_harness.cassette import Cassette, CassetteError

    tape = tmp_path / "tapes" / "t1.0.jsonl"
    nine = ContextConfig(strategy="fresh-rounds", rounds=9).describe()
    with pytest.raises(CassetteError, match="same strategy"):
        Cassette(tape, "replay", context=nine)


# --- the loop on its own ----------------------------------------------------------------


class Said:
    name = "said"

    def __init__(self, *texts: str) -> None:
        self.texts = list(texts)
        self.seen: list[list[dict[str, Any]]] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.seen.append([dict(m) for m in messages])
        return ModelTurn(
            text=self.texts.pop(0),
            stop_reason="end_turn",
            usage=Usage(input_tokens=10, output_tokens=2),
        )

    async def aclose(self) -> None:
        pass


def test_the_loop_hands_each_round_the_notes_the_progress_reader_returns() -> None:
    notes = ["found the bug", "tests pass"]

    async def progress() -> str:
        return notes.pop(0) if notes else ""

    model = Said("round one", "round two", f"all done {ROUND_DONE}")
    events: list[Any] = []
    loop = AgentLoop(
        model,
        [],
        LoopConfig(context=ContextConfig(strategy="fresh-rounds", rounds=4, round_steps=5)),
        events.append,
        progress=progress,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "task"}]))
    assert final.stop_reason == "completed"
    assert final.output is not None
    assert final.output.as_text().endswith(ROUND_DONE)
    second, third = model.seen[1][-1]["content"], model.seen[2][-1]["content"]
    assert "found the bug" in second
    assert "tests pass" in third
    rounds = [e.step for e in events if isinstance(e, StepEvent) and e.step.name == "context:round"]
    assert [s.attributes["evalsi.context.round"] for s in rounds] == [2, 3]


def test_a_round_with_no_notes_is_told_so() -> None:
    async def nothing() -> str:
        return "  "

    model = Said("first", f"done {ROUND_DONE}")
    loop = AgentLoop(
        model,
        [],
        LoopConfig(context=ContextConfig(strategy="fresh-rounds", rounds=3, round_steps=5)),
        lambda e: None,
        progress=nothing,
    )
    asyncio.run(loop.run([{"role": "user", "content": "task"}]))
    assert "(none: nothing was written to progress.md)" in model.seen[1][-1]["content"]


def test_budgets_and_errors_end_the_run_not_just_the_round() -> None:
    model = Said("one", "two", "three")
    loop = AgentLoop(
        model,
        [FunctionTool("t", lambda: "x", input_schema={"type": "object"})],
        LoopConfig(
            max_steps=2, context=ContextConfig(strategy="fresh-rounds", rounds=5, round_steps=5)
        ),
        lambda e: None,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "task"}]))
    # Two model calls is the whole run's limit, however many rounds are left.
    assert final.stop_reason == "max_steps"
    assert len(model.seen) == 2


def test_the_config_validates_rounds_and_describes_only_what_a_replay_depends_on() -> None:
    with pytest.raises(ValueError, match="rounds"):
        ContextConfig(strategy="fresh-rounds", rounds=0)
    assert ContextConfig(strategy="fresh-rounds").describe() == {
        "strategy": "fresh-rounds",
        "rounds": 5,
        "round_steps": 30,
    }
    ContextConfig(strategy="fresh-rounds")  # no window needed
    assert ToolCallRequest("a", "b", "{}")  # keeps the import honest


def test_the_exit_criterion_holds_for_300_steps_in_a_200k_window_over_rounds() -> None:
    """The control (no strategy) overflows this window; ten rounds of thirty steps do not."""
    from tests.test_context import WINDOW, WindowedModel

    class Finishing(WindowedModel):
        async def complete(self, messages: Any, tools: Any, **kw: Any) -> ModelTurn:
            turn = await super().complete(messages, tools, **kw)
            if not turn.tool_calls and kw.get("system") != "":
                turn.text = f"finished {ROUND_DONE}"
            return turn

    model = Finishing()
    events: list[Any] = []
    loop = AgentLoop(
        model,
        [FunctionTool("blob", lambda n: "x" * 4000, input_schema={"type": "object"})],
        LoopConfig(
            max_steps=1000,
            context=ContextConfig(strategy="fresh-rounds", rounds=12, round_steps=30),
        ),
        events.append,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "read every blob"}]))
    assert final.stop_reason == "completed"
    assert model.calls == 301
    assert model.peak <= WINDOW  # each round's context was a fraction of the window
    assert model.peak < WINDOW // 4
    assert final.extra["context"]["rounds"] == 10  # eleven rounds, ten fresh starts

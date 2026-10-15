"""Fixes from the review of the H2 pull request, each with the case that found it."""

from __future__ import annotations

import asyncio
import inspect
import json
import os
from pathlib import Path
from typing import Any

import pytest

from evalsi.types import Usage
from evalsi_harness import harness as harness_module
from evalsi_harness.archive import SessionSearchTool
from evalsi_harness.cassette import Cassette, CassetteError
from evalsi_harness.context import SUMMARY_SYSTEM, ContextConfig
from evalsi_harness.evidence import (
    MAX_ENTRIES,
    MAX_HISTORY,
    EvidenceState,
    EvidenceStateStrategy,
)
from evalsi_harness.harness import BuiltinHarness, _elicitation, _hook_identity
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest, ToolSpec
from evalsi_harness.tools import FunctionTool
from evalsi_harness.toolsearch import ToolSearchTool
from tests.harness_support import spec_of


def entry(text: str = "hi") -> dict[str, Any]:
    return {"kind": "model", "turn": {"text": text, "stop_reason": "end_turn"}}


# --- the session log is never truncated -------------------------------------------------


def test_repairing_a_log_replaces_it_whole_so_a_kill_mid_repair_loses_nothing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record")
    for i in range(3):
        tape.record(entry(f"turn {i}"))
    tape.save()
    with path.open("a") as f:
        f.write('{"id": 9, "kind": "model", "turn": {"tex')  # killed mid-write
    before = path.read_text()

    def killed(src: Any, dst: Any) -> None:
        raise OSError("killed during the swap")

    monkeypatch.setattr(os, "replace", killed)
    with pytest.raises(OSError, match="killed"):
        Cassette(path, "resume")
    assert path.read_text() == before  # the whole old log is still there, not a truncated one
    monkeypatch.undo()
    resumed = Cassette(path, "resume")
    assert [json.loads(ln)["id"] for ln in path.read_text().splitlines()[1:]] == [0, 1, 2]
    assert resumed.next_turn() is not None


def test_a_clean_log_is_not_rewritten_on_resume(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record")
    tape.record(entry())
    tape.save()
    stamp = path.stat().st_mtime_ns
    Cassette(path, "resume")
    assert path.stat().st_mtime_ns == stamp
    assert not list(tmp_path.glob("*.tmp"))


def test_saving_twice_or_recording_after_a_save_keeps_every_entry(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record")
    tape.record(entry("first"))
    tape.save()
    tape.save()  # idempotent: no second header, nothing recreated
    tape.record(entry("second"))  # reopens by appending
    tape.save()
    tape.save()
    kinds = [json.loads(ln)["kind"] for ln in path.read_text().splitlines()]
    assert kinds == ["header", "model", "model"]
    texts = [json.loads(ln)["turn"]["text"] for ln in path.read_text().splitlines()[1:]]
    assert texts == ["first", "second"]
    empty = Cassette(tmp_path / "e.jsonl", "record")
    empty.save()
    empty.save()
    assert (tmp_path / "e.jsonl").read_text().count("header") == 1


# --- the replay guard -------------------------------------------------------------------


def test_spilling_is_part_of_what_a_recording_depends_on(tmp_path: Path) -> None:
    assert ContextConfig().describe() == {"strategy": "none"}  # the default is unchanged
    assert ContextConfig(spill_outputs=True).describe() == {
        "strategy": "none",
        "spill_outputs": True,
    }
    compact = ContextConfig(strategy="compact", window_tokens=50_000)
    assert "spill_outputs" not in compact.describe()
    assert ContextConfig(strategy="compact", window_tokens=50_000, spill_outputs=True).describe()[
        "spill_outputs"
    ]
    rounds = ContextConfig(strategy="fresh-rounds", spill_outputs=True).describe()
    assert rounds["spill_outputs"] is True
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record", context=ContextConfig(spill_outputs=True).describe())
    tape.record(entry())
    tape.save()
    with pytest.raises(CassetteError, match="same strategy"):
        Cassette(path, "replay", context=ContextConfig().describe())


def test_hook_configuration_is_part_of_the_guard(tmp_path: Path) -> None:
    class Cfg:
        def __init__(self, python: str, config: dict[str, Any] | None) -> None:
            self.python = python
            self._config = config

        def HasField(self, name: str) -> bool:
            return self._config is not None

        @property
        def config(self) -> Any:
            from google.protobuf import json_format, struct_pb2

            msg = struct_pb2.Struct()
            json_format.ParseDict(self._config or {}, msg)
            return msg

    assert _hook_identity(Cfg("m:Gate", None)) == "m:Gate"  # no config: the old identity
    ban = _hook_identity(Cfg("m:Gate", {"ban": "rm"}))
    assert ban == {"python": "m:Gate", "config": {"ban": "rm"}}
    other = _hook_identity(Cfg("m:Gate", {"ban": "curl"}))
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record", hooks=[ban])
    tape.record(entry())
    tape.save()
    assert Cassette(path, "replay", hooks=[ban]).next_turn() is not None
    with pytest.raises(CassetteError, match="same hooks"):
        Cassette(path, "replay", hooks=[other])
    with pytest.raises(CassetteError, match="same hooks"):
        Cassette(path, "replay", hooks=["m:Gate"])


# --- the adapters' extension point ------------------------------------------------------


def test_tools_keeps_the_signature_subclasses_override() -> None:
    """The tau-bench adapter overrides _tools(self, live); a new argument would break it."""
    assert list(inspect.signature(BuiltinHarness._tools).parameters) == ["self", "live"]
    assert "user" in {f for f in harness_module.LiveTask.__dataclass_fields__}


# --- the evidence list ------------------------------------------------------------------


def state_reading(*docs: str) -> EvidenceState:
    from evalsi_harness.archive import Archive

    archive = Archive()
    for doc in docs:
        archive.add({"role": "tool", "name": "read", "content": doc})
    return EvidenceState(archive)


def test_corrections_cannot_fill_the_list_and_retracting_frees_room() -> None:
    doc = " ".join(f"fact number {i} is stated here plainly." for i in range(400))
    s = state_reading(doc)
    for i in range(MAX_ENTRIES):
        assert s.add(f"f{i}", "d", f"fact number {i} is stated here plainly.")[0]
    err = s.add("one more", "d", "fact number 300 is stated here plainly.")[1]
    assert err is not None
    assert "active entries" in err
    # A correction replaces an active entry, so it works at capacity ...
    new, err = s.supersede("E1", "f0 corrected", "d", "fact number 301 is stated here plainly.")
    assert err is None
    assert new is not None
    # ... and retracting really does make room.
    assert s.retract("E2", "wrong") is None
    assert s.add("fits now", "d", "fact number 302 is stated here plainly.")[0]
    # History is bounded: a long run of corrections does not grow the list for ever.
    for i in range(MAX_HISTORY + 30):
        eid = next(e.id for e in s.evidence.values() if e.status == "active")
        assert s.retract(eid, f"r{i}") is None
        assert s.add(f"again {i}", "d", f"fact number {i} is stated here plainly.")[0]
    assert sum(1 for e in s.evidence.values() if e.status != "active") <= MAX_HISTORY
    assert len(s.evidence) <= MAX_ENTRIES + MAX_HISTORY


def test_entries_and_conclusions_are_bounded() -> None:
    s = state_reading("A passage that is long enough to quote from, for the tests.")
    quote = "passage that is long enough"
    assert s.add("x" * 501, "d", quote)[1] is not None
    assert s.add("ok", "s" * 201, quote)[1] is not None
    assert s.add("ok", "d", quote)[0] == "E1"
    assert s.claim(None, "c" * 501, ["E1"], [])[1] is not None
    assert s.claim(None, "c", ["E1"], ["g"] * 11)[1] is not None
    assert s.claim(None, "c", ["E1"], ["g" * 301])[1] is not None
    for i in range(50):
        assert s.claim(None, f"claim {i}", [], [])[0]
    assert "at most 50" in (s.claim(None, "too many", [], [])[1] or "")
    # Retractions append a gap, but never past the cap.
    first = next(iter(s.claims))
    s.claim(first, "", ["E1"], [f"gap {i}" for i in range(10)])
    s.retract("E1", "gone")
    assert len(s.claims[first].gaps) <= 10


def test_the_state_shown_with_every_call_is_held_to_a_share_of_the_window() -> None:
    doc = " ".join(f"entry {i} is written down in this document." for i in range(300))
    s = state_reading(doc)
    for i in range(150):
        s.add(f"A fairly long statement about entry {i} " * 3, "document", f"entry {i} is written")
    s.claim(None, "The conclusion.", ["E1"], ["still open"])
    full = s.render()
    assert len(full) > 15_000
    small = s.render(2_000)
    assert len(small) <= 2_000
    assert "The conclusion." in small
    assert "open gap: still open" in small  # what steers the search stays
    assert "older entries are not shown" in small
    # The strategy asks for a share of its window, so the state cannot crowd the conversation out.
    from evalsi_harness.context import ContextView

    strategy = EvidenceStateStrategy(
        ContextConfig(strategy="evidence-state", window_tokens=4_000, trigger=0.9, target=0.5), s
    )
    view = ContextView(model=None, system="", tools_chars=0, bill=lambda t: None)  # type: ignore[arg-type]
    result = asyncio.run(strategy.prepare([{"role": "user", "content": "task"}], view))
    state_tokens = view.tokens(result.transient)
    assert state_tokens <= 4_000 * 0.25 + 50


# --- compaction fits the window ---------------------------------------------------------


def test_a_compaction_request_fits_a_small_window() -> None:
    from evalsi_harness.context import Compact, ContextView

    class Recording:
        name = "m"

        def __init__(self) -> None:
            self.max_tokens = 0
            self.prompt_chars = 0

        async def complete(
            self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
        ) -> ModelTurn:
            self.max_tokens = max_tokens
            self.prompt_chars = len(system) + sum(len(m["content"]) for m in messages)
            return ModelTurn(text="summary", stop_reason="end_turn", usage=Usage(10, 5))

        async def aclose(self) -> None:
            pass

    model = Recording()
    window = 4_000
    cfg = ContextConfig(strategy="compact", window_tokens=window, keep_recent=100_000, keep_turns=2)
    messages: list[dict[str, Any]] = [{"role": "user", "content": "task"}]
    for i in range(30):
        messages.append(
            {
                "role": "assistant",
                "content": "",
                "tool_calls": [
                    {
                        "id": f"c{i}",
                        "type": "function",
                        "function": {"name": "t", "arguments": "{}"},
                    }
                ],
            }
        )
        messages.append({"role": "tool", "tool_call_id": f"c{i}", "content": "x" * 1500})
    view = ContextView(model=model, system="", tools_chars=0, bill=lambda t: None)
    view.observe(messages, input_tokens=sum(len(m["content"]) for m in messages) // 4)
    out = asyncio.run(Compact(cfg).prepare(messages, view))
    assert out.steps
    # The default asks for 4,096 tokens, more than the whole window: the request is clamped.
    assert model.max_tokens <= window // 4
    prompt_tokens = model.prompt_chars * view.ratio()
    assert prompt_tokens + model.max_tokens <= window
    assert SUMMARY_SYSTEM  # the framing is counted in the budget


# --- tool search and sub-agents ---------------------------------------------------------


def test_a_tool_without_a_description_can_be_found_and_loaded() -> None:
    class Bare:
        deferred = True

        def __init__(self, name: str) -> None:
            self.spec = ToolSpec(name, "", {"type": "object"})

        async def call(self, args: dict[str, Any], *, span: str = "") -> Any:
            raise AssertionError

    loop = AgentLoop(
        type("M", (), {"name": "m"})(),
        [Bare("weather_lookup"), Bare("mail_send")],
        LoopConfig(),
        lambda e: None,
    )
    search = ToolSearchTool(loop)
    result = asyncio.run(search.call({"query": "weather"}))
    assert not result.is_error
    assert result.content.endswith("- weather_lookup")
    assert "weather_lookup" in loop.active


def test_a_subagent_searches_its_own_history_not_the_parents() -> None:
    seen: dict[str, Any] = {}

    class Router:
        name = "r"

        def __init__(self) -> None:
            self.calls = 0

        async def complete(
            self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
        ) -> ModelTurn:
            self.calls += 1
            names = [t.name for t in tools]
            usage = Usage(input_tokens=10, output_tokens=2)
            if "Do only the task" in system:
                results = [m for m in messages if m["role"] == "tool"]
                seen["sub_tools"] = names
                if not results:
                    call = ToolCallRequest("s1", "session_search", '{"query": "parent-secret"}')
                    return ModelTurn(
                        text="", tool_calls=[call], stop_reason="tool_use", usage=usage
                    )
                seen["sub_result"] = results[-1]["content"]
                return ModelTurn(text="report", stop_reason="end_turn", usage=usage)
            if not any(m["role"] == "tool" for m in messages):
                call = ToolCallRequest("p0", "note", "{}")
                return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage)
            if not any(m["tool_call_id"] == "d1" for m in messages if m["role"] == "tool"):
                call = ToolCallRequest("d1", "delegate", '{"task": "look"}')
                return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage)
            return ModelTurn(text="done", stop_reason="end_turn", usage=usage)

        async def aclose(self) -> None:
            pass

    loop = AgentLoop(
        Router(),
        [FunctionTool("note", lambda: "parent-secret", input_schema={"type": "object"})],
        LoopConfig(delegate_steps=5, session_search=True),
        lambda e: None,
    )
    asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    assert "session_search" in seen["sub_tools"]
    assert "delegate" not in seen["sub_tools"]
    # "parent-secret" is in the parent's history only: the sub-agent's own search finds nothing.
    assert "tool note" not in seen["sub_result"]  # the parent's tool result is not there
    parent_search = [t for t in loop.sub_tools() if isinstance(t, SessionSearchTool)]
    assert parent_search == []  # the loop passes on only what its caller gave it


# --- elicitation and the simulated user -------------------------------------------------


def test_an_elicitation_answer_does_not_spend_the_conversations_user_turns() -> None:
    class User:
        def __init__(self) -> None:
            self.turns = 3
            self.calls = 0

        async def reply(self, conversation: list[dict[str, Any]]) -> Any:
            self.turns += 1  # what the real simulator does
            self.calls += 1
            return type("R", (), {"message": "staging", "done": False, "usage": Usage()})()

    async def wait(awaitable: Any) -> Any:
        return await awaitable

    user = User()
    answer = _elicitation(user)  # type: ignore[arg-type]
    schema = {"properties": {"env": {"type": "string"}}, "required": ["env"]}
    out: dict[str, Any] = asyncio.run(wait(answer("Which environment?", schema)))
    assert out == {"action": "accept", "content": {"env": "staging"}}
    assert user.turns == 3  # unchanged: a replay would not take this turn either
    for _ in range(9):
        asyncio.run(wait(answer("again", schema)))
    assert user.calls == 10
    assert asyncio.run(wait(answer("once too often", schema))) == {"action": "cancel"}
    assert user.calls == 10  # the cap is on elicitations, not on the conversation


def test_the_live_task_carries_the_simulated_user() -> None:
    spec = spec_of({"harness": {"builtin": {}}})
    assert spec.harness.builtin is not None

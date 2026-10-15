"""evidence-state: structured state outside the conversation."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

from evalsi.types import Usage
from evalsi_harness.archive import Archive
from evalsi_harness.context import ContextConfig
from evalsi_harness.evidence import STATE_HEADER, EvidenceState, EvidenceTool
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.tools import FunctionTool
from tests.harness_support import record, run, spec_of
from tests.test_context import BLOB, WINDOW, WindowedModel
from tests.test_context import run_loop as run_context_loop

DOC = "The Treaty of Aurelia was signed in 1847 by delegates from three states. It ended the war."


def state_with_doc() -> EvidenceState:
    archive = Archive()
    archive.add({"role": "user", "content": "When was the Treaty of Aurelia signed?"})
    archive.add({"role": "tool", "name": "read_doc", "content": DOC})
    archive.add({"role": "assistant", "content": "I recall it was signed in 1850."})
    return EvidenceState(archive)


def test_evidence_must_quote_something_the_agent_read() -> None:
    s = state_with_doc()
    eid, err = s.add("It was signed in 1847.", "doc-1", "signed in 1847 by delegates")
    assert (eid, err) == ("E1", None)
    # Whitespace does not matter; the words do.
    assert s.add("Three states.", "doc-1", "delegates   from\nthree states")[0] == "E2"
    bad, err = s.add("It was signed in 1850.", "memory", "signed in 1850 by the council")
    assert bad is None
    assert err is not None
    assert "was not found in anything you have read" in err
    # The agent's own words are not evidence, and neither is a made-up source quote.
    assert s.add("1850", "me", "I recall it was signed in 1850")[0] is None
    # The task counts as read.
    assert s.add("The question asks about Aurelia.", "task", "When was the Treaty of Aurelia")[0]
    assert s.add("x", "src", "too short")[0] is None
    assert s.add("", "src", "signed in 1847 by delegates")[0] is None
    assert s.add("x", "", "signed in 1847 by delegates")[0] is None
    assert s.add("x", "src", "q" * 700)[0] is None


def test_a_correction_supersedes_or_retracts_instead_of_appending() -> None:
    s = state_with_doc()
    s.archive.add(
        {
            "role": "tool",
            "name": "read_doc",
            "content": "Corrigendum: the Treaty of Aurelia was signed in 1848.",
        }
    )
    e1, _ = s.add("Signed in 1847.", "doc-1", "signed in 1847 by delegates")
    c1, _ = s.claim(None, "The treaty was signed in 1847.", [e1 or ""], [])
    assert s.claims[c1 or ""].status == "supported"
    e2, err = s.supersede("E1", "Signed in 1848.", "doc-2", "signed in 1848")
    assert (e2, err) == ("E2", None)
    assert s.evidence["E1"].status == "superseded"
    assert s.evidence["E1"].note == "superseded by E2"
    assert s.claims["C1"].support == ["E2"]  # the claim follows the correction
    assert s.supersede("E1", "again", "d", "signed in 1848")[1] == "E1 is not an active entry"
    assert s.counts() == {
        "evidence_active": 1,
        "evidence_superseded": 1,
        "evidence_retracted": 0,
        "claims": 1,
        "claims_open": 0,
    }
    # A failed supersede leaves the old entry standing.
    assert s.supersede("E2", "x", "d", "not in anything read")[0] is None
    assert s.evidence["E2"].status == "active"
    # Retracting the only support reopens the claim with a gap saying why.
    assert s.retract("E2", "the corrigendum is itself disputed") is None
    assert s.claims["C1"].status == "open"
    assert "E2 was retracted" in s.claims["C1"].gaps[0]
    assert s.retract("E2", "again") == "E2 is not an active entry"
    assert s.retract("E9", "x") == "E9 is not an active entry"


def test_claims_track_support_and_open_gaps() -> None:
    s = state_with_doc()
    s.add("Signed in 1847.", "doc-1", "signed in 1847 by delegates")
    cid, _ = s.claim(None, "The treaty ended the war in 1847.", ["E1"], ["who ratified it?"])
    assert s.claims[cid or ""].status == "open"  # a gap keeps it open
    s.claim(cid, "", ["E1"], [])
    assert s.claims["C1"].status == "supported"
    assert s.claim(None, "x", ["E7"], [])[1] == "E7 are not active evidence entries"
    assert s.claim("C9", "x", [], [])[1] == "there is no conclusion C9"
    # A claim with no support is open.
    other, _ = s.claim(None, "Something to find out.", [], [])
    assert s.claims[other or ""].status == "open"


def test_the_rendered_state_shows_what_is_valid_and_what_to_do_next() -> None:
    s = state_with_doc()
    assert not s
    s.add("Signed in 1847.", "doc-1", "signed in 1847 by delegates")
    s.add("Three states took part.", "doc-1", "delegates from three states")
    s.retract("E2", "it was four")
    s.claim(None, "Date of signing.", ["E1"], ["confirm with a second source"])
    out = s.render()
    assert out.startswith(STATE_HEADER)
    assert 'E1 [doc-1] Signed in 1847.\n    "signed in 1847 by delegates"' in out
    assert "No longer valid: E2 (retracted: it was four)" in out
    assert "C1 OPEN: Date of signing. supported by E1" in out
    assert "open gap: confirm with a second source" in out
    assert "Next: search to close the open gaps" in out
    assert "E2 [" not in out  # a retracted entry is not listed as evidence


def call(tool: EvidenceTool, **args: Any) -> Any:
    return asyncio.run(tool.call(args))


def test_the_tool_exposes_every_action_and_rejects_bad_input() -> None:
    tool = EvidenceTool(state_with_doc())
    assert call(tool, action="view").content.endswith("Evidence: none yet.")
    ok = call(tool, action="add", statement="s", source="doc", quote="signed in 1847 by delegates")
    assert ok.content == "added E1"
    assert call(
        tool, action="add", statement="s", source="doc", quote="nowhere to be found"
    ).is_error
    assert (
        call(tool, action="claim", claim="c", support=["E1"], gaps=[]).content == "C1 is supported"
    )
    assert call(tool, action="claim", claim="c", support="E1").content == "C2 is open"  # not a list
    assert call(tool, action="claim").is_error
    assert call(tool, action="retract", id="E1").is_error  # no reason
    assert call(tool, action="retract", id="E1", reason="wrong").content == "retracted E1"
    assert call(
        tool, action="supersede", id="E1", statement="s", source="d", quote="q" * 10
    ).is_error
    assert call(tool, action="nonsense").is_error
    assert call(tool).is_error


# --- the strategy -----------------------------------------------------------------------


class Reader:
    """Reads a document each turn, notes a finding about it, and answers from the notes."""

    name = "reader"

    def __init__(self, docs: int) -> None:
        self.docs = docs
        self.turn = 0
        self.states: list[str] = []
        self.sizes: list[int] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        chars = len(system) + sum(len(str(m.get("content") or "")) for m in messages)
        self.sizes.append(chars // 4)
        # What the agent was shown of its own state on this call.
        shown = [
            m["content"] for m in messages if str(m.get("content")).startswith("[Your evidence")
        ]
        self.states.append(shown[-1] if shown else "")
        usage = Usage(input_tokens=chars // 4, output_tokens=5)
        self.turn += 1
        n = (self.turn + 1) // 2
        if n > self.docs:
            return ModelTurn(text="final answer", stop_reason="end_turn", usage=usage)
        if self.turn % 2 == 1:
            call = ToolCallRequest(f"r{n}", "read_doc", json.dumps({"n": n}))
        else:
            args = {
                "action": "add",
                "statement": f"Document {n} states fact {n}.",
                "source": f"doc-{n}",
                "quote": f"FACT-{n} is established here",
            }
            call = ToolCallRequest(f"e{n}", "evidence", json.dumps(args))
        return ModelTurn(text="", tool_calls=[call], stop_reason="tool_use", usage=usage)

    async def aclose(self) -> None:
        pass


def doc(n: int) -> str:
    return f"FACT-{n} is established here. " + "filler " * 400


def test_the_state_is_shown_every_call_and_survives_the_conversation_being_cut() -> None:
    model = Reader(docs=40)
    events: list[Any] = []
    loop = AgentLoop(
        model,
        [FunctionTool("read_doc", doc, input_schema={"type": "object", "properties": {"n": {}}})],
        LoopConfig(
            max_steps=500,
            context=ContextConfig(
                strategy="evidence-state",
                window_tokens=20_000,
                trigger=0.7,
                target=0.4,
                keep_recent=100_000,
                keep_turns=3,
            ),
        ),
        events.append,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "collect the facts"}]))
    assert final.stop_reason == "completed"
    # 40 documents of ~700 tokens do not fit a 20k window; the run stayed inside it.
    assert max(model.sizes) <= 20_000
    assert final.extra["context"]["compactions"] >= 1  # the conversation was cut at least once
    assert final.extra["context"]["turns_summarized"] > 0
    # Every one of the 40 findings was still on screen at the end, from the state alone.
    last = model.states[-1]
    assert all(f"E{n} [doc-{n}]" in last for n in range(1, 41))
    assert final.extra["evidence"]["evidence_active"] == 40
    # The state grows as findings are added, and is empty before the first.
    assert model.states[0] == ""
    assert "E1 [doc-1]" in model.states[3]
    steps = [
        e.step for e in events if hasattr(e, "step") and e.step.name == "context:evidence-state"
    ]
    assert steps
    assert steps[0].attributes["evalsi.context.evidence_active"] >= 1
    # The state is not in the conversation itself: nothing in the archive carries it.
    assert not any(STATE_HEADER in e.text for e in loop.archive.entries)


def test_the_conversation_is_cut_to_the_task_and_the_latest_turns() -> None:
    model = Reader(docs=40)
    seen: list[list[dict[str, Any]]] = []
    original = model.complete

    async def spy(messages: Any, tools: Any, **kw: Any) -> ModelTurn:
        seen.append([dict(m) for m in messages])
        return await original(messages, tools, **kw)

    model.complete = spy  # type: ignore[method-assign]
    loop = AgentLoop(
        model,
        [FunctionTool("read_doc", doc, input_schema={"type": "object", "properties": {"n": {}}})],
        LoopConfig(
            max_steps=500,
            context=ContextConfig(
                strategy="evidence-state",
                window_tokens=20_000,
                trigger=0.7,
                target=0.4,
                keep_recent=100_000,
                keep_turns=3,
            ),
        ),
        lambda e: None,
    )
    asyncio.run(loop.run([{"role": "user", "content": "collect the facts"}]))
    after = seen[-1]
    assert after[0]["content"] == "collect the facts"
    # Cut down: what grew since the last cut, not all 80 turns of work.
    assert len(after) < 40


def test_the_exit_criterion_holds_for_300_steps_in_a_200k_window() -> None:
    model = WindowedModel()
    final, _, _ = run_context_loop(model, "evidence-state", keep_recent=100_000)
    assert final.stop_reason == "completed"
    assert model.peak <= WINDOW
    assert final.extra["context"]["compactions"] >= 1
    assert BLOB  # the fixture's size is what makes the control overflow


def test_an_evidence_run_replays_exactly(model: Any, sandboxes: Any, tmp_path: Path) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        results = [m for m in messages if m["role"] == "tool"]
        if not results:
            return {
                "tool_calls": [("bash", {"command": "echo 'KEY is hello' > out.txt; cat out.txt"})]
            }
        if len(results) == 1:
            args = {
                "action": "add",
                "statement": "the key is hello",
                "source": "out.txt",
                "quote": "KEY is hello",
            }
            return {"tool_calls": [("evidence", args)]}
        return {"text": "done"}

    server = model(script)

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
                        "context": {"strategy": "evidence-state", "window_tokens": 50_000},
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {
                    "checker": {"command": ["sh", "-c", 'test "$(cat out.txt)" = "KEY is hello"']}
                },
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed
    assert first.metadata["agent"]["evidence"]["evidence_active"] == 1
    names = [t["function"]["name"] for t in server.requests[0]["tools"]]
    assert "evidence" in names
    assert "evidence tool" in server.requests[0]["messages"][0]["content"]
    sent = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.metadata["agent"]["evidence"] == first.metadata["agent"]["evidence"]


def test_a_quote_the_agent_never_read_is_refused_in_a_real_run(
    model: Any, sandboxes: Any, tmp_path: Path
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        results = [m for m in messages if m["role"] == "tool"]
        if not results:
            args = {
                "action": "add",
                "statement": "s",
                "source": "x",
                "quote": "invented passage here",
            }
            return {"tool_calls": [("evidence", args)]}
        return {"text": "ok"}

    server = model(script)
    spec = spec_of(
        {
            "target": {"connector": "openai-compatible", "model": "m", "base_url": server.base_url},
            "harness": {
                "builtin": {"context": {"strategy": "evidence-state", "window_tokens": 50_000}}
            },
            "environment": {},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert rec.metadata["agent"]["evidence"]["evidence_active"] == 0
    refused = next(m for m in server.requests[1]["messages"] if m["role"] == "tool")
    assert "was not found in anything you have read" in refused["content"]

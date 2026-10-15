"""The CLI trajectory readers (H3), against fixtures written to each CLI's documented schema."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi_harness.formats import FORMATS, parse_output
from evalsi_harness.readers import Parsed

FIXTURES = Path(__file__).parent / "fixtures"


def read(fmt: str, name: str) -> Parsed:
    run = parse_output(fmt, (FIXTURES / name).read_text())
    assert run is not None
    return run


def kinds(run: Parsed) -> list[tuple[str, str]]:
    return [(s.type, s.name) for s in run.steps]


def test_claude_code_run_with_a_sub_agent() -> None:
    run = read("claude-stream-json", "claude-code-0.3.300-fix-calc.jsonl")
    assert run.answer == "Fixed add."
    assert run.session_id == "c1"
    assert run.model == "claude-sonnet-5-5"
    assert run.error == ""
    assert run.usage.cost_usd == pytest.approx(0.0123)
    # The result's per-model totals cover sub-agents; cache reads and writes
    # count as input the model was sent.
    assert run.usage.input_tokens == 19 + 100 + 5
    assert run.usage.output_tokens == 37
    # One response arrives as several events with one message id: one step.
    llms = [s for s in run.steps if s.type == "llm" and not s.parent_span_id]
    assert len(llms) == 3
    assert llms[0].output is not None
    assert llms[0].output.messages is not None
    (first,) = llms[0].output.messages
    assert first.content == "Let me look."
    assert [c.name for c in first.tool_calls] == ["Bash"]
    assert llms[0].usage is not None
    assert llms[0].usage.input_tokens == 10 + 100 + 5  # once, though two events carried it
    assert run.tools == ["Bash", "Task", "Bash"]

    by_id = {s.attributes.get("call_id"): s for s in run.steps if s.type == "tool"}
    assert by_id["t1"].output is not None
    assert by_id["t1"].output.as_text() == "def add(a,b): return a-b"
    assert by_id["t3"].error == "1 failed"  # a list of text blocks, is_error
    # The sub-agent's model call hangs under the Task tool step that started it.
    sub = next(
        s for s in run.steps if s.type == "llm" and s.attributes.get("claude.parent_tool_use_id")
    )
    assert sub.parent_span_id == by_id["t2"].span_id
    assert by_id["t3"].parent_span_id == sub.span_id


def test_claude_code_error_result_is_the_runs_error() -> None:
    out = (
        '{"type":"system","subtype":"init","session_id":"c","model":"m"}\n'
        '{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"c"}\n'
    )
    run = parse_output("claude-stream-json", out)
    assert run is not None
    assert run.error == "error_max_turns"


def test_codex_items_attach_to_the_message_that_follows_them() -> None:
    run = read("codex-json", "codex-0.162.1-fix-calc.jsonl")
    assert run.answer == "Fixed add."
    assert run.session_id == "th1"
    assert kinds(run) == [
        ("llm", "codex"),
        ("tool", "command"),
        ("tool", "file_change"),
        ("tool", "docs.lookup"),
        ("llm", "codex"),
    ]
    command = next(s for s in run.steps if s.name == "command")
    assert command.error == "1 failed"  # a non-zero exit is the step's error
    assert command.attributes["codex.exit_code"] == 1
    mcp = next(s for s in run.steps if s.name == "docs.lookup")
    assert mcp.output is not None
    assert mcp.output.as_text() == "ok"
    assert mcp.error == ""
    assert (run.usage.input_tokens, run.usage.output_tokens) == (120, 30)
    # The commands hang under the message that announced them.
    first = run.steps[0]
    assert all(s.parent_span_id == first.span_id for s in run.steps if s.type == "tool")
    assert first.output is not None
    assert first.output.messages is not None
    assert [c.name for c in first.output.messages[0].tool_calls] == [
        "command",
        "file_change",
        "docs.lookup",
    ]


def test_codex_failed_turn() -> None:
    run = parse_output(
        "codex-json",
        '{"type":"thread.started","thread_id":"t"}\n'
        '{"type":"turn.failed","error":{"message":"rate limited"}}\n',
    )
    assert run is not None
    assert run.error == "rate limited"


def test_gemini_stream() -> None:
    run = read("gemini-json", "gemini-0.64.0-fix-calc.jsonl")
    assert run.answer == "Fixed add."
    assert run.model == "gemini-3-pro"
    assert run.tools == ["read_file", "run_shell_command"]
    assert (run.usage.input_tokens, run.usage.output_tokens) == (250, 50)
    shell = next(s for s in run.steps if s.name == "run_shell_command")
    assert shell.error == "command failed"
    first = next(s for s in run.steps if s.type == "llm")
    assert first.output is not None
    assert first.output.as_text() == "Reading the file."


def test_gemini_json_object_with_session_metrics() -> None:
    stats = {"models": {"m": {"tokens": {"prompt": 40, "candidates": 9, "total": 49}}}}
    out = json.dumps({"session_id": "s", "response": "done", "stats": stats})
    run = parse_output("gemini-json", out)
    assert run is not None
    assert (run.usage.input_tokens, run.usage.output_tokens) == (40, 9)


def test_gemini_single_json_object() -> None:
    run = parse_output(
        "gemini-json", '{"response":"done","stats":{"input_tokens":7,"output_tokens":3}}'
    )
    assert run is not None
    assert run.answer == "done"
    assert (run.usage.input_tokens, run.usage.output_tokens) == (7, 3)
    assert run.error == ""


def test_pi_messages_and_cost() -> None:
    run = read("pi-json", "pi-0.73.1-fix-calc.jsonl")
    assert run.answer == "Fixed add."
    assert run.tools == ["read"]
    assert run.usage.input_tokens == 30 + 15
    assert run.usage.cost_usd == pytest.approx(0.003)
    tool = next(s for s in run.steps if s.type == "tool")
    assert tool.output is not None
    assert tool.output.as_text() == "def add(a,b): return a-b"


@pytest.mark.parametrize("fmt", [f for f in FORMATS if f not in ("", "dsh-json")])
def test_the_wrong_format_is_an_error_not_an_empty_run(fmt: str) -> None:
    run = parse_output(fmt, "just some text the agent printed\n")
    assert run is not None
    assert run.error.startswith(f"no {fmt} events")
    assert run.steps == []


@pytest.mark.parametrize("fmt", [f for f in FORMATS if f not in ("", "dsh-json")])
def test_garbage_and_unknown_events_are_skipped(fmt: str) -> None:
    run = parse_output(fmt, '{"type":"future_event","x":1}\n{not json\n["a"]\n')
    assert run is not None
    assert run.steps == []


def test_pretty_printed_gemini_json_is_read_whole() -> None:
    doc = json.dumps(
        {
            "response": "done",
            "stats": {"models": {"m": {"tokens": {"prompt": 5, "candidates": 2}}}},
        },
        indent=2,
    )
    run = parse_output("gemini-json", doc)
    assert run is not None
    assert run.answer == "done"
    assert (run.usage.input_tokens, run.usage.output_tokens) == (5, 2)
    assert run.error == ""

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
    # The explicit handoff links the Task tool to the sub-agent's model call.
    sub = next(
        s for s in run.steps if s.type == "llm" and s.attributes.get("claude.parent_tool_use_id")
    )
    handoff = next(s for s in run.steps if s.type == "handoff")
    assert handoff.parent_span_id == by_id["t2"].span_id
    assert sub.parent_span_id == handoff.span_id
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


def test_codex_completed_turn_clears_transient_reconnect_and_fallback_warnings() -> None:
    run = parse_output(
        "codex-json",
        '{"type":"thread.started","thread_id":"t"}\n'
        '{"type":"item.completed","item":{"id":"i0","type":"error","message":"fallback meta"}}\n'
        '{"type":"turn.started"}\n'
        '{"type":"error","message":"Reconnecting... 2/5 (WebSocket protocol error)"}\n'
        '{"type":"item.completed","item":{"id":"i1","type":"error","message":"HTTPS fallback"}}\n'
        '{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"Fixed add."}}\n'
        '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20}}\n',
    )
    assert run is not None
    assert run.error == ""
    assert run.answer == "Fixed add."
    assert (run.usage.input_tokens, run.usage.output_tokens) == (100, 20)


def test_codex_completed_turn_keeps_an_earlier_failed_turn() -> None:
    run = parse_output(
        "codex-json",
        '{"type":"thread.started","thread_id":"t"}\n'
        '{"type":"turn.started"}\n'
        '{"type":"turn.failed","error":{"message":"rate limited"}}\n'
        '{"type":"turn.started"}\n'
        '{"type":"error","message":"Reconnecting... 1/5"}\n'
        '{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Done."}}\n'
        '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}\n',
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
    assert run.session_id == "s"
    assert (run.usage.input_tokens, run.usage.output_tokens) == (40, 9)


def test_gemini_consecutive_tool_turns_without_intermediate_text() -> None:
    out = (
        '{"type":"init","session_id":"g2","model":"gemini-2.5-flash"}\n'
        '{"type":"tool_use","tool_name":"read_file","tool_id":"c1","parameters":{"p":"calc.js"}}\n'
        '{"type":"tool_result","tool_id":"c1","status":"success","output":"a - b"}\n'
        '{"type":"tool_use","tool_name":"write_file","tool_id":"c2","parameters":{"p":"calc.js"}}\n'
        '{"type":"tool_result","tool_id":"c2","status":"success","output":"ok"}\n'
        '{"type":"message","role":"assistant","content":"Fixed calc.js.","delta":true}\n'
        '{"type":"result","status":"success","stats":{"input_tokens":80,"output_tokens":15}}\n'
    )
    run = parse_output("gemini-json", out)
    assert run is not None
    assert kinds(run) == [
        ("llm", "gemini"),
        ("tool", "read_file"),
        ("llm", "gemini"),
        ("tool", "write_file"),
        ("llm", "gemini"),
    ]
    assert run.answer == "Fixed calc.js."


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


def test_opencode_recorded_run() -> None:
    # Recorded from opencode 1.19.3 against a scripted chat-completions server.
    run = read("opencode-json", "opencode-1.19.3-fix-calc.jsonl")
    assert run.error == ""
    assert run.answer == "Fixed add in calc.js."
    assert run.session_id == "ses_e9c6e047effezvvsHi0zqLwVPx"
    assert kinds(run) == [
        ("llm", "opencode"),
        ("tool", "bash"),
        ("llm", "opencode"),
        ("tool", "edit"),
        ("llm", "opencode"),
    ]
    assert run.tools == ["bash", "edit"]
    # Three model calls of 200 fresh + 1000 cached input tokens and 30 output each.
    assert (run.usage.input_tokens, run.usage.output_tokens) == (3600, 90)
    assert run.usage.cost_usd == 0
    llm, bash = run.steps[0], run.steps[1]
    assert bash.parent_span_id == llm.span_id
    assert llm.output is not None
    assert llm.output.messages is not None
    (call,) = llm.output.messages[0].tool_calls
    assert (call.id, call.name) == ("call_1", "bash")
    assert json.loads(call.arguments)["command"] == "cat calc.js"
    assert bash.output is not None
    assert "return a - b" in bash.output.as_text()
    assert bash.duration_ms == 513
    assert bash.attributes["opencode.title"] == "cat calc.js"
    assert run.steps[-1].attributes["opencode.finish_reason"] == "stop"


def test_opencode_tool_error_and_api_error() -> None:
    run = read("opencode-json", "opencode-1.19.3-tool-and-api-error.jsonl")
    assert run.error == "context window exceeded (scripted)"
    assert run.answer == ""
    tool = next(s for s in run.steps if s.type == "tool")
    assert tool.name == "read"
    assert tool.error == "File not found: /workspace/missing.js"


def test_opencode_partial_stream_and_reasoning() -> None:
    lines = [
        {"type": "step_start", "sessionID": "s", "part": {"type": "step-start"}},
        {"type": "reasoning", "sessionID": "s", "part": {"type": "reasoning", "text": "hmm"}},
        {"type": "text", "sessionID": "s", "part": {"type": "text", "text": "half done"}},
        {"type": "text", "part": {"type": "text", "text": "ignored", "synthetic": True}},
    ]
    run = parse_output("opencode-json", "\n".join(json.dumps(x) for x in lines))
    assert run is not None
    assert run.answer == "half done"  # no step_finish arrived
    assert run.steps[0].attributes["opencode.reasoning"] == "hmm"
    assert run.usage.input_tokens is None


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

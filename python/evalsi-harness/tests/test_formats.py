"""Reading DeepSeek Harness's `--json` run events, against runs recorded from dsh 0.2.1-alpha.2."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi_harness.formats import parse_dsh_events, parse_output
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

FIXTURES = Path(__file__).parent / "fixtures"
DONE = "I made the change and the command succeeded."


def events(name: str) -> str:
    return (FIXTURES / f"dsh-0.2.1-alpha.2-{name}.jsonl").read_text()


def test_a_run_is_model_calls_and_tool_calls() -> None:
    run_ = parse_dsh_events(events("fix-calc"))
    assert run_.answer == DONE
    assert run_.session_id.startswith("session-")
    assert run_.error == ""
    kinds = [(s.type, s.name) for s in run_.steps]
    assert kinds == [("llm", "dsh"), ("tool", "bash"), ("llm", "dsh")]

    asked, tool, answered = run_.steps
    assert asked.output is not None
    assert asked.output.messages is not None
    (message,) = asked.output.messages
    assert [c.name for c in message.tool_calls] == ["bash"]
    assert json.loads(message.tool_calls[0].arguments)["command"].startswith("sed -i")
    assert tool.input is not None
    assert "sed -i" in tool.input.as_text()
    assert tool.output is not None
    assert tool.output.as_text() == "(no output)"
    assert tool.error == ""
    assert tool.parent_span_id == asked.span_id
    assert answered.output is not None
    assert answered.output.as_text() == run_.answer

    # Tokens are summed over the model calls.
    assert (run_.usage.input_tokens, run_.usage.output_tokens) == (775 + 778, 65 + 12)
    assert asked.usage is not None
    assert asked.usage.input_tokens == 775


def test_a_failed_tool_call_is_that_steps_error() -> None:
    run_ = parse_dsh_events(events("tool-error"))
    tool = next(s for s in run_.steps if s.type == "tool")
    assert 'missing required property "description"' in tool.error
    assert run_.error == ""  # the turn itself completed


def test_a_turn_that_ends_in_an_error_is_the_runs_error() -> None:
    run_ = parse_dsh_events(events("no-credential"))
    assert run_.answer == ""
    assert run_.error.startswith("MISSING_CREDENTIAL: llm-deepseek: no API key")
    assert [s.type for s in run_.steps] == ["llm"]


def test_lines_that_are_not_events_are_skipped() -> None:
    text = "warning: telemetry is off\n" + events("fix-calc") + "\n{not json\n"
    assert parse_dsh_events(text).answer == DONE


def test_formats() -> None:
    assert parse_output("", "anything") is None
    with pytest.raises(ValueError, match="unknown output_format"):
        parse_output("stream-json", "")


def test_a_cli_agent_with_the_format_has_a_trajectory(sandboxes: LocalSandboxClient) -> None:
    fixture = FIXTURES / "dsh-0.2.1-alpha.2-fix-calc.jsonl"
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": [
                            "sh",
                            "-c",
                            f"cat {fixture}; echo 'unrelated: {{instruction}}' >&2",
                        ],
                        "output_format": "dsh-json",
                    }
                }
            },
            "environment": {"checker": {"command": ["true"]}},
        }
    )
    rec = run(spec, record("fix it"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == DONE
    assert rec.trajectory is not None
    assert [s.type for s in rec.trajectory.steps] == ["llm", "tool", "llm", "agent"]
    assert rec.usage is not None
    assert rec.usage.input_tokens == 775 + 778

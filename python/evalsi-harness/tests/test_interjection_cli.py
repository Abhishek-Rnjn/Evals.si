"""Interjections for CLI agents: a resume command delivers them, or they are undeliverable (H4)."""

from __future__ import annotations

import json
from typing import Any

from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of


def assistant(content: list[dict[str, Any]]) -> dict[str, Any]:
    msg = {
        "role": "assistant",
        "content": content,
        "usage": {"input_tokens": 5, "output_tokens": 7},
    }
    return {"type": "assistant", "session_id": "sess-1", "message": msg}


def tool_use(name: str, **args: Any) -> dict[str, Any]:
    return assistant([{"type": "tool_use", "id": f"t-{name}", "name": name, "input": args}])


def result(text: str) -> dict[str, Any]:
    return {"type": "result", "subtype": "success", "result": text, "session_id": "sess-1"}


def jsonl(events: list[dict[str, Any]]) -> str:
    return "\n".join(json.dumps(e) for e in events) + "\n"


FIRST = jsonl(
    [
        {"type": "system", "subtype": "init", "session_id": "sess-1"},
        assistant([{"type": "text", "text": "Looking."}]),
        tool_use("Bash", command="pytest -q"),
        tool_use("Edit", file_path="src/calc.py", old_string="a", new_string="b"),
        result("first answer"),
    ]
)
FOLLOW = jsonl(
    [
        {"type": "system", "subtype": "init", "session_id": "sess-1"},
        assistant([{"type": "text", "text": "Noted."}]),
        result("second answer"),
    ]
)
NOTE = "Please also update the docs."
RESUME = ["sh", "-c", 'echo "$0|$1" >> got.txt; cat follow.jsonl', "{session}", "{message}"]


def spec_for(items: list[dict[str, Any]], resume: list[str] | None = RESUME, **cli: Any) -> Any:
    return spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": ["sh", "-c", "cat first.jsonl"],
                        "output_format": "claude-stream-json",
                        **({"resume_command": resume} if resume else {}),
                        **cli,
                    }
                }
            },
            "harness": {"builtin": {"interjections": items}},
            "environment": {
                "files": {"first.jsonl": FIRST, "follow.jsonl": FOLLOW},
                "checker": {"command": ["sh", "-c", "cat got.txt"], "parser": "exit-code"},
            },
        }
    )


def interjection_steps(rec: Any) -> list[Any]:
    return [s for s in rec.trajectory.steps if s.name == "user-interjection"]


def test_a_matching_file_write_is_delivered_by_resuming_the_session(
    sandboxes: LocalSandboxClient,
) -> None:
    items = [{"event": "file_write", "match": "src/*.py", "message": NOTE}]
    rec = run(spec_for(items), record("fix it"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed  # got.txt exists: the resume command ran in the sandbox
    [step] = interjection_steps(rec)
    assert step.attributes["evalsi.interjection.delivered"] is True
    assert step.attributes["evalsi.interjection.timing"] == "after-run"
    assert step.attributes["evalsi.interjection.trigger"] == "Edit file write"
    # The follow-up's steps join the trajectory after the interjection, and its answer is final.
    names = [s.name for s in rec.trajectory.steps]  # type: ignore[union-attr]
    assert names.index("user-interjection") < len(names) - 1
    assert rec.output is not None
    assert rec.output.as_text() == "second answer"
    agent = rec.metadata["agent"]
    assert agent["interjections"] == 1
    assert agent["interjections_undeliverable"] == 0


def test_the_session_and_message_reach_the_resume_command(
    sandboxes: LocalSandboxClient,
) -> None:
    spec = spec_for([{"event": "stop", "message": NOTE}])
    spec.environment.checker.command[:] = ["sh", "-c", f'test "$(cat got.txt)" = "sess-1|{NOTE}"']
    rec = run(spec, record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed


def test_a_tool_call_event_matches_name_and_arguments(sandboxes: LocalSandboxClient) -> None:
    items = [
        {"event": "tool_call", "match": "Bash", "pattern": "pytest", "message": "TESTS"},
        {"event": "tool_call", "match": "Bash", "pattern": "rm -rf", "message": "NEVER"},
    ]
    rec = run(spec_for(items), record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert step.output is not None
    assert step.output.as_text() == "TESTS"
    agent = rec.metadata["agent"]
    assert agent["interjections"] == 1
    assert agent["interjections_unfired"] == 1  # nothing ran rm -rf


def test_a_step_count_is_judged_against_the_model_calls_the_run_reported(
    sandboxes: LocalSandboxClient,
) -> None:
    items = [
        {"at_step": 2, "message": "EARLY"},
        {"at_step": 99, "message": "NEVER REACHED"},
    ]
    rec = run(spec_for(items), record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert step.output is not None
    assert step.output.as_text() == "EARLY"
    assert rec.metadata["agent"]["interjections_unfired"] == 1


def test_without_a_resume_command_the_interjection_is_recorded_as_undeliverable(
    sandboxes: LocalSandboxClient,
) -> None:
    items = [{"event": "stop", "message": NOTE}]
    rec = run(spec_for(items, resume=None), record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert step.error.startswith("undeliverable: the agent has no resume_command")
    assert step.attributes["evalsi.interjection.delivered"] is False
    agent = rec.metadata["agent"]
    assert agent["interjections"] == 0
    assert agent["interjections_undeliverable"] == 1
    assert rec.output is not None
    assert rec.output.as_text() == "first answer"  # the run itself is unchanged


def test_a_session_the_output_never_named_is_undeliverable(sandboxes: LocalSandboxClient) -> None:
    spec = spec_for([{"event": "stop", "message": NOTE}])
    spec.target.agent.cli.output_format = ""  # plain text: no session id, no steps
    rec = run(spec, record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert "no session to resume" in step.error


def test_a_step_timed_message_needs_a_reader_that_reports_steps(
    sandboxes: LocalSandboxClient,
) -> None:
    spec = spec_for([{"at_step": 1, "message": NOTE}])
    spec.target.agent.cli.output_format = ""
    rec = run(spec, record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert "reported no steps" in step.error


def test_a_failed_follow_up_is_recorded_and_keeps_the_first_answer(
    sandboxes: LocalSandboxClient,
) -> None:
    items = [{"event": "stop", "message": NOTE}]
    spec = spec_for(items, resume=["sh", "-c", "echo boom >&2; exit 3"])
    rec = run(spec, record("x"), sandboxes).record
    assert rec is not None
    [step] = interjection_steps(rec)
    assert step.error.startswith("undeliverable:")
    assert "exit 3" in step.error
    assert rec.output is not None
    assert rec.output.as_text() == "first answer"
    assert rec.metadata["agent"]["interjections_undeliverable"] == 1

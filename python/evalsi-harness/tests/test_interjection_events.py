"""Interjections timed by an event, not a step count (H4)."""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then, target
from tests.test_interjections import NOTE

Model = Callable[..., ScriptedModelServer]


def spec_events(server: ScriptedModelServer, items: list[dict[str, Any]], **builtin: Any) -> Any:
    return spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {"interjections": items, **builtin}},
            "environment": {"checker": CHECK_HELLO},
        }
    )


def event_script() -> Any:
    return calls_then(
        [
            {"tool_calls": [("bash", {"command": "ls"})]},
            {"tool_calls": [("write_file", {"path": "out.txt", "content": "hello\n"})]},
            {"tool_calls": [("bash", {"command": "pytest -q"})]},
            {"text": "done"},
        ]
    )


def interjection_steps(rec: Any) -> list[Any]:
    return [s for s in rec.trajectory.steps if s.name == "user-interjection"]


def test_a_tool_call_event_delivers_after_the_matching_call(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(event_script())
    items = [{"event": "tool_call", "match": "bash", "pattern": "pytest", "message": NOTE}]
    rec = run(spec_events(server, items), record(), sandboxes).record
    assert rec is not None
    # Not after `ls` (the pattern does not match) but after `pytest -q`: the fourth call.
    assert [NOTE in str(r) for r in server.requests] == [False, False, False, True]
    [step] = interjection_steps(rec)
    assert step.attributes["evalsi.interjection.on"] == "tool_call"
    assert step.attributes["evalsi.interjection.trigger"] == "bash tool call"
    assert step.attributes["evalsi.interjection.delivered_at"] == 3
    assert rec.metadata["agent"]["interjections"] == 1


def test_a_file_write_event_matches_the_path_and_fires_once(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    script = calls_then(
        [
            {"tool_calls": [("write_file", {"path": "notes/a.md", "content": "x"})]},
            {"tool_calls": [("write_file", {"path": "out.txt", "content": "hello\n"})]},
            {"tool_calls": [("write_file", {"path": "out.txt", "content": "hello\n"})]},
            {"text": "done"},
        ]
    )
    server = model(script)
    items = [{"event": "file_write", "match": "out.txt", "message": NOTE}]
    rec = run(spec_events(server, items), record(), sandboxes).record
    assert rec is not None
    assert [NOTE in str(r) for r in server.requests] == [False, False, True, True]
    assert len(interjection_steps(rec)) == 1  # once, though the path was written twice


def test_a_failed_write_is_not_a_file_write(model: Model, sandboxes: LocalSandboxClient) -> None:
    script = calls_then(
        [
            {"tool_calls": [("write_file", {"content": "no path"})]},  # an error result
            {"text": "done"},
        ]
    )
    server = model(script)
    items = [{"event": "file_write", "match": "*", "message": NOTE}]
    rec = run(spec_events(server, items), record(), sandboxes).record
    assert rec is not None
    assert interjection_steps(rec) == []


def test_a_stop_event_sends_the_agent_back_to_work_once(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "all done"}, {"text": "ok, changed"}]))
    items = [{"event": "stop", "message": NOTE}]
    rec = run(spec_events(server, items), record(), sandboxes).record
    assert rec is not None
    assert len(server.requests) == 2
    assert NOTE in str(server.requests[1])
    assert rec.output is not None
    assert rec.output.as_text() == "ok, changed"
    [step] = interjection_steps(rec)
    assert step.attributes["evalsi.interjection.trigger"] == "the agent tried to finish"


def test_an_event_waits_for_its_earliest_step(model: Model, sandboxes: LocalSandboxClient) -> None:
    server = model(event_script())
    items = [{"event": "tool_call", "match": "bash", "at_step": 3, "message": NOTE}]
    rec = run(spec_events(server, items), record(), sandboxes).record
    assert rec is not None
    # The bash call at step 1 is too early; the one at step 3 triggers it.
    assert [NOTE in str(r) for r in server.requests] == [False, False, False, True]


def test_events_and_steps_mix_and_the_events_are_part_of_the_replay_guard(
    model: Model, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(event_script())
    items: list[dict[str, Any]] = [
        {"at_step": 1, "message": "FIRST"},
        {"event": "file_write", "match": "out.txt", "message": NOTE},
    ]
    run(
        spec_events(server, items, recording={"mode": "record", "dir": "tapes"}),
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    assert "FIRST" in str(server.requests[1])
    assert NOTE in str(server.requests[2])
    other = [{"at_step": 1, "message": "FIRST"}]
    out = run(
        spec_events(server, other, recording={"mode": "replay", "dir": "tapes"}),
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    assert "needs the same tools" in out.error


@pytest.mark.parametrize(
    ("item", "message"),
    [
        ({"event": "tool_call", "message": "x", "pattern": "("}, "regular expression"),
        ({"event": "sometimes", "message": "x"}, "must be one of"),
        ({"event": "file_write", "message": "x"}, "needs match"),
        ({"event": "stop", "match": "a", "message": "x"}, "takes no match"),
        ({"at_step": 1, "match": "a", "message": "x"}, "apply only with an event"),
        ({"message": "x"}, "at_step of at least 1"),
    ],
)
def test_bad_event_interjections_are_refused(
    model: Model, sandboxes: LocalSandboxClient, item: dict[str, Any], message: str
) -> None:
    out = run(spec_events(model(event_script()), [item]), record(), sandboxes)
    assert message in out.error

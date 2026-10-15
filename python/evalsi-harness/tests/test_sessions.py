"""Session logs: written as they happen, resumed after an interruption, forked."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.cassette import Cassette, CassetteError, session_tree
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then

STEPS: list[dict[str, Any]] = [
    {"tool_calls": [("bash", {"command": "echo one >> log.txt"})]},
    {"tool_calls": [("bash", {"command": "echo two >> log.txt"})]},
    {"tool_calls": [("bash", {"command": "echo three >> log.txt"})]},
    {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
    {"text": "finished"},
]


def spec(server: Any, mode: str, **recording: Any) -> Any:
    return spec_of(
        {
            "target": {"connector": "openai-compatible", "model": "m", "base_url": server.base_url},
            "harness": {"builtin": {"recording": {"mode": mode, "dir": "tapes", **recording}}},
            "environment": {"checker": CHECK_HELLO},
        }
    )


def failing_after(n: int, steps: list[dict[str, Any]]) -> Any:
    """Serves ``n`` model turns, then fails every call, as a dying provider does."""
    script = calls_then(steps)
    served = 0

    def go(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        nonlocal served
        served += 1
        return {"status": 400} if served > n else script(messages, tools)

    return go


def kinds(path: Path) -> list[str]:
    return [json.loads(line)["kind"] for line in path.read_text().splitlines()]


def steps_of(rec: Any) -> list[tuple[str, str]]:
    return [(s.type, s.name) for s in rec.trajectory.steps if s.type in ("llm", "tool")]


def test_an_interrupted_trial_resumes_and_grades_like_an_uninterrupted_one(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    # The control: the whole run, uninterrupted.
    control_dir = tmp_path / "control"
    control_dir.mkdir()
    server = model(calls_then(STEPS))
    control = run(spec(server, "record"), record(), sandboxes, base_dir=control_dir).record
    assert control is not None
    assert control.check is not None
    assert control.check.passed

    # The provider dies on the third model call.
    server.script = failing_after(2, STEPS)
    died = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path)
    assert died.record is None
    assert died.error
    tape = tmp_path / "tapes" / "t1.0.jsonl"
    # Everything up to the last step is on disk: two turns and their results, and the
    # refusal the third call met. A resume drops that last one: it is where the trial died.
    assert kinds(tape) == ["header", "model", "tool", "model", "tool", "model"]
    assert "error" in json.loads(tape.read_text().splitlines()[-1])["turn"]

    # Resume: replay the two turns (the sandbox is rebuilt by re-running them),
    # then carry on live with the rest.
    server.script = calls_then(STEPS[2:])
    before = len(server.requests)
    resumed = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path).record
    assert resumed is not None
    assert len(server.requests) - before == 3  # only the three turns that had not happened
    assert resumed.check is not None
    assert resumed.check.passed
    assert steps_of(resumed) == steps_of(control)
    assert resumed.output is not None
    assert control.output is not None
    assert resumed.output.as_text() == control.output.as_text()
    # The log holds the whole run exactly once: nothing replayed was written twice.
    assert kinds(tape) == ["header", *["model", "tool"] * 4, "model"]
    ids = [json.loads(line)["id"] for line in tape.read_text().splitlines()[1:]]
    assert ids == list(range(len(ids)))
    # And a finished log resumes to the same end without calling the model.
    sent = len(server.requests)
    again = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.check is not None
    assert again.check.passed


def test_a_half_written_last_line_is_dropped_on_resume(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(failing_after(2, STEPS))
    run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path)
    tape = tmp_path / "tapes" / "t1.0.jsonl"
    with tape.open("a") as f:
        f.write('{"id": 5, "kind": "model", "turn": {"text": "tru')  # killed mid-write
    server.script = calls_then(STEPS[2:])
    resumed = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path).record
    assert resumed is not None
    assert resumed.check is not None
    assert resumed.check.passed
    for line in tape.read_text().splitlines():
        json.loads(line)  # every line of the log parses again


def test_a_trial_killed_between_a_turn_and_its_tool_result_runs_that_tool_on_resume(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(calls_then(STEPS))
    run(spec(server, "record"), record(), sandboxes, base_dir=tmp_path)
    tape = tmp_path / "tapes" / "t1.0.jsonl"
    lines = tape.read_text().splitlines()
    # Cut the log after the fourth turn, before the result of its tool call.
    cut = max(i for i, line in enumerate(lines) if json.loads(line)["kind"] == "model")
    tape.write_text("\n".join(lines[: cut - 1]) + "\n")
    assert kinds(tape)[-1] == "model"
    server.script = calls_then(STEPS[4:])
    resumed = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path).record
    assert resumed is not None
    assert resumed.check is not None
    assert resumed.check.passed  # the missing result was produced by running the call


def test_a_resume_with_no_log_is_a_recording(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(calls_then(STEPS))
    rec = run(spec(server, "resume"), record(), sandboxes, base_dir=tmp_path).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    assert kinds(tmp_path / "tapes" / "t1.0.jsonl")[0] == "header"


def test_a_fork_replays_a_prefix_then_writes_its_own_log(
    model: Any, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(calls_then(STEPS))
    base = run(spec(server, "record"), record(), sandboxes, base_dir=tmp_path).record
    assert base is not None
    source = tmp_path / "tapes" / "t1.0.jsonl"
    source_text = source.read_text()

    # Fork after two turns, and finish differently.
    server.script = calls_then(
        [
            {"tool_calls": [("bash", {"command": "echo goodbye > out.txt"})]},
            {"text": "a different ending"},
        ]
    )
    before = len(server.requests)
    forked = run(spec(server, "fork", branch_at_step=2), record(), sandboxes, base_dir=tmp_path)
    assert forked.record is not None
    assert len(server.requests) - before == 2
    assert forked.record.check is not None
    assert not forked.record.check.passed  # it wrote goodbye, not hello
    assert forked.record.output is not None
    assert forked.record.output.as_text() == "a different ending"
    assert source.read_text() == source_text  # the source log is untouched

    child = tmp_path / "tapes" / "t1.0.fork-1.jsonl"
    header = json.loads(child.read_text().splitlines()[0])
    assert header["forked_from"] == {"session": "t1.0.jsonl", "turns": 2}
    # The new log holds the prefix and the continuation, so it replays on its own.
    assert kinds(child).count("model") == 4
    sent = len(server.requests)
    replayed = run(spec(server, "replay"), record(), sandboxes, base_dir=tmp_path)
    assert replayed.record is not None
    assert len(server.requests) == sent
    # A second fork gets the next free name; a given name is used as asked.
    run(spec(server, "fork", branch_at_step=1), record(), sandboxes, base_dir=tmp_path)
    run(
        spec(server, "fork", branch_at_step=3, fork_as="try-b"),
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    assert (tmp_path / "tapes" / "t1.0.fork-2.jsonl").exists()
    assert (tmp_path / "tapes" / "t1.0.try-b.jsonl").exists()
    tree = session_tree(tmp_path / "tapes")
    assert tree["t1.0.jsonl"] == {"parent": None, "forked_at_turn": 0, "turns": 5}
    assert tree["t1.0.fork-1.jsonl"]["parent"] == "t1.0.jsonl"
    assert tree["t1.0.fork-1.jsonl"]["forked_at_turn"] == 2
    assert tree["t1.0.try-b.jsonl"]["forked_at_turn"] == 3


def test_cassette_modes_and_their_checks(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    with pytest.raises(ValueError, match="recording mode"):
        Cassette(path, "rewind")
    with pytest.raises(ValueError, match="fork needs"):
        Cassette(path, "fork", branch_at_step=0, fork_to=tmp_path / "x.jsonl")
    with pytest.raises(ValueError, match="fork needs"):
        Cassette(path, "fork", branch_at_step=2)
    with pytest.raises(CassetteError, match="no cassette to replay"):
        Cassette(path, "replay")
    with pytest.raises(CassetteError, match="no cassette to replay"):
        Cassette(path, "fork", branch_at_step=1, fork_to=tmp_path / "x.jsonl")
    # An empty log is damaged for a replay, and a fresh start for a resume.
    path.write_text('{"kind": "header", "version": 2, "ctx')
    with pytest.raises(CassetteError, match="damaged"):
        Cassette(path, "replay")
    assert Cassette(path, "resume").next_turn() is None


def test_a_resumed_log_still_refuses_a_different_context_strategy_or_hooks(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    tape = Cassette(path, "record", hooks=["m:A"])
    tape.record({"kind": "model", "turn": {"text": "x", "stop_reason": "end_turn"}})
    tape.save()
    with pytest.raises(CassetteError, match="same hooks"):
        Cassette(path, "resume")
    with pytest.raises(CassetteError, match="same strategy"):
        Cassette(path, "resume", hooks=["m:A"], context={"strategy": "compact"})
    assert Cassette(path, "resume", hooks=["m:A"]).next_turn() is not None


def test_a_log_only_replays_with_the_tools_it_was_recorded_with(tmp_path: Path) -> None:
    path = tmp_path / "t.jsonl"
    tools = {"builtin": ["code_mode", "delegate"], "delegate_steps": 5}
    tape = Cassette(path, "record", toolset=tools)
    tape.record({"kind": "model", "turn": {"text": "x", "stop_reason": "end_turn"}})
    tape.save()
    assert Cassette(path, "replay", toolset=tools).next_turn() is not None
    for other in (
        None,
        {"builtin": ["code_mode"], "delegate_steps": 5},
        {**tools, "delegate_steps": 9},
    ):
        with pytest.raises(CassetteError, match="same tools"):
            Cassette(path, "replay", toolset=other)
    # A log written before toolsets were recorded is a log with none.
    lines = path.read_text().splitlines()
    header = json.loads(lines[0])
    del header["toolset"]
    path.write_text("\n".join([json.dumps(header), *lines[1:]]) + "\n")
    assert Cassette(path, "replay").next_turn() is not None
    with pytest.raises(CassetteError, match="same tools"):
        Cassette(path, "replay", toolset=tools)

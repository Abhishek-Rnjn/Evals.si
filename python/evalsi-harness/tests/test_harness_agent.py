"""The built-in harness end to end, offline: a scripted model, an unconfined
local stand-in for the sandbox, real tools, checkers and budgets."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.testing import LocalSandboxClient, Script, ScriptedModelServer
from tests.harness_support import judge_answering, record, run, spec_of

Model = Callable[[Script], ScriptedModelServer]


def calls_then(answers: list[dict[str, Any]]) -> Script:
    """Replies in order, then repeats the last one."""
    it = iter(answers)
    last: dict[str, Any] = {}

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        nonlocal last
        last = next(it, last)
        return last

    return script


def target(server: ScriptedModelServer, connector: str = "openai-compatible") -> dict[str, Any]:
    base = (
        server.base_url if connector == "openai-compatible" else server.base_url.removesuffix("/v1")
    )
    return {"connector": connector, "model": "scripted", "base_url": base}


CHECK_HELLO = {"command": ["sh", "-c", 'test "$(cat out.txt)" = hello']}


@pytest.mark.parametrize("connector", ["openai-compatible", "anthropic"])
def test_the_builtin_agent_does_a_sandbox_task(
    model: Model, sandboxes: LocalSandboxClient, connector: str
) -> None:
    server = model(
        calls_then(
            [
                {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
                {"tool_calls": [("read_file", {"path": "out.txt"})]},
                {"text": "I wrote hello to out.txt."},
            ]
        )
    )
    spec = spec_of({"target": target(server, connector), "environment": {"checker": CHECK_HELLO}})
    out = run(spec, record(), sandboxes)
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "I wrote hello to out.txt."
    assert rec.check is not None
    assert rec.check.passed
    assert [s.type for s in rec.trajectory.steps] == ["llm", "tool", "llm", "tool", "llm"]  # type: ignore[union-attr]
    tools = rec.trajectory.tool_uses()  # type: ignore[union-attr]
    assert [t.name for t in tools] == ["bash", "read_file"]
    assert "[exit code 0]" in tools[0].result
    assert tools[1].result == "hello\n"
    assert rec.usage is not None
    assert rec.usage.input_tokens == 300
    assert rec.metadata["agent"]["stop_reason"] == "completed"
    assert rec.metadata["agent"]["tool_calls"] == 2
    assert rec.metadata["isolation"]["driver"] == "local-test"
    # The model saw every tool the harness offers in a sandbox.
    request_tools = server.requests[0]["tools"]
    names = [t["function"]["name"] if "function" in t else t["name"] for t in request_tools]
    assert names == ["bash", "read_file", "write_file", "request_escalation"]


def test_budgets_stop_the_agent_and_the_check_still_runs(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"tool_calls": [("bash", {"command": "true"})]}]))
    spec = spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {"max_steps": 3}},
            "environment": {"checker": CHECK_HELLO},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert rec.metadata["agent"]["stop_reason"] == "max_steps"
    assert rec.metadata["agent"]["steps"] == 3
    assert rec.check is not None
    assert not rec.check.passed

    spec = spec_of(
        {
            "target": target(server),
            "harness": {
                "builtin": {
                    "budget": {"usd": 0.001},
                    "pricing": {"input_per_mtok": 3, "output_per_mtok": 15},
                }
            },
            "environment": {},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert rec.metadata["agent"]["stop_reason"] == "budget"
    # 100 in + 20 out per call: $0.0006, so the second call crosses $0.001.
    assert rec.metadata["agent"]["steps"] == 2
    assert rec.usage is not None
    assert rec.usage.cost_usd == pytest.approx(0.0012)


def test_setup_runs_once_per_environment_and_trials_start_clean(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(
        calls_then(
            [
                {"tool_calls": [("bash", {"command": "cat setup.txt; echo agent >> setup.txt"})]},
                {"text": "ok"},
            ]
        )
    )
    spec = spec_of(
        {
            "target": target(server),
            "environment": {
                "setup": ["echo built >> setup.txt"],
                "setup_network": "allow",
                "sandbox": {"network": "deny"},
                "checker": {"command": ["sh", "-c", "test $(wc -l < setup.txt) -eq 2"]},
            },
        }
    )
    for trial in range(3):
        server.script = calls_then(
            [
                {"tool_calls": [("bash", {"command": "cat setup.txt; echo agent >> setup.txt"})]},
                {"text": "ok"},
            ]
        )
        rec = run(spec, record(), sandboxes, trial=trial).record
        assert rec is not None
        assert rec.check is not None
        assert rec.check.passed, rec.check
        assert rec.trajectory.tool_uses()[0].result.startswith("built\n")  # type: ignore[union-attr]
    # Each run() is its own harness, so setup ran per call here; within one harness it is shared.
    assert [s.network for s in sandboxes.created] == ["allow"] * 3
    assert [n for _, n in sandboxes.restored] == ["deny"] * 3


def test_setup_is_shared_within_a_harness(model: Model, sandboxes: LocalSandboxClient) -> None:
    import asyncio

    from evalsi_harness import HarnessContext, Task, load_harness, run_task

    server = model(calls_then([{"text": "ok"}]))
    spec = spec_of({"target": target(server), "environment": {"setup": ["echo x > x.txt"]}})

    async def go() -> None:
        async def get() -> LocalSandboxClient:
            return sandboxes

        harness = load_harness(spec, HarnessContext(sandboxes=get))
        try:
            outs = await asyncio.gather(
                *(
                    run_task(Task.build(spec, record(id=f"r{i}"), trial=0), harness)
                    for i in range(4)
                )
            )
        finally:
            await harness.aclose()
        assert all(o.record is not None for o in outs)

    asyncio.run(go())
    assert len(sandboxes.created) == 1
    assert len(sandboxes.restored) == 4


def test_a_failed_setup_is_an_error_not_a_failure(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "ok"}]))
    spec = spec_of({"target": target(server), "environment": {"setup": ["echo boom >&2; exit 3"]}})
    out = run(spec, record(), sandboxes)
    assert out.record is None
    assert "environment setup failed" in out.error
    assert "boom" in out.error
    assert server.requests == []


def test_bad_tool_calls_are_reported_to_the_agent(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if not any(m["role"] == "tool" for m in messages):
            return {"tool_calls": [("nope", {}), ("bash", {})]}
        return {"text": "gave up"}

    server = model(script)
    rec = run(spec_of({"target": target(server), "environment": {}}), record(), sandboxes).record
    assert rec is not None
    errors = [t.error for t in rec.trajectory.tool_uses()]  # type: ignore[union-attr]
    assert "there is no tool 'nope'" in errors[0]
    assert "needs a 'command'" in errors[1]
    second = server.requests[1]["messages"]
    assert [m["role"] for m in second[-2:]] == ["tool", "tool"]
    assert second[-1]["content"].startswith("ERROR: ")


def test_mock_tools_and_deterministic_faults(model: Model, sandboxes: LocalSandboxClient) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if sum(m["role"] == "tool" for m in messages) < 10:
            return {"tool_calls": [("lookup_order", {"id": 42})]}
        return {"text": "done"}

    server = model(script)
    harness = {
        "builtin": {
            "max_steps": 30,
            "tools": {
                "sandbox": False,
                "mocks": [
                    {"name": "lookup_order", "responses": {'{"id": 42}': '{"status": "shipped"}'}},
                ],
                "faults": [{"tool": "lookup_order", "kind": "error", "rate": 0.5}],
            },
        }
    }
    spec = spec_of({"target": target(server), "harness": harness})
    first = run(spec, record(), sandboxes).record
    again = run(spec, record(), sandboxes).record
    other_trial = run(spec, record(), sandboxes, trial=1).record
    assert first is not None
    assert again is not None
    assert other_trial is not None

    def pattern(rec: Any) -> list[bool]:
        return [bool(t.error) for t in rec.trajectory.tool_uses()]

    assert pattern(first) == pattern(again)
    assert any(pattern(first))
    assert not all(pattern(first))
    assert pattern(first) != pattern(other_trial)
    ok = [t.result for t in first.trajectory.tool_uses() if not t.error]  # type: ignore[union-attr]
    assert ok[0] == '{"status": "shipped"}'


def test_escalation_is_denied_by_default_and_recorded(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(
        calls_then(
            [
                {
                    "tool_calls": [
                        ("request_escalation", {"kind": "network", "reason": "pip install"})
                    ]
                },
                {"text": "could not install"},
            ]
        )
    )
    rec = run(spec_of({"target": target(server), "environment": {}}), record(), sandboxes).record
    assert rec is not None
    assert rec.metadata["policy_events"] == [
        {"kind": "escalation_request", "detail": "network: pip install", "granted": False}
    ]
    guard = [s for s in rec.trajectory.steps if s.type == "guardrail"]  # type: ignore[union-attr]
    assert [s.name for s in guard] == ["sandbox.escalation_request"]
    assert sandboxes.restored == []

    server.script = calls_then(
        [
            {"tool_calls": [("request_escalation", {"kind": "network", "reason": "pip"})]},
            {"text": "ok"},
        ]
    )
    spec = spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {"escalation": {"allow": ["network"]}}},
            "environment": {},
        }
    )
    rec = run(spec, record(), sandboxes).record
    assert rec is not None
    assert rec.metadata["policy_events"][0]["granted"] is True
    assert [n for _, n in sandboxes.restored] == ["allow"]


def test_a_simulated_user_drives_a_multi_turn_task(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    def agent(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        users = [m["content"] for m in messages if m["role"] == "user"]
        return {"text": f"agent reply {len(users)}"}

    replies = iter(
        [{"message": "my order number is 42", "done": False}, {"message": "thanks", "done": True}]
    )
    judge = judge_answering(lambda prompt, schema: next(replies))
    server = model(agent)
    spec = spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {"user_simulator": {"persona": "impatient", "max_turns": 5}}},
        }
    )
    rec = run(spec, record("Where is my order?"), sandboxes, judge=judge).record
    assert rec is not None
    steps = rec.trajectory.steps  # type: ignore[union-attr]
    assert [s.type for s in steps] == ["llm", "user", "llm", "user"]
    assert steps[1].output is not None
    assert steps[1].output.as_text() == "my order number is 42"
    assert rec.output is not None
    assert rec.output.as_text() == "agent reply 2"
    prompt = judge.backend.calls[0][0]  # type: ignore[attr-defined]
    assert "impatient" in prompt
    assert "Where is my order?" in prompt


def test_record_replay_and_branch(
    model: Model, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    steps: list[dict[str, Any]] = [
        {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
        {"text": "recorded answer"},
    ]
    server = model(calls_then(steps))
    base = {"environment": {"checker": CHECK_HELLO}}
    recording = {"builtin": {"recording": {"mode": "record", "dir": "tapes"}}}
    rec = run(
        spec_of({"target": target(server), "harness": recording, **base}),
        record(),
        sandboxes,
        base_dir=tmp_path,
    ).record
    assert rec is not None
    tape = tmp_path / "tapes" / "t1.0.jsonl"
    assert [json.loads(line)["kind"] for line in tape.read_text().splitlines()] == [
        "model",
        "tool",
        "model",
    ]

    # Replay needs no model, rebuilds the environment (the checker passes)
    # and reproduces the answer.
    replay = {"builtin": {"recording": {"mode": "replay", "dir": "tapes"}}}
    calls = len(server.requests)
    again = run(spec_of({"harness": replay, **base}), record(), sandboxes, base_dir=tmp_path).record
    assert again is not None
    assert again.output is not None
    assert again.output.as_text() == "recorded answer"
    assert again.check is not None
    assert again.check.passed
    assert len(server.requests) == calls

    # Branch after the first turn: the live model takes over from there.
    server.script = calls_then([{"text": "a different ending"}])
    branch = {"builtin": {"recording": {"mode": "replay", "dir": "tapes", "branch_at_step": 1}}}
    forked = run(
        spec_of({"target": target(server), "harness": branch, **base}),
        record(),
        sandboxes,
        base_dir=tmp_path,
    ).record
    assert forked is not None
    assert forked.output is not None
    assert forked.output.as_text() == "a different ending"
    assert forked.check is not None
    assert forked.check.passed
    assert len(server.requests) == calls + 1


def test_a_task_without_a_target_model_is_an_error(sandboxes: LocalSandboxClient) -> None:
    out = run(spec_of({"harness": {"builtin": {}}}), record(), sandboxes)
    assert out.record is None
    assert "target model" in out.error


def test_the_record_carries_the_agents_diff(model: Model, sandboxes: LocalSandboxClient) -> None:
    server = model(
        calls_then(
            [
                {
                    "tool_calls": [
                        (
                            "bash",
                            {"command": "sed -i s/-/+/ calc.py && echo new > notes.txt"},
                        )
                    ]
                },
                {"text": "Fixed."},
            ]
        )
    )
    setup = "git init -q && git add -A && git -c user.email=t@e -c user.name=t commit -qm base"
    spec = spec_of(
        {
            "target": target(server),
            "environment": {
                "files": {"calc.py": "def add(a, b):\n    return a - b\n"},
                "setup": [setup],
                "checker": {
                    "command": ["sh", "-c", "grep -q 'a + b' calc.py"],
                    "files": {".evalsi-check.sh": "true\n"},
                },
            },
        }
    )
    rec = run(spec, record("Fix add in calc.py."), sandboxes).record
    assert rec is not None
    diff = rec.metadata["diff"]
    assert "-    return a - b\n+    return a + b" in diff
    assert "+++ b/notes.txt" in diff
    assert ".evalsi-" not in diff
    # Not a git repository: no diff.
    plain = spec_of({"target": target(server), "environment": {}})
    rec = run(plain, record("Anything."), sandboxes).record
    assert rec is not None
    assert "diff" not in rec.metadata

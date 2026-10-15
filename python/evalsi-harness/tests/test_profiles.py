"""Versioned harness profiles (H3)."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

import pytest
from google.protobuf import json_format

from evalsi.v1alpha1 import agent_pb2
from evalsi_harness import HarnessContext
from evalsi_harness.harness import BuiltinHarness
from evalsi_harness.profiles import MINIMAL_SYSTEM, PROFILES, resolve
from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then, target

Model = Callable[..., ScriptedModelServer]


def names(server: ScriptedModelServer) -> list[str]:
    return [t["function"]["name"] for t in server.requests[0]["tools"]]


def spec_with(server: ScriptedModelServer, **builtin: object) -> object:
    return spec_of(
        {
            "target": target(server),
            "harness": {"builtin": builtin},
            "environment": {"checker": CHECK_HELLO},
        }
    )


def test_resolve_names_versions_and_conflicts() -> None:
    assert resolve("", []) is None
    assert resolve("minimal", []).ident == "minimal@1"  # type: ignore[union-attr]
    assert resolve("standard@1", []).ident == "standard@1"  # type: ignore[union-attr]
    with pytest.raises(ValueError, match="unknown profile"):
        resolve("tiny", [])
    with pytest.raises(ValueError, match="no version"):
        resolve("minimal@9", [])
    with pytest.raises(ValueError, match="no version"):
        resolve("minimal@x", [])
    with pytest.raises(ValueError, match="sets the toolset"):
        resolve("standard", ["grep"])
    # A profile's version is never rewritten once shipped: v1 stays v1.
    assert set(PROFILES["minimal"]) == {1}


def test_minimal_is_four_tools_a_short_prompt_and_no_escalation(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "done"}]))
    outcome = run(spec_with(server, profile="minimal"), record(), sandboxes)  # type: ignore[arg-type]
    assert names(server) == ["bash", "read_file", "write_file", "edit_file"]
    sent = server.requests[0]
    assert sent["messages"][0]["content"] == MINIMAL_SYSTEM
    # Prompt plus tool definitions stay near pi's budget of about 1,000 tokens.
    size = len(MINIMAL_SYSTEM) + len(json.dumps(sent["tools"]))
    assert size / 4 < 1000
    assert outcome.record is not None
    assert outcome.record.metadata["agent"]["profile"] == "minimal@1"


def test_standard_has_the_h2_toolset_and_compacts_when_given_a_window(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "done"}]))
    run(spec_with(server, profile="standard"), record(), sandboxes)  # type: ignore[arg-type]
    assert names(server) == [
        "bash",
        "read_file",
        "write_file",
        "request_escalation",
        "edit_file",
        "glob",
        "grep",
        "apply_patch",
        "memory",
        "session_search",
    ]


def strategy_of(profile: str, **context: object) -> str:
    cfg = agent_pb2.BuiltinHarness(profile=profile)
    json_format.ParseDict({"context": context}, cfg)
    return BuiltinHarness(HarnessContext(sandboxes=None), cfg).config.context.strategy  # type: ignore[arg-type]


def test_standard_compacts_when_given_a_window_and_an_explicit_strategy_wins() -> None:
    assert strategy_of("standard") == ""  # no window to compact against
    assert strategy_of("standard", window_tokens=100_000) == "compact"
    assert strategy_of("standard", strategy="clear-tool-results", window_tokens=1) == (
        "clear-tool-results"
    )
    assert strategy_of("minimal", window_tokens=100_000) == ""  # only standard defaults one


def test_code_mode_profile_shows_the_model_one_tool(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "done"}]))
    run(spec_with(server, profile="code-mode"), record(), sandboxes)  # type: ignore[arg-type]
    assert names(server) == ["run_code"]


def test_a_conflicting_or_unknown_profile_fails_the_trial_with_a_reason(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(calls_then([{"text": "done"}]))
    bad = spec_with(server, profile="minimal", tools={"builtin": ["grep"]})
    out = run(bad, record(), sandboxes)  # type: ignore[arg-type]
    assert "sets the toolset" in out.error
    assert server.requests == []


def test_the_profile_is_part_of_the_replay_guard(
    model: Model, sandboxes: LocalSandboxClient, tmp_path: Path
) -> None:
    server = model(calls_then([{"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}]))
    rec = {"mode": "record", "dir": "tapes"}
    run(
        spec_with(server, profile="minimal", recording=rec),  # type: ignore[arg-type]
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    replay = {"mode": "replay", "dir": "tapes"}
    same = run(
        spec_with(server, profile="minimal", recording=replay),  # type: ignore[arg-type]
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    assert same.record is not None
    assert not same.record.metadata["agent"].get("error")
    other = run(
        spec_with(server, profile="code-mode", recording=replay),  # type: ignore[arg-type]
        record(),
        sandboxes,
        base_dir=tmp_path,
    )
    assert "needs the same tools" in other.error

"""The per-run determinism record, and comparing a replay with its recording."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main
from evalsi.determinism import compare_runs, first_divergence
from evalsi.types import Record
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then

STEPS: list[dict[str, Any]] = [
    {"tool_calls": [("bash", {"command": "echo hi > out.txt"})]},
    {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]},
    {"text": "finished"},
]


def spec(server: Any, mode: str, **harness: Any) -> Any:
    return spec_of(
        {
            "target": {
                "connector": "openai-compatible",
                "model": "m",
                "base_url": server.base_url,
                "temperature": 0.2,
            },
            "harness": {"builtin": {"recording": {"mode": mode, "dir": "tapes"}, **harness}},
            "environment": {"image": "python:3.12", "checker": CHECK_HELLO},
        }
    )


def as_results(rec: Record) -> dict[str, Any]:
    """The part of an `evalsi run --output` file the comparison reads."""
    data = rec.to_dict()
    data["provenance"] = {"run": {"trial": 0}}
    return {"records": [data]}


def sandboxes_with_digest() -> LocalSandboxClient:
    client = LocalSandboxClient()
    original = client.create

    async def create(spec: Any = None) -> Any:
        sandbox = await original(spec)
        sandbox.image_digest = "sha256:" + "ab" * 32
        return sandbox

    client.create = create  # type: ignore[method-assign]
    return client


def test_a_run_writes_its_determinism_record(model: Any, tmp_path: Path) -> None:
    server = model(calls_then(STEPS))
    server.served_model = "m-2026-10-01"
    rec = run(spec(server, "off"), record(), sandboxes_with_digest(), base_dir=tmp_path).record
    assert rec is not None
    d = rec.metadata["determinism"]
    assert d["version"] == 1
    assert d["seeds"] == {"temperature": 0.2}
    # The model as the provider answered, apart from the name the spec asked for.
    assert d["models"] == {"m": {"served": ["m-2026-10-01"]}}
    assert d["image"]["name"] == "python:3.12"
    assert d["image"]["digest"] == "sha256:" + "ab" * 32
    assert d["image"]["isolation"]["driver"] == "local-test"
    assert d["harness"]["name"] == "evalsi-harness"
    assert d["harness"]["context"] == {"strategy": "none"}
    assert {"python", "evalsi_harness", "evalsi"} <= set(d["tools"])
    json.dumps(d)  # it travels in the record's metadata


def test_seeds_and_the_profile_are_recorded_when_they_exist(model: Any, tmp_path: Path) -> None:
    server = model(calls_then([{"text": "done"}]))
    rec = run(
        spec(server, "off", profile="minimal"), record(), LocalSandboxClient(), base_dir=tmp_path
    ).record
    assert rec is not None
    d = rec.metadata["determinism"]
    assert d["harness"]["profile"].startswith("minimal@")
    assert "image" in d


def test_a_replay_matches_its_recording_and_the_comparison_says_so(
    model: Any, tmp_path: Path
) -> None:
    server = model(calls_then(STEPS))
    server.served_model = "m-2026-10-01"
    recorded = run(spec(server, "record"), record(), LocalSandboxClient(), base_dir=tmp_path).record
    assert recorded is not None
    before = len(server.requests)
    replayed = run(spec(server, "replay"), record(), LocalSandboxClient(), base_dir=tmp_path).record
    assert replayed is not None
    assert len(server.requests) == before  # the model was not called
    report = compare_runs(as_results(recorded), as_results(replayed))
    assert (report["identical"], report["diverged"]) == (1, 0)
    [only] = report["runs"]
    assert only["status"] == "identical"
    # A replay serves the recorded turns, response model included, so nothing moved.
    assert "determinism_changes" not in only or all(
        not c["path"].startswith("models") for c in only["determinism_changes"]
    )


def test_a_rerun_that_chose_differently_names_the_first_step_that_differed(
    model: Any, tmp_path: Path
) -> None:
    server = model(calls_then(STEPS))
    server.served_model = "m-2026-10-01"
    recorded = run(spec(server, "off"), record(), sandboxes_with_digest(), base_dir=tmp_path).record
    assert recorded is not None
    # The provider moves its snapshot, and the model answers the second turn differently.
    changed = [STEPS[0], {"tool_calls": [("bash", {"command": "echo hello >> out.txt"})]}, STEPS[2]]
    server.script = calls_then(changed)
    again = run(spec(server, "off"), record(), sandboxes_with_digest(), base_dir=tmp_path).record
    assert again is not None
    report = compare_runs(as_results(recorded), as_results(again))
    [only] = report["runs"]
    assert only["status"] == "diverged"
    first = only["first_divergence"]
    # Step 0 (the first model turn) and 1 (its tool call) are the same; the second model
    # turn, step 2, is where the two runs part.
    assert (first["step"], first["type"]) == (2, "llm")
    assert "output" in {d["field"] for d in first["differences"]}
    assert "determinism_changes" not in only

    # A provider that moves its model snapshot is named at the first step, and in the record.
    server.script = calls_then(STEPS)
    server.served_model = "m-2026-11-01"
    moved = run(spec(server, "off"), record(), sandboxes_with_digest(), base_dir=tmp_path).record
    assert moved is not None
    [second] = compare_runs(as_results(recorded), as_results(moved))["runs"]
    assert second["first_divergence"]["step"] == 0
    fields = [d["field"] for d in second["first_divergence"]["differences"]]
    assert fields == ["gen_ai.response.model"]
    changes = {c["path"]: (c["recorded"], c["replayed"]) for c in second["determinism_changes"]}
    assert changes == {"models.m.served": (["m-2026-10-01"], ["m-2026-11-01"])}


def test_a_replay_that_stops_early_or_runs_on_diverges_where_a_step_is_missing() -> None:
    steps = [{"type": "llm", "name": "m"}, {"type": "tool", "name": "bash"}]
    short = first_divergence(steps, steps[:1])
    assert short is not None
    assert (short["step"], short["name"]) == (1, "bash")
    assert short["differences"][0] == {
        "field": "step",
        "recorded": "present",
        "replayed": "missing",
    }
    longer = first_divergence(steps[:1], steps)
    assert longer is not None
    assert longer["differences"][0]["replayed"] == "present"
    assert first_divergence(steps, copy.deepcopy(steps)) is None


def test_timing_and_ids_are_not_divergence() -> None:
    a = [
        {"type": "tool", "name": "bash", "span_id": "x", "duration_ms": 3, "output": {"text": "ok"}}
    ]
    b = [
        {
            "type": "tool",
            "name": "bash",
            "span_id": "y",
            "duration_ms": 90,
            "output": {"text": "ok"},
        }
    ]
    assert first_divergence(a, b) is None
    b[0]["output"] = {"text": "different"}
    found = first_divergence(a, b)
    assert found is not None
    assert found["differences"] == [{"field": "output", "recorded": "ok", "replayed": "different"}]


def test_the_cli_compares_two_results_files_and_fails_on_divergence(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    def rec(output: str) -> dict[str, Any]:
        return {
            "records": [
                {
                    "id": "r",
                    "provenance": {"run": {"trial": 1}},
                    "trajectory": {
                        "steps": [{"type": "tool", "name": "bash", "output": {"text": output}}]
                    },
                }
            ]
        }

    a, b, c = tmp_path / "a.json", tmp_path / "b.json", tmp_path / "c.json"
    a.write_text(json.dumps(rec("ok")))
    b.write_text(json.dumps(rec("ok")))
    c.write_text(json.dumps(rec("changed")))
    assert main(["determinism", str(a), "--against", str(b)]) == 0
    capsys.readouterr()
    assert main(["determinism", str(a), "--against", str(c)]) == 1
    out = json.loads(capsys.readouterr().out)
    assert out["runs"][0]["trial"] == 1
    assert out["runs"][0]["first_divergence"]["step"] == 0

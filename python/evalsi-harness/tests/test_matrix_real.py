"""``evalsi matrix`` over results files written by real embedded runs (H3).

The unit tests in evalsi/tests build results by hand; this one runs the CLI twice,
as two profiles on the same model, and reads what it wrote. It is the closest
thing here to the exit test "a run compares two harness profiles on the same
model and reports the difference with its interval".
"""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main
from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer

Model = Callable[..., ScriptedModelServer]


def write_run(path: Path, server: ScriptedModelServer, profile: str, **context: Any) -> None:
    spec = {
        "target": {
            "connector": "openai-compatible",
            "model": "scripted",
            "base_url": server.base_url,
        },
        "dataset": {
            "inline": {
                "records": [{"id": f"t{i}", "input": {"text": "write hello"}} for i in range(3)]
            }
        },
        "harness": {"builtin": {"profile": profile, "context": context}},
        "environment": {"checker": {"command": ["sh", "-c", 'test "$(cat out.txt)" = hello']}},
        "evaluators": [{"ref": "task-success"}],
        "trials": 1,
    }
    path.write_text(
        json.dumps(
            {
                "apiVersion": "evals.si/v1alpha1",
                "kind": "EvalRun",
                "metadata": {"name": profile},
                "spec": spec,
            }
        )
    )


def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
    """Write the file, then finish: stateless, so every task gets the same replies."""
    if any(m.get("role") == "tool" for m in messages):
        return {"text": "done"}
    return {"tool_calls": [("bash", {"command": "echo hello > out.txt"})]}


def test_two_profiles_on_one_model_compared_from_real_results_files(
    model: Model,
    sandboxes: LocalSandboxClient,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    async def connect() -> LocalSandboxClient:
        return sandboxes

    monkeypatch.setattr("evalsi.sandbox.client.connect", connect)
    out: dict[str, Path] = {}
    for name, profile, context in [
        ("minimal", "minimal", {}),
        ("standard", "standard", {"window_tokens": 100_000}),
    ]:
        server = model(script)
        spec_path = tmp_path / f"{name}.yaml"
        write_run(spec_path, server, profile, **context)
        out[name] = tmp_path / f"{name}.json"
        assert main(["run", "-f", str(spec_path), "--output", str(out[name]), "--quiet"]) in (0, 3)
    capsys.readouterr()

    status = main(
        [
            "matrix",
            str(out["minimal"]),
            str(out["standard"]),
            "--metric",
            "task-success",
            "--mechanisms",
            "--format",
            "json",
        ]
    )
    assert status == 0
    cells = json.loads(capsys.readouterr().out)
    minimal, standard = cells
    assert (minimal["harness"], standard["harness"]) == ("minimal@1", "standard@1")
    # The model comes from where an agent run records its target.
    assert minimal["model"] == standard["model"] == "scripted"
    # Cost sits beside the score, from the records' own usage and step counts.
    assert minimal["cost"]["calls"] is not None
    assert minimal["cost"]["tokens"] is not None
    assert standard["n"] == minimal["n"] == 3
    assert minimal["mean"] == standard["mean"] == 1.0  # every task was fixed
    assert minimal["cost"]["calls"] == 2  # a tool call, then the answer
    assert standard["diff"] is not None  # paired against the first cell

    # The profile's own choices show up as mechanisms: standard's compaction came
    # from the profile, not the spec, and is read from what the run recorded.
    found = {m["name"]: m for m in standard["mechanisms"]}
    assert "context:compact" in found
    assert found["context:compact"]["of"] == 3
    assert found["context:compact"]["ran"] == 0  # 100k window: it never filled
    assert found["tool:edit_file"]["ran"] == 0  # offered, and the script never called it
    assert "tool:edit_file" in {m["name"] for m in minimal["mechanisms"]}
    assert "tool:grep" not in {m["name"] for m in minimal["mechanisms"]}  # minimal has four tools

"""The harness-by-model matrix over results files (H3)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from evalsi import matrix
from evalsi.cli import main


def results(
    profile: str,
    scores: list[float],
    *,
    tokens: int,
    steps: int,
    name: str = "run",
    dataset: str = "d" * 64,
    model: str = "m1",
) -> dict[str, Any]:
    n = len(scores)
    mean = sum(scores) / n
    return {
        "manifest": {
            "name": name,
            "dataset": {"sha256": dataset},
            "target": {"connector": "openai-compatible", "model": model},
            "spec": {"harness": {"builtin": {"profile": profile}}},
        },
        "summaries": [
            {
                "metric": "task-success",
                "n": n,
                "mean": mean,
                "ci": {"low": mean - 0.2, "high": min(1.0, mean + 0.2), "level": 0.95},
            }
        ],
        "results": [
            {
                "record_id": f"t{i}",
                "evaluator": "task-success",
                "outcome": "scored",
                "trial": 0,
                "scores": [{"name": "task-success", "number": v}],
            }
            for i, v in enumerate(scores)
        ],
        "records": [
            {
                "id": f"t{i}",
                "usage": {"input_tokens": tokens - 100, "output_tokens": 100, "cost_usd": 0.01},
                "metadata": {"agent": {"steps": steps, "profile": profile, "tools_offered": []}},
            }
            for i in range(n)
        ],
    }


A = results("standard@1", [1, 1, 0, 1, 1, 0, 1, 1, 1, 0], tokens=6600, steps=26)
B = results("minimal@1", [1, 1, 1, 1, 1, 0, 1, 1, 1, 1], tokens=3600, steps=10)


def test_cost_sits_beside_the_score_and_the_difference_has_an_interval() -> None:
    cells = matrix.build([("standard", A), ("minimal", B)], "task-success")
    base, cand = cells
    assert (base.harness, cand.harness) == ("standard@1", "minimal@1")
    assert base.model == "m1"
    assert (base.cost.calls, base.cost.tokens, base.cost.usd) == (26, 6600, 0.01)
    assert (cand.cost.calls, cand.cost.tokens) == (10, 3600)
    # Cheaper and no worse: the score per thousand tokens shows it.
    assert cand.score_per_ktok is not None
    assert base.score_per_ktok is not None
    assert cand.score_per_ktok > base.score_per_ktok
    assert base.diff is None
    assert cand.diff is not None
    assert cand.diff == pytest.approx(0.2)
    assert cand.diff_low is not None
    assert cand.diff_high is not None
    assert cand.diff_low <= cand.diff <= cand.diff_high
    text = matrix.table(cells)
    assert "minimal@1" in text
    assert "tokens/task" in text


def test_a_cell_missing_the_metric_or_the_cost_is_reported_not_guessed() -> None:
    with pytest.raises(ValueError, match="no metric 'nope'"):
        matrix.build([("a", A)], "nope")
    bare = {"summaries": A["summaries"], "results": A["results"], "records": [], "manifest": {}}
    (cell,) = matrix.build([("bare", bare)], "task-success")
    assert cell.cost.tokens is None
    assert cell.score_per_ktok is None
    assert cell.harness == "custom"
    assert "-" in matrix.table([cell])


def test_tuned_and_held_out_cells_are_split() -> None:
    held = results("minimal@1", [1, 0, 1, 1], tokens=3000, steps=9, name="other", dataset="e" * 64)
    cells = matrix.build(
        [("standard", A), ("minimal", B), ("held", held)],
        "task-success",
        tuned_on=["d" * 64],
    )
    assert [c.split for c in cells] == ["tuned", "tuned", "held-out"]
    assert matrix.build([("a", A)], "task-success")[0].split == ""


def test_the_cli_prints_a_table_and_json(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    pa, pb = tmp_path / "a.json", tmp_path / "b.json"
    pa.write_text(json.dumps(A))
    pb.write_text(json.dumps(B))
    assert main(["matrix", str(pa), str(pb), "--metric", "task-success"]) == 0
    assert "minimal@1" in capsys.readouterr().out
    assert main(["matrix", str(pa), str(pb), "--metric", "task-success", "--format", "json"]) == 0
    rows = json.loads(capsys.readouterr().out)
    assert [r["harness"] for r in rows] == ["standard@1", "minimal@1"]
    assert main(["matrix", str(pa), "--metric", "nope"]) == 2
    assert main(["matrix", str(tmp_path / "missing.json"), "--metric", "x"]) == 2


def with_trajectory(
    data: dict[str, Any], called: list[list[str]], offered: list[str]
) -> dict[str, Any]:
    for rec, names in zip(data["records"], called, strict=True):
        rec["trajectory"] = {"steps": [{"type": "STEP_TYPE_TOOL", "name": n} for n in names]}
        rec["metadata"]["agent"]["tools_offered"] = offered
    return data


def test_a_mechanism_that_never_ran_is_flagged(capsys: pytest.CaptureFixture[str]) -> None:
    data = results("standard@1", [1, 0, 1], tokens=1000, steps=5)
    data["manifest"]["spec"]["harness"]["builtin"]["context"] = {"strategy": "compact"}
    with_trajectory(
        data,
        [["bash", "grep"], ["bash"], ["bash", "grep"]],
        ["bash", "grep", "edit_file", "request_escalation"],
    )
    found = {m.name: (m.ran, m.of) for m in matrix.mechanisms(data)}
    assert found["tool:grep"] == (2, 3)
    assert found["tool:edit_file"] == (0, 3)  # offered, never called
    assert found["context:compact"] == (0, 3)  # the window never filled
    text = matrix.mechanism_lines([("standard", data)])
    assert "tool:edit_file 0/3 (never ran)" in text
    assert "context:compact 0/3 (never ran)" in text
    assert "tool:grep 2/3" in text
    assert "(never ran)" not in text.split("tool:grep 2/3")[1].split(",")[0]


def test_a_context_strategy_that_acted_is_counted(tmp_path: Path) -> None:
    data = results("standard@1", [1, 1], tokens=1000, steps=5)
    data["manifest"]["spec"]["harness"]["builtin"]["context"] = {"strategy": "compact"}
    data["records"][0]["metadata"]["agent"]["context"] = {"clears": 1, "compactions": 1}
    data["records"][1]["metadata"]["agent"]["context"] = {"clears": 0, "compactions": 0}
    (m,) = matrix.mechanisms(data)
    assert (m.name, m.ran, m.of) == ("context:compact", 1, 2)
    path = tmp_path / "a.json"
    path.write_text(json.dumps(data))
    assert main(["matrix", str(path), "--metric", "task-success", "--mechanisms"]) == 0
    assert (
        main(["matrix", str(path), "--metric", "task-success", "--mechanisms", "--format", "json"])
        == 0
    )


def test_cells_on_different_datasets_are_not_paired() -> None:
    other = results("minimal@1", [1, 1, 1], tokens=3000, steps=9, dataset="e" * 64)
    _, cand = matrix.build([("a", A), ("other", other)], "task-success")
    assert cand.diff is None
    assert cand.mean == 1.0  # its own score and cost are still reported
    assert cand.cost.tokens == 3000


def test_the_ci_level_shown_is_the_cells_own() -> None:
    wide = results("minimal@1", [1, 0, 1, 1], tokens=100, steps=2)
    wide["summaries"][0]["ci"]["level"] = 0.9
    cells = matrix.build([("a", A), ("w", wide)], "task-success")
    assert [c.level for c in cells] == [0.95, 0.9]
    assert "90%" in matrix.table(cells)


def test_a_cli_agents_calls_are_its_model_steps_not_its_sends() -> None:
    cli = results("agent:cli", [1, 1], tokens=500, steps=1)
    for rec in cli["records"]:
        del rec["metadata"]["agent"]["tools_offered"]  # a CLI agent has no tool list
        rec["trajectory"] = {
            "steps": [{"type": "STEP_TYPE_LLM"}] * 7 + [{"type": "STEP_TYPE_AGENT"}]
        }
    (cell,) = matrix.build([("cli", cli)], "task-success")
    assert cell.cost.calls == 7
    none = results("agent:cli", [1], tokens=500, steps=1)
    del none["records"][0]["metadata"]["agent"]["tools_offered"]
    (blank,) = matrix.build([("cli", none)], "task-success")
    assert blank.cost.calls is None  # unknown, not the one send


def test_an_idle_evidence_strategy_is_not_counted_as_run() -> None:
    data = results("standard@1", [1, 1], tokens=100, steps=2)
    data["manifest"]["spec"]["harness"]["builtin"]["context"] = {"strategy": "evidence-state"}
    data["records"][0]["metadata"]["agent"]["evidence"] = {"entries": 0, "conclusions": 0}
    data["records"][1]["metadata"]["agent"]["evidence"] = {"entries": 3, "conclusions": 1}
    (m,) = matrix.mechanisms(data)
    assert (m.name, m.ran, m.of) == ("context:evidence-state", 1, 2)

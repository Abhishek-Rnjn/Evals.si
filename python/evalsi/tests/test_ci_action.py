from __future__ import annotations

import copy
import json
import os
import runpy
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
import yaml

from evalsi.cli import main
from evalsi.targets import Generation, TargetConfig
from evalsi.types import Record, Usage

ROOT = Path(__file__).resolve().parents[3]
ADAPTER = ROOT / ".github/actions/evalsi/run.py"


def invoke(tmp_path: Path, project: Path, spec: str, baseline: str = "") -> tuple[int, str, Path]:
    env = {
        **os.environ,
        "GITHUB_WORKSPACE": str(project.parent),
        "GITHUB_SHA": "abc123",
        "RUNNER_TEMP": str(tmp_path),
        "GITHUB_OUTPUT": str(tmp_path / "outputs"),
        "GITHUB_STEP_SUMMARY": str(tmp_path / "summary"),
        "EVALSI_ACTION_DIRECTORY": project.name,
        "EVALSI_ACTION_SPEC": spec,
        "EVALSI_ACTION_BASELINE": baseline,
    }
    proc = subprocess.run(
        [sys.executable, str(ADAPTER)], env=env, capture_output=True, text=True, check=False
    )
    assert proc.returncode == 0  # The composite final step propagates exit-code after upload.
    assert not proc.stdout
    assert not proc.stderr
    outputs = dict(line.split("=", 1) for line in (tmp_path / "outputs").read_text().splitlines())
    return (
        int(outputs["exit-code"]),
        (tmp_path / "summary").read_text(),
        Path(outputs["artifact-directory"]),
    )


@pytest.mark.parametrize(("spec", "expected"), [("run.yaml", 0), ("regressed.yaml", 3)])
def test_real_cli_gate_outcome_and_private_data(tmp_path: Path, spec: str, expected: int) -> None:
    project = tmp_path / "project ; touch PWNED"
    assert main(["init", str(project)]) == 0
    records = (
        (project / "data.jsonl").read_text().replace("Capital of France?", "private-input-849")
    )
    (project / "data.jsonl").write_text(records)
    code, summary, artifacts = invoke(tmp_path, project, spec)
    assert code == expected
    assert "private-input-849" not in summary
    assert "abc123" in summary
    assert "exact-match" in summary
    assert (artifacts / "results.json").exists()
    assert not (tmp_path / "PWNED").exists()


@pytest.mark.parametrize(
    "baseline_kind",
    ["matching", "different-data", "partial", "malformed", "judge", "summary", "trials"],
)
def test_baseline_requires_same_contract_and_complete_pairs(
    tmp_path: Path, baseline_kind: str
) -> None:
    project = tmp_path / "project"
    assert main(["init", str(project)]) == 0
    _, _, artifacts = invoke(tmp_path, project, "run.yaml")
    data = json.loads((artifacts / "results.json").read_text())
    if baseline_kind == "different-data":
        data["manifest"]["dataset"]["sha256"] = "other"
    elif baseline_kind == "partial":
        data["results"].pop()
    elif baseline_kind == "summary":
        data["manifest"]["spec"]["summary"] = {"confidence_level": 0.8}
    elif baseline_kind in ("judge", "trials"):
        data["manifest"][baseline_kind] = "different"
    path = project / "baseline.json"
    path.write_text("{" if baseline_kind == "malformed" else json.dumps(data))
    code, summary, _ = invoke(tmp_path, project, "run.yaml", "baseline.json")
    assert code == (0 if baseline_kind == "matching" else 1)
    if baseline_kind == "matching":
        assert "Baseline comparison (advisory)" in summary
        assert "| 2 | 0 |" in summary
    else:
        assert "Action failed (exit 1)" in summary


@pytest.fixture
def comparison_data(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> dict[str, Any]:
    class Target:
        config = TargetConfig("openai-compatible", "scripted", base_url="http://unused")

        async def generate(self, record: Record) -> Generation:
            return Generation(output=record.reference, usage=Usage())

        async def aclose(self) -> None:
            pass

    monkeypatch.setattr("evalsi.run.create_target", lambda config: Target())
    project = tmp_path / "project"
    assert main(["init", str(project)]) == 0
    # Repeated trials also cover derived pass@k/pass^k and clustering.
    path = project / "run.yaml"
    spec = yaml.safe_load(path.read_text())
    spec["spec"]["trials"] = 2
    spec["spec"]["target"] = {
        "connector": "openai-compatible",
        "model": "scripted",
        "base_url": "http://unused",
        "api_key_env": "none",
    }
    path.write_text(yaml.safe_dump(spec))
    output = tmp_path / "results.json"
    assert main(["run", "-f", str(path), "--output", str(output)]) == 0
    data: dict[str, Any] = json.loads(output.read_text())
    return data


@pytest.mark.parametrize(
    "change", ["command", "files", "parser", "protect", "on_change", "timeout"]
)
def test_checker_grading_changes_cannot_be_compared(
    comparison_data: dict[str, Any], change: str
) -> None:
    adapter = runpy.run_path(str(ADAPTER))
    baseline = copy.deepcopy(comparison_data)
    baseline["manifest"]["spec"]["environment"] = {
        "files": {"agent.py": "original agent"},
        "checker": {"command": ["check"], "files": {"test.py": "assert actual == expected"}},
    }
    candidate = copy.deepcopy(baseline)
    candidate["manifest"]["spec"]["environment"]["checker"][change] = "changed grading"
    with pytest.raises(ValueError, match="checker configuration"):
        adapter["comparison"](baseline, candidate)
    # Agent implementation and target changes remain valid comparison candidates.
    candidate = copy.deepcopy(baseline)
    candidate["manifest"]["spec"]["environment"]["files"]["agent.py"] = "new agent"
    candidate["manifest"]["target"] = {"model": "candidate"}
    assert "Baseline comparison" in "\n".join(adapter["comparison"](baseline, candidate))


@pytest.mark.parametrize(
    "missing", ["skipped", "error", "omitted", "trial", "record", "metric", "duplicate"]
)
def test_shared_incomplete_coverage_cannot_be_compared(
    comparison_data: dict[str, Any], missing: str
) -> None:
    data = copy.deepcopy(comparison_data)
    if missing in ("skipped", "error"):
        data["results"][0]["outcome"] = missing
        data["results"][0]["scores"] = []
    elif missing == "omitted":
        data["results"].pop(0)
    elif missing == "record":
        data["records"].pop(0)
    elif missing == "trial":
        data["results"][0]["trial"] = 9
    elif missing == "duplicate":
        data["results"][0] = copy.deepcopy(data["results"][1])
    else:
        # A scored row missing a numeric metric must not be silently paired away.
        data["results"][0]["scores"] = []
    adapter = runpy.run_path(str(ADAPTER))
    with pytest.raises(ValueError, match=r"coverage|missing|incomplete"):
        adapter["comparison"](data, copy.deepcopy(data))


def test_optional_unproduced_metric_does_not_block_complete_coverage(
    comparison_data: dict[str, Any],
) -> None:
    data = copy.deepcopy(comparison_data)
    data["summaries"].append(
        {
            "metric": "exact-match.optional",
            "evaluator": "exact-match",
            "kind": "number",
            "n": 0,
            "mean": None,
            "errors": 0,
            "skipped": 0,
        }
    )
    adapter = runpy.run_path(str(ADAPTER))
    assert adapter["comparison"](data, copy.deepcopy(data))


def test_configuration_error_cannot_pass(tmp_path: Path) -> None:
    code, summary, _ = invoke(tmp_path, tmp_path, "missing.yaml")
    assert code == 1
    assert "Action failed (exit 1)" in summary


def test_spec_without_explicit_gates_cannot_pass(tmp_path: Path) -> None:
    project = tmp_path / "project"
    assert main(["init", str(project)]) == 0
    spec_path = project / "run.yaml"
    spec = yaml.safe_load(spec_path.read_text())
    del spec["spec"]["gates"]
    spec_path.write_text(yaml.safe_dump(spec))
    code, _, _ = invoke(tmp_path, project, "run.yaml")
    assert code == 1


def test_summary_escapes_metric_labels_and_omits_free_text() -> None:
    adapter = runpy.run_path(str(ADAPTER))
    data: dict[str, Any] = {
        "manifest": {"evalsi_version": "0.3.0", "dataset": {"sha256": "abc"}},
        "summaries": [{"metric": "<img src=x>|\nmetric ![link](url) `x`", "n": 2, "mean": 1}],
        "gates": [
            {
                "metric": "<script>",
                "stat": "mean",
                "passed": False,
                "reason": "private-reason-849",
            }
        ],
        "records": [{"output": "private-output-849"}],
    }
    rendered = "\n".join(adapter["summary"](data, 3))
    assert "<img" not in rendered
    assert "<script>" not in rendered
    assert "&#124;" in rendered
    assert "![link]" not in rendered
    assert "`x`" not in rendered
    assert "private-" not in rendered


def test_composite_upload_is_opt_in_and_final_step_preserves_failure() -> None:
    action = yaml.safe_load((ROOT / "action.yml").read_text())
    assert action["inputs"]["artifact-name"]["default"] == ""
    steps = action["runs"]["steps"]
    upload = next(
        step for step in steps if step.get("uses", "").startswith("actions/upload-artifact")
    )
    assert "inputs.artifact-name != ''" in upload["if"]
    assert steps[-1]["if"] == "always()"
    assert "else 1" in steps[-1]["run"]

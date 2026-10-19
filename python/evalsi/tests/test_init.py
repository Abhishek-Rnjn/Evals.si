from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi.cli import main
from evalsi.runspec import load_spec


def test_recorded_starter_runs_and_rejects_regression(tmp_path: Path) -> None:
    directory = tmp_path / "project with spaces"
    assert main(["init", str(directory)]) == 0
    for name, expected in [("run", 0), ("regressed", 3)]:
        output = tmp_path / f"{name}.json"
        assert (
            main(["run", "-f", str(directory / f"{name}.yaml"), "--output", str(output)])
            == expected
        )
        result = json.loads(output.read_text())
        assert result["manifest"]["dataset"]["records"] == 2
        assert not any(s["errors"] for s in result["summaries"])


@pytest.mark.parametrize("existing", ["directory", "file", "symlink"])
def test_init_never_overwrites_existing_paths(tmp_path: Path, existing: str) -> None:
    dest = tmp_path / "dest"
    if existing == "directory":
        dest.mkdir()
        (dest / "run.yaml").write_text("keep")
    elif existing == "file":
        dest.write_text("keep")
    else:
        real = tmp_path / "real"
        real.mkdir()
        dest.symlink_to(real, target_is_directory=True)
    assert main(["init", str(dest)]) == 1
    if existing == "directory":
        assert (dest / "run.yaml").read_text() == "keep"
        assert list(dest.iterdir()) == [dest / "run.yaml"]
    elif existing == "file":
        assert dest.read_text() == "keep"
    else:
        assert list(dest.iterdir()) == []


def test_agent_template_validates_and_preserves_real_isolation(tmp_path: Path) -> None:
    dest = tmp_path / "agent"
    assert main(["init", str(dest), "--template", "agent"]) == 0
    for file in ("run.yaml", "regressed.yaml"):
        spec = load_spec(dest / file).spec
        assert spec.environment.sandbox.min_isolation == "namespaced"
        assert spec.environment.sandbox.network == "deny"
        assert spec.environment.checker.command
    assert "synthetic" in (dest / "README.md").read_text()

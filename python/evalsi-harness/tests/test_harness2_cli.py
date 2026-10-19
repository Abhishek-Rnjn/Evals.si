"""``evalsi harness2`` over the synthetic fixture (written from source, not a recorded run)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi.cli import main

NS = Path(__file__).parent / "fixtures" / "harness2" / "ns" / "jb-oc-agg2"


def test_import_then_matrix(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["harness2", "import", str(NS), "--out", str(tmp_path)]) == 0
    names = sorted(p.name for p in tmp_path.glob("*.json"))
    assert names == ["agg2.json", "base.json", "seq2.json"]
    capsys.readouterr()
    code = main(
        ["matrix", str(tmp_path / "base.json"), str(tmp_path / "agg2.json"),
         "--metric", "h2-criteria-fraction", "--format", "json"]
    )  # fmt: skip
    assert code == 0
    assert len(json.loads(capsys.readouterr().out)) == 2


def test_import_refuses_to_write_inside_the_run(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["harness2", "import", str(NS), "--out", str(NS / "out")]) == 2
    assert "refusing" in capsys.readouterr().err
    assert not (NS / "out").exists()


def test_calibrate_json(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["harness2", "calibrate", str(NS), "--format", "json"]) == 0
    rows = json.loads(capsys.readouterr().out)
    assert rows[-1]["cell"] == "all"


def test_calibrate_results_needs_metric(tmp_path: Path) -> None:
    assert main(["harness2", "calibrate", str(NS), "--results", str(tmp_path / "x.json")]) == 2

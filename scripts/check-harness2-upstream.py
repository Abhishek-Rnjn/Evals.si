#!/usr/bin/env python3
"""Verify the importer against a pinned upstream deterministic refinement run.

This executes upstream Python code from a separately supplied checkout. It does
not download benchmark data, use a model or certify harness quality.
"""

from __future__ import annotations

import argparse
import csv
import importlib
import json
import subprocess
import sys
from dataclasses import replace
from decimal import Decimal
from pathlib import Path
from typing import Any

REVISION = "1cfe254e8d25b15f37048c3730134686abe60335"


def verify(upstream: Path, output: Path) -> None:
    revision = subprocess.check_output(
        ["git", "-C", str(upstream), "rev-parse", "HEAD"], text=True
    ).strip()
    if revision != REVISION:
        raise ValueError(f"expected upstream revision {REVISION}, got {revision}")
    if output.exists():
        raise ValueError("output must be a new directory")
    sys.path.insert(0, str(upstream))
    demo = importlib.import_module("harness2.demo")
    schedule = importlib.import_module("harness2.core.schedule")
    judge = importlib.import_module("harness2.core.judge")
    judge_cli = importlib.import_module("harness2.cli.judge")
    importer = importlib.import_module("evalsi_harness.benchmarks.harness2")

    class GradedDemo(demo.DemoAdapter):
        def score(self, run_dir: Path, task_id: str) -> dict[str, Any]:
            with (self.task_files(task_id) / "ledger.csv").open() as source:
                expected = sum(
                    (Decimal(row["amount"]) for row in csv.DictReader(source)), Decimal(0)
                )
            passed = self.deliverable_text(run_dir) == f"Total: {expected:.2f}\n"
            return {"n_passed": int(passed), "n_criteria": 1, "all_pass": passed}

    adapter = GradedDemo(output)
    proposer = demo.DemoProposer(adapter.spec.channels["proposer"])
    kwargs = {
        "adapter": adapter,
        "prop": proposer,
        "runs_root": adapter.spec.runs_root,
        "library_root": output / "library" / "k2",
        "modes": ("agg", "seq"),
    }
    result = schedule.run_domain("accounting", adapter.tasks(), 2, **kwargs)
    assert result["done"] == 2
    assert result["failed"] == 0
    executions = adapter.execution_count
    resumed = schedule.run_domain("accounting", adapter.tasks(), 2, **kwargs)
    assert adapter.execution_count == executions
    assert all(task["status"] == "skipped" for task in resumed["tasks"])
    namespace = adapter.spec.runs_root / "demo-oc-agg2"
    assert judge_cli.judge_bases(adapter, namespace) == 0
    selections = judge_cli.collect_selections(namespace, ("agg", "seq"))
    rows = judge.judge_selections(adapter, selections, parallel=1)
    assert len(rows) == 4
    assert all(row.graded and row.all_pass for row in rows)
    judge.write(rows, namespace / "judged.json")
    imported = importer.import_namespace(namespace)
    check_cells(imported.cells(), output)
    check_missing_grades(namespace, importer, judge, rows)
    check_ablation(adapter, namespace, output, imported, selections)
    check_commands(output, namespace)
    receipt = {
        "upstream_revision": revision,
        "kind": "upstream deterministic demo, independent arithmetic checker",
        "tasks": 2,
        "k": 2,
        "modes": ["agg", "seq"],
        "executor_runs": executions,
        "resume_added_runs": 0,
        "baseline_mean": 0,
        "adopted_means": {"agg2": 1, "seq2": 1},
        "missing_grades": "skipped, no score",
        "verifier_failure": "error, no score",
        "ablation": "script revert effect +1; README-stub revert effect 0; two tasks each",
        "matrix_gate": "positive exits 0; baseline/candidate reversed exits 1",
        "calibration": "four recorded selections agree with deterministic grade; not human labels",
        "provider_calls": 0,
        "quality_certification": False,
    }
    (output / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(receipt, indent=2))


def check_cells(cells: dict[str, Any], output: Path) -> None:
    assert set(cells) == {"base", "agg2", "seq2"}
    for name, cell in cells.items():
        expected = 0 if name == "base" else 1
        assert len(cell["records"]) == (4 if name == "base" else 2)
        assert all(summary["mean"] == expected for summary in cell["summaries"])
        assert all(summary["errors"] == 0 for summary in cell["summaries"])
        destination = output / "imported"
        destination.mkdir(exist_ok=True)
        (destination / f"{name}.json").write_text(
            json.dumps(cell, indent=2) + "\n", encoding="utf-8"
        )


def check_missing_grades(namespace: Path, importer: Any, judge: Any, rows: list[Any]) -> None:
    judged_path = namespace / "judged.json"
    judged = judged_path.read_text(encoding="utf-8")
    try:
        judged_path.unlink()
        ungraded = importer.import_namespace(namespace).cells()
        for name in ("agg2", "seq2"):
            assert all(row["outcome"] == "skipped" for row in ungraded[name]["results"])
            assert all(not row.get("scores") for row in ungraded[name]["results"])
        rows[0].n_passed = None
        rows[0].n_criteria = None
        rows[0].all_pass = False
        rows[0].error = "deliberate verifier failure"
        judge.write(rows, judged_path)
        broken = importer.import_namespace(namespace).cells()
        errors = [
            row
            for name in ("agg2", "seq2")
            for row in broken[name]["results"]
            if row["outcome"] == "error"
        ]
        assert errors
        assert all(not row.get("scores") for row in errors)
    finally:
        judged_path.write_text(judged, encoding="utf-8")


def check_ablation(
    adapter: Any, namespace: Path, output: Path, imported: Any, selections: list[dict[str, Any]]
) -> None:
    from evalsi.ablation import attribute, plan_dirs
    from evalsi_harness.benchmarks.harness2 import Harness2Import

    chosen = next(selection for selection in selections if selection["mode"] == "agg")
    adopted = namespace / chosen["task_id"] / "harnesses" / chosen["chosen_label"]
    destination = output / "ablation"
    manifest = plan_dirs(adapter.base_harness(), adopted, destination, by="unit")
    assert {component["component"] for component in manifest["components"]} == {
        "c7:scripts/summarize.py",
        "other",
    }
    variants = {}
    for component in manifest["components"]:
        variant = destination / component["variant_dir"]
        measured = {}
        for task_id in adapter.tasks():
            run_dir = adapter.execute(
                task_id, variant, f"verification-ablation/{component['variant_dir']}/{task_id}"
            )
            measured[task_id] = adapter.score(run_dir, task_id)
        executions = [
            replace(
                execution,
                n_passed=measured[execution.task_id]["n_passed"],
                n_criteria=measured[execution.task_id]["n_criteria"],
            )
            for execution in imported.executions
            if execution.kind == "chosen" and execution.mode == "agg"
        ]
        variants[component["component"]] = Harness2Import(
            imported.namespace, imported.root, executions
        ).cells()["agg2"]
    cells = imported.cells()
    attribution = attribute(cells["base"], cells["agg2"], variants, "h2-criteria-fraction")
    effect = next(row for row in attribution if row.component == "c7:scripts/summarize.py")
    assert effect.effect == 1
    assert effect.paired_n == 2
    assert next(row for row in attribution if row.component == "other").effect == 0
    (destination / "attribution.json").write_text(
        json.dumps([row.to_dict() for row in attribution], indent=2) + "\n", encoding="utf-8"
    )


def check_commands(output: Path, namespace: Path) -> None:
    cli = [sys.executable, "-m", "evalsi.cli"]
    base, adopted = (str(output / "imported" / f"{name}.json") for name in ("base", "agg2"))
    for label, paths, expected in (
        ("positive", [base, adopted], 0),
        ("negative", [adopted, base], 1),
    ):
        result = subprocess.run(
            [
                *cli,
                "matrix",
                *paths,
                "--metric",
                "h2-criteria-fraction",
                "--gate",
                "--format",
                "json",
            ],
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
        assert result.returncode == expected, result.stderr
        assert len(json.loads(result.stdout)) == 2
        (output / f"matrix-{label}.json").write_text(result.stdout, encoding="utf-8")
    result = subprocess.run(
        [*cli, "harness2", "calibrate", str(namespace), "--format", "json"],
        capture_output=True,
        text=True,
        check=True,
        timeout=30,
    )
    calibration = json.loads(result.stdout)
    assert calibration[-1]["compared"] == calibration[-1]["agreed"] == 4
    (output / "calibration.json").write_text(result.stdout, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    verify(args.upstream.resolve(), args.output.resolve())


if __name__ == "__main__":
    main()

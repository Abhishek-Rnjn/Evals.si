"""The Harness² run importer, over a synthetic namespace directory written from
the Harness² source code (tests/fixtures/harness2/README.txt), not from a
recorded run."""

from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

import pytest

from evalsi.matrix import build, harness_of
from evalsi_harness.benchmarks.harness2 import (
    ALL_PASS,
    FRACTION,
    component_of,
    import_namespace,
    write_cells,
)

FIXTURE = Path(__file__).parent / "fixtures" / "harness2" / "ns" / "jb-oc-agg2"


@pytest.fixture
def ns(tmp_path: Path) -> Path:
    dest = tmp_path / "jb-oc-agg2"
    shutil.copytree(FIXTURE, dest)
    return dest


def _snapshot(root: Path) -> dict[str, tuple[float, bytes | None]]:
    return {
        str(p.relative_to(root)): (p.stat().st_mtime, p.read_bytes() if p.is_file() else None)
        for p in sorted(root.rglob("*"))
    }


def _by_trial(cell: dict[str, Any], metric: str) -> dict[str, dict[str, Any]]:
    return {
        f"{r['record_id']}#{r.get('trial', 0)}": r
        for r in cell["results"]
        if r["evaluator"] == metric
    }


def _summary(cell: dict[str, Any], metric: str) -> dict[str, Any]:
    return next(s for s in cell["summaries"] if s["metric"] == metric)


def test_cells_are_base_and_each_mode_and_k(ns: Path) -> None:
    imp = import_namespace(ns)
    assert imp.cell_names() == ["base", "agg2", "seq2"]
    cells = imp.cells()
    assert harness_of(cells["base"]) == "harness2:base"
    assert harness_of(cells["agg2"]) == "harness2:agg-k2"


def test_chosen_runs_score_as_passed_over_criteria(ns: Path) -> None:
    agg = import_namespace(ns).cells()["agg2"]
    values = {k: r["scores"][0]["number"] for k, r in _by_trial(agg, FRACTION).items()}
    # alpha/one 8/10, beta 3/4, and gamma 3/5 from the legacy "arm" spelling.
    assert values == {"alpha/one#0": 0.8, "beta#0": 0.75, "gamma#0": 0.6}
    assert _summary(agg, FRACTION)["mean"] == pytest.approx((0.8 + 0.75 + 0.6) / 3)
    assert _by_trial(agg, ALL_PASS)["beta#0"]["scores"][0]["passed"] is False
    meta = next(r for r in agg["records"] if r["id"] == "alpha/one")["metadata"]
    h2 = meta["harness2"]
    assert (h2["mode"], h2["k"], h2["label"], h2["chosen_is_base"]) == ("agg", 2, "agg2", False)
    assert h2["run_id"] == "jb-oc-agg2/alpha/one/agg2"
    assert h2["usd"] == 0.5
    assert h2["harness_fingerprint"] == "fp-alpha-agg2"
    assert h2["harness_path"] == "alpha__one/harnesses/agg2"
    assert len(h2["tree_sha256"]) == 64
    assert h2["components"] == ["guardrails", "scripts", "skills", "system_prompt"]
    assert meta["agent"]["profile"] == "harness2:agg-k2"


def test_an_ungraded_run_is_an_error_not_a_zero(ns: Path) -> None:
    seq = import_namespace(ns).cells()["seq2"]
    beta = _by_trial(seq, FRACTION)["beta#0"]
    assert beta["outcome"] == "error"
    assert "judge returned nothing" in beta["reason"]
    assert "scores" not in beta
    s = _summary(seq, FRACTION)
    assert (s["n"], s["errors"], s["mean"]) == (1, 1, 0.5)


def test_unjudged_reference_runs_are_skipped_and_base_trials_follow_labels(ns: Path) -> None:
    base = import_namespace(ns).cells()["base"]
    fractions = _by_trial(base, FRACTION)
    assert fractions["alpha/one#0"]["scores"][0]["number"] == 0.4
    assert fractions["alpha/one#1"]["scores"][0]["number"] == 0.6
    assert fractions["beta#1"]["outcome"] == "skipped"
    s = _summary(base, FRACTION)
    assert (s["n"], s["skipped"]) == (5, 1)
    assert s["mean"] == pytest.approx((0.4 + 0.6 + 0.5 + 0.2 + 0.4) / 5)


def test_matrix_pairs_the_refined_harness_with_base_by_task(ns: Path) -> None:
    cells = import_namespace(ns).cells()
    out = build([("base", cells["base"]), ("agg2", cells["agg2"])], FRACTION)
    agg = out[1]
    assert agg.harness == "harness2:agg-k2"
    # Paired on trial 0: (0.8-0.4, 0.75-0.5, 0.6-0.2).
    assert agg.diff == pytest.approx((0.4 + 0.25 + 0.4) / 3)
    assert agg.diff_low is not None
    assert agg.diff_high is not None


def test_the_directory_is_only_read(ns: Path, tmp_path: Path) -> None:
    before = _snapshot(ns)
    imp = import_namespace(ns)
    imp.cells()
    assert _snapshot(ns) == before
    paths = write_cells(imp, tmp_path / "out")
    assert sorted(p.name for p in paths) == ["agg2.json", "base.json", "seq2.json"]
    assert json.loads(paths[0].read_text())["summaries"]
    with pytest.raises(ValueError, match="refusing to write"):
        write_cells(imp, ns / "out")
    assert _snapshot(ns) == before


def test_a_run_with_no_summary_is_empty_not_a_crash(tmp_path: Path) -> None:
    empty = tmp_path / "empty"
    empty.mkdir()
    imp = import_namespace(empty)
    assert imp.executions == []
    assert imp.cells() == {}
    assert any("no <task>/summary.json" in n for n in imp.notes)
    with pytest.raises(FileNotFoundError):
        import_namespace(tmp_path / "missing")


def test_missing_keys_give_skipped_records(tmp_path: Path) -> None:
    root = tmp_path / "ns"
    (root / "t1").mkdir(parents=True)
    # Only a task id; selections without a verdict; a judged row of the wrong types.
    (root / "t1" / "summary.json").write_text(
        json.dumps({"task_id": "t1", "selections": {"agg": {"k": 2}, "seq": "oops"}})
    )
    (root / "judged.json").write_text(json.dumps([{"task_id": "t1", "mode": "agg", "k": "x"}, 7]))
    cells = import_namespace(root).cells()
    # The judged row's k is unreadable, so it is a different (task, mode, k).
    assert set(cells) == {"agg", "agg2"}
    for cell in cells.values():
        assert _summary(cell, FRACTION)["n"] == 0
        assert all(r["outcome"] != "scored" for r in cell["results"])
    assert cells["agg2"]["results"][0]["outcome"] == "skipped"


def test_inconsistent_scores_are_errors(tmp_path: Path) -> None:
    root = tmp_path / "ns"
    (root / "t1").mkdir(parents=True)
    (root / "t1" / "summary.json").write_text(json.dumps({"task_id": "t1"}))
    rows = [
        {"task_id": "t1", "mode": "agg", "k": 1, "label": "agg1", "n_passed": 5, "n_criteria": 3}
    ]
    (root / "judged.json").write_text(json.dumps(rows))
    cell = import_namespace(root).cells()["agg1"]
    assert cell["results"][0]["outcome"] == "error"
    assert "inconsistent" in cell["results"][0]["reason"]


def test_a_symlinked_summary_outside_the_root_is_refused(ns: Path, tmp_path: Path) -> None:
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "summary.json").write_text(json.dumps({"task_id": "evil", "base": []}))
    (ns / "evil").mkdir()
    (ns / "evil" / "summary.json").symlink_to(outside / "summary.json")
    # A whole task directory that is a link to elsewhere.
    shutil.copytree(ns / "beta", outside / "linked")
    (ns / "linked").symlink_to(outside / "linked")
    imp = import_namespace(ns)
    assert "evil" not in {e.task_id for e in imp.executions}
    assert any("evil/summary.json" in n and "refused" in n for n in imp.notes)
    assert any(n.startswith("linked") and "refused" in n for n in imp.notes)


def test_a_judged_file_that_is_a_link_outside_is_not_read(ns: Path, tmp_path: Path) -> None:
    other = tmp_path / "other-judged.json"
    other.write_text("[]")
    (ns / "judged.json").unlink()
    (ns / "judged.json").symlink_to(other)
    imp = import_namespace(ns)
    assert any("judged.json" in n and "refused" in n for n in imp.notes)
    # Without the verdicts every chosen run is unjudged, so skipped.
    agg = imp.cells()["agg2"]
    assert {r["outcome"] for r in agg["results"]} == {"skipped"}


def test_a_harness_tree_with_an_escaping_link_has_no_fingerprint(ns: Path, tmp_path: Path) -> None:
    secret = tmp_path / "secret.txt"
    secret.write_text("outside")
    tree = ns / "alpha__one" / "harnesses" / "agg2"
    (tree / "LongTermMEMORY.md").symlink_to(secret)
    imp = import_namespace(ns)
    execution = next(e for e in imp.executions if e.mode == "agg" and e.task_id == "alpha/one")
    assert execution.tree is not None
    assert execution.tree.sha256 is None
    assert "memory" not in execution.tree.components
    assert any("LongTermMEMORY.md" in n and "not read" in n for n in imp.notes)


def test_a_label_that_is_a_path_is_never_opened(ns: Path) -> None:
    path = ns / "beta" / "summary.json"
    data = json.loads(path.read_text())
    data["selections"]["agg"]["chosen_label"] = "../../gamma"
    path.write_text(json.dumps(data))
    rows = json.loads((ns / "judged.json").read_text())
    for r in rows:
        if r["task_id"] == "beta" and r["mode"] == "agg":
            r["label"] = "../../gamma"
    (ns / "judged.json").write_text(json.dumps(rows))
    imp = import_namespace(ns)
    execution = next(e for e in imp.executions if e.mode == "agg" and e.task_id == "beta")
    assert execution.tree is None


def test_component_of_maps_the_eight_components() -> None:
    cases = {
        "systemprompt.md": "system_prompt",
        "guardrails.md": "guardrails",
        "LongTermMEMORY.md": "memory",
        "tool_descriptions/tool_guidance.md": "tool_guidance",
        "skills/a/SKILL.md": "skills",
        "scripts/x.py": "scripts",
        "sub_agents/a/AGENT.md": "subagents",
        "plugin/index.js": "plugins",
    }
    assert {p: component_of(p) for p in cases} == cases
    assert component_of("notes.txt") is None


def test_seq_mode_trees_are_read_from_the_sibling_namespace() -> None:
    imp = import_namespace(FIXTURE)
    seq = [e for e in imp.executions if e.kind == "chosen" and e.mode == "seq"]
    assert seq
    trees = {e.task_id: e.tree for e in seq}
    assert trees["alpha/one"] is not None
    assert trees["alpha/one"].path == "alpha__one/harnesses/c1"
    assert trees["alpha/one"].sha256

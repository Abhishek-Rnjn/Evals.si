"""Component ablation: planning reverts and attributing the effect (H7)."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import pytest
from test_matrix import results

from evalsi import ablation
from evalsi.cli import main

M = "task-success"


def write(root: Path, files: dict[str, str]) -> Path:
    for rel, text in files.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)
    return root


BASE = {
    "systemprompt.md": "be brief\n",
    "guardrails.md": "no network\n",
    "tool_descriptions/tool_guidance.md": "use grep\n",
    "scripts/helper.py": "print(1)\n",
}
ADOPTED = {
    **BASE,
    "systemprompt.md": "be brief and careful\n",  # c1
    "guardrails.md": "no network\n \n",  # c2: whitespace only
    "skills/fmt/SKILL.md": "---\nname: fmt\n---\n",  # c6 added
    "scripts/helper.py": "print(2)\n",  # c7
}
del ADOPTED["tool_descriptions/tool_guidance.md"]  # c4 removed


def test_components_follow_the_opencode_surface() -> None:
    cases = {
        "systemprompt.md": "c1",
        "guardrails.md": "c2",
        "LongTermMEMORY.md": "c3",
        "tool_descriptions/tool_guidance.md": "c4",
        "sub_agents/x/AGENT.md": "c5",
        "skills/x/SKILL.md": "c6",
        "skills/x/scripts/run.sh": "c6",
        "scripts/a.py": "c7",
        "plugin/p.ts": "c8",
        "scripts/README.md": "other",
        "notes.txt": "other",
    }
    assert {k: ablation.component_of(k) for k in cases} == cases


def test_the_planner_writes_one_revert_variant_per_edited_component(tmp_path: Path) -> None:
    base = write(tmp_path / "base", BASE)
    adopted = write(tmp_path / "adopted", ADOPTED)
    manifest = ablation.plan_dirs(base, adopted, tmp_path / "out")
    assert [c["component"] for c in manifest["components"]] == ["c1", "c2", "c4", "c6", "c7"]
    assert json.loads((tmp_path / "out" / "manifest.json").read_text()) == manifest
    c1 = ablation.read_dir(tmp_path / "out" / "c1")
    assert c1["systemprompt.md"] == b"be brief\n"  # reverted
    assert c1["scripts/helper.py"] == b"print(2)\n"  # adopted
    assert "tool_descriptions/tool_guidance.md" not in c1
    # Reverting a removal restores the file; reverting an addition drops it.
    assert "tool_descriptions/tool_guidance.md" in ablation.read_dir(tmp_path / "out" / "c4")
    assert "skills/fmt/SKILL.md" not in ablation.read_dir(tmp_path / "out" / "c6")
    c2 = next(c for c in manifest["components"] if c["component"] == "c2")
    assert c2["modified"] == ["guardrails.md"]


def test_the_planner_refuses_symlinks_traversal_and_dirty_output(tmp_path: Path) -> None:
    base = write(tmp_path / "base", BASE)
    adopted = write(tmp_path / "adopted", ADOPTED)
    os.symlink(tmp_path / "base" / "systemprompt.md", adopted / "link.md")
    with pytest.raises(ablation.AblationError, match="symlink"):
        ablation.plan_dirs(base, adopted, tmp_path / "out")
    (adopted / "link.md").unlink()
    os.symlink(tmp_path, adopted / "dirlink")
    with pytest.raises(ablation.AblationError, match="symlink"):
        ablation.plan_dirs(base, adopted, tmp_path / "out")
    (adopted / "dirlink").unlink()
    with pytest.raises(ablation.AblationError, match="escapes"):
        ablation.plan({"../x": b"1"}, {})
    with pytest.raises(ablation.AblationError, match="escapes"):
        ablation.plan({}, {"/etc/passwd": b"1"})
    out = tmp_path / "out"
    out.mkdir()
    (out / "stale").write_text("x")
    with pytest.raises(ablation.AblationError, match="not empty"):
        ablation.plan_dirs(base, adopted, out)
    with pytest.raises(ablation.AblationError, match="overlaps"):
        ablation.plan_dirs(base, adopted, adopted / "inner")


def run(scores: list[float]) -> dict[str, Any]:
    return results("h@1", scores, tokens=100, steps=3)


SIX = [0.0, 1, 0, 0, 1, 0, 0, 1]


def test_a_planted_noop_edit_has_exactly_zero_effect_and_is_flagged() -> None:
    base = run([0.0] * 8)
    full = run([1.0, 1, 1, 0, 1, 1, 0, 1])
    revert_real = run([0.0, 1, 0, 0, 1, 0, 0, 0])  # reverting c1 loses real gains
    revert_noop = run([1.0, 1, 1, 0, 1, 1, 0, 1])  # reverting c2 changes nothing
    rows = {
        r.component: r
        for r in ablation.attribute(base, full, {"c1": revert_real, "c2": revert_noop}, M)
    }
    assert rows["c2"].effect == 0.0
    assert rows["c2"].status == ablation.NO_EFFECT
    assert (rows["c2"].low, rows["c2"].high) == (0.0, 0.0)
    assert rows["c1"].effect is not None
    assert rows["c1"].effect > 0
    assert rows["c1"].status == ablation.EFFECT
    assert rows["total"].status == ablation.EFFECT


def test_a_harmful_edit_and_missing_data_are_reported_honestly() -> None:
    base = run([0.0] * 8)
    full = run([0.0, 0, 0, 0, 0, 0, 0, 0])
    better_without = run([1.0, 1, 1, 1, 1, 1, 1, 0])
    one = run([0.0])
    rows = {
        r.component: r for r in ablation.attribute(base, full, {"c1": better_without, "c3": one}, M)
    }
    assert rows["c1"].status == ablation.HARMFUL
    assert rows["c3"].status == ablation.INSUFFICIENT  # not "no effect"
    other = results("h@1", [0.0] * 8, tokens=1, steps=1, dataset="f" * 64)
    with pytest.raises(ablation.AblationError, match="same dataset"):
        ablation.attribute(base, full, {"c1": other}, M)


def test_the_cli_plans_and_attributes(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    base = write(tmp_path / "base", BASE)
    adopted = write(tmp_path / "adopted", ADOPTED)
    assert main(["ablate", "plan", str(base), str(adopted), "--out", str(tmp_path / "o")]) == 0
    assert '"c7"' in capsys.readouterr().out
    assert main(["ablate", "plan", str(base), str(adopted), "--out", str(tmp_path / "o")]) == 2
    capsys.readouterr()
    files = {}
    for name, scores in (
        ("base", [0.0] * 8),
        ("full", [1.0, 1, 1, 0, 1, 1, 0, 1]),
        ("c2", [1.0, 1, 1, 0, 1, 1, 0, 1]),
    ):
        files[name] = tmp_path / f"{name}.json"
        files[name].write_text(json.dumps(run(scores)))
    argv = ["ablate", "attribute", "--metric", M, "--base", str(files["base"]), "--full"]
    argv += [str(files["full"]), "--variant", f"c2={files['c2']}", "--format", "json"]
    assert main(argv) == 0
    rows = json.loads(capsys.readouterr().out)
    assert rows[0]["effect"] == 0.0
    assert rows[0]["status"] == "no measurable effect"
    assert main([*argv[:-2], "--variant", "bad"]) == 2

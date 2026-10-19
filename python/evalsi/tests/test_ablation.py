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


def test_a_lower_is_better_metric_flips_effect_and_harm() -> None:
    def lower(scores: list[float]) -> dict[str, Any]:
        data = run(scores)
        data["summaries"][0]["higher_is_better"] = False
        return data

    base = lower([1.0] * 8)
    # Lower is better: the full harness scores 0 where reverting c1 scores 1 (an
    # improvement), and the full harness scores 1 where reverting c2 scores 0 (harm).
    full_good = lower([0.0] * 8)
    rows = {
        r.component: r for r in ablation.attribute(base, full_good, {"c1": lower([1.0] * 8)}, M)
    }
    assert rows["c1"].status == ablation.EFFECT
    assert rows["c1"].effect == -1.0  # the raw difference stays in the metric's units
    full_bad = lower([1.0, 1, 1, 1, 1, 1, 1, 0])
    rows = {r.component: r for r in ablation.attribute(base, full_bad, {"c2": lower([0.0] * 8)}, M)}
    assert rows["c2"].status == ablation.HARMFUL


@pytest.mark.skipif(os.name == "nt", reason="execute bits")
def test_variants_keep_the_execute_bit_of_the_side_their_content_came_from(
    tmp_path: Path,
) -> None:
    base = write(tmp_path / "base", {**BASE, "scripts/run.sh": "echo base\n"})
    adopted = write(tmp_path / "adopted", {**ADOPTED, "scripts/run.sh": "echo adopted\n"})
    helper = adopted / "scripts" / "helper.py"
    for path in (base / "scripts" / "run.sh", adopted / "scripts" / "run.sh", helper):
        path.chmod(0o755)
    out = tmp_path / "plan"
    ablation.plan_dirs(base, adopted, out)
    # Reverting c7 restores the base's scripts: run.sh keeps its bit, helper.py (not
    # executable in the base) does not.
    assert os.access(out / "c7" / "scripts" / "run.sh", os.X_OK)
    assert not os.access(out / "c7" / "scripts" / "helper.py", os.X_OK)
    # Reverting c1 leaves the adopted scripts, so their bits are the adopted ones.
    assert os.access(out / "c1" / "scripts" / "helper.py", os.X_OK)
    assert not os.access(out / "c1" / "systemprompt.md", os.X_OK)


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


# --- per skill, sub-agent and script ----------------------------------------------

UNIT_BASE = {"systemprompt.md": "p\n", "scripts/a.py": "1\n", "plugin/p.ts": "1\n"}
UNIT_ADOPTED = {
    **UNIT_BASE,
    "skills/fmt/SKILL.md": "fmt\n",
    "skills/fmt/scripts/run.sh": "echo\n",
    "skills/lint/SKILL.md": "lint\n",
    "sub_agents/coder/AGENT.md": "coder\n",
    "scripts/a.py": "2\n",
    "plugin/p.ts": "2\n",
}


def test_units_split_skills_subagents_scripts_and_plugins() -> None:
    cases = {
        "systemprompt.md": "c1",
        "skills/fmt/SKILL.md": "c6:fmt",
        "skills/fmt/scripts/run.sh": "c6:fmt",
        "sub_agents/coder/AGENT.md": "c5:coder",
        "scripts/a.py": "c7:scripts/a.py",
        "plugin/p.ts": "c8:plugin/p.ts",
        "notes.txt": "other",
        "skills/README.md": "other",
    }
    for rel, unit in cases.items():
        assert ablation.unit_of(rel) == unit, rel
    with pytest.raises(ablation.AblationError, match="granularity"):
        ablation.group_of("systemprompt.md", "file")


def test_unit_planning_reverts_one_skill_and_keeps_the_rest() -> None:
    base = {k: v.encode() for k, v in UNIT_BASE.items()}
    adopted = {k: v.encode() for k, v in UNIT_ADOPTED.items()}
    edits = ablation.plan(base, adopted, "unit")
    assert [e.component for e in edits] == [
        "c5:coder",
        "c6:fmt",
        "c6:lint",
        "c7:scripts/a.py",
        "c8:plugin/p.ts",
    ]
    assert edits[1].files == ["skills/fmt/SKILL.md", "skills/fmt/scripts/run.sh"]
    assert edits[1].name == "skills: fmt"
    # Component granularity still lumps the two skills.
    assert [e.component for e in ablation.plan(base, adopted)] == ["c5", "c6", "c7", "c8"]
    variant = ablation.revert_variant(base, adopted, "c6:fmt", "unit")
    assert "skills/fmt/SKILL.md" not in variant  # reverted: the base has no such skill
    assert variant["skills/lint/SKILL.md"] == b"lint\n"  # another skill stays adopted
    assert variant["scripts/a.py"] == b"2\n"


def test_unit_variants_get_safe_distinct_directories(tmp_path: Path) -> None:
    base = write(tmp_path / "base", UNIT_BASE)
    adopted = write(tmp_path / "adopted", UNIT_ADOPTED)
    manifest = ablation.plan_dirs(base, adopted, tmp_path / "out", "unit")
    assert manifest["granularity"] == "unit"
    dirs = [c["variant_dir"] for c in manifest["components"]]
    assert len(set(dirs)) == len(dirs) == 5
    for d in dirs:
        assert "/" not in d
        assert (tmp_path / "out" / d).is_dir()
    by_unit = {c["component"]: c["variant_dir"] for c in manifest["components"]}
    assert not (tmp_path / "out" / by_unit["c6:fmt"] / "skills" / "fmt").exists()
    assert (tmp_path / "out" / by_unit["c6:fmt"] / "skills" / "lint" / "SKILL.md").exists()


def test_a_unit_is_attributed_like_a_component() -> None:
    base = run([0.0] * 8)
    full = run([1.0, 1, 1, 0, 1, 1, 0, 1])
    rows = {
        r.component: r
        for r in ablation.attribute(base, full, {"c6:fmt": run([0.0, 1, 0, 0, 1, 0, 0, 0])}, M)
    }
    assert rows["c6:fmt"].name == "skills: fmt"
    assert rows["c6:fmt"].status == ablation.EFFECT


# --- cross-task harm ---------------------------------------------------------------


def test_an_edit_that_helps_some_tasks_and_hurts_others_is_flagged_mixed() -> None:
    full = run([1.0, 1, 1, 1, 0, 0, 1, 1])
    # Reverting c1 loses t0..t2 (the edit helped them) and gains t4, t5 (the edit hurt them).
    revert_c1 = run([0.0, 0, 0, 1, 1, 1, 1, 1])
    revert_c2 = run([1.0, 1, 1, 1, 0, 0, 1, 1])  # a no-op edit
    harms = {
        h.component: h for h in ablation.harm_check(full, {"c1": revert_c1, "c2": revert_c2}, M)
    }
    assert [t.record for t in harms["c1"].helped] == ["t0", "t1", "t2"]
    assert [t.record for t in harms["c1"].hurt] == ["t4", "t5"]
    assert harms["c1"].unchanged == 3
    assert harms["c1"].mixed
    assert not harms["c2"].mixed
    assert harms["c2"].unchanged == 8
    # The average hides it: the edit's net effect on this metric is only +1/8.
    net = sum(t.effect for t in harms["c1"].helped + harms["c1"].hurt) / 8
    assert net == pytest.approx(1 / 8)


def test_harm_follows_the_metric_direction_and_min_effect() -> None:
    def lower(scores: list[float]) -> dict[str, Any]:
        data = run(scores)
        data["summaries"][0]["higher_is_better"] = False
        return data

    # Lower is better: full scores 0 where the revert scores 1 (helped t0), and 1 where
    # the revert scores 0 (hurt t1).
    harms = ablation.harm_check(lower([0.0, 1.0]), {"c1": lower([1.0, 0.0])}, M)
    assert [t.record for t in harms[0].helped] == ["t0"]
    assert [t.record for t in harms[0].hurt] == ["t1"]
    assert harms[0].hurt[0].effect == -1.0  # a positive number is always an improvement
    quiet = ablation.harm_check(run([0.55, 0.5]), {"c1": run([0.5, 0.5])}, M, min_effect=0.1)
    assert quiet[0].helped == []
    assert quiet[0].unchanged == 2
    with pytest.raises(ablation.AblationError, match="min_effect"):
        ablation.harm_check(run([1.0]), {"c1": run([1.0])}, M, min_effect=-1)
    other = results("h@1", [0.0] * 8, tokens=1, steps=1, dataset="f" * 64)
    with pytest.raises(ablation.AblationError, match="same dataset"):
        ablation.harm_check(run([0.0] * 8), {"c1": other}, M)


def test_trials_of_one_record_are_averaged() -> None:
    def trials(values: list[float]) -> dict[str, Any]:
        data = run([0.0])
        data["results"] = [
            {
                "record_id": "t0",
                "evaluator": "task-success",
                "outcome": "scored",
                "trial": k,
                "scores": [{"name": "task-success", "number": v}],
            }
            for k, v in enumerate(values)
        ]
        return data

    harms = ablation.harm_check(trials([1.0, 1.0, 0.0, 0.0]), {"c1": trials([0.0] * 4)}, M)
    assert harms[0].helped[0].effect == pytest.approx(0.5)


def test_the_cli_plans_by_unit_and_prints_the_harm_table(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    base = write(tmp_path / "base", UNIT_BASE)
    adopted = write(tmp_path / "adopted", UNIT_ADOPTED)
    argv = ["ablate", "plan", str(base), str(adopted), "--out", str(tmp_path / "o"), "--by", "unit"]
    assert main(argv) == 0
    assert '"c6:fmt"' in capsys.readouterr().out
    files = {}
    for name, scores in (
        ("base", [0.0] * 8),
        ("full", [1.0, 1, 1, 1, 0, 0, 1, 1]),
        ("c1", [0.0, 0, 0, 1, 1, 1, 1, 1]),
    ):
        files[name] = tmp_path / f"{name}.json"
        files[name].write_text(json.dumps(run(scores)))
    argv = ["ablate", "attribute", "--metric", M, "--base", str(files["base"]), "--full"]
    argv += [str(files["full"]), "--variant", f"c6:fmt={files['c1']}", "--harm"]
    assert main(argv) == 0
    out = capsys.readouterr().out
    assert "mixed: helps some, hurts others" in out
    assert "sign p" in out
    assert "t4 (-1.00)" in out
    assert main([*argv, "--format", "json"]) == 0
    data = json.loads(capsys.readouterr().out)
    assert data["harm"][0]["mixed"] is True
    assert data["attribution"][0]["component"] == "c6:fmt"
    # Without --harm the output is the plain attribution list, as before.
    plain = [a for a in argv if a != "--harm"]
    assert main([*plain, "--format", "json"]) == 0
    assert isinstance(json.loads(capsys.readouterr().out), list)


def test_the_sign_test_is_exact_and_says_little_with_few_tasks() -> None:
    def harm(helped: int, hurt: int) -> ablation.Harm:
        return ablation.Harm(
            "c1",
            "systemprompt",
            helped=[ablation.TaskEffect(f"h{i}", 1.0) for i in range(helped)],
            hurt=[ablation.TaskEffect(f"x{i}", -1.0) for i in range(hurt)],
        )

    assert harm(0, 0).sign_p is None
    assert harm(1, 1).sign_p == 1.0  # an even split
    assert harm(5, 0).sign_p == pytest.approx(0.0625)  # 2 * (1/32): even 5 for 0 is not p < 0.05
    assert harm(6, 0).sign_p == pytest.approx(0.03125)
    assert harm(8, 2).sign_p == pytest.approx(2 * (1 + 10 + 45) / 1024)
    assert harm(2, 8).sign_p == harm(8, 2).sign_p  # direction does not matter, only the split
    assert harm(3, 3).to_dict()["sign_p"] == 1.0


def test_a_harm_check_that_compared_nothing_says_insufficient_data(
    capsys: pytest.CaptureFixture[str],
) -> None:
    full = run([1.0, 1])
    elsewhere = run([0.0, 0])
    elsewhere["results"] = []  # a variant with no scored records at all
    (harm,) = ablation.harm_check(full, {"c1": elsewhere}, M)
    assert harm.compared == 0
    assert harm.to_dict()["compared"] == 0
    assert ablation.INSUFFICIENT in ablation.harm_table([harm])
    assert "ok" not in ablation.harm_table([harm]).splitlines()[1].split()
    # A variant that shares tasks is compared, and a clean one still reads "ok".
    (clean,) = ablation.harm_check(full, {"c1": run([1.0, 1])}, M)
    assert clean.compared == 2
    assert "ok" in ablation.harm_table([clean]).splitlines()[1].split()


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), -0.1])
def test_min_effect_must_be_finite_and_not_negative(bad: float) -> None:
    with pytest.raises(ablation.AblationError, match="min_effect"):
        ablation.harm_check(run([1.0]), {"c1": run([0.0])}, M, min_effect=bad)

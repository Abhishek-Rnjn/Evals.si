"""The importers for the benchmarks Harness² runs on: WorkBuddy-Bench through
``harbor://`` (``reward=score.json``), and the LAB and JobBench loaders.

Every task directory here is SYNTHETIC, invented from the layouts Harness²'s
adapters read; no benchmark data, instruction or rubric is vendored. The
rubric strings are made-up markers, so the tests can check they never reach
the instruction or the sandbox's files."""

from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest

from evalsi.datasets import load_records
from evalsi.runspec import load_spec
from evalsi_harness.benchmarks.harbor import (
    MARK,
    TaskFormatError,
    grade_harbor,
    load_harbor,
    score_from_payload,
)
from evalsi_harness.benchmarks.jobbench import load_jobbench
from evalsi_harness.benchmarks.lab import load_lab
from evalsi_harness.formats import FORMATS

SECRET = "SYNTHETIC-RUBRIC-MARKER-7f3a"


def env_of(row: dict) -> dict:
    env: dict = row["metadata"]["environment"]
    return env


# --- WorkBuddy-Bench through harbor:// ---


def wb_task(root: Path, name: str, field: str, group: str) -> Path:
    """A synthetic task in Harbor's shape, with a score.json-writing verifier."""
    d = root / name
    (d / "tests").mkdir(parents=True)
    (d / "task.toml").write_text(
        f'[metadata]\n{field} = "{group}"\n\n[environment]\ndocker_image = "alpine:3.22"\n'
    )
    (d / "instruction.md").write_text("Do the synthetic thing.\n")
    (d / "tests" / "test.sh").write_text(
        "#!/bin/sh\necho '{\"reward\": 1.0}' > /logs/verifier/score.json\n"
    )
    return d


def out(code: int, kind: str | None, text: str = "") -> str:
    tail = f"{MARK} {kind}\n{text}" if kind else ""
    return f"tests ran\n{MARK} exit {code}\n{tail}"


def test_a_workbuddy_shaped_task_loads_as_harbor_with_its_grouping(tmp_path: Path) -> None:
    wb_task(tmp_path, "code-a", "role", "backend-engineer")
    wb_task(tmp_path, "office-a", "category", "finance")
    rows = {r["id"]: r for r in load_harbor(str(tmp_path), reward="score.json")}
    # Grouping metadata (role for code tasks, category for office and web) is
    # carried as the task.toml [metadata] table.
    assert rows["code-a"]["metadata"]["harbor"]["metadata"]["role"] == "backend-engineer"
    assert rows["office-a"]["metadata"]["harbor"]["metadata"]["category"] == "finance"
    assert rows["code-a"]["metadata"]["harbor"]["reward"] == "score.json"
    script = env_of(rows["code-a"])["checker"]["command"][2]
    assert script.index("score.json") < script.index("reward.json") < script.index("reward.txt")


def test_the_default_checker_does_not_read_score_json(tmp_path: Path) -> None:
    wb_task(tmp_path, "code-a", "role", "backend-engineer")
    [row] = load_harbor(str(tmp_path))
    script = env_of(row)["checker"]["command"][2]
    assert "score.json" not in script
    assert "reward" not in row["metadata"]["harbor"]
    assert f"if [ -f /logs/verifier/reward.json ]; then printf '{MARK} reward.json" in script
    with pytest.raises(TaskFormatError, match="reward must be"):
        load_harbor(str(tmp_path), reward="rubric.json")


def test_score_json_precedence_and_range() -> None:
    assert score_from_payload({"reward": 0.5, "overall": 1.0}) == 0.5
    assert score_from_payload({"overall": 0.25, "test_pass_rate": 1}) == 0.25
    # An out-of-range value is skipped, not clamped.
    assert score_from_payload({"reward": 1.5, "overall": 0.4}) == 0.4
    assert score_from_payload({"reward": float("nan"), "test_pass_rate": 0.9}) == 0.9
    assert score_from_payload({"tests_passed": 3, "tests_total": 4}) == 0.75
    assert score_from_payload({"tests_passed": 9, "tests_total": 4}) == 1.0
    assert score_from_payload({"tests_passed": 1, "tests_total": 0}) is None
    assert score_from_payload({"note": "x"}) is None


def test_grading_a_score_json() -> None:
    ok = grade_harbor(out(0, "score.json", '{"overall": 1}'), "", 0, None)
    assert ok.passed
    assert ok.score == 1.0
    part = grade_harbor(out(1, "score.json", '{"tests_passed": 1, "tests_total": 4}'), "", 0, None)
    assert not part.passed
    assert part.score == 0.25
    assert not part.error
    # No usable score, or unreadable JSON, is a checker error: never a zero.
    for text in ('{"overall": 7}', "[1, 2]", "{torn"):
        bad = grade_harbor(out(0, "score.json", text), "", 0, None)
        assert not bad.passed
        assert bad.error
        assert bad.score != 0.0 or bad.error
    # Harbor's own reward files still grade as before.
    assert grade_harbor(out(0, "reward.txt", "1"), "", 0, None).passed


# --- LAB ---


def lab_tree(root: Path) -> Path:
    t = root / "tasks" / "antitrust-competition" / "task-one"
    (t / "documents").mkdir(parents=True)
    (t / "task.json").write_text(
        json.dumps(
            {
                "title": "Synthetic memo",
                "work_type": "analyze",
                "tags": ["synthetic"],
                "instructions": "Summarise the synthetic data room.",
                "deliverables": {"memo.docx": "A short memo", "notes.md": "notes.md"},
                "criteria": [{"id": "c1", "text": SECRET}],
            }
        )
    )
    (t / "documents" / "a.txt").write_text("source text\n")
    (t / "documents" / "b.bin").write_bytes(bytes(range(256)))
    other = root / "tasks" / "banking-finance" / "task-two"
    other.mkdir(parents=True)
    (other / "task.json").write_text(json.dumps({"instructions": "Second.", "criteria": []}))
    return root / "tasks"


def test_lab_tasks_are_discovered_and_carry_the_rubric_only_in_metadata(tmp_path: Path) -> None:
    rows = load_lab(str(lab_tree(tmp_path)))
    assert [r["id"] for r in rows] == [
        "antitrust-competition/task-one",
        "banking-finance/task-two",
    ]
    row = rows[0]
    assert "Summarise the synthetic data room." in row["input"]
    assert "`memo.docx` — A short memo" in row["input"]
    assert "`notes.md`\n" in row["input"] + "\n"
    assert SECRET not in row["input"]
    env = env_of(row)
    assert SECRET not in json.dumps(env)
    assert env["files"]["/workspace/documents/a.txt"] == "source text\n"
    assert base64.b64decode(env["files"]["/workspace/documents/b.bin.evalsi-b64"]) == bytes(
        range(256)
    )
    assert env["setup"][0].startswith("for f in /workspace/documents/b.bin;")
    assert "output" in env["setup"][-1]
    assert "checker" not in env  # judge-graded: task-success skips it
    assert env["sandbox"]["network"] == "deny"
    grading = row["metadata"]["grading"]
    assert grading["method"] == "llm-judge"
    assert grading["reproduced_offline"] is False
    assert grading["rubric"] == [{"id": "c1", "text": SECRET}]
    assert row["metadata"]["lab"]["practice_area"] == "antitrust-competition"


def test_lab_options_and_errors(tmp_path: Path) -> None:
    tasks = lab_tree(tmp_path)
    [one] = load_lab(str(tasks), tasks="banking-finance/*", image="img:1", rubric="false")
    assert env_of(one)["image"] == "img:1"
    assert one["metadata"]["grading"]["rubric"] is None
    # A single area or task directory works, and keeps its <area>/<task> id.
    [t] = load_lab(str(tasks / "antitrust-competition" / "task-one"))
    assert t["id"] == "antitrust-competition/task-one"
    [area] = load_lab(str(tasks / "banking-finance"))
    assert area["id"] == "banking-finance/task-two"
    with pytest.raises(TaskFormatError, match="no tasks"):
        load_lab(str(tasks), tasks="nope")
    with pytest.raises(TaskFormatError, match="no task.json"):
        load_lab(str(tmp_path / "tasks" / "banking-finance" / "task-two" / "x"), tasks="")
    with pytest.raises(TaskFormatError, match="network must be"):
        load_lab(str(tasks), network="host")
    (tasks / "banking-finance" / "task-two" / "task.json").write_text('{"criteria": []}')
    with pytest.raises(TaskFormatError, match="banking-finance/task-two: task.json has no"):
        load_lab(str(tasks))


def test_a_document_link_that_leaves_the_task_is_refused(tmp_path: Path) -> None:
    tasks = lab_tree(tmp_path)
    outside = tmp_path / "outside.txt"
    outside.write_text("not part of the task")
    (tasks / "antitrust-competition" / "task-one" / "documents" / "leak.txt").symlink_to(outside)
    with pytest.raises(TaskFormatError, match="links outside"):
        load_lab(str(tasks), tasks="antitrust-competition/*")


# --- JobBench ---


def jb_tree(root: Path, rubric: bool = True) -> Path:
    t = root / "main" / "accountant" / "task1"
    (t / "task_folder").mkdir(parents=True)
    (t / "task_folder" / "TASK_INSTRUCTIONS.txt").write_text("Prepare the synthetic ledger.\n")
    (t / "task_folder" / "ledger.csv").write_text("a,b\n1,2\n")
    for name in ("expected", "reference", "eval_result"):
        (t / name).mkdir()
        (t / name / "answer.txt").write_text(SECRET)
    if rubric:
        (t / "RUBRICS.json").write_text(
            json.dumps({"rubrics": [{"weight": 2, "criterion": [SECRET]}]})
        )
    return root / "main"


def test_jobbench_copies_only_the_task_folder(tmp_path: Path) -> None:
    [row] = load_jobbench(str(jb_tree(tmp_path)))
    assert row["id"] == "accountant/task1"
    assert row["input"] == "Prepare the synthetic ledger."
    env = env_of(row)
    assert sorted(env["files"]) == [
        "/workspace/task_folder/TASK_INSTRUCTIONS.txt",
        "/workspace/task_folder/ledger.csv",
    ]
    assert SECRET not in json.dumps(env) + row["input"]
    assert "checker" not in env
    grading = row["metadata"]["grading"]
    assert grading["method"] == "llm-judge"
    assert grading["rubric"]["rubrics"][0]["weight"] == 2
    assert row["metadata"]["jobbench"]["occupation"] == "accountant"


def test_jobbench_rubric_is_optional_and_can_be_left_out(tmp_path: Path) -> None:
    [bare] = load_jobbench(str(jb_tree(tmp_path / "a", rubric=False)))
    assert bare["metadata"]["grading"]["rubric"] is None
    [skipped] = load_jobbench(str(jb_tree(tmp_path / "b")), rubric="false")
    assert skipped["metadata"]["grading"]["rubric"] is None
    root = jb_tree(tmp_path / "c")
    (root / "accountant" / "task1" / "RUBRICS.json").write_text("{torn")
    with pytest.raises(TaskFormatError, match="accountant/task1: RUBRICS.json is not valid"):
        load_jobbench(str(root))
    # One task directory, with its parent as the occupation.
    [one] = load_jobbench(str(root / "accountant" / "task1"), rubric="false")
    assert one["id"] == "accountant/task1"


def test_the_new_schemes_load_through_the_entry_points(tmp_path: Path) -> None:
    [lab] = load_records(f"lab://{lab_tree(tmp_path / 'l')}?tasks=antitrust-competition/*")
    assert lab.id == "antitrust-competition/task-one"
    [jb] = load_records(f"jobbench://{jb_tree(tmp_path / 'j')}")
    assert jb.id == "accountant/task1"
    assert jb.metadata["grading"]["method"] == "llm-judge"


# --- the example run specs ---

EXAMPLES = Path(__file__).resolve().parents[3] / "examples" / "harness2"


@pytest.mark.parametrize("name", ["codex-sandboxed.yaml", "opencode-sandboxed.yaml"])
def test_the_example_specs_load_and_use_an_allowlist(name: str) -> None:
    run = load_spec(str(EXAMPLES / name))
    cli = run.spec.target.agent.cli
    assert list(cli.allow_hosts)
    assert cli.output_format in FORMATS
    sandbox = run.spec.environment.sandbox
    assert sandbox.network == "allowlist"
    assert list(sandbox.allow_hosts) == list(cli.allow_hosts)
    assert sandbox.min_isolation == "namespaced"


def test_the_codex_example_reads_codex_json() -> None:
    run = load_spec(str(EXAMPLES / "codex-sandboxed.yaml"))
    assert run.spec.target.agent.cli.output_format == "codex-json"

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main


@pytest.fixture
def application_project(tmp_path: Path) -> Path:
    project = tmp_path / "my application"
    assert main(["init", str(project), "--template", "application"]) == 0
    return project


def run_application(project: Path, *arguments: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(project / "evaluate.py"), *arguments],
        cwd=project,
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )


def read_result(project: Path) -> dict[str, Any]:
    result: dict[str, Any] = json.loads((project / "results.json").read_text())
    return result


def test_application_first_case_full_run_and_report(application_project: Path) -> None:
    first = run_application(application_project, "--limit", "1")
    assert first.returncode == 0, first.stderr
    result = read_result(application_project)
    assert len(result["records"]) == 1
    assert len(result["results"]) == 1
    assert result["records"][0]["output"] == {"text": "Paris"}
    assert "small sample" in first.stdout

    full = run_application(application_project)
    assert full.returncode == 0, full.stderr
    result = read_result(application_project)
    assert len(result["records"]) == 3
    assert all(item["outcome"] == "scored" for item in result["results"])
    assert all(item["scores"][0]["passed"] for item in result["results"])
    assert not any(summary["errors"] or summary["skipped"] for summary in result["summaries"])
    report = application_project / "report.html"
    assert main(["report", str(application_project / "results.json"), "-o", str(report)]) == 0
    assert "application-exact-match" in report.read_text()


def test_application_negative_control_is_business_failure(application_project: Path) -> None:
    completed = run_application(application_project, "--callable", "application:regressed")
    assert completed.returncode == 3, completed.stderr
    result = read_result(application_project)
    assert all(item["outcome"] == "scored" for item in result["results"])
    failed = [item for item in result["results"] if item["scores"][0]["passed"] is False]
    assert [item["record_id"] for item in failed] == ["japan"]
    assert result["records"][1]["output"] == {"text": "Kyoto"}
    assert result["records"][1]["reference"] == {"text": "Tokyo"}
    assert result["records"][1]["metadata"]["evalsi_application_reproduction"] == {
        "version": 1,
        "callable": "application:regressed",
        "cases": "cases.jsonl",
        "case": "japan",
    }
    report = application_project / "report-regressed.html"
    assert main(["report", str(application_project / "results.json"), "-o", str(report)]) == 0
    assert "Kyoto" in report.read_text()
    assert "Tokyo" in report.read_text()
    assert "application:regressed" in report.read_text()
    assert "evaluate.py" in report.read_text()
    assert "--case" in report.read_text()
    rerun = run_application(
        application_project, "--callable", "application:regressed", "--case", "japan"
    )
    assert rerun.returncode == 3
    assert len(read_result(application_project)["records"]) == 1


def test_init_does_not_import_user_application(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    module_name = "must_never_import_during_init"
    (tmp_path / f"{module_name}.py").write_text("raise RuntimeError('should never execute')\n")
    monkeypatch.syspath_prepend(str(tmp_path))
    project = tmp_path / "project"
    target = f"{module_name}:answer"
    assert main(["init", str(project), "--template", "application", "--callable", target]) == 0
    assert module_name not in sys.modules
    assert f"DEFAULT_CALLABLE = {target!r}" in (project / "evaluate.py").read_text()
    assert not (project / "run.yaml").exists()


@pytest.mark.parametrize("target", ["bad", "https://secret@example.com", "mod:fn()", "a:b:c"])
def test_init_rejects_invalid_callable_before_creating_directory(
    tmp_path: Path, target: str
) -> None:
    project = tmp_path / "project"
    assert main(["init", str(project), "--template", "application", "--callable", target]) == 1
    assert not project.exists()


def test_callable_option_requires_application_template(tmp_path: Path) -> None:
    project = tmp_path / "project"
    assert main(["init", str(project), "--callable", "application:answer"]) == 1
    assert not project.exists()


@pytest.mark.parametrize("existing", ["directory", "file", "symlink"])
def test_application_init_preserves_existing_paths(tmp_path: Path, existing: str) -> None:
    project = tmp_path / "project"
    if existing == "file":
        project.write_text("keep")
    elif existing == "directory":
        project.mkdir()
        (project / "keep.txt").write_text("keep")
    else:
        real = tmp_path / "real"
        real.mkdir()
        project.symlink_to(real, target_is_directory=True)
    assert main(["init", str(project), "--template", "application"]) == 1
    if existing == "file":
        assert project.read_text() == "keep"
    elif existing == "directory":
        assert [path.name for path in project.iterdir()] == ["keep.txt"]
    else:
        assert list(project.iterdir()) == []


@pytest.mark.parametrize(
    "body",
    [
        "return None",
        "return ''",
        "return '  '",
        "return {'output': 'Paris'}",
        "return 42",
        "raise RuntimeError('credential-do-not-expose')",
        "raise SystemExit('credential-do-not-expose')",
    ],
)
def test_invalid_outputs_and_exceptions_are_execution_failures(
    application_project: Path, body: str
) -> None:
    (application_project / "custom.py").write_text(
        f"def answer(question):\n    print('credential-do-not-expose')\n    {body}\n"
    )
    completed = run_application(application_project, "--callable", "custom:answer")
    assert completed.returncode == 1, completed.stderr
    result = read_result(application_project)
    assert len(result["results"]) == 3
    assert all(item["outcome"] == "error" for item in result["results"])
    assert all(not item.get("scores") for item in result["results"])
    assert not any("output" in record for record in result["records"])
    assert "credential-do-not-expose" not in completed.stdout + completed.stderr
    assert "credential-do-not-expose" not in (application_project / "results.json").read_text()


def test_subprocess_and_descriptor_output_is_suppressed(application_project: Path) -> None:
    # A child process and native code write to file descriptors 1 and 2, not sys.stdout.
    (application_project / "noisy.py").write_text(
        "import os, subprocess, sys\n"
        "subprocess.run([sys.executable, '-c', 'print(\"import-canary\")'])\n"
        "def answer(question):\n"
        "    subprocess.run([sys.executable, '-c', 'print(\"subprocess-canary\")'])\n"
        "    os.write(1, b'stdout-fd-canary')\n"
        "    os.write(2, b'stderr-fd-canary')\n"
        "    return 'Paris'\n"
    )
    completed = run_application(application_project, "--callable", "noisy:answer", "--limit", "1")
    assert completed.returncode == 0, completed.stderr
    assert "canary" not in completed.stdout + completed.stderr
    assert "canary" not in (application_project / "results.json").read_text()
    assert read_result(application_project)["results"][0]["outcome"] == "scored"


def test_errors_take_precedence_over_business_failures(application_project: Path) -> None:
    (application_project / "custom.py").write_text(
        "def answer(question):\n"
        "    if 'Japan' in question:\n"
        "        raise RuntimeError('broken')\n"
        "    return 'wrong'\n"
    )
    completed = run_application(application_project, "--callable", "custom:answer")
    assert completed.returncode == 1
    result = read_result(application_project)
    assert [item["outcome"] for item in result["results"]] == ["scored", "error", "scored"]
    assert result["results"][0]["scores"][0]["passed"] is False


def test_first_case_invokes_application_only_once(application_project: Path) -> None:
    (application_project / "custom.py").write_text(
        "from pathlib import Path\n"
        "def answer(question):\n"
        "    with Path('calls.txt').open('a') as calls:\n"
        "        calls.write(question + '\\n')\n"
        "    return 'Paris'\n"
    )
    completed = run_application(application_project, "--callable", "custom:answer", "--limit", "1")
    assert completed.returncode == 0, completed.stderr
    assert (application_project / "calls.txt").read_text().splitlines() == ["Capital of France?"]


def test_sync_callable_returning_coroutine_is_execution_error(application_project: Path) -> None:
    (application_project / "custom.py").write_text(
        "async def async_answer():\n"
        "    return 'Paris'\n"
        "def answer(question):\n"
        "    return async_answer()\n"
    )
    completed = run_application(application_project, "--callable", "custom:answer")
    assert completed.returncode == 1
    assert all(item["outcome"] == "error" for item in read_result(application_project)["results"])
    assert "was never awaited" not in completed.stderr


@pytest.mark.parametrize(
    "source",
    [
        "answer = 'not a callable'\n",
        "async def answer(question):\n    return 'Paris'\n",
        "raise RuntimeError('credential-do-not-expose')\n",
        "raise SystemExit('credential-do-not-expose')\n",
    ],
)
def test_bad_import_or_async_callable_fails_actionably(
    application_project: Path, source: str
) -> None:
    (application_project / "custom.py").write_text(source)
    completed = run_application(application_project, "--callable", "custom:answer")
    assert completed.returncode == 1
    assert "Execution/configuration error" in completed.stderr
    assert "credential-do-not-expose" not in completed.stdout + completed.stderr
    assert not (application_project / "results.json").exists()


@pytest.mark.parametrize(
    "cases",
    [
        "",
        "not JSON\n",
        "[]\n",
        '{"id":"a","input":"question"}\n',
        '{"id":"a","input":42,"reference":"answer"}\n',
        '{"id":"a","input":"question","reference":""}\n',
        '{"id":"a","input":"q","reference":"a"}\n{"id":"a","input":"q","reference":"a"}\n',
    ],
)
def test_invalid_cases_fail_before_loading_application(
    application_project: Path, cases: str
) -> None:
    (application_project / "cases.jsonl").write_text(cases)
    completed = run_application(application_project, "--callable", "does_not_exist:answer")
    assert completed.returncode == 1
    assert "cases" in completed.stderr
    assert "cannot load callable" not in completed.stderr
    assert not (application_project / "results.json").exists()


@pytest.mark.parametrize("arguments", [("--limit", "0"), ("--limit", "-1")])
def test_invalid_limit_is_usage_error(
    application_project: Path, arguments: tuple[str, ...]
) -> None:
    assert run_application(application_project, *arguments).returncode == 2


def test_missing_cases_unknown_id_and_unwritable_output_are_errors(
    application_project: Path,
) -> None:
    for arguments in (
        ("--cases", "missing.jsonl"),
        ("--case", "unknown"),
        ("--output", "missing-directory/results.json"),
        ("--callable", "missing_module:answer"),
    ):
        completed = run_application(application_project, *arguments)
        assert completed.returncode == 1, completed.stderr
        assert "Execution/configuration error" in completed.stderr
    assert not (application_project / "results.json").exists()


def test_custom_cases_and_persisted_callable(application_project: Path, tmp_path: Path) -> None:
    project = tmp_path / "configured"
    assert (
        main(
            [
                "init",
                str(project),
                "--template",
                "application",
                "--callable",
                "application:regressed",
            ]
        )
        == 0
    )
    assert run_application(project).returncode == 3
    cases = application_project / "custom.jsonl"
    cases.write_text('{"id":"new","input":"Not a known capital","reference":"Unknown"}\n')
    completed = run_application(application_project, "--cases", str(cases))
    assert completed.returncode == 0, completed.stderr
    assert read_result(application_project)["records"][0]["id"] == "new"


def test_reproduction_metadata_preserves_project_relative_cases_and_literal_id(
    application_project: Path,
) -> None:
    cases = application_project / "data with spaces" / "custom cases.jsonl"
    cases.parent.mkdir()
    case_id = "case with spaces; $(not-a-command)"
    cases.write_text(
        json.dumps({"id": case_id, "input": "Unknown question", "reference": "Unknown"}) + "\n"
    )
    completed = run_application(application_project, "--cases", str(cases))
    assert completed.returncode == 0, completed.stderr
    assert read_result(application_project)["records"][0]["metadata"] == {
        "evalsi_application_reproduction": {
            "version": 1,
            "callable": "application:answer",
            "cases": "data with spaces/custom cases.jsonl",
            "case": case_id,
        }
    }


def test_external_cases_do_not_claim_project_local_reproduction(
    application_project: Path, tmp_path: Path
) -> None:
    cases = tmp_path / "external.jsonl"
    cases.write_text('{"id":"external","input":"Unknown question","reference":"Unknown"}\n')
    completed = run_application(application_project, "--cases", str(cases))
    assert completed.returncode == 0, completed.stderr
    assert not read_result(application_project)["records"][0].get("metadata")


@pytest.mark.parametrize(
    "case_id", ["line\nbreak", "del\x7f", "bidi\u202e", "a" * 257, "-option", "東京"]
)
def test_unbounded_or_nonprintable_ids_omit_reproduction_hint(
    application_project: Path, case_id: str
) -> None:
    cases = application_project / "cases.jsonl"
    cases.write_text(
        json.dumps({"id": case_id, "input": "Unknown question", "reference": "Unknown"}) + "\n"
    )
    completed = run_application(application_project)
    assert completed.returncode == 0, completed.stderr
    assert not read_result(application_project)["records"][0].get("metadata")

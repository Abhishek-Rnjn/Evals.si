from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "release_version.py"


def metadata(version: str) -> dict[str, Any]:
    result = subprocess.run(
        [sys.executable, str(SCRIPT), version, "--owner", "Abhishek-Rnjn"],
        text=True,
        capture_output=True,
        check=True,
    )
    result_json: dict[str, Any] = json.loads(result.stdout)
    return result_json


@pytest.mark.parametrize("version", ["0.4.0-rc.1", "0.4.0-rc1", "0.4.0-rc.12"])
def test_candidates_cannot_update_stable_registry_or_github_latest(version: str) -> None:
    release = metadata(version)
    assert release["prerelease"] == "true"
    assert release["make_latest"] == "false"
    for image, output in (("evalsi", "image_tags"), ("evalsi-collector", "collector_tags")):
        assert release[output] == f"ghcr.io/abhishek-rnjn/{image}:{version}"
    assert release["python_version"] == ("0.4.0rc12" if version.endswith("12") else "0.4.0rc1")


def test_stable_releases_retain_their_version_and_latest_tags() -> None:
    release = metadata("0.4.0")
    assert release["python_version"] == "0.4.0"
    assert release["prerelease"] == "false"
    assert release["make_latest"] == "true"
    assert release["image_tags"].split(",") == [
        "ghcr.io/abhishek-rnjn/evalsi:0.4.0",
        "ghcr.io/abhishek-rnjn/evalsi:latest",
    ]
    assert release["collector_tags"].endswith("/evalsi-collector:latest")


@pytest.mark.parametrize(
    "version",
    [
        "v0.4.0",
        "0.4",
        "0.4.0.rc1",
        "0.4.0rc1",
        "0.4.0-preview",
        "0.4.0+build",
        "0.4.0-rc.01",
        "00.4.0",
        "0.4.0\nowner=other",
        "$(touch injected)",
    ],
)
def test_ambiguous_or_unsafe_release_inputs_are_rejected(version: str, tmp_path: Path) -> None:
    output = tmp_path / "output"
    result = subprocess.run(
        [sys.executable, str(SCRIPT), version, "--github-output", str(output)],
        capture_output=True,
        check=False,
    )
    assert result.returncode == 1
    assert not output.exists()


def copy_projects(tmp_path: Path) -> Path:
    directory = tmp_path / "python"
    for package in ("evalsi", "evalsi-harness"):
        relative = Path(package) / "src" / package.replace("-", "_") / "_version.py"
        (directory / relative).parent.mkdir(parents=True)
        shutil.copyfile(ROOT / "python" / relative, directory / relative)
    shutil.copyfile(
        ROOT / "python/evalsi-harness/pyproject.toml", directory / "evalsi-harness/pyproject.toml"
    )
    return directory


def test_stamping_keeps_import_versions_and_the_harness_pin_canonical(tmp_path: Path) -> None:
    directory = copy_projects(tmp_path)
    subprocess.run(
        [sys.executable, str(SCRIPT), "0.4.0-rc.1", "--stamp", str(directory)], check=True
    )
    for package in ("evalsi", "evalsi-harness"):
        path = directory / package / "src" / package.replace("-", "_") / "_version.py"
        assert path.read_text() == '__version__ = "0.4.0rc1"\n'
    project = (directory / "evalsi-harness/pyproject.toml").read_text()
    assert '"evalsi[server]==0.4.0rc1"' in project
    assert "0.4.0-rc.1" not in project


def test_a_missing_dependency_pin_cannot_partially_stamp_the_packages(tmp_path: Path) -> None:
    directory = copy_projects(tmp_path)
    (directory / "evalsi-harness/pyproject.toml").write_text("[project]\n")
    before = {p: p.read_bytes() for p in directory.rglob("*") if p.is_file()}
    result = subprocess.run(
        [sys.executable, str(SCRIPT), "0.4.0-rc.1", "--stamp", str(directory)],
        capture_output=True,
        check=False,
    )
    assert result.returncode == 1
    assert {p: p.read_bytes() for p in before} == before


def test_workflow_uses_classified_tags_and_waits_for_installed_artifact_checks() -> None:
    jobs = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())["jobs"]
    for name, output in (("image", "image_tags"), ("collector", "collector_tags")):
        build = next(step for step in jobs[name]["steps"] if step.get("id") == "build")
        assert build["with"]["tags"] == "${{ needs.version.outputs." + output + " }}"
    for name in ("image", "collector", "charts"):
        assert "onboarding" in jobs[name]["needs"]
        assert "needs.onboarding.result == 'success'" in jobs[name]["if"]
    release_step = next(
        step for step in jobs["release"]["steps"] if "action-gh-release" in step.get("uses", "")
    )
    assert release_step["with"]["make_latest"] == "${{ needs.version.outputs.make_latest }}"
    assert release_step["with"]["prerelease"] == "${{ needs.version.outputs.prerelease == 'true' }}"
    assert {row["arch"] for row in jobs["onboarding"]["strategy"]["matrix"]["include"]} == {
        "amd64",
        "arm64",
        "portable",
    }
    ci = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())["jobs"]
    assert set(ci["release-check"]["needs"]) == {"release-artifacts", "portable-onboarding"}
    assert "matrix" not in ci["release-check"].get("strategy", {})
    guard = ci["release-check"]["steps"][0]
    assert guard["env"]["LINUX_RESULT"] == "${{ needs.release-artifacts.result }}"
    assert guard["env"]["PORTABLE_RESULT"] == "${{ needs.portable-onboarding.result }}"
    assert guard["run"] == 'test "$LINUX_RESULT" = success && test "$PORTABLE_RESULT" = success'


def test_missing_candidate_wheels_cannot_fall_back_to_published_packages(tmp_path: Path) -> None:
    report = tmp_path / "report.json"
    result = subprocess.run(
        [
            sys.executable,
            str(ROOT / "scripts/smoke_release.py"),
            "0.4.0-rc.1",
            "--wheels",
            str(tmp_path),
            "--report",
            str(report),
        ],
        capture_output=True,
        check=False,
    )
    assert result.returncode != 0
    assert b"missing expected 0.4.0rc1 wheels" in result.stderr
    assert not report.exists()

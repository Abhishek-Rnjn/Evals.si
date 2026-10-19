from __future__ import annotations

import json
import subprocess
from typing import Any

import pytest

from evalsi import cli_doctor
from evalsi._version import __version__
from evalsi.cli import main


def test_sdk_install_does_not_require_optional_execution(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr(cli_doctor, "installed_version", lambda name: None)
    monkeypatch.setattr(cli_doctor, "evalsid_path", lambda: None)
    monkeypatch.setattr(cli_doctor.importlib.util, "find_spec", lambda name: None)
    monkeypatch.setenv("SOME_API_KEY", "must-not-appear")
    assert main(["doctor", "--format", "json"]) == 0
    report = capsys.readouterr().out
    assert "must-not-appear" not in report
    assert json.loads(report)["ready"] is True
    assert main(["doctor", "--require", "agent", "--format", "json"]) == 1
    checks = json.loads(capsys.readouterr().out)["checks"]
    assert {c["name"] for c in checks if c["status"] == "error"} == {
        "harness",
        "worker",
        "server",
        "sandbox",
    }


@pytest.mark.parametrize("available", [True, False])
def test_agent_readiness_uses_actual_probe(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], available: bool
) -> None:
    calls: list[list[str]] = []

    def run(cmd: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
        calls.append(cmd)
        assert kwargs["timeout"] in (10, 15)
        output = (
            f"evalsid {__version__} (api evalsi.v1alpha1)\n"
            if cmd[-1] == "version"
            else json.dumps([{"driver": "bwrap", "available": available}])
        )
        return subprocess.CompletedProcess(cmd, 0, output, "")

    monkeypatch.setattr(cli_doctor, "installed_version", lambda name: __version__)
    monkeypatch.setattr(cli_doctor, "evalsid_path", lambda: "/release/evalsid")
    monkeypatch.setattr(cli_doctor.importlib.util, "find_spec", lambda name: object())
    monkeypatch.setattr(cli_doctor.subprocess, "run", run)
    assert main(["doctor", "--require", "agent", "--format", "json"]) == (0 if available else 1)
    assert json.loads(capsys.readouterr().out)["ready"] is available
    assert calls == [["/release/evalsid", "version"], ["/release/evalsid", "sandbox", "probe"]]


@pytest.mark.parametrize("failure", ["timeout", "invalid", "wrong-version"])
def test_failed_or_mismatched_executable_is_not_reported_ready(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str], failure: str
) -> None:
    def run(cmd: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
        if failure == "timeout":
            raise subprocess.TimeoutExpired(cmd, 10, output="private output")
        output = "bad data" if failure == "invalid" else "evalsid 99.0.0 (api evalsi.v1alpha1)"
        if cmd[-1] == "probe":
            output = "[]"
        return subprocess.CompletedProcess(cmd, 0, output, "private error")

    monkeypatch.setattr(cli_doctor, "evalsid_path", lambda: "/bad/evalsid")
    monkeypatch.setattr(cli_doctor.subprocess, "run", run)
    assert main(["doctor", "--require", "server", "--format", "json"]) == 1
    output = capsys.readouterr().out
    assert "private" not in output
    assert json.loads(output)["ready"] is False


def test_probe_failure_does_not_require_a_sandbox_for_scoring_server(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def run(cmd: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
        if cmd[-1] == "probe":
            raise subprocess.TimeoutExpired(cmd, 15)
        return subprocess.CompletedProcess(
            cmd, 0, f"evalsid {__version__} (api evalsi.v1alpha1)", ""
        )

    monkeypatch.setattr(cli_doctor, "evalsid_path", lambda: "/release/evalsid")
    monkeypatch.setattr(cli_doctor.importlib.util, "find_spec", lambda name: object())
    monkeypatch.setattr(cli_doctor.subprocess, "run", run)
    assert not any(c.status == "error" for c in cli_doctor.diagnose("server"))
    assert any(c.name == "sandbox" and c.status == "error" for c in cli_doctor.diagnose("agent"))

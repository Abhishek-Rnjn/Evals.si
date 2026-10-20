from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi.cli import main
from evalsi.client import Client
from evalsi.release_decisions import (
    decision_hash,
    exit_code,
    policy_hash,
    read_report,
    write_report,
)

POLICY: dict[str, Any] = {
    "name": "production",
    "version": "v1",
    "dataset": {"project": "support", "name": "refunds", "version": "v1", "sha256": "a" * 64},
    "min_records": 2,
    "rules": [
        {
            "metric": "exact-match",
            "direction": "METRIC_DIRECTION_HIGHER",
            "max_regression": 0,
            "min_case": 1,
        }
    ],
}


def decision(outcome: str = "RELEASE_OUTCOME_PASS") -> dict[str, Any]:
    value: dict[str, Any] = {
        "id": "release-v1",
        "project": "support",
        "policy": POLICY,
        "policySha256": policy_hash(POLICY),
        "outcome": outcome,
        "baseline": {"reference": {"runId": "base", "sha256": "b" * 64, "commit": "c" * 40}},
        "candidate": {"reference": {"runId": "next", "sha256": "d" * 64, "commit": "e" * 40}},
    }
    value["sha256"] = decision_hash(value)
    return value


@pytest.mark.parametrize(
    ("outcome", "code"),
    [
        ("RELEASE_OUTCOME_PASS", 0),
        ("RELEASE_OUTCOME_BLOCK", 3),
        ("RELEASE_OUTCOME_INSUFFICIENT", 1),
    ],
)
def test_private_portable_report_round_trip_and_cli_exit_codes(
    tmp_path: Path, outcome: str, code: int
) -> None:
    path = tmp_path / "report.json"
    value = decision(outcome)
    write_report(path, value)
    loaded = read_report(path, value["sha256"])
    assert loaded == value
    assert exit_code(loaded) == code
    assert main(["release", "verify", str(path), "--sha256", value["sha256"]]) == code
    original = path.read_bytes()
    with pytest.raises(FileExistsError):
        write_report(path, value)
    assert path.read_bytes() == original
    assert not list(tmp_path.glob(".evalsi-release-*"))
    if os.name == "posix":
        assert path.stat().st_mode & 0o777 == 0o600


def test_report_tampering_missing_fields_and_wrong_external_pin_are_rejected(
    tmp_path: Path,
) -> None:
    path = tmp_path / "report.json"
    value = decision()
    write_report(path, value)
    assert main(["release", "verify", str(path), "--sha256", "0" * 64]) == 1
    raw = json.loads(path.read_text())
    raw["decision"]["policy"]["rules"][0]["min_case"] = 0
    path.write_text(json.dumps(raw))
    assert main(["release", "verify", str(path), "--sha256", value["sha256"]]) == 1
    del raw["decision"]["policy"]
    path.write_text(json.dumps(raw))
    assert main(["release", "verify", str(path), "--sha256", value["sha256"]]) == 1
    assert exit_code({}) == 1


def test_policy_hash_is_format_independent_but_binds_thresholds_and_order() -> None:
    original = policy_hash(POLICY)
    wire = json.loads(json.dumps(POLICY))
    wire["minRecords"] = wire.pop("min_records")
    assert policy_hash(wire) == original
    wire["rules"][0]["max_regression"] = 0.1
    assert policy_hash(wire) != original
    with pytest.raises(ValueError, match="invalid release policy"):
        policy_hash({**POLICY, "unrecognized": True})


def test_cli_policy_pin_fails_before_any_network_or_target_call(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi import cli_release

    def no_request(_: httpx.Request) -> httpx.Response:
        pytest.fail("a changed policy must be rejected before network/target calls")

    monkeypatch.setattr(
        cli_release,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(no_request)),
    )
    path = tmp_path / "policy.json"
    path.write_text(json.dumps(POLICY))
    assert (
        main(
            [
                "release",
                "gate",
                "-f",
                str(path),
                "--policy-sha256",
                "0" * 64,
                "--server",
                "http://unused",
                "--id",
                "release-v1",
                "--baseline",
                "base",
                "--baseline-sha256",
                "a" * 64,
                "--baseline-commit",
                "b" * 40,
                "--spec",
                str(tmp_path / "never-opened.yaml"),
                "--commit",
                "c" * 40,
            ]
        )
        == 1
    )


def test_sdk_and_cli_decision_preserve_explicit_pins_and_block_status(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi import cli_release

    sent = []
    expected = decision("RELEASE_OUTCOME_BLOCK")

    def send(request: httpx.Request) -> httpx.Response:
        sent.append(json.loads(request.content))
        return httpx.Response(200, json={"decision": expected})

    monkeypatch.setattr(
        cli_release,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    path = tmp_path / "policy.json"
    path.write_text(json.dumps(POLICY))
    output = tmp_path / "decision.json"
    assert (
        main(
            [
                "release",
                "decide",
                "-f",
                str(path),
                "--policy-sha256",
                policy_hash(POLICY),
                "--server",
                "http://unused",
                "--id",
                "release-v1",
                "--project",
                "support",
                "--baseline",
                "base",
                "--baseline-sha256",
                "b" * 64,
                "--baseline-commit",
                "c" * 40,
                "--candidate",
                "next",
                "--candidate-sha256",
                "d" * 64,
                "--candidate-commit",
                "e" * 40,
                "-o",
                str(output),
            ]
        )
        == 3
    )
    assert sent[0]["baseline"]["sha256"] == "b" * 64
    assert sent[0]["candidate"]["commit"] == "e" * 40
    assert read_report(output, expected["sha256"]) == expected


@pytest.mark.parametrize(
    "raw",
    [
        "[]",
        '{"schema":"wrong"}',
        '{"schema":"evalsi.release-report/v1","decision":null}',
        '{"schema":"evalsi.release-report/v1","schema":"other"}',
        '{"schema":"evalsi.release-report/v1","decision":{"x":NaN}}',
    ],
)
def test_invalid_portable_shapes_fail_without_tracebacks(tmp_path: Path, raw: str) -> None:
    path = tmp_path / "invalid.json"
    path.write_text(raw)
    assert main(["release", "verify", str(path), "--sha256", "0" * 64]) == 1

"""The client side of a server-side harness sweep: the spec parses, an embedded run
refuses it, the client reads every run the server created, and the report has one
row per configuration."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main, sweep_table
from evalsi.client import Client
from evalsi.runspec import load_spec, spec_to_dict

SPEC = """\
name: harness-sweep
spec:
  target: {connector: openai-compatible, model: m, base_url: "http://localhost:1/v1"}
  dataset: {inline: {records: [{id: a, input: {text: hi}}]}}
  evaluators: [{ref: task-success}]
  harness: {builtin: {max_steps: 10, tools: {builtin: [grep]}}}
  environment: {image: "python:3.12-slim"}
  sweep:
    variants:
      - {name: base, harness: {}}
      - {name: minimal, harness: {profile: minimal, tools: {}}}
"""


def write(tmp_path: Path) -> Path:
    path = tmp_path / "run.yaml"
    path.write_text(SPEC)
    return path


def test_the_sweep_survives_parsing_and_json_round_trip(tmp_path: Path) -> None:
    run_file = load_spec(write(tmp_path))
    data = spec_to_dict(run_file.spec)
    assert [v["name"] for v in data["sweep"]["variants"]] == ["base", "minimal"]
    assert data["sweep"]["variants"][1]["harness"]["profile"] == "minimal"


def test_an_embedded_run_refuses_a_sweep(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    assert main(["run", "-f", str(write(tmp_path))]) == 2
    assert "spec.sweep expands on the server" in capsys.readouterr().err


def test_create_sweep_returns_every_run(monkeypatch: pytest.MonkeyPatch) -> None:
    client = Client("http://localhost:1")
    seen: list[dict[str, Any]] = []

    def call(service: str, method: str, body: dict[str, Any], **_: Any) -> dict[str, Any]:
        seen.append(body)
        return {"run": {"id": "r1"}, "sweep_runs": [{"id": "r1"}, {"id": "r2"}]}

    monkeypatch.setattr(client, "call", call)
    assert [r["id"] for r in client.create_sweep({"x": 1}, name="n")] == ["r1", "r2"]
    # A server without sweeps answers with the one run.
    monkeypatch.setattr(client, "call", lambda *a, **k: {"run": {"id": "only"}})
    assert [r["id"] for r in client.create_sweep({})] == ["only"]


def test_the_report_has_a_row_per_configuration_and_metric() -> None:
    def run(variant: str, mean: float, low: float, high: float) -> dict[str, Any]:
        return {
            "name": f"s/{variant}",
            "status": "RUN_STATUS_SUCCEEDED",
            "labels": {"evalsi.variant": variant},
            "summaries": [
                {
                    "metric": "task-success",
                    "kind": "METRIC_KIND_PROPORTION",
                    "n": "10",
                    "mean": mean,
                    "ci": {"low": low, "high": high, "level": 0.95, "method": "wilson"},
                }
            ],
        }

    table = sweep_table([run("base", 0.5, 0.24, 0.76), run("minimal", 0.7, 0.4, 0.89)])
    lines = table.splitlines()
    assert lines[0].split()[:2] == ["variant", "metric"]
    assert lines[1].split() == [
        "base",
        "task-success",
        "10",
        "0.500",
        "[0.240,",
        "0.760]",
        "succeeded",
    ]
    assert lines[2].startswith("minimal")
    failed = sweep_table([{"name": "s/x", "status": "RUN_STATUS_FAILED", "labels": {}}])
    assert "failed" in failed

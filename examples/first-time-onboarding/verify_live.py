"""Opt-in paid Haiku onboarding: business tools, streaming, replay and optional SQL."""

from __future__ import annotations

import argparse
import copy
import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parent
POLICY = (
    "First call lookup_order for the requested numeric id. Refund exactly once only if "
    "the order is paid and not already refunded. Do not call any other tools. "
    "After a successful refund answer 'Refunded order <id>' with the numeric id. "
)
SQL_POLICY = (
    "First call lookup_order for the requested numeric id. If refunded is 0, call refund "
    "exactly once with that id. Do not call any other tools. After success answer "
    "'Refunded order <id>' with the numeric id. "
)


def configuration() -> tuple[str, str, str]:
    model = os.environ.get("ANTHROPIC_DEFAULT_HAIKU_MODEL")
    if not model:
        raise ValueError(
            "Set ANTHROPIC_DEFAULT_HAIKU_MODEL explicitly; no model is billed by default"
        )
    key_env = next(
        (name for name in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN") if os.environ.get(name)),
        None,
    )
    if key_env is None:
        raise ValueError("Set ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the environment")
    base = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com").rstrip("/")
    return model, key_env, base


def build_spec(
    model: str, key_env: str, base: str, *, stream: bool, database: bool = False
) -> dict[str, Any]:
    source = ROOT / "database.yaml" if database else ROOT.parent / "tinylm" / "support.yaml"
    document: dict[str, Any] = yaml.safe_load(source.read_text())
    spec = document["spec"]
    spec["target"] = {
        "connector": "anthropic",
        "model": model,
        "api_key_env": key_env,
        "base_url": base,
        "stream": stream,
        "max_tokens": 512,
    }
    dataset = ROOT / "tasks.jsonl" if database else ROOT.parent / "tinylm" / "support.jsonl"
    records = [json.loads(line) for line in dataset.read_text().splitlines() if line.strip()]
    for record in records:
        record["input"] = {"text": (SQL_POLICY if database else POLICY) + record["input"]}
        record["reference"] = {"json": record["reference"]}
    spec["dataset"] = {"inline": {"records": records}}
    spec["harness"]["builtin"]["tools"]["sandbox"] = False
    spec["harness"]["builtin"]["budget"] = {"tokens": 20000, "wall_clock": "90s"}
    if not database:
        spec.pop("environment", None)
        spec["gates"].append({"metric": "run-completed", "min": 1.0})
    spec["gates"].append(
        {"metric": "tool-call-accuracy.recall", "stat": "GATE_STAT_ERROR_RATE", "max": 0.0}
    )
    return document


def check_result(data: dict[str, Any], *, database: bool = False) -> dict[str, float]:
    if len(data["records"]) != 3:
        raise AssertionError("Expected exactly three records")
    if any(summary["errors"] for summary in data["summaries"]):
        raise AssertionError("Evaluator errors are not model failures")
    means = {summary["metric"]: summary["mean"] for summary in data["summaries"]}
    required = ["tool-call-accuracy.recall", "tool-call-accuracy.precision"]
    required += ["task-success", "contains"] if database else ["run-completed"]
    for metric in required:
        if means.get(metric) != 1.0:
            raise AssertionError(f"Expected {metric}=1, got {means.get(metric)}")
    if means.get("tool-errors") != 0.0:
        raise AssertionError(f"Expected tool-errors=0, got {means.get('tool-errors')}")
    return means


def run_case(
    cli: str, name: str, document: dict[str, Any], work: Path, *, database: bool = False
) -> dict[str, float]:
    path, output = work / f"{name}.yaml", work / f"{name}.json"
    path.write_text(yaml.safe_dump(document))
    result = subprocess.run(
        [cli, "run", "-f", str(path), "--output", str(output), "--quiet"],
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if result.returncode:
        detail = result.stdout + result.stderr
        target = document["spec"]["target"]
        for value in (target["base_url"], os.environ.get(target["api_key_env"], "")):
            if value:
                detail = detail.replace(value, "[redacted]")
        raise RuntimeError(f"{name}: CLI exit {result.returncode}\n{detail}")
    means = check_result(json.loads(output.read_text()), database=database)
    print(json.dumps({"case": name, "exit": result.returncode, "records": 3, "means": means}))
    return means


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--allow-paid", action="store_true", help="Explicitly permit provider calls"
    )
    parser.add_argument("--database", action="store_true", help="Also test source-only SQL state")
    args = parser.parse_args()
    if not args.allow_paid:
        parser.error("Provider calls cost money; pass --allow-paid to opt in")
    model, key_env, base = configuration()
    cli = shutil.which("evalsi")
    if cli is None:
        parser.error("evalsi is not on PATH; activate the environment being tested")
    with tempfile.TemporaryDirectory(prefix="evalsi-live-onboarding-") as tmp:
        work = Path(tmp)
        run_case(cli, "haiku-nonstream", build_spec(model, key_env, base, stream=False), work)
        streamed = build_spec(model, key_env, base, stream=True)
        streamed["spec"]["harness"]["builtin"]["recording"] = {
            "mode": "record",
            "dir": str(work / "tapes"),
        }
        recorded = run_case(cli, "haiku-stream-record", streamed, work)
        replay = copy.deepcopy(streamed)
        replay["spec"]["target"]["base_url"] = "http://127.0.0.1:1"
        replay["spec"]["harness"]["builtin"]["recording"]["mode"] = "replay"
        if run_case(cli, "haiku-offline-replay", replay, work) != recorded:
            raise AssertionError("Recorded and replayed metric means differ")
        if args.database:
            run_case(
                cli,
                "haiku-sql-state",
                build_spec(model, key_env, base, stream=False, database=True),
                work,
                database=True,
            )


if __name__ == "__main__":
    main()

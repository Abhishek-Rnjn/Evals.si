#!/usr/bin/env python3
"""Execute real published CLI commands and capture both demo controls.

This verification harness handles the intentionally failing negative control.
The actual CI-gate example runs evalsi directly and propagates its nonzero exit.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from importlib.metadata import version
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=ROOT / "results")
    args = parser.parse_args()
    for package in ("evalsi", "evalsi-harness"):
        if version(package) != "0.3.1":
            parser.error(f"{package} must be the published 0.3.1 package")
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    transcript: list[dict[str, object]] = []

    def run(arguments: list[str], expected: int) -> None:
        start = time.monotonic()
        result = subprocess.run(
            [sys.executable, "-m", "evalsi", *arguments],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        entry = {
            "command": ["evalsi", *arguments],
            "exit_code": result.returncode,
            "expected_exit_code": expected,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "elapsed_seconds": round(time.monotonic() - start, 4),
        }
        transcript.append(entry)
        print("$ " + " ".join(entry["command"]))
        print(result.stdout, end="")
        print(result.stderr, end="", file=sys.stderr)
        print(f"exit code: {result.returncode}\n")
        (out / "transcript.json").write_text(json.dumps(transcript, indent=2) + "\n")
        if result.returncode != expected:
            raise RuntimeError(f"Expected exit {expected}; got {result.returncode}")

    run(["--version"], 0)
    for name, spec, expected in (
        ("baseline", "run.yaml", 0),
        ("regressed", "regressed.yaml", 3),
    ):
        source = out / f"{name}.json"
        run(["run", "-f", spec, "--output", str(source)], expected)
        payload = json.loads(source.read_text())
        summary = payload["summaries"][0]
        assert summary["metric"] == "exact-match"
        assert summary["n"] == 2
        assert summary["errors"] == summary["skipped"] == 0
        assert summary["mean"] == (1.0 if name == "baseline" else 0.5)
        assert {row["record_id"] for row in payload["results"]} == {"france", "japan"}
        assert len(payload["results"]) == len(payload["records"]) == 2
        assert payload["manifest"]["evalsi_version"] == "0.3.1"
        assert payload["manifest"]["target"] is None
        assert payload["gates"][0]["passed"] == (name == "baseline")
        run(["report", str(source), "-o", str(out / f"{name}.html")], 0)
        run(["report", str(source), "-o", str(out / f"{name}.md")], 0)
    print("Both controls behaved as expected; reports are in " + str(out))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

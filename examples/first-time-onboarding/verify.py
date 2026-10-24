"""Run the tutorial matrix against the installed evalsi CLI, with no external API."""

from __future__ import annotations

import argparse
import json
import subprocess
import tempfile
import threading
from pathlib import Path

import yaml
from fake_model import Handler, Server


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", action="store_true", help="Also test source-only SQL tasks")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    server = Server(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    cases = [
        ("run", 0),
        ("regressed", 3),
        ("loop", 3),
        ("bad-arguments", 3),
        ("fault", 3),
        ("record", 0),
        ("replay", 0),
    ]
    if args.database:
        cases += [("database", 0), ("database-regressed", 3)]
    try:
        with tempfile.TemporaryDirectory(prefix="evalsi-onboarding-") as tmp:
            work = Path(tmp)
            for name, expected in cases:
                source = "database" if name == "database-regressed" else name
                spec = yaml.safe_load((root / f"{source}.yaml").read_text())
                spec["spec"]["dataset"]["path"] = str(root / "tasks.jsonl")
                spec["spec"]["target"]["base_url"] = f"http://127.0.0.1:{server.server_port}/v1"
                if name == "database-regressed":
                    spec["spec"]["target"]["model"] = "skip-refund"
                if name == "replay":
                    # Make model access impossible: replay must use the tape.
                    spec["spec"]["target"]["base_url"] = "http://127.0.0.1:1/v1"
                path = work / f"{name}.yaml"
                path.write_text(yaml.safe_dump(spec))
                output = work / f"{name}.json"
                result = subprocess.run(
                    ["evalsi", "run", "-f", str(path), "--output", str(output)],
                    capture_output=True,
                    text=True,
                    timeout=60,
                    check=False,
                )
                if result.returncode != expected:
                    raise AssertionError(
                        f"{name}: expected {expected}, got {result.returncode}\n"
                        f"{result.stdout}\n{result.stderr}"
                    )
                data = json.loads(output.read_text())
                assert len(data["records"]) == 3, (name, "missing records")
                assert all(s["errors"] == 0 for s in data["summaries"]), name
                means = {s["metric"]: s["mean"] for s in data["summaries"]}
                if name in {"run", "record", "replay", "database"}:
                    assert means["tool-call-accuracy.recall"] == 1, name
                if name == "regressed":
                    assert means["tool-call-accuracy.recall"] == 0.5
                    assert means["contains"] == 1, (
                        "The false success claim must still pass text check"
                    )
                if name == "database-regressed":
                    assert means["task-success"] == 0
                if name == "loop":
                    assert means["loop-detection"] == 0
                print(json.dumps({"case": name, "exit": result.returncode, "means": means}))
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()

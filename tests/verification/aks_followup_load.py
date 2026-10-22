"""Run every predeclared load replicate; preserve failed outcomes and reports."""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path


def main() -> int:
    output = Path("/work/evidence")
    output.mkdir(exist_ok=True)
    outcomes = []
    for backend, mode in [
        ("sqlite", "full"),
        ("sqlite", "normal"),
        ("clickhouse", "full"),
    ]:
        for repeat in range(1, 4):
            name = f"{backend}-{mode}-{repeat}"
            env = dict(
                os.environ,
                EVALSI_E2E_WORKER="/work/venv/bin/python -m evalsi",
                EVALSI_AKS_FOLLOWUP_LOAD="1",
                EVALSI_AKS_SQLITE_SYNCHRONOUS=mode,
                EVALSI_LOAD_REPORT=str(output / f"{name}.json"),
            )
            env.pop("EVALSI_LOAD_CLICKHOUSE_URL", None)
            if backend == "clickhouse":
                env["EVALSI_LOAD_CLICKHOUSE_URL"] = "http://clickhouse:8123"
            command = [
                "/work/build/e2e.test",
                "-test.run",
                "^TestAKSFollowupLoad$",
                "-test.v",
                "-test.timeout=12m",
            ]
            with (output / f"{name}.log").open("w") as log:
                result = subprocess.run(
                    command, env=env, stdout=log, stderr=subprocess.STDOUT, check=False
                )
            row = {"name": name, "returncode": result.returncode, "command": command}
            if (output / f"{name}.json").exists():
                row["measurements"] = json.loads((output / f"{name}.json").read_text())
            outcomes.append(row)
            (output / "load-outcomes.json").write_text(json.dumps(outcomes, indent=2))
            print(json.dumps(row), flush=True)
    return int(any(row["returncode"] for row in outcomes))


if __name__ == "__main__":
    raise SystemExit(main())

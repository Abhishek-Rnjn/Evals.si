"""Read PR-schema data with v0.1.0 and its matching published Python worker.

This is a reader compatibility check on an isolated copy of the SQL backup,
not certification of rolling upgrades or a reversible migration framework.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import subprocess
import time
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit, urlunsplit

import httpx

from evalsi.client import Client


def check(binary: str, worker: str, work: Path, state_path: Path) -> dict[str, Any]:
    secret = json.loads(
        subprocess.check_output(
            [
                "kubectl",
                "-n",
                "evalsi-leftovers-restore",
                "get",
                "secret",
                "restored-dev-postgres",
                "-o",
                "json",
            ]
        )
    )["data"]
    original = urlsplit(base64.b64decode(secret["dsn"]).decode())
    dsn = urlunsplit(
        (
            original.scheme,
            original.netloc.rsplit("@", 1)[0] + "@localhost:15432",
            "/compatibility",
            original.query,
            "",
        )
    )
    work.mkdir(parents=True, exist_ok=True)
    config = work / "config.json"
    config.write_text(
        json.dumps(
            {
                "listen": "127.0.0.1:18084",
                "data_dir": str(work / "data"),
                "storage": {"postgres": {"dsn_env": "EVALSI_TEST_COMPAT_DSN"}},
                "worker": {"command": [worker, "-m", "evalsi"], "no_cache": True},
            }
        )
    )
    state = json.loads(state_path.read_text())
    log = (work / "server.log").open("w")
    process = subprocess.Popen(
        [binary, "serve", "-config", str(config), "-no-auth"],
        env={**os.environ, "EVALSI_TEST_COMPAT_DSN": dsn},
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    try:
        for _ in range(100):
            assert process.poll() is None, "old server exited; inspect private server.log"
            try:
                if httpx.get("http://localhost:18084/healthz").status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            time.sleep(0.1)
        else:
            raise AssertionError("old server did not start")
        with Client("http://localhost:18084") as client:
            run = client.get_run(state["run_id"])
            assert run["status"] == "RUN_STATUS_SUCCEEDED", run
            assert len(client.list_run_results(state["run_id"])["results"]) == state["results"]
            assert client.get_run(state["archive_id"]).get("summaries")
        return {
            "reader": "published evalsid 0.1.0",
            "worker": "published evalsi 0.1.0",
            "schema": "PR b342df8",
            "database": "isolated restored copy",
            "completed_results_read": state["results"],
            "archived_summary_read": True,
            "rolling_upgrade_certified": False,
        }
    finally:
        process.terminate()
        process.wait(30)
        log.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--worker", required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    result = check(args.binary, args.worker, args.work_dir, args.state)
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))

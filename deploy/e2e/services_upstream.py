"""Audit a pinned Terminal-Bench checkout and replay one explicit environment adaptation.

Prepare creates Docker contexts and a restricted Compose task, without changing
upstream files. Build/preload the two images before running; see verification notes.
No benchmark instructions, reference solutions or tests are vendored in this repo.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import shutil
import subprocess
from pathlib import Path
from typing import Any

import yaml

from evalsi.sandbox.client import SandboxClient
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness.benchmarks.harbor import TaskFormatError, load_terminal_bench
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool
from evalsi_harness.task import Task

REVISION = "d28711d0da2675d0bb1d56de45ae5df6082438a3"
TASK = "simple-web-scraper"
CLIENT = "evalsi-upstream-client:test"
SERVER = "evalsi-upstream-server:test"
PRESERVED = ("task.yaml", "solution.sh", "tests/test_outputs.py")


def audit(root: Path) -> dict[str, Any]:
    revision = subprocess.check_output(
        ["git", "-C", str(root), "rev-parse", "HEAD"], text=True
    ).strip()
    if revision != REVISION:
        raise ValueError(f"expected upstream {REVISION}, got {revision}")
    changed = subprocess.check_output(["git", "-C", str(root), "status", "--porcelain"], text=True)
    if changed:
        raise ValueError("use an unmodified upstream checkout")
    rejected = []
    tasks = sorted((root / "original-tasks").glob("*/task.yaml"))
    if not tasks:
        raise ValueError("checkout must include original-tasks")
    for marker in tasks:
        try:
            load_terminal_bench(str(marker.parent))
        except (TaskFormatError, FileNotFoundError) as exc:
            rejected.append({"task": marker.parent.name, "reason": str(exc)})
    return {
        "repository": "laude-institute/terminal-bench",
        "revision": revision,
        "audit_scope": "import only; not task execution",
        "total": len(tasks),
        "imported": len(tasks) - len(rejected),
        "rejected": rejected,
    }


def prepare(root: Path, work: Path) -> dict[str, str]:
    source = root / "original-tasks" / TASK
    adapted = work / "task"
    adapted.mkdir(parents=True, exist_ok=False)
    hashes = {}
    for relative in PRESERVED:
        data = (source / relative).read_bytes()
        target = adapted / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        hashes[relative] = hashlib.sha256(data).hexdigest()
    (adapted / "Dockerfile").write_text(f"FROM {CLIENT}\nWORKDIR /app\n")
    (adapted / "run-tests.sh").write_text(
        '#!/bin/sh\n/opt/checker/bin/python -m pytest "$TEST_DIR/test_outputs.py" -rA\n'
    )
    (adapted / "docker-compose.yaml").write_text(
        yaml.safe_dump(
            {
                "services": {
                    "client": {"build": ".", "depends_on": ["server"]},
                    "server": {
                        "image": SERVER,
                        "entrypoint": ["python", "/app/mock_website.py"],
                        "x-evalsi": {"port": 8000, "memory_mb": 64, "cpu_millis": 100},
                    },
                }
            }
        )
    )
    client = work / "client"
    client.mkdir()
    # Same agent dependencies as upstream; same checker versions in a separate venv.
    # Optional BuildKit CA secret supports this workspace's outbound HTTPS proxy.
    (client / "Dockerfile").write_text("""FROM python:3.13-slim
WORKDIR /app
RUN --mount=type=secret,id=proxy_ca \\
    if [ -f /run/secrets/proxy_ca ]; then export PIP_CERT=/run/secrets/proxy_ca; fi; \\
    pip install --no-cache-dir requests==2.32.4 beautifulsoup4==4.13.4 pandas==2.3.0 && \\
    python -m venv /opt/checker && \\
    /opt/checker/bin/pip install --no-cache-dir pytest==8.4.1 pandas==2.3.2
""")
    server = work / "server"
    server.mkdir()
    shutil.copyfile(source / "server/mock_website.py", server / "mock_website.py")
    (server / "Dockerfile").write_text(
        "FROM python:3.13-slim\nWORKDIR /app\nCOPY mock_website.py /app/mock_website.py\n"
    )
    return hashes


async def replay(work: Path, hashes: dict[str, str]) -> dict[str, Any]:
    adapted = work / "task"
    for relative, expected in hashes.items():
        assert hashlib.sha256((adapted / relative).read_bytes()).hexdigest() == expected
    [row] = load_terminal_bench(str(adapted), oracle="true", network="deny")
    record = Record(id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"])
    task = Task.build(run_pb2.RunSpec(), record)
    assert task.environment is not None
    assert task.environment.checker is not None
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:
        pool = EnvironmentPool(client)
        env = await pool.acquire(task.environment)
        try:
            before = await run_checker(env, task.environment.checker, task)
            assert not before.passed, before
            assert not before.error, before
            result = await env.sandbox.exec(["sh", "/solution/solution.sh"])
            assert result.ok, result
            after = await run_checker(env, task.environment.checker, task)
            assert after.passed, after
            assert not after.error, after
            assert after.details == "5/5 tests passed", after
            assert (await env.sandbox.exec(["rm", "/app/books.csv"])).ok
            regressed = await run_checker(env, task.environment.checker, task)
            assert not regressed.passed, regressed
            assert not regressed.error, regressed
            report = {
                "task": TASK,
                "preserved_sha256": hashes,
                "before": before.details,
                "oracle": after.details,
                "removed_output": regressed.details,
                "environment": "adapted prebuilt images; restricted Compose; network deny",
            }
            print("PASS: upstream task oracle and negative controls " + json.dumps(report))
            return report
        finally:
            await env.sandbox.destroy()
            await pool.aclose()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream-root", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--prepare", action="store_true")
    args = parser.parse_args()
    report = audit(args.upstream_root)
    report_path = args.work_dir / "verification.json"
    if args.prepare:
        report["preserved_sha256"] = prepare(args.upstream_root, args.work_dir)
    else:
        previous = json.loads(report_path.read_text())
        report["execution"] = asyncio.run(replay(args.work_dir, previous["preserved_sha256"]))
        report["preserved_sha256"] = previous["preserved_sha256"]
    report_path.write_text(json.dumps(report, indent=2) + "\n")
    print(f"Import audit: {report['imported']}/{report['total']}; report: {report_path}")


if __name__ == "__main__":
    main()

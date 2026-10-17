#!/usr/bin/env python3
"""Real pod service lifecycle checks, provisioned by services-kind.sh.

Needs EVALSI_SANDBOX_ADDR, mutual TLS paths and kubectl. Uses no model keys.
"""

from __future__ import annotations

import asyncio
import json
import os
import subprocess
from contextlib import AsyncExitStack, suppress
from dataclasses import replace
from pathlib import Path
from typing import Any

import yaml
from google.protobuf.json_format import ParseDict

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import SandboxClient
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness.benchmarks.harbor import load_terminal_bench
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool, TaskEnvironment
from evalsi_harness.task import EnvironmentConfig, Task, TaskError, environment_for

ROOT = Path(__file__).resolve().parents[2]
NS = os.environ.get("EVALSI_SERVICES_NAMESPACE", "evalsi-services")


def pods() -> list[dict[str, Any]]:
    result = subprocess.check_output(
        ["kubectl", "get", "pods", "-n", NS, "-l", "evals.si/service-owner", "-o", "json"]
    )
    return json.loads(result)["items"]


async def check_cross_trial_isolation(first: TaskEnvironment) -> None:
    live = pods()
    assert len(live) == 2, live
    ip_script = "import socket; print(socket.gethostbyname(socket.gethostname()))"
    own_ip = (await first.sandbox.exec(["python", "-c", ip_script])).stdout.strip()
    other_ip = next(p["status"]["podIP"] for p in live if p["status"]["podIP"] != own_ip)
    for port in (6379, 7070):
        denied = await first.sandbox.exec(
            [
                "python",
                "-c",
                "import socket; s=socket.socket(); s.settimeout(2); "
                f"assert s.connect_ex(({other_ip!r},{port})) != 0",
            ]
        )
        assert denied.ok, (port, denied)


async def check_service_crash(first: TaskEnvironment, task: Task) -> None:
    assert first.config.checker is not None
    kill = (
        "import socket; s=socket.create_connection(('db',6379)); "
        "s.sendall(b'SHUTDOWN NOSAVE\\r\\n')"
    )
    # The post-command health check may already observe the crash.
    with suppress(SandboxError):
        await first.sandbox.exec(["python", "-c", kill])
    for _ in range(30):
        states = await first.sandbox.services()
        if any(s["restarts"] or not s["ready"] for s in states):
            break
        await asyncio.sleep(1)
    failure = await run_checker(first, first.config.checker, task)
    assert failure.error, failure


async def check_trials(pool: EnvironmentPool, config: EnvironmentConfig, task: Task) -> None:
    assert config.checker is not None
    async with AsyncExitStack() as cleanup:
        first = await pool.acquire(config)
        cleanup.push_async_callback(first.sandbox.destroy)
        second = await pool.acquire(config)
        cleanup.push_async_callback(second.sandbox.destroy)
        assert first.isolation.enforcement == "full", first.isolation
        states = await first.sandbox.services(include_logs=True)
        assert len(states) == 2, states
        assert all(s["ready"] for s in states), states
        bad = await run_checker(first, config.checker, task)
        assert not bad.passed, bad
        assert not bad.error, bad
        result = await first.sandbox.exec(list(task.spec.target.agent.cli.command))
        assert result.ok, result
        good = await run_checker(first, config.checker, task)
        assert good.passed, good
        assert not good.error, good
        fresh = await run_checker(second, config.checker, task)
        assert not fresh.passed, fresh
        assert not fresh.error, fresh
        await check_cross_trial_isolation(first)
        await check_service_crash(first, task)
    print(
        "PASS: readiness, API/database grading, fresh state, trial isolation, crash errors"
    )


async def check_terminal_bench(pool: EnvironmentPool) -> None:
    [row] = load_terminal_bench(
        str(ROOT / "examples/multi-service/terminal-bench"), oracle="true", network="deny"
    )
    record = Record(id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"])
    task = Task.build(run_pb2.RunSpec(), record)
    assert task.environment is not None
    assert task.environment.checker is not None
    imported = await pool.acquire(task.environment)
    try:
        assert (await imported.sandbox.exec(["sh", "/solution/solution.sh"])).ok
        checked = await run_checker(imported, task.environment.checker, task)
        assert checked.passed, checked
        assert not checked.error, checked
        print("PASS: multi-service Terminal-Bench-format import and execution")
    finally:
        await imported.sandbox.destroy()


async def check_failed_startup(pool: EnvironmentPool, config: EnvironmentConfig) -> None:
    broken = replace(config, services=[replace(config.services[1], command=["sh", "-c", "exit 7"])])
    failure = ""
    try:
        env = await pool.acquire(broken)
    except TaskError as exc:
        failure = str(exc)
    else:
        await env.sandbox.destroy()
        raise AssertionError("broken service started")
    assert "service db" in failure, failure
    for _ in range(30):
        if not pods():
            break
        await asyncio.sleep(1)
    assert not pods(), "orphaned service pods"
    print("PASS: destroy and failed-startup cleanup")


async def main() -> None:
    spec = ParseDict(
        yaml.safe_load((ROOT / "examples/multi-service/run.yaml").read_text())["spec"],
        run_pb2.RunSpec(),
    )
    record = Record(id="fixture", input=Content(text="Set answer to 42"))
    config = environment_for(spec, record)
    assert config is not None
    task = Task.build(spec, record)
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:
        pool = EnvironmentPool(client)
        try:
            await check_trials(pool, config, task)
            await check_terminal_bench(pool)
            await check_failed_startup(pool, config)
        finally:
            await pool.aclose()


if __name__ == "__main__":
    asyncio.run(main())

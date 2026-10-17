"""Destructive lifecycle checks, restricted to this runner's local kind pool."""

from __future__ import annotations

import asyncio
import json
import math
import os
import signal
import subprocess
import sys
import time
from dataclasses import asdict, replace
from typing import Any

import yaml

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import Isolation, Sandbox, SandboxClient, SandboxSpec
from evalsi_harness.environment import EnvironmentPool
from evalsi_harness.task import EnvironmentConfig

NS = os.environ.get("EVALSI_SERVICES_NAMESPACE", "evalsi-services")
CLUSTER = os.environ.get("EVALSI_SERVICES_KIND_CLUSTER", "evalsi-services")


def kube(*args: str) -> dict[str, Any]:
    return json.loads(subprocess.check_output(["kubectl", "-n", NS, *args, "-o", "json"]))


def service_pods() -> list[dict[str, Any]]:
    return kube("get", "pods", "-l", "evals.si/service-owner")["items"]


def db_spec(config: EnvironmentConfig, *, lifetime: float = 120, idle: float = 0) -> SandboxSpec:
    db = next(s for s in config.services if s.name == "db")
    return SandboxSpec(
        image=config.image,
        min_isolation="namespaced",
        memory_mb=64,
        services=[db],
        service_lifetime_s=lifetime,
        idle_timeout_s=idle,
    )


async def wait_absent(names: set[str], timeout: float = 40) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not names.intersection(p["metadata"]["name"] for p in service_pods()):
            return
        await asyncio.sleep(0.5)
    raise AssertionError(f"orphaned service pods: {sorted(names)}")


async def check_deadline(client: SandboxClient, config: EnvironmentConfig) -> dict[str, float]:
    before = {p["metadata"]["name"] for p in service_pods()}
    started = time.monotonic()
    sandbox = await client.create(db_spec(config, lifetime=25))
    [pod] = [p for p in service_pods() if p["metadata"]["name"] not in before]
    name = pod["metadata"]["name"]
    command = asyncio.create_task(sandbox.exec(["sh", "-c", "sleep 120"], timeout_s=150))
    try:
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            current = kube("get", "pod", name)
            if current["status"].get("reason") == "DeadlineExceeded":
                statuses = current["status"].get("containerStatuses", []) + current["status"].get(
                    "initContainerStatuses", []
                )
                if statuses and all("terminated" in s["state"] for s in statuses):
                    break
            await asyncio.sleep(0.5)
        else:
            raise AssertionError("kubelet did not terminate every container at the deadline")
        failure = ""
        try:
            await asyncio.wait_for(command, 10)
        except SandboxError as exc:
            failure = str(exc)
        assert failure, "expired execution did not produce an infrastructure error"
        elapsed = time.monotonic() - started
        assert elapsed < 60, elapsed
        print(f"PASS: live deadline expiry stopped all containers and execution ({elapsed:.1f}s)")
        return {"deadline_elapsed_s": round(elapsed, 3)}
    finally:
        command.cancel()
        await asyncio.gather(command, return_exceptions=True)
        await sandbox.destroy()
        await wait_absent({name})


async def check_setup_cancellation(client: SandboxClient, config: EnvironmentConfig) -> None:
    created: asyncio.Future[Sandbox] = asyncio.get_running_loop().create_future()

    class ObservedClient:
        async def create(self, spec: SandboxSpec | None = None) -> Sandbox:
            sb = await client.create(spec)
            created.set_result(sb)
            return sb

    cfg = replace(
        config,
        services=[next(s for s in config.services if s.name == "db")],
        setup=["echo started > setup-started; sleep 120"],
        setup_timeout_s=150,
    )
    before = {p["metadata"]["name"] for p in service_pods()}
    task = asyncio.create_task(EnvironmentPool(ObservedClient()).acquire(cfg))
    try:
        sb = await asyncio.wait_for(asyncio.shield(created), 45)
        for _ in range(30):
            if await sb.read_files(["setup-started"]):
                break
            await asyncio.sleep(0.2)
        else:
            raise AssertionError("setup never started")
        names = {p["metadata"]["name"] for p in service_pods()} - before
        assert len(names) == 1, names
        task.cancel()
        result = await asyncio.gather(task, return_exceptions=True)
        assert isinstance(result[0], asyncio.CancelledError), result
        await wait_absent(names)
        print("PASS: live cancellation during setup removed the complete service pod")
    finally:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        if created.done() and not created.cancelled():
            await created.result().destroy()


async def check_worker_loss(client: SandboxClient, config: EnvironmentConfig) -> dict[str, float]:
    # This is a real SDK worker process, killed while a command is in flight.
    # Its per-session idle lease is short so the normal manager reaper is observable.
    spec = db_spec(config, lifetime=120, idle=3)

    script = """
import asyncio, json, os
from evalsi.sandbox.client import SandboxClient, SandboxSpec
async def run():
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:
        spec = SandboxSpec.from_dict(json.loads(os.environ["EVALSI_WORKER_SPEC"]))
        sb = await client.create(spec)
        print(sb.id, flush=True)
        await sb.exec(["sh", "-c", "echo started > worker-started; sleep 120"], timeout_s=150)
asyncio.run(run())
"""
    before = {p["metadata"]["name"] for p in service_pods()}
    worker = await asyncio.create_subprocess_exec(
        sys.executable,
        "-u",
        "-c",
        script,
        env={**os.environ, "EVALSI_WORKER_SPEC": json.dumps(asdict(spec))},
        stdout=asyncio.subprocess.PIPE,
    )
    try:
        assert worker.stdout is not None
        session_id = (await asyncio.wait_for(worker.stdout.readline(), 45)).decode().strip()
        assert session_id.startswith("sbx_"), session_id
        sb = Sandbox(client, session_id, Isolation("pod", "namespaced", "full"))
        for _ in range(30):
            if await sb.read_files(["worker-started"]):
                break
            await asyncio.sleep(0.2)
        else:
            raise AssertionError("worker command never started")
        names = {p["metadata"]["name"] for p in service_pods()} - before
        assert len(names) == 1, names
        started = time.monotonic()
        worker.kill()
        assert await worker.wait() == -signal.SIGKILL
        await wait_absent(names, timeout=35)
        elapsed = time.monotonic() - started
        print(f"PASS: SIGKILL of an SDK worker reclaimed its live service pod ({elapsed:.1f}s)")
        return {"worker_reap_s": round(elapsed, 3)}
    finally:
        if worker.returncode is None:
            worker.kill()
        await worker.wait()


def kill_pool_process(pool: dict[str, Any]) -> None:
    node = pool["spec"]["nodeName"]
    docker = ["docker", "--host=unix:///var/run/docker.sock"]
    docker_env = {
        k: v for k, v in os.environ.items() if not k.startswith("DOCKER_") or k == "DOCKER_CONFIG"
    }
    labels = json.loads(subprocess.check_output([*docker, "inspect", node], env=docker_env))[0][
        "Config"
    ]["Labels"]
    assert labels.get("io.x-k8s.kind.cluster") == CLUSTER, (
        "refusing to kill outside the test kind cluster"
    )
    assert pool["metadata"]["labels"].get("app") == "pool", "not the test pool"
    container = pool["status"]["containerStatuses"][0]["containerID"].removeprefix("containerd://")
    info = json.loads(
        subprocess.check_output(
            [*docker, "exec", node, "crictl", "inspect", container], env=docker_env
        )
    )
    pid = int(info["info"]["pid"])
    assert pid > 1, pid
    subprocess.run([*docker, "exec", node, "kill", "-KILL", str(pid)], env=docker_env, check=True)


async def check_pool_restart(client: SandboxClient, config: EnvironmentConfig) -> None:
    old_pool = kube("get", "pod", "pool")
    before = {p["metadata"]["name"] for p in service_pods()}
    orphan = await client.create(db_spec(config, lifetime=180))
    names = {p["metadata"]["name"] for p in service_pods()} - before
    assert len(names) == 1, names
    old_restarts = old_pool["status"]["containerStatuses"][0]["restartCount"]
    kill_pool_process(old_pool)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        current = kube("get", "pod", "pool")
        state = current["status"]["containerStatuses"][0]
        if state["restartCount"] > old_restarts and state.get("ready"):
            break
        await asyncio.sleep(0.5)
    else:
        raise AssertionError("pool did not restart")
    assert current["metadata"]["uid"] == old_pool["metadata"]["uid"]
    assert names.issubset(p["metadata"]["name"] for p in service_pods()), (
        "pool was not killed abruptly"
    )
    # Reconnect explicitly: the old process and all of its session IDs are gone.
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as fresh:
        await asyncio.wait_for(fresh._channel.channel_ready(), 30)
        replacement = await fresh.create(db_spec(config))
        try:
            await wait_absent(names)
            result = await replacement.exec(["sh", "-c", "echo recovered"])
            assert result.ok, result
            assert result.stdout.strip() == "recovered", result
            failure = ""
            try:
                await Sandbox(fresh, orphan.id, orphan.isolation).exec(["true"])
            except SandboxError as exc:
                failure = str(exc)
            assert "NOT_FOUND" in failure, failure
        finally:
            await replacement.destroy()
    print("PASS: pool SIGKILL preserved its UID, reaped old pods, and accepted fresh work")


async def check_creation_cancellation(client: SandboxClient, config: EnvironmentConfig) -> None:
    before = {p["metadata"]["name"] for p in service_pods()}
    spec = db_spec(config)
    spec.services = [replace(spec.services[0], command=["sh", "-c", "sleep 120"])]
    task = asyncio.create_task(client.create(spec))
    names: set[str] = set()
    try:
        for _ in range(90):
            names = {p["metadata"]["name"] for p in service_pods()} - before
            if names:
                break
            if task.done():
                await task
                raise AssertionError("unready service unexpectedly started")
            await asyncio.sleep(0.2)
        assert len(names) == 1, names
        task.cancel()
        result = await asyncio.gather(task, return_exceptions=True)
        assert isinstance(result[0], asyncio.CancelledError), result
        await wait_absent(names)
        replacement = await client.create(db_spec(config))
        await replacement.destroy()
        print("PASS: cancelling startup removed its pending pod and released the admission slot")
    finally:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)


async def check_capacity(client: SandboxClient, config: EnvironmentConfig) -> int:
    limits = yaml.safe_load(kube("get", "configmap", "pool")["data"]["evalsi.yaml"])
    cap = limits["sandbox"]["max_sandboxes"]
    assert cap == 4, f"apply the test pool fixture first (configured max is {cap})"
    requests = cap * 4
    created = await asyncio.gather(
        *(client.create(db_spec(config)) for _ in range(requests)),
        return_exceptions=True,
    )
    accepted = [s for s in created if isinstance(s, Sandbox)]
    rejected = [s for s in created if isinstance(s, BaseException)]
    names = {p["metadata"]["name"] for p in service_pods()}
    try:
        assert len(accepted) == cap, (len(accepted), rejected)
        assert len(rejected) == requests - cap, rejected
        assert all("too many sandboxes" in str(e) for e in rejected), rejected
        assert len(names) == cap, names
    finally:
        await asyncio.gather(*(s.destroy() for s in accepted))
        await wait_absent(names)
    print(f"PASS: capacity burst admitted {cap}/{requests}, without exceeding max_sandboxes")

    return cap


async def check_capacity_and_load(
    client: SandboxClient, config: EnvironmentConfig, trials: int = 24
) -> dict[str, Any]:
    cap = await check_capacity(client, config)
    cfg = replace(config, sandbox=replace(config.sandbox, memory_mb=64))
    pool = EnvironmentPool(client)
    semaphore = asyncio.Semaphore(cap)
    durations: list[float] = []
    scoring: list[float] = []
    peak = 0
    started = time.monotonic()
    command = """
import socket,sys,urllib.request
value = sys.argv[1].encode()
with socket.create_connection(("db",6379), timeout=3) as conn:
    conn.sendall(b"SET answer " + value + b"\\r\\n")
    assert conn.recv(1024) == b"+OK\\r\\n"
assert urllib.request.urlopen("http://api:8080", timeout=3).read() == value
"""

    async def trial(index: int) -> None:
        nonlocal peak
        async with semaphore:
            begin = time.monotonic()
            env = await pool.acquire(cfg)
            note = next(n for n in env.isolation.notes if n.startswith("pod "))
            name = note.split(",", 1)[0].split("/", 1)[1]
            try:
                live = service_pods()
                peak = max(peak, len(live))
                assert len(live) <= cap, len(live)
                score_start = time.monotonic()
                result = await env.sandbox.exec(["python", "-c", command, str(index)])
                assert result.ok, (index, result)
                scoring.append(time.monotonic() - score_start)
            finally:
                await env.sandbox.destroy()
                await wait_absent({name})
            durations.append(time.monotonic() - begin)
            if len(durations) % cap == 0:
                print(f"load: {len(durations)}/{trials} trials passed", flush=True)

    tasks = [asyncio.create_task(trial(i)) for i in range(trials)]
    try:
        await asyncio.gather(*tasks)
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        await pool.aclose()
    assert not service_pods(), "load left service pods behind"
    elapsed = time.monotonic() - started

    def percentile(values: list[float], fraction: float) -> float:
        return round(sorted(values)[max(0, math.ceil(len(values) * fraction) - 1)], 3)

    report = {
        "burst_requests": cap * 4,
        "burst_accepted": cap,
        "completed_trials": len(durations),
        "concurrency": cap,
        "peak_service_pods": peak,
        "elapsed_s": round(elapsed, 3),
        "trial_p50_s": percentile(durations, 0.5),
        "trial_p95_s": percentile(durations, 0.95),
        "scoring_p50_s": percentile(scoring, 0.5),
        "scoring_p95_s": percentile(scoring, 0.95),
    }
    print("PASS: bounded concurrent service load " + json.dumps(report, sort_keys=True))
    return report

"""Owned-worker daemon crash and historical/current PostgreSQL read/write drills."""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
import signal
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Iterator
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from pathlib import Path

BASE = "http://127.0.0.1:8085"


def request(path: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        BASE + path, data=data, headers={"content-type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"HTTP {exc.code}: {exc.read().decode()}") from exc


@contextmanager
def daemon(
    binary: Path, root: Path, storage: dict, data_dir: Path | None = None
) -> Iterator[subprocess.Popen]:
    root.mkdir(parents=True)
    config = {
        "listen": "127.0.0.1:8085",
        "data_dir": str(root / "data" if data_dir is None else data_dir),
        "worker": {
            "command": ["/work/venv/bin/python", "-m", "evalsi"],
            "start_timeout": "60s",
        },
        "otlp": {"grace": "2s", "max_traces": 100000},
        "storage": storage,
        "sinks": [],
    }
    path = root / "config.json"
    path.write_text(json.dumps(config))
    with (root / "daemon.log").open("w") as log:
        process = subprocess.Popen(
            [str(binary), "serve", "--config", str(path), "--no-auth"],
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        children: list[int] = []
        try:
            for _ in range(120):
                if process.poll() is not None:
                    raise RuntimeError(
                        f"daemon exited {process.returncode}; inspect {root / 'daemon.log'}"
                    )
                try:
                    with urllib.request.urlopen(BASE + "/ui/", timeout=2) as response:
                        if response.status == 200:
                            break
                except urllib.error.URLError as exc:
                    if not isinstance(exc.reason, ConnectionRefusedError):
                        raise
                time.sleep(0.5)
            else:
                raise TimeoutError("daemon not responsive within 60 seconds")
            children = [
                int(pid)
                for pid in Path(f"/proc/{process.pid}/task/{process.pid}/children")
                .read_text()
                .split()
            ]
            yield process
        finally:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=30)
            for pid in children:
                try:
                    os.kill(pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass


def spans(first: int, count: int) -> dict:
    now = time.time_ns()
    return {
        "resourceSpans": [
            {
                "resource": {
                    "attributes": [
                        {
                            "key": "service.name",
                            "value": {"stringValue": "synthetic-crash-control"},
                        }
                    ]
                },
                "scopeSpans": [
                    {
                        "spans": [
                            {
                                "traceId": f"{index:032x}",
                                "spanId": f"{index:016x}",
                                "name": "chat",
                                "startTimeUnixNano": str(now),
                                "endTimeUnixNano": str(now + 1000),
                                "attributes": [
                                    {
                                        "key": "gen_ai.operation.name",
                                        "value": {"stringValue": "chat"},
                                    }
                                ],
                            }
                            for index in range(first, first + count)
                        ]
                    }
                ],
            }
        ]
    }


def stored(path: Path) -> dict[str, str]:
    with sqlite3.connect(f"file:{path}?mode=ro", uri=True) as db:
        return {
            trace: hashlib.sha256(summary + record).hexdigest()
            for trace, summary, record in db.execute(
                "SELECT trace_id, summary, record FROM traces ORDER BY trace_id"
            )
        }


def export(first: int, count: int) -> None:
    response = request("/v1/traces", spans(first, count))
    if int(response.get("partialSuccess", {}).get("rejectedSpans", 0)):
        raise RuntimeError(f"OTLP export partially rejected: {response}")


def sqlite_crash(binary: Path, root: Path, mode: str, replicate: int) -> dict:
    phase = root / f"sqlite-{mode}-{replicate}"
    storage = {"sqlite": {"synchronous": mode}}
    preexisting = {f"{index:032x}" for index in range(1, 101)}
    accepted = set(preexisting)
    uncertain: list[str] = []
    errors: list[str] = []
    threshold = threading.Event()
    db = phase / "data" / "evalsi.db"
    with daemon(binary, phase, storage) as process:
        export(1, 100)
        deadline = time.monotonic() + 30
        while not preexisting.issubset(stored(db)):
            if time.monotonic() > deadline:
                raise TimeoutError("100 pre-crash committed traces not visible")
            time.sleep(0.05)
        before = stored(db)

        def ingest() -> None:
            for first in range(101, 5101, 20):
                ids = [f"{index:032x}" for index in range(first, first + 20)]
                try:
                    export(first, 20)
                except (
                    urllib.error.URLError,
                    ConnectionError,
                    http.client.RemoteDisconnected,
                ) as exc:
                    uncertain.extend(ids)
                    errors.append(f"{type(exc).__name__}: {exc}")
                    return
                accepted.update(ids)
                if len(accepted) >= 300:
                    threshold.set()

        with ThreadPoolExecutor(max_workers=1) as pool:
            writer = pool.submit(ingest)
            if not threshold.wait(30):
                writer.result(timeout=35)
                raise RuntimeError(f"ingest did not reach crash threshold: {errors}")
            if not writer.running():
                writer.result()
                raise RuntimeError("ingest finished before SIGKILL")
            observed_before_kill = stored(db)
            process.kill()
            assert process.wait(timeout=30) == -signal.SIGKILL
            writer.result(timeout=35)

    restart = root / f"sqlite-{mode}-{replicate}-restart"
    with daemon(binary, restart, storage, data_dir=phase / "data"):
        recovered = stored(db)
        with sqlite3.connect(f"file:{db}?mode=ro", uri=True) as connection:
            integrity = connection.execute("PRAGMA integrity_check").fetchall()
            journal = connection.execute("PRAGMA journal_mode").fetchone()
        if integrity != [("ok",)] or journal != ("wal",):
            raise AssertionError(f"unexpected database integrity/mode: {integrity} {journal}")
        if any(recovered.get(key) != value for key, value in before.items()):
            raise AssertionError("a confirmed pre-crash stored trace changed or was lost")
        if not observed_before_kill.keys() <= recovered.keys():
            raise AssertionError("a trace observed stored immediately before kill was lost")
    return {
        "mode": mode,
        "replicate": replicate,
        "before": before,
        "observed_before_kill": observed_before_kill,
        "accepted": sorted(accepted),
        "uncertain": uncertain,
        "transport_errors_at_crash": errors,
        "recovered": recovered,
        "accepted_trace_count": len(accepted),
        "observed_stored_before_kill_count": len(observed_before_kill),
        "recovered_trace_count": len(recovered),
        "lost_acknowledged_trace_count": len(accepted - recovered.keys()),
        "lost_previously_observed_stored_trace_count": len(
            observed_before_kill.keys() - recovered.keys()
        ),
        "integrity_check": "ok",
        "journal_mode": "wal",
        "power_loss_tested": False,
    }


def run_snapshot(run_id: str) -> dict:
    prefix = "/evalsi.v1alpha1.RunService/"
    run = request(prefix + "GetRun", {"id": run_id})["run"]
    run.pop("evaluationContract", None)
    results = request(prefix + "ListRunResults", {"runId": run_id, "pageSize": 100})
    return {
        "run": run,
        "results": results["results"],
        "records": results["records"],
    }


def postgres_phase(args: argparse.Namespace) -> None:
    expected = [] if args.expected is None else json.loads(args.expected.read_text())
    storage = {"postgres": {"dsn": args.dsn}}
    with daemon(args.binary, args.output, storage):
        for snapshot in expected:
            observed = run_snapshot(snapshot["run"]["id"])
            if observed != snapshot:
                raise AssertionError("stored common run/results/records changed")
        if args.create:
            records = [
                {
                    "id": f"synthetic/{index}",
                    "input": {"text": f"question {index}"},
                    "output": {"text": f"answer {index}"},
                    "reference": {"text": f"answer {index}"},
                }
                for index in range(3)
            ]
            response = request(
                "/evalsi.v1alpha1.RunService/CreateRun",
                {
                    "name": args.create,
                    "project": "default",
                    "spec": {
                        "dataset": {"inline": {"records": records}},
                        "evaluators": [{"ref": "exact-match"}],
                        "trials": 1,
                    },
                },
            )
            run_id = response["run"]["id"]
            deadline = time.monotonic() + 60
            while True:
                run = request("/evalsi.v1alpha1.RunService/GetRun", {"id": run_id})["run"]
                if run["status"] == "RUN_STATUS_SUCCEEDED":
                    break
                if run["status"] in {"RUN_STATUS_FAILED", "RUN_STATUS_CANCELLED"}:
                    raise RuntimeError(f"recorded run failed: {run}")
                if time.monotonic() > deadline:
                    raise TimeoutError("recorded run did not complete in 60 seconds")
                time.sleep(0.2)
            snapshot = run_snapshot(run_id)
            if len(snapshot["results"]) != 3:
                raise AssertionError("expected 3 recorded records x 1 trial")
            if any(not result.get("scores") for result in snapshot["results"]):
                raise AssertionError("recorded controls must have nonempty scores")
            if any(
                not score["passed"]
                for result in snapshot["results"]
                for score in result.get("scores", [])
            ):
                raise AssertionError("recorded exact-match control did not pass")
            expected.append(snapshot)
    (args.output / "snapshots.json").write_text(json.dumps(expected, indent=2))
    print(f"PASS: {len(expected)} common run snapshots; daemon stopped", flush=True)


def schema_guard(args: argparse.Namespace) -> None:
    try:
        with daemon(args.binary, args.output, {"postgres": {"dsn": args.dsn}}):
            raise AssertionError("daemon opened a database marked beyond its reader version")
    except RuntimeError:
        log = (args.output / "daemon.log").read_text()
        message = "the database schema is newer than this evalsid can read"
        if message not in log:
            raise
    (args.output / "schema-guard.json").write_text(
        json.dumps({"refused": True, "expected_error": message}, indent=2)
    )
    print("PASS: current daemon refused the simulated future minimum reader", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["sqlite", "postgres", "schema-guard"])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--binary", type=Path, default=Path("/work/build/evalsid"))
    parser.add_argument("--dsn")
    parser.add_argument("--expected", type=Path)
    parser.add_argument("--create")
    args = parser.parse_args()
    if args.mode != "sqlite":
        if args.dsn is None:
            parser.error("postgres requires --dsn for the owned disposable database")
        if args.mode == "schema-guard":
            schema_guard(args)
        else:
            postgres_phase(args)
    else:
        args.output.mkdir(parents=True)
        for mode in ["full", "normal"]:
            for replicate in range(1, 4):
                result = sqlite_crash(args.binary, args.output, mode, replicate)
                (args.output / f"{mode}-{replicate}.json").write_text(json.dumps(result, indent=2))
                print(f"completed {mode} replicate {replicate}", flush=True)


if __name__ == "__main__":
    main()

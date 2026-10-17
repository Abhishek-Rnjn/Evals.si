"""A disposable PostgreSQL WAL/PITR drill on restored synthetic data.

Mutates only restored-dev-postgres in evalsi-leftovers-restore, with API/workers
stopped. Local WAL storage proves recovery mechanics; production needs an
independent durable archive, retention, monitoring and a separate restore drill.
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import subprocess
import time
from pathlib import Path
from typing import Any

NS = "evalsi-leftovers-restore"
PG = "restored-dev-postgres-0"


def kube(*args: str, data: bytes | None = None) -> bytes:
    return subprocess.check_output(["kubectl", "-n", NS, *args], input=data)


def sql(statement: str, pod: str = PG) -> str:
    return (
        kube("exec", pod, "--", "psql", "-U", "evalsi", "-d", "evalsi", "-Atc", statement)
        .decode()
        .strip()
    )


def prepare(work: Path) -> str:
    pods = json.loads(kube("get", "pod", "-o", "json"))["items"]
    assert all(
        not any(o["kind"] == "ReplicaSet" for o in p["metadata"].get("ownerReferences", []))
        for p in pods
    ), "stop API/workers first"
    kube("exec", PG, "--", "mkdir", "-p", "/var/lib/postgresql/data/archive")
    sql("ALTER SYSTEM SET archive_mode = 'on'")
    sql(
        "ALTER SYSTEM SET archive_command = 'test ! -f /var/lib/postgresql/data/archive/%f "
        "&& cp %p /var/lib/postgresql/data/archive/%f'"
    )
    kube("delete", "pod", PG, "--wait=true")
    kube("wait", "--for=create", "pod/" + PG, "--timeout=90s")
    kube("wait", "--for=condition=Ready", "pod/" + PG, "--timeout=90s")
    sql(
        "DROP TABLE IF EXISTS pitr_proof; CREATE TABLE pitr_proof (id integer PRIMARY KEY); "
        "INSERT INTO pitr_proof VALUES (1)"
    )
    kube("exec", PG, "--", "rm", "-rf", "/var/lib/postgresql/data/basebackup")
    kube(
        "exec",
        PG,
        "--",
        "sh",
        "-c",
        (
            'PGPASSWORD="$POSTGRES_PASSWORD" pg_basebackup -h 127.0.0.1 -U evalsi '
            "-D /var/lib/postgresql/data/basebackup -Fp -Xs -c fast"
        ),
    )
    sql("INSERT INTO pitr_proof VALUES (2)")
    target = sql("SELECT clock_timestamp()")
    datetime.datetime.fromisoformat(target)
    (work / "target.txt").write_text(target)
    sql("SELECT pg_sleep(0.1); INSERT INTO pitr_proof VALUES (3)")
    segment = sql("SELECT pg_walfile_name(pg_current_wal_lsn())")
    sql("SELECT pg_switch_wal()")
    for _ in range(100):
        archived = sql("SELECT last_archived_wal FROM pg_stat_archiver")
        if archived >= segment:
            break
        time.sleep(0.2)
    else:
        raise AssertionError("WAL did not archive")
    (work / "basebackup.tgz").write_bytes(
        kube("exec", PG, "--", "tar", "czf", "-", "-C", "/var/lib/postgresql/data/basebackup", ".")
    )
    (work / "wal.tgz").write_bytes(
        kube("exec", PG, "--", "tar", "czf", "-", "-C", "/var/lib/postgresql/data/archive", ".")
    )
    assert sql("SELECT string_agg(id::text, ',' ORDER BY id) FROM pitr_proof") == "1,2,3"
    return target


def recover(work: Path, target: str) -> dict[str, Any]:
    datetime.datetime.fromisoformat(target)
    pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "pitr-recovery", "namespace": NS},
        "spec": {
            "restartPolicy": "Never",
            "securityContext": {"runAsUser": 999, "runAsGroup": 999, "fsGroup": 999},
            "containers": [
                {
                    "name": "postgres",
                    "image": "postgres:17-alpine",
                    "command": ["sh", "-c", "sleep 600"],
                    "volumeMounts": [
                        {"name": "data", "mountPath": "/restore"},
                        {"name": "wal", "mountPath": "/archive"},
                        {"name": "run", "mountPath": "/var/run/postgresql"},
                    ],
                }
            ],
            "volumes": [{"name": n, "emptyDir": {}} for n in ["data", "wal", "run"]],
        },
    }
    kube("create", "-f", "-", data=json.dumps(pod).encode())
    started = time.monotonic()
    try:
        kube("wait", "--for=condition=Ready", "pod/pitr-recovery", "--timeout=90s")
        kube("exec", "pitr-recovery", "--", "mkdir", "-m", "700", "/restore/pgdata")
        kube(
            "exec",
            "-i",
            "pitr-recovery",
            "--",
            "tar",
            "xzf",
            "-",
            "-C",
            "/restore/pgdata",
            data=(work / "basebackup.tgz").read_bytes(),
        )
        kube(
            "exec",
            "-i",
            "pitr-recovery",
            "--",
            "tar",
            "xzf",
            "-",
            "-C",
            "/archive",
            data=(work / "wal.tgz").read_bytes(),
        )
        kube("exec", "pitr-recovery", "--", "touch", "/restore/pgdata/recovery.signal")
        options = (
            "-c archive_mode=off -c restore_command='cp /archive/%f %p' "
            f"-c recovery_target_time='{target}' "
            "-c recovery_target_action=promote -k /var/run/postgresql"
        )
        kube(
            "exec",
            "pitr-recovery",
            "--",
            "pg_ctl",
            "-D",
            "/restore/pgdata",
            "-l",
            "/restore/server.log",
            "-o",
            options,
            "-w",
            "start",
        )
        ids = sql("SELECT string_agg(id::text, ',' ORDER BY id) FROM pitr_proof", "pitr-recovery")
        assert ids == "1,2", ids
        assert sql("SELECT pg_is_in_recovery()", "pitr-recovery") == "f"
        return {
            "postgres": "17-alpine",
            "source_rows": [1, 2, 3],
            "recovered_rows": [1, 2],
            "after_target_commit_excluded": True,
            "promoted": True,
            "recovery_elapsed_s": round(time.monotonic() - started, 3),
            "archive": "local fixture PVC copied off-pod for restore",
            "production_pitr_certified": False,
        }
    finally:
        log = subprocess.run(
            ["kubectl", "-n", NS, "exec", "pitr-recovery", "--", "cat", "/restore/server.log"],
            capture_output=True,
            check=False,
        )
        (work / "recovery.log").write_bytes(log.stdout)
        kube("delete", "pod", "pitr-recovery", "--wait=true")


if __name__ == "__main__":
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--recover-only", action="store_true")
    args = parser.parse_args()
    args.work_dir.mkdir(parents=True, exist_ok=True)
    target = (
        (args.work_dir / "target.txt").read_text() if args.recover_only else prepare(args.work_dir)
    )
    result = recover(args.work_dir, target)
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))

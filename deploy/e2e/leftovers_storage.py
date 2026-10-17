"""Live two-replica pipeline, quiesced backup, and isolated restore checks.

Uses only explicitly named evalsi-leftovers namespaces and synthetic records.
Backup files can contain application state: keep the work directory private.
Kubernetes port forwards must expose API 18080/18083 and MinIO 19000 first.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import json
import os
import secrets
import subprocess
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import boto3
from botocore.config import Config

from evalsi.client import Client, ServerError


def kube(ns: str, *args: str, data: bytes | None = None) -> bytes:
    assert ns in {"evalsi-leftovers", "evalsi-leftovers-restore"}, ns
    return subprocess.check_output(["kubectl", "-n", ns, *args], input=data)


def token(ns: str, name: str) -> str:
    return kube(ns, "create", "token", name, "--audience=evals.si").decode().strip()


def s3(ns: str, release: str) -> Any:
    secret = json.loads(kube(ns, "get", "secret", f"{release}-dev-minio", "-o", "json"))["data"]
    return boto3.client(
        "s3",
        endpoint_url="http://localhost:19000",
        aws_access_key_id=base64.b64decode(secret["access_key"]).decode(),
        aws_secret_access_key=base64.b64decode(secret["secret_key"]).decode(),
        region_name="us-east-1",
        config=Config(s3={"addressing_style": "path"}),
    )


def expect_error(call: Callable[[], Any], code: str) -> None:
    try:
        call()
    except ServerError as exc:
        if exc.code != code:
            raise
    else:
        raise AssertionError(f"expected {code}, operation succeeded")


def wait_completed(client: Client, run_id: str) -> dict[str, Any]:
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        run = client.get_run(run_id)
        if run["status"] in {"RUN_STATUS_SUCCEEDED", "RUN_STATUS_FAILED", "RUN_STATUS_ERROR"}:
            return run
        time.sleep(0.5)
    raise AssertionError("run did not finish")


def upload_fixture(work: Path, credential: str) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    ns = "evalsi-leftovers"
    urls = ["http://localhost:18080"]
    source = work / "records.jsonl"
    rows = [
        {"id": str(i), "output": {"text": "yes"}, "reference": {"text": "yes"}} for i in range(16)
    ]
    source.write_text("".join(json.dumps(r) + "\n" for r in rows))
    with Client(urls[0], token=credential) as c:
        uploaded = c.upload_dataset(str(source), name="restore-proof", project="default")
    assert uploaded["path"].startswith("uploaded/default/restore-proof/"), uploaded
    stored = (
        s3(ns, "leftovers")
        .get_object(Bucket="evalsi", Key="datasets/" + uploaded["path"])["Body"]
        .read()
    )
    assert hashlib.sha256(stored).hexdigest() == uploaded["sha256"]
    return uploaded, rows


def pipeline(work: Path) -> dict[str, Any]:
    credential = token("evalsi-leftovers", "leftovers-operator")
    prefix = "leftovers-" + secrets.token_hex(4)
    urls = ["http://localhost:18080", "http://localhost:18083"]
    uploaded, rows = upload_fixture(work, credential)
    spec = {"dataset": {"path": uploaded["path"]}, "evaluators": [{"ref": "exact-match"}]}

    def create(i: int) -> str:
        with Client(urls[i % 2], token=credential) as c:
            for _ in range(100):
                try:
                    return c.create_run(
                        spec, project="default", idempotency_key=prefix + "-create"
                    )["id"]
                except ServerError as exc:
                    if exc.code != "unavailable":
                        raise
                    time.sleep(0.1)
            raise AssertionError("idempotency claim never resolved")

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as threads:
        ids = list(threads.map(create, range(16)))
    assert len(set(ids)) == 1, ids
    run_id = ids[0]
    with Client(urls[0], token=credential) as c:
        run = wait_completed(c, run_id)
        assert run["status"] == "RUN_STATUS_SUCCEEDED", run
        results = c.list_run_results(run_id)["results"]
        assert len(results) == len(rows), results
        assert len({r["recordId"] for r in results}) == len(rows)
        assert (
            c.create_run(spec, project="default", idempotency_key=prefix + "-create")["id"]
            == run_id
        )
        conflict = {**spec, "trials": 2}
        expect_error(
            lambda: c.create_run(conflict, project="default", idempotency_key=prefix + "-create"),
            "already_exists",
        )
        archived = c.create_run(spec, project="default", idempotency_key=prefix + "-archive")
        for _ in c.watch_run(archived["id"]):
            pass
        c.archive_run(archived["id"])
        expect_error(lambda: c.list_run_results(archived["id"]), "failed_precondition")
        assert c.get_run(archived["id"]).get("summaries")
        deleted = c.create_run(spec, project="default", idempotency_key=prefix + "-delete-create")
        for _ in c.watch_run(deleted["id"]):
            pass

    def delete(i: int) -> None:
        with Client(urls[i % 2], token=credential) as c:
            for _ in range(100):
                try:
                    c.delete_run(deleted["id"], idempotency_key=prefix + "-delete")
                    return
                except ServerError as exc:
                    if exc.code != "unavailable":
                        raise
                    time.sleep(0.1)
            raise AssertionError("delete claim never resolved")

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as threads:
        list(threads.map(delete, range(16)))
    with Client(urls[1], token=credential) as c:
        expect_error(lambda: c.get_run(deleted["id"]), "not_found")
    state = {
        "run_id": run_id,
        "dataset": uploaded["path"],
        "results": len(results),
        "archive_id": archived["id"],
        "deleted_id": deleted["id"],
    }
    (work / "state.json").write_text(json.dumps(state, indent=2) + "\n")
    report = {
        "pipeline": "S3-compatible MinIO, PostgreSQL, NATS, two API replicas",
        "records": len(results),
        "create_requests": len(ids),
        "unique_runs": 1,
        "delete_requests": 16,
        "archive": "summary retained, results removed",
    }
    print("PASS " + json.dumps(report), flush=True)
    return report


def backup(work: Path) -> dict[str, Any]:
    ns = "evalsi-leftovers"
    deployments = json.loads(kube(ns, "get", "deployment", "-o", "json"))["items"]
    if not (work / "deployments.json").exists():
        (work / "deployments.json").write_text(json.dumps(deployments))
    for dep in deployments:
        kube(ns, "scale", "deployment/" + dep["metadata"]["name"], "--replicas=0")
    for dep in deployments:
        kube(ns, "rollout", "status", "deployment/" + dep["metadata"]["name"], "--timeout=90s")
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        pods = json.loads(kube(ns, "get", "pod", "-o", "json"))["items"]
        if all(
            not any(o["kind"] == "ReplicaSet" for o in p["metadata"].get("ownerReferences", []))
            for p in pods
        ):
            break
        time.sleep(0.5)
    else:
        raise AssertionError("writers remain")
    dump = kube(
        ns,
        "exec",
        "leftovers-dev-postgres-0",
        "--",
        "pg_dump",
        "-U",
        "evalsi",
        "-d",
        "evalsi",
        "--format=custom",
        "--no-owner",
    )
    assert len(dump) > 1000
    (work / "postgres.dump").write_bytes(dump)
    store = s3(ns, "leftovers")
    manifest = {}
    for page in store.get_paginator("list_objects_v2").paginate(Bucket="evalsi"):
        for obj in page.get("Contents", []):
            key = obj["Key"]
            path = work / "objects" / key
            assert path.resolve().is_relative_to((work / "objects").resolve())
            path.parent.mkdir(parents=True, exist_ok=True)
            data = store.get_object(Bucket="evalsi", Key=key)["Body"].read()
            path.write_bytes(data)
            manifest[key] = hashlib.sha256(data).hexdigest()
    assert manifest, "empty dataset backup"
    (work / "objects.json").write_text(json.dumps(manifest, indent=2))
    report = {
        "postgres_sha256": hashlib.sha256(dump).hexdigest(),
        "objects": len(manifest),
        "quiesced": True,
        "scope": "SQL and dataset bucket; no pending jobs or ClickHouse in this fixture",
    }
    print("PASS backup " + json.dumps(report), flush=True)
    return report


def restore(work: Path) -> dict[str, Any]:
    ns = "evalsi-leftovers-restore"
    # Fail closed before touching a database: this must be a new empty instance.
    tables = (
        kube(
            ns,
            "exec",
            "restored-dev-postgres-0",
            "--",
            "psql",
            "-U",
            "evalsi",
            "-d",
            "evalsi",
            "-Atc",
            "SELECT count(*) FROM pg_tables WHERE schemaname='public'",
        )
        .decode()
        .strip()
    )
    assert tables == "0", f"refusing nonempty restore database ({tables} tables)"
    store = s3(ns, "restored")
    assert store.list_objects_v2(Bucket="evalsi").get("KeyCount", 0) == 0
    kube(
        ns,
        "exec",
        "-i",
        "restored-dev-postgres-0",
        "--",
        "pg_restore",
        "-U",
        "evalsi",
        "-d",
        "evalsi",
        "--no-owner",
        "--exit-on-error",
        data=(work / "postgres.dump").read_bytes(),
    )
    manifest = json.loads((work / "objects.json").read_text())
    for key, digest in manifest.items():
        data = (work / "objects" / key).read_bytes()
        assert hashlib.sha256(data).hexdigest() == digest
        store.put_object(Bucket="evalsi", Key=key, Body=data)
        restored = store.get_object(Bucket="evalsi", Key=key)["Body"].read()
        assert hashlib.sha256(restored).hexdigest() == digest
    print("PASS: restored SQL into empty database and verified every object checksum", flush=True)
    return {"objects_restored": len(manifest), "empty_database_guard": True}


def check_restored(work: Path) -> dict[str, Any]:
    state = json.loads((work / "state.json").read_text())
    credential = token("evalsi-leftovers-restore", "verify")
    with Client("http://localhost:18080", token=credential) as c:
        old = c.get_run(state["run_id"])
        assert old["status"] == "RUN_STATUS_SUCCEEDED"
        assert len(c.list_run_results(state["run_id"])["results"]) == state["results"]
        assert c.get_run(state["archive_id"]).get("summaries")
        expect_error(lambda: c.list_run_results(state["archive_id"]), "failed_precondition")
        new = c.create_run(
            {"dataset": {"path": state["dataset"]}, "evaluators": [{"ref": "exact-match"}]},
            project="default",
            idempotency_key="restored-rerun",
        )
        for _ in c.watch_run(new["id"]):
            pass
        assert c.get_run(new["id"])["status"] == "RUN_STATUS_SUCCEEDED"
        assert len(c.list_run_results(new["id"])["results"]) == state["results"]
    report = {
        "authenticated_completed_run": True,
        "archived_summary": True,
        "restored_dataset_rerun_records": state["results"],
    }
    print("PASS restore verification " + json.dumps(report), flush=True)
    return report


def main() -> None:
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["pipeline", "backup", "restore", "check-restored"])
    parser.add_argument("--work-dir", type=Path, required=True)
    args = parser.parse_args()
    args.work_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    report = {
        "pipeline": pipeline,
        "backup": backup,
        "restore": restore,
        "check-restored": check_restored,
    }[args.mode](args.work_dir)
    (args.work_dir / (args.mode + ".json")).write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()

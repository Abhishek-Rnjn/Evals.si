"""Client.evaluate() against a real evalsid: the same results as in-process evaluate().

The server is the repository's own evalsid with a worker running this
checkout's evaluators. Set EVALSI_E2E_EVALSID to a built binary, or have `go`
on PATH to build one. Without either the tests skip, unless
EVALSI_REQUIRE_EVALSID is set (CI sets it).
"""

from __future__ import annotations

import asyncio
import json
import os
import shutil
import socket
import subprocess
import sys
import time
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import httpx
import pytest

import evalsi
from evalsi.client import Client, ServerError
from evalsi.evaluator import EvaluatorConfigError

ROOT = Path(__file__).resolve().parents[3]

DATA = [
    {"id": "capital-fr", "output": "Paris", "reference": "Paris"},
    {"id": "capital-jp", "output": "Kyoto", "reference": "Tokyo"},
    {"id": "capital-au", "output": "canberra", "reference": "Canberra"},
    {"id": "apples", "output": "3 x 12 = 36, minus 4 leaves 32 apples.", "reference": "32"},
    {"id": "train", "output": "The average speed is 80 km/h.", "reference": "80"},
    {"id": "budget", "output": "That comes to $14,000 per year.", "reference": "14400"},
]
EVALUATORS = ["exact-match", "numeric-match", "fuzzy-match"]


def _skip(reason: str) -> None:
    if os.environ.get("EVALSI_REQUIRE_EVALSID"):
        pytest.fail(reason)
    pytest.skip(reason)


@pytest.fixture(scope="module")
def server_datasets(tmp_path_factory: pytest.TempPathFactory) -> Path:
    return tmp_path_factory.mktemp("server-datasets")


@pytest.fixture(scope="module")
def server(tmp_path_factory: pytest.TempPathFactory, server_datasets: Path) -> Iterator[str]:
    work = tmp_path_factory.mktemp("evalsid")
    binary = os.environ.get("EVALSI_E2E_EVALSID", "")
    if not binary:
        if shutil.which("go") is None:
            _skip("no evalsid: set EVALSI_E2E_EVALSID or install Go")
        binary = str(work / "evalsid")
        build = subprocess.run(
            ["go", "build", "-o", binary, "./cmd/evalsid"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        if build.returncode != 0:
            pytest.fail(f"go build: {build.stderr}")
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    config = work / "evalsi.yaml"
    config.write_text(
        json.dumps(
            {
                "listen": f"127.0.0.1:{port}",
                "data_dir": str(work / "data"),
                "datasets_dir": str(server_datasets),
                "worker": {"command": [sys.executable, "-m", "evalsi"], "no_cache": True},
            }
        )
    )
    log = (work / "evalsid.log").open("w")
    proc = subprocess.Popen(
        [binary, "serve", "--config", str(config)], stdout=log, stderr=subprocess.STDOUT
    )
    base = f"http://127.0.0.1:{port}"
    try:
        deadline = time.monotonic() + 90
        while True:
            if proc.poll() is not None:
                pytest.fail(f"evalsid exited:\n{(work / 'evalsid.log').read_text()}")
            try:
                if httpx.get(f"{base}/healthz", timeout=1).status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            if time.monotonic() > deadline:
                pytest.fail(f"evalsid did not start:\n{(work / 'evalsid.log').read_text()}")
            time.sleep(0.2)
        yield base
    finally:
        proc.terminate()
        proc.wait(timeout=30)
        log.close()


def _comparable(result: evalsi.EvalResult) -> tuple[list[Any], list[Any]]:
    """What must be identical: every score and every summary, intervals included."""
    results = sorted(
        (
            r.record_id,
            r.evaluator,
            r.outcome.value,
            [(s.name, s.number, s.passed, s.label) for s in r.scores],
        )
        for r in result.results
    )
    summaries = sorted(
        (
            s.metric,
            s.evaluator,
            s.kind,
            s.n,
            s.mean,
            s.std,
            (s.ci.low, s.ci.high, s.ci.level, s.ci.method) if s.ci else None,
            s.skipped,
            s.errors,
            s.labels,
            s.clusters,
        )
        for s in result.summaries
    )
    return results, summaries


def _assert_same(remote: evalsi.EvalResult, local: evalsi.EvalResult) -> None:
    (r_results, r_summaries), (l_results, l_summaries) = _comparable(remote), _comparable(local)
    assert r_results == l_results
    assert len(r_summaries) == len(l_summaries)
    for got, want in zip(r_summaries, l_summaries, strict=True):
        assert got[:4] == want[:4]
        for a, b in zip(got[4:7], want[4:7], strict=True):
            if isinstance(a, tuple) and isinstance(b, tuple):
                assert a[2:] == b[2:]
                assert a[:2] == pytest.approx(b[:2], abs=1e-9)
            else:
                assert a == pytest.approx(b, abs=1e-9)
        assert got[7:] == want[7:]


def test_evaluate_matches_in_process(server: str) -> None:
    local = evalsi.evaluate(DATA, EVALUATORS)
    with Client(server) as client:
        remote = client.evaluate(DATA, EVALUATORS)
    assert isinstance(remote, evalsi.EvalResult)
    assert all(isinstance(r, evalsi.EvaluationResult) for r in remote.results)
    _assert_same(remote, local)
    assert remote.metric("exact-match").ci is not None
    assert remote.manifest["dataset"]["sha256"] == local.manifest["dataset"]["sha256"]


def test_upload_retry_run_and_delete(server: str, tmp_path: Path) -> None:
    source = tmp_path / "pipeline.jsonl"
    source.write_text(json.dumps({"id": "one", "output": "Paris", "reference": "Paris"}) + "\n")
    with Client(server) as client:
        uploaded = client.upload_dataset(str(source), name="pipeline", project="default")
        assert int(uploaded["records"]) == 1
        spec = {"dataset": {"path": uploaded["path"]}, "evaluators": [{"ref": "exact-match"}]}
        first = client.create_run(spec, project="default", idempotency_key="pipeline-create")
        retry = client.create_run(spec, project="default", idempotency_key="pipeline-create")
        assert first["id"] == retry["id"]
        for _ in client.watch_run(first["id"]):
            pass
        finished = client.get_run(first["id"])
        assert finished["status"] == "RUN_STATUS_SUCCEEDED"
        assert len(client.list_run_results(first["id"])["results"]) == 1
        client.delete_run(first["id"])
        with pytest.raises(ServerError, match="not_found"):
            client.get_run(first["id"])


def test_snapshot_run_verifies_pin_and_confines_the_importer(
    server: str,
    server_datasets: Path,
    tmp_path: Path,
) -> None:
    from evalsi.dataset_snapshots import IDENTITY_KEY, create_snapshot

    source = tmp_path / "snapshot-source.jsonl"
    source.write_text(
        json.dumps(
            {
                "id": "one",
                "output": "Paris",
                "reference": "Paris",
                "provenance": {"trace_id": "approved-fixture"},
            }
        )
        + "\n"
    )
    folder = server_datasets / "uploaded/default/support"
    folder.mkdir(parents=True)
    artifact = folder / "v1.snapshot.json"
    snapshot = create_snapshot(source, artifact, name="support", version="v1", owner="test-team")
    relative = artifact.relative_to(server_datasets).as_posix()
    spec = {
        "dataset": {"uri": f"snapshot://{relative}?sha256={snapshot.sha256}"},
        "evaluators": [{"ref": "exact-match"}],
        "gates": [{"metric": "exact-match", "min": 1}],
    }
    with Client(server) as client:
        created = client.create_run(spec, project="default")
        for _ in client.watch_run(created["id"]):
            pass
        assert client.get_run(created["id"])["status"] == "RUN_STATUS_SUCCEEDED"
        page = client.list_run_results(created["id"])
        identity = page["records"][0]["metadata"][IDENTITY_KEY]
        assert identity["sha256"] == snapshot.sha256
        assert identity["provenance"]["trace_id"] == "approved-fixture"
        # A valid artifact outside datasets_dir still cannot bypass path confinement.
        outside = server_datasets.parent / "outside-snapshot.json"
        shutil.copyfile(artifact, outside)
        escaped = {
            **spec,
            "dataset": {"uri": f"snapshot://../{outside.name}?sha256={snapshot.sha256}"},
        }
        with pytest.raises(ServerError, match=r"outside|inside"):
            client.create_run(escaped, project="default")
        artifact.write_text(artifact.read_text().replace(snapshot.sha256, "0" * 64))
        with pytest.raises(ServerError, match="SHA-256 mismatch"):
            client.create_run(spec, project="default")


def test_evaluate_options_match_in_process(server: str) -> None:
    data = [{**row, "metadata": {"group": "a" if i % 2 else "b"}} for i, row in enumerate(DATA)]
    local = evalsi.evaluate(data, ["exact-match"], confidence=0.9, cluster_by="group")
    with Client(server) as client:
        remote = client.evaluate(data, ["exact-match"], confidence=0.9, cluster_by="group")
    _assert_same(remote, local)
    assert remote.metric("exact-match").clusters == 2


def test_evaluate_params_and_alias_match_in_process(server: str) -> None:
    spec = [{"ref": "length", "name": "chars", "params": {"unit": "chars"}}]
    local = evalsi.evaluate(DATA, spec)
    with Client(server) as client:
        remote = client.evaluate(DATA, spec)
    _assert_same(remote, local)
    assert {r.evaluator for r in remote.results} == {"chars"}


def test_evaluate_async_matches_in_process(server: str) -> None:
    local = evalsi.evaluate(DATA, EVALUATORS)

    async def go() -> evalsi.EvalResult:
        with Client(server) as client:
            return await client.evaluate_async(DATA, EVALUATORS)

    _assert_same(asyncio.run(go()), local)


def test_evaluate_stream_matches_in_process(server: str) -> None:
    pytest.importorskip("grpc")
    local = evalsi.evaluate(DATA, EVALUATORS)
    with Client(server) as client:
        stream = client.evaluate_stream(DATA, EVALUATORS)
        seen = list(stream)
        assert len(seen) == len(DATA) * len(EVALUATORS)
        assert stream.summaries
        remote = stream.collect()
    _assert_same(remote, local)


def test_evaluate_server_errors_are_raised(server: str) -> None:
    with Client(server) as client, pytest.raises(ServerError):
        client.evaluate(DATA, ["no-such-evaluator"])


def test_evaluate_needs_an_evaluator_the_server_has() -> None:
    @evalsi.evaluator(name="acme/local-only", version="1.0.0")
    def local_only(record: evalsi.Record) -> evalsi.Score:
        return evalsi.Score(number=1.0)

    with Client("http://unused") as client, pytest.raises(EvaluatorConfigError):
        client.evaluate(DATA, [local_only])  # type: ignore[list-item]


def test_real_incident_review_snapshot_and_gate_controls(server: str, tmp_path: Path) -> None:
    from evalsi.dataset_snapshots import IDENTITY_KEY, read_snapshot
    from evalsi.datasets import load_records

    root = ROOT
    artifact = tmp_path / "approved.evalsi.json"
    completed = subprocess.run(
        [
            sys.executable,
            str(root / "examples/failure-inbox/demo.py"),
            "--server",
            server,
            "--output",
            str(artifact),
        ],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    assert completed.returncode == 0, completed.stdout + completed.stderr
    result = json.loads(completed.stdout)
    assert result["observed_gate_exit"] == 3
    assert result["candidate_gate_exit"] == 0
    assert result["synthetic"] is True
    approved = read_snapshot(artifact, expected=result["snapshot_sha256"])
    records = load_records(f"snapshot://{artifact}?sha256={approved.sha256}")
    provenance = records[0].metadata[IDENTITY_KEY]["provenance"]["failure"]
    assert provenance["case_id"] == result["case_id"]
    assert provenance["revision"] == str(result["revision"])
    assert provenance["reviewed_by"] == "anonymous"
    assert "synthetic-private-217" not in artifact.read_text()
    assert artifact.stat().st_mode & 0o777 == 0o600
    with Client(server) as client:
        failure = client.get_failure(result["case_id"])
        assert failure["status"] == "FAILURE_STATUS_APPROVED"
        assert len(client.failure_events(result["case_id"])["events"]) == 3
        assert "record" not in client.list_failures(status="FAILURE_STATUS_APPROVED")["failures"][0]


def test_shared_dataset_incident_to_candidate_end_to_end(server: str, tmp_path: Path) -> None:
    """Real trace intake, reviews, CLI download and worker calls; no model credentials."""
    destination = tmp_path / "shared.json"
    run = subprocess.run(
        [
            sys.executable,
            str(ROOT / "examples/dataset-catalog/demo.py"),
            "--server",
            server,
            "--output",
            str(destination),
        ],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert run.returncode == 0, run.stderr
    report = json.loads(run.stdout)
    assert report["observed_status"] == "RUN_STATUS_FAILED"
    assert report["candidate_status"] == "RUN_STATUS_SUCCEEDED"
    assert all(report[k] for k in ("draft_rejected", "wrong_pin_rejected", "retired_rejected"))
    from evalsi.dataset_snapshots import read_snapshot

    snapshot = read_snapshot(destination)
    assert snapshot.records[0]["provenance"]["catalog"]["sha256"] == report["sha256"]
    assert snapshot.records[0]["provenance"]["record_source"]["trace"]["traceId"]
    assert "synthetic-private-528" not in destination.read_text()


def test_production_failure_to_release_decision_end_to_end(server: str, tmp_path: Path) -> None:
    run = subprocess.run(
        [
            sys.executable,
            str(ROOT / "examples/release-decisions/demo.py"),
            "--server",
            server,
            "--output-dir",
            str(tmp_path / "reports"),
        ],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert run.returncode == 0, run.stderr
    report = json.loads(run.stdout)
    assert len(report["failure_cases"]) == 2
    assert report["controls"]["passed"]["exit_code"] == 0
    assert report["controls"]["passed"]["action_exit_code"] == 0
    assert report["controls"]["blocked"]["exit_code"] == 3
    assert report["controls"]["blocked"]["action_exit_code"] == 3
    assert report["controls"]["insufficient"]["exit_code"] == 1
    assert report["controls"]["insufficient"]["action_exit_code"] == 1
    assert report["tamper_rejected"]
    assert report["actual_agent_calls"] >= 8
    assert "synthetic-private-528" not in (tmp_path / "reports/passed.release.json").read_text()

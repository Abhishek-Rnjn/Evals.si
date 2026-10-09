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
def server(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
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

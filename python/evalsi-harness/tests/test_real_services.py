"""A Terminal-Bench style multi-service task on a real bubblewrap sandbox (H4).

The task directory is imported from a Compose file (a one-shot seeding service, an HTTP
service that serves what it wrote, and the agent's container on the same network), then
played by a scripted CLI agent. Needs a built evalsid (``EVALSI_TEST_EVALSID``), user
namespaces and ``nsenter``; skips where the host cannot give a private network, unless
``EVALSI_REQUIRE_SANDBOX`` is set.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path
from typing import Any

import pytest
import yaml

from evalsi.sandbox.client import connect
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.benchmarks.harbor import load_terminal_bench
from tests.harness_support import spec_of
from tests.test_compose import CLIENT, task

pytestmark = pytest.mark.skipif(
    not os.environ.get("EVALSI_TEST_EVALSID"), reason="needs a built evalsid"
)

COMPOSE = {
    "services": {
        "client": {**CLIENT, "volumes": [*CLIENT["volumes"], "shared:/app"], "depends_on": ["api"]},
        "seed": {
            "image": "unused",
            "entrypoint": ["sh", "-c", "mkdir -p /srv && echo forty-two > /srv/answer.txt"],
            "volumes": ["site:/srv"],
        },
        "api": {
            "image": "unused",
            "entrypoint": ["python3", "-u", "-m", "http.server", "8000", "--directory", "/srv"],
            "ports": ["8000"],
            "volumes": ["site:/srv"],
            "depends_on": {"seed": {"condition": "service_completed_successfully"}},
        },
    },
    "volumes": {"site": None, "shared": None},
}
FETCH = (
    'python3 -c "import urllib.request; '
    "print(urllib.request.urlopen('http://api:8000/answer.txt', timeout=5).read().decode())\""
)


async def _play(agent: str, tmp_path: Path) -> TaskOutcome:
    d = task(tmp_path, COMPOSE)
    [row] = load_terminal_bench(str(d))
    env = row["metadata"]["environment"]
    # The host's own interpreter and tools stand in for images, which need a registry.
    env["image"] = ""
    for s in env["services"]:
        s["image"] = ""
    env["setup"] = []
    env["checker"] = {"command": ["sh", "-c", "grep -q forty-two got.txt"]}
    env["sandbox"]["min_isolation"] = "namespaced"
    spec = spec_of({"target": {"agent": {"cli": {"command": ["sh", "-c", agent]}}}})
    from evalsi.types import Content, Record

    rec = Record(id="multi", input=Content(text="fetch the answer"), metadata={"environment": env})
    async with await connect() as client:

        async def get() -> Any:
            return client

        harness = load_harness(spec, HarnessContext(sandboxes=get, base_dir=Path.cwd()))
        try:
            return await run_task(Task.build(spec, rec, run_id="run1"), harness)
        finally:
            await harness.aclose()


def play(agent: str, tmp_path: Path) -> TaskOutcome:
    out = asyncio.run(_play(agent, tmp_path))
    if out.record is None and any(
        s in out.error for s in ("nsenter", "private network", "bubblewrap")
    ):
        if os.environ.get("EVALSI_REQUIRE_SANDBOX"):
            pytest.fail(out.error)
        pytest.skip(out.error)
    return out


@pytest.fixture
def pinned(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    monkeypatch.setenv("EVALSID", os.environ["EVALSI_TEST_EVALSID"])
    monkeypatch.delenv("EVALSI_SANDBOX_ADDR", raising=False)
    path = tmp_path.parent / f"{tmp_path.name}-bwrap.yaml"
    path.write_text(
        yaml.safe_dump({"sandbox": {"ladder": ["bwrap"], "min_isolation": "namespaced"}})
    )
    monkeypatch.setenv("EVALSI_SANDBOX_CONFIG", str(path))
    return path


def test_the_agent_reads_what_one_service_wrote_through_another(
    pinned: Path, tmp_path: Path
) -> None:
    out = play(f"{FETCH} > got.txt", tmp_path)
    assert out.error == "", out.error
    assert out.record is not None
    assert out.record.check is not None
    assert out.record.check.passed
    assert out.record.metadata["isolation"]["driver"] == "bwrap"
    assert any("services" in n for n in out.record.metadata["isolation"]["notes"])


def test_an_agent_that_does_not_ask_the_service_fails_the_check(
    pinned: Path, tmp_path: Path
) -> None:
    out = play("echo nothing > got.txt", tmp_path)
    assert out.error == "", out.error
    assert out.record is not None
    assert out.record.check is not None
    assert not out.record.check.passed


def test_the_services_network_has_no_way_out(pinned: Path, tmp_path: Path) -> None:
    out = play(
        "python3 -c \"import socket; socket.create_connection(('1.1.1.1', 53), timeout=2)\" "
        "&& echo forty-two > got.txt || echo blocked > got.txt",
        tmp_path,
    )
    assert out.error == "", out.error
    assert out.record is not None
    assert out.record.check is not None
    assert not out.record.check.passed  # it reached nothing, so it wrote "blocked"

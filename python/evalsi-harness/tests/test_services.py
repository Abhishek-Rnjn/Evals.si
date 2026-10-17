"""Service tasks are fresh per trial and fail closed on unsupported backends."""

import asyncio
from dataclasses import asdict
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock

import pytest
import yaml

from evalsi.sandbox.client import SandboxSpec, ServiceSpec
from evalsi_harness.benchmarks import fixtures
from evalsi_harness.benchmarks.harbor import TaskFormatError, load_terminal_bench
from evalsi_harness.environment import EnvironmentPool
from evalsi_harness.task import EnvironmentConfig, TaskError, environment_for
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, spec_of


def service() -> ServiceSpec:
    return ServiceSpec(
        name="db",
        image="redis:7-alpine",
        command=["redis-server"],
        port=6379,
        memory_mb=64,
        cpu_millis=100,
    )


def test_service_schema_roundtrip_and_record_override() -> None:
    raw = asdict(service())
    spec = spec_of({"environment": {"services": [raw], "service_lifetime": "60s"}})
    env = environment_for(spec, record())
    assert env is not None
    assert env.services == [service()]
    assert env.service_lifetime_s == 60
    replaced = environment_for(
        spec,
        record(
            metadata={
                "environment": {
                    "services": [{**raw, "name": "other"}],
                    "serviceLifetime": "120s",
                }
            }
        ),
    )
    assert replaced is not None
    assert replaced.services[0].name == "other"
    assert replaced.key() != env.key()
    pb = SandboxSpec.from_dict({"services": [raw], "service_lifetime_s": 60}).to_proto()
    assert pb.services[0].name == "db"
    assert pb.service_lifetime.seconds == 60
    with pytest.raises(TaskError, match="invalid environment services"):
        environment_for(spec, record(metadata={"environment": {"services": [{"typo": 1}]}}))


def test_services_never_reuse_setup_or_snapshots() -> None:
    async def go() -> None:
        client = AsyncMock()
        first, second = AsyncMock(), AsyncMock()
        first.exec.return_value.ok = second.exec.return_value.ok = True
        client.create.side_effect = [first, second]
        pool = EnvironmentPool(client)
        cfg = EnvironmentConfig(services=[service()], service_lifetime_s=60, setup=["seed-db"])
        assert (await pool.acquire(cfg)).sandbox is first
        assert (await pool.acquire(cfg)).sandbox is second
        assert client.create.await_count == 2
        first.exec.assert_awaited_once()
        second.exec.assert_awaited_once()
        first.snapshot.assert_not_called()
        client.restore.assert_not_called()
        for call in client.create.call_args_list:
            assert call.args[0].services == [service()]
        await pool.aclose()
        client.delete_snapshot.assert_not_called()

    asyncio.run(go())


def test_service_setup_cancellation_cleans_up() -> None:
    async def go() -> None:
        client = AsyncMock()
        sb = client.create.return_value
        sb.exec.side_effect = asyncio.CancelledError()
        pool = EnvironmentPool(client)
        cfg = EnvironmentConfig(services=[service()], service_lifetime_s=60, setup=["seed"])
        with pytest.raises(asyncio.CancelledError):
            await pool.acquire(cfg)
        sb.destroy.assert_awaited_once()

    asyncio.run(go())


def test_services_refuse_unsupported_backend_and_wider_setup_network() -> None:
    async def go() -> None:
        pool = EnvironmentPool(LocalSandboxClient())
        cfg = EnvironmentConfig(services=[service()], service_lifetime_s=60)
        with pytest.raises(TaskError, match="services require the pod backend"):
            await pool.acquire(cfg)
        cfg.setup_network = "allow"
        with pytest.raises(TaskError, match="narrower than setup_network"):
            await pool.acquire(cfg)

    asyncio.run(go())


def compose_fixture(tmp_path: Path) -> tuple[Path, dict[str, Any]]:
    fixtures.write(tmp_path)
    task = tmp_path / "terminal-bench" / "hello"
    db = {
        "image": "redis:7-alpine",
        "entrypoint": ["redis-server"],
        "x-evalsi": {"port": 6379, "memory_mb": 64, "cpu_millis": 100},
    }
    return task, {"services": {"client": {"build": "."}, "db": db}}


def test_multi_service_terminal_bench_import(tmp_path: Path) -> None:
    task, doc = compose_fixture(tmp_path)
    (task / "docker-compose.yaml").write_text(yaml.safe_dump(doc))
    [row] = load_terminal_bench(str(task))
    env = row["metadata"]["environment"]
    assert env["services"] == [asdict(service())]
    assert env["service_lifetime"] == "3600s"


def test_terminal_bench_service_lifetime_outlasts_task_limits(tmp_path: Path) -> None:
    task, doc = compose_fixture(tmp_path)
    (task / "docker-compose.yaml").write_text(yaml.safe_dump(doc))
    config = yaml.safe_load((task / "task.yaml").read_text())
    config.update(max_agent_timeout_sec=3600, max_test_timeout_sec=600)
    (task / "task.yaml").write_text(yaml.safe_dump(config))
    [row] = load_terminal_bench(str(task))
    assert row["metadata"]["environment"]["service_lifetime"] == "6000s"
    config["max_agent_timeout_sec"] = 200000
    (task / "task.yaml").write_text(yaml.safe_dump(config))
    [row] = load_terminal_bench(str(task))
    assert row["metadata"]["environment"]["service_lifetime"] == "86400s"


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("privileged", True),
        ("volumes", ["/:/host"]),
        ("network_mode", "host"),
        ("ports", ["6379:6379"]),
        ("build", "."),
    ],
)
def test_compose_does_not_drop_unsupported_options(tmp_path: Path, field: str, value: Any) -> None:
    task, doc = compose_fixture(tmp_path)
    doc["services"]["db"][field] = value
    (task / "docker-compose.yaml").write_text(yaml.safe_dump(doc))
    with pytest.raises(TaskFormatError, match="unsupported Compose fields"):
        load_terminal_bench(str(task))


def test_old_sandbox_server_cannot_silently_ignore_services() -> None:
    from evalsi.sandbox.client import SandboxClient, SandboxUnavailable
    from evalsi.sandbox.v1alpha1 import sandbox_pb2

    async def go() -> None:
        async with SandboxClient("127.0.0.1:1") as client:
            client.stub = AsyncMock()
            client.stub.Probe.return_value = sandbox_pb2.ProbeResponse()
            with pytest.raises(SandboxUnavailable, match="upgrade"):
                await client.create(SandboxSpec(services=[service()], service_lifetime_s=60))
            client.stub.Create.assert_not_called()

    asyncio.run(go())


def test_compose_literal_dollar_and_interpolation(tmp_path: Path) -> None:
    task, doc = compose_fixture(tmp_path)
    doc["services"]["db"]["command"] = ["--literal=$-1"]
    compose = task / "docker-compose.yaml"
    compose.write_text(yaml.safe_dump(doc))
    [row] = load_terminal_bench(str(task))
    assert row["metadata"]["environment"]["services"][0]["command"][-1] == "--literal=$-1"
    doc["services"]["db"]["command"] = ["$DATA"]
    compose.write_text(yaml.safe_dump(doc))
    with pytest.raises(TaskFormatError, match="interpolation"):
        load_terminal_bench(str(task))

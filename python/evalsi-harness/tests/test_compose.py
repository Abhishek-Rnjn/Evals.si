"""The Compose importer: what Terminal-Bench 1 tasks use, and what is refused (H4)."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
import yaml

from evalsi.sandbox.client import ServiceSpec
from evalsi_harness.benchmarks.compose import interpolate, split_installs
from evalsi_harness.benchmarks.dockerfile import TaskFormatError
from evalsi_harness.benchmarks.harbor import load_terminal_bench

CLIENT = {
    "build": {"dockerfile": "Dockerfile"},
    "image": "${T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME}",
    "container_name": "${T_BENCH_TASK_DOCKER_CLIENT_CONTAINER_NAME}",
    "command": ["sh", "-c", "sleep infinity"],
    "environment": ["TEST_DIR=${T_BENCH_TEST_DIR}"],
    "volumes": [
        "${T_BENCH_TASK_LOGS_PATH}:${T_BENCH_CONTAINER_LOGS_PATH}",
        "${T_BENCH_TASK_AGENT_LOGS_PATH}:${T_BENCH_CONTAINER_AGENT_LOGS_PATH}",
    ],
}


def task(tmp_path: Path, compose: dict[str, Any] | None, **files: str) -> Path:
    """A Terminal-Bench 1 task directory around a Compose file and extra files."""
    d = tmp_path / "t"
    d.mkdir()
    (d / "task.yaml").write_text("instruction: do it\nparser_name: pytest\n")
    (d / "run-tests.sh").write_text("#!/bin/sh\necho hi\n")
    (d / "Dockerfile").write_text("FROM python:3.13\nWORKDIR /app\n")
    for name, text in files.items():
        path = d / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
    if compose is not None:
        (d / "docker-compose.yaml").write_text(yaml.safe_dump(compose, sort_keys=False))
    return d


def env_of(d: Path, **options: str) -> dict[str, Any]:
    [row] = load_terminal_bench(str(d), **options)
    env: dict[str, Any] = row["metadata"]["environment"]
    return env


def services(d: Path) -> dict[str, dict[str, Any]]:
    return {s["name"]: s for s in env_of(d).get("services", [])}


def test_a_flask_service_with_installs_files_and_a_shared_workdir(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {
                **CLIENT,
                "volumes": [*CLIENT["volumes"], "shared:/app"],
                "depends_on": ["api"],
            },
            "api": {
                "image": "python:3.13-slim-bookworm",
                "ports": ["8090:8090"],
                "volumes": ["./server.py:/app/server.py", "shared:/app"],
                "environment": ["PYTHONUNBUFFERED=1"],
                "command": ["sh", "-c", "pip install flask==3.0.3 && python /app/server.py"],
            },
        },
        "volumes": {"shared": None},
    }
    d = task(tmp_path, compose, **{"server.py": "print('serve')\n"})
    env = env_of(d)
    api = services(d)["api"]
    assert api["port"] == 8090
    assert api["files"] == {"/app/server.py": "print('serve')\n"}
    assert api["workdir_mount"] == "/app"  # the volume is the client's workdir
    assert "volumes" not in api
    # The install runs once, with the network; the running service has none.
    assert api["setup"] == ["pip install flask==3.0.3"]
    assert api["setup_network"] == "allow"
    assert api["command"] == ["sh", "-c", "python /app/server.py"]
    assert api["image_entrypoint"] is True  # command alone keeps the image's ENTRYPOINT
    assert api["env"] == {"PYTHONUNBUFFERED": "1"}
    assert env["sandbox"]["network"] == "deny"
    assert "volumes" not in env
    assert env["service_lifetime"].endswith("s")
    ServiceSpec.from_dict({k: v for k, v in api.items()})  # the Python spec accepts it


def test_named_volumes_healthchecks_and_conditions(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {**CLIENT, "volumes": [*CLIENT["volumes"], "logs:/app/logs:ro"]},
            "web": {
                "image": "python:3.13-slim",
                "command": ["python", "-m", "http.server", "8000"],
                "volumes": ["data:/shared", "logs:/logs"],
                "depends_on": {
                    "seed": {"condition": "service_completed_successfully"},
                    "db": {"condition": "service_healthy"},
                },
                "hostname": "web.internal",
                "expose": ["8000"],
            },
            "seed": {
                "image": "python:3.13-slim",
                "command": ["python", "seed.py"],
                "volumes": ["data:/shared"],
            },
            "db": {
                "image": "postgres:16",
                "environment": {"POSTGRES_PASSWORD": "pw"},
                "healthcheck": {"test": ["CMD-SHELL", "pg_isready -U postgres"], "interval": "5s"},
                "ports": ["5432"],
            },
        }
    }
    d = task(tmp_path, compose)
    env = env_of(d)
    s = services(d)
    assert env["volumes"] == ["logs:/app/logs:ro"]
    assert s["web"]["volumes"] == ["data:/shared", "logs:/logs"]
    assert s["web"]["depends_on"] == ["seed", "db"]
    assert s["web"]["aliases"] == ["web.internal"]
    assert s["seed"]["oneshot"] is True
    assert "port" not in s["seed"]
    assert s["db"]["ready_command"] == ["sh", "-c", "pg_isready -U postgres"]
    assert s["db"]["command"] == []
    assert s["db"]["image_entrypoint"] is True
    assert s["db"]["port"] == 5432


def test_entrypoint_and_command_follow_dockers_rules(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": CLIENT,
            "a": {"image": "x", "entrypoint": ["redis-server", "--save", ""], "ports": ["6379"]},
            "b": {"image": "x", "entrypoint": ["app"], "command": ["--flag"], "ports": ["6380"]},
            "c": {"image": "x", "entrypoint": [], "command": ["run", "it"], "ports": ["6381"]},
            "d": {"image": "x", "command": "serve --port 8000", "ports": ["8000"]},
        }
    }
    s = services(task(tmp_path, compose))
    assert (s["a"]["command"], "image_entrypoint" in s["a"]) == (
        ["redis-server", "--save", ""],
        False,
    )
    assert (s["b"]["command"], "image_entrypoint" in s["b"]) == (["app", "--flag"], False)
    assert (s["c"]["command"], "image_entrypoint" in s["c"]) == (["run", "it"], False)
    assert (s["d"]["command"], s["d"]["image_entrypoint"]) == (["serve", "--port", "8000"], True)


def test_a_service_built_from_a_dockerfile(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {**CLIENT, "build": {"context": "client", "dockerfile": "Dockerfile.c"}},
            "api": {"build": {"context": "api", "args": {"GREETING": "hi"}}, "ports": ["8000"]},
        }
    }
    d = task(
        tmp_path,
        compose,
        **{
            "client/Dockerfile.c": "FROM alpine:3.22\nWORKDIR /srv\n",
            "api/Dockerfile": (
                "FROM python:3.13-slim\nARG GREETING=hello\nENV MSG=$GREETING\nWORKDIR /opt\n"
                'COPY app.py .\nRUN pip install flask\nCMD ["python", "app.py"]\n'
            ),
            "api/app.py": "print(1)\n",
        },
    )
    env = env_of(d)
    api = services(d)["api"]
    # The client's build is where Compose says, with its own workdir.
    assert env["image"] == "alpine:3.22"
    assert env["sandbox"]["workdir"] == "/srv"
    assert api["image"] == "python:3.13-slim"
    assert api["env"] == {"MSG": "hi"}  # the build arg beat the default
    assert api["files"] == {"/opt/app.py": "print(1)\n"}
    assert api["workdir"] == "/opt"
    assert any("pip install flask" in c for c in api["setup"])
    assert api["setup_network"] == "allow"
    # The Dockerfile's CMD is the command, with the base image's ENTRYPOINT kept.
    assert api["command"] == ["python", "app.py"]
    assert api["image_entrypoint"] is True


def test_the_clients_task_files_hosts_and_environment(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {
                **CLIENT,
                "command": [
                    "sh",
                    "-c",
                    "echo '127.0.0.1 host.docker.internal' >> /etc/hosts && sleep infinity",
                ],
                "environment": {"TEST_DIR": "/tests", "DATABASE_URL": "postgresql://db/x"},
                "volumes": ["./:/app/data", "./extra.py:/app/extra.py:ro"],
            },
            "db": {"image": "postgres:16", "ports": ["5432"]},
        }
    }
    d = task(
        tmp_path, compose, **{"seed.csv": "a,b\n", "extra.py": "x=1\n", "tests/test_a.py": "t"}
    )
    env = env_of(d)
    assert env["env"] == {"TEST_DIR": "/tests", "DATABASE_URL": "postgresql://db/x"}
    # The task directory is mounted, without the files that are the benchmark's own.
    assert env["files"]["/app/data/seed.csv"] == "a,b\n"
    assert env["files"]["/app/extra.py"] == "x=1\n"
    assert not [f for f in env["files"] if "run-tests" in f or "test_a" in f or "task.yaml" in f]
    assert services(d)["db"]["aliases"] == ["host.docker.internal"]


def test_a_client_without_a_root_dockerfile_builds_from_compose(tmp_path: Path) -> None:
    d = task(
        tmp_path,
        {"services": {"client": {**CLIENT, "build": {"context": "client"}}}},
        **{"client/Dockerfile": "FROM alpine:3.22\n"},
    )
    (d / "Dockerfile").unlink()
    env = env_of(d)
    assert env["image"] == "alpine:3.22"
    assert "services" not in env


def test_the_agent_service_can_be_named_by_its_image_variable(tmp_path: Path) -> None:
    compose = {
        "services": {
            "webserver": {
                **CLIENT,
                "networks": ["homelab"],
                "ports": ["443", "80"],
                "extra_hosts": ["homeserver.local:127.0.0.1"],
            },
            "mock": {"image": "python:3.11", "networks": ["homelab"], "ports": ["8080"]},
        },
        "networks": {"homelab": {"driver": "bridge"}},
    }
    d = task(tmp_path, compose)
    assert list(services(d)) == ["mock"]
    assert services(d)["mock"]["aliases"] == ["homeserver.local"]


def test_a_host_mapping_beyond_loopback_is_written_into_the_clients_hosts(
    tmp_path: Path,
) -> None:
    compose = {"services": {"client": {**CLIENT, "extra_hosts": ["example.com:131.25.18.2"]}}}
    env = env_of(task(tmp_path, compose))
    assert "echo '131.25.18.2 example.com' >> /etc/hosts" in env["setup"]


def test_limits_underscored_names_and_the_restricted_form(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": CLIENT,
            "master_service": {
                "image": "x",
                "ports": ["5000:5000"],
                "deploy": {"resources": {"limits": {"memory": "512M", "cpus": "0.5"}}},
            },
            "old": {
                "image": "y",
                "entrypoint": ["y"],
                "x-evalsi": {"port": 7001, "memory_mb": 64, "cpu_millis": 100},
            },
        }
    }
    s = services(task(tmp_path, compose))
    assert s["master-service"]["aliases"] == ["master_service"]
    assert (s["master-service"]["memory_mb"], s["master-service"]["cpu_millis"]) == (512, 500)
    assert (s["old"]["memory_mb"], s["old"]["cpu_millis"], s["old"]["port"]) == (64, 100, 7001)


# --- what is refused, with the reason ---


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ({"privileged": True}, "privileged is not supported"),
        ({"cap_add": ["NET_ADMIN"]}, "cap_add"),
        ({"network_mode": "host"}, "network_mode"),
        ({"devices": ["/dev/fuse"]}, "devices"),
        ({"extends": {"service": "x"}}, "extends"),
        ({"profiles": ["debug"]}, "profiles"),
        ({"secrets": ["s"]}, "secrets"),
        ({"volumes": ["/:/host"]}, "host path /"),
        ({"volumes": ["/var/run/docker.sock:/var/run/docker.sock"]}, "host path"),
        ({"volumes": ["./missing:/x"]}, "does not exist"),
        ({"volumes": ["../outside:/x"]}, "outside the task"),
        ({"image": "x:${NOT_DEFINED}"}, "not defined"),
        ({"depends_on": {"client": {}}}, "depends on the agent"),
        ({"depends_on": {"nope": {}}}, "unknown service"),
        ({"depends_on": {"web": {"condition": "service_sleepy"}}}, "condition"),
        ({"frobnicate": 1}, "unsupported Compose fields"),
        ({"extra_hosts": ["a:10.0.0.1"]}, "no other address"),
        ({"build": {"context": ".", "ssh": ["default"]}}, "unsupported build fields"),
        ({"image": None}, "neither image nor build"),
    ],
)
def test_what_a_service_cannot_keep_is_refused_with_a_reason(
    tmp_path: Path, change: dict[str, Any], message: str
) -> None:
    web = {"image": "x", "ports": ["8000"], **change}
    d = task(tmp_path, {"services": {"client": CLIENT, "web": web}})
    with pytest.raises(TaskFormatError, match=message):
        load_terminal_bench(str(d))


def test_services_that_share_no_network_would_lose_their_isolation(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {**CLIENT, "networks": ["a"]},
            "web": {"image": "x", "networks": ["b"], "ports": ["8000"]},
        }
    }
    with pytest.raises(TaskFormatError, match="share none"):
        load_terminal_bench(str(task(tmp_path, compose)))


def test_two_services_on_one_port_are_refused(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": CLIENT,
            "a": {"image": "x", "ports": ["9000"]},
            "b": {"image": "x", "ports": ["9000"]},
        }
    }
    with pytest.raises(TaskFormatError, match="both listen on port 9000"):
        load_terminal_bench(str(task(tmp_path, compose)))


def test_a_client_that_starts_something_else_is_refused(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": {**CLIENT, "command": ["sh", "-c", "python3 srv.py & sleep infinity"]}
        }
    }
    with pytest.raises(TaskFormatError, match="only idles"):
        load_terminal_bench(str(task(tmp_path, compose)))


def test_a_binary_bind_mount_cannot_be_given_to_a_service(tmp_path: Path) -> None:
    compose = {
        "services": {
            "client": CLIENT,
            "web": {"image": "x", "ports": ["80"], "volumes": ["./blob:/blob"]},
        }
    }
    d = task(tmp_path, compose)
    (d / "blob").write_bytes(bytes(range(256)))
    with pytest.raises(TaskFormatError, match="binary files"):
        load_terminal_bench(str(d))


def test_a_multi_stage_service_build_is_refused(tmp_path: Path) -> None:
    compose = {"services": {"client": CLIENT, "web": {"build": "web", "ports": ["80"]}}}
    d = task(
        tmp_path,
        compose,
        **{"web/Dockerfile": "FROM a AS b\nRUN true\nFROM c\nCOPY --from=b /x /x\n"},
    )
    with pytest.raises(TaskFormatError, match="multi-stage"):
        load_terminal_bench(str(d))


# --- the small parsers ---


@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("a-$$-b", "a-$-b"),
        ("${V}", "val"),
        ("$V", "val"),
        ("${MISSING:-dflt}", "dflt"),
        ("${EMPTY:-dflt}", "dflt"),
        ("${EMPTY-dflt}", ""),
        ("${V:+alt}", "alt"),
        ("${MISSING:+alt}", ""),
        ("--literal=$-1", "--literal=$-1"),
    ],
)
def test_interpolation(text: str, want: str) -> None:
    assert interpolate(text, {"V": "val", "EMPTY": ""}) == want


def test_interpolation_refuses_what_compose_would_silently_blank() -> None:
    with pytest.raises(TaskFormatError, match="not defined"):
        interpolate("$NOPE", {})
    with pytest.raises(TaskFormatError, match="required"):
        interpolate("${NOPE:?required}", {})


@pytest.mark.parametrize(
    ("argv", "installs", "rest"),
    [
        (["sh", "-c", "pip install a b && python x.py"], ["pip install a b"], "python x.py"),
        (
            ["sh", "-c", "apt-get update && apt-get install -y curl && exec app"],
            ["apt-get update", "apt-get install -y curl"],
            "exec app",
        ),
        (["sh", "-c", "python x.py && pip install a"], [], None),
        (["sh", "-c", "pip install a"], [], None),
        (["python", "x.py"], [], None),
        (["sh", "-c", "echo 'a && b' && python x"], [], None),
        (
            ["sh", "-c", "\n  pip install a==1 b==2 &&\n  echo hi &&\n  python x.py &\n  wait\n"],
            ["pip install a==1 b==2"],
            "echo hi &&\n  python x.py &\n  wait",
        ),
    ],
)
def test_installs_before_a_server_become_setup(
    argv: list[str], installs: list[str], rest: str | None
) -> None:
    got_installs, got_argv = split_installs(argv)
    assert got_installs == installs
    assert got_argv == (argv if rest is None else [argv[0], "-c", rest])

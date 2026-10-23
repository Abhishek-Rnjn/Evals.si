"""Multi-stage Dockerfiles: earlier stages are built on their own and files taken out (H4)."""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest

from evalsi_harness.benchmarks.dockerfile import TaskFormatError, translate_dockerfile
from evalsi_harness.environment import EnvironmentPool
from evalsi_harness.task import (
    BuildStageConfig,
    EnvironmentConfig,
    StageCopy,
    TaskError,
    environment_for,
)
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, spec_of

# The shapes of the multi-stage Dockerfiles in the pinned Terminal-Bench 1 checkout.
BUILD_AND_COPY = """\
FROM ubuntu:24.04 AS build
COPY --from=ghcr.io/astral-sh/uv:0.8.14 /uv /uvx /bin/
WORKDIR /root
RUN uv run /root/randomize.py
FROM ubuntu:24.04 AS target
COPY --from=ghcr.io/astral-sh/uv:0.8.14 /uv /uvx /bin/
COPY --from=build /app/documents /app/documents
WORKDIR /app
"""
ONLY_AN_IMAGE = """\
FROM python:3.13
COPY --from=ghcr.io/astral-sh/uv:0.8.15 /uv /uvx /bin/
RUN uv run setup.py
"""
UNUSED_FIRST_STAGE = """\
FROM ubuntu:24.04
FROM python:3.11-slim
RUN pip install numpy
"""
CHAIN = """\
ARG LEVEL=standard
ARG BASE=ubuntu:22.04
FROM ${BASE} AS root
RUN apt-get install -y sudo
ENV A=1
FROM root AS stage-standard
RUN useradd agent
WORKDIR /app
CMD ["sh", "-c", "sleep infinity"]
FROM stage-${LEVEL}
"""


def test_a_stage_the_target_copies_from_is_built_apart(tmp_path: Path) -> None:
    build = translate_dockerfile(BUILD_AND_COPY, tmp_path)
    assert build.image == "ubuntu:24.04"
    assert [(c.stage, c.source, c.destination) for c in build.copies] == [
        ("ghcr.io/astral-sh/uv:0.8.14", "/uv", "/bin/"),
        ("ghcr.io/astral-sh/uv:0.8.14", "/uvx", "/bin/"),
        ("build", "/app/documents", "/app/documents"),
    ]
    # Stages come after the stages they copy from: the uv image, then the build stage.
    assert [s.name for s in build.stages] == ["ghcr.io/astral-sh/uv:0.8.14", "build"]
    [uv, stage] = build.stages
    assert uv.setup == []
    assert stage.image == "ubuntu:24.04"
    assert stage.setup == ["mkdir -p /root && cd /root && uv run /root/randomize.py"]
    assert [c.stage for c in stage.copies] == ["ghcr.io/astral-sh/uv:0.8.14"] * 2


def test_copying_from_an_image_alone_needs_no_stage_commands(tmp_path: Path) -> None:
    build = translate_dockerfile(ONLY_AN_IMAGE, tmp_path)
    assert [s.name for s in build.stages] == ["ghcr.io/astral-sh/uv:0.8.15"]
    assert build.setup == ["uv run setup.py"]


def test_a_stage_nothing_copies_from_is_not_built(tmp_path: Path) -> None:
    build = translate_dockerfile(UNUSED_FIRST_STAGE, tmp_path)
    assert (build.image, build.stages, build.copies) == ("python:3.11-slim", [], [])
    assert build.setup == ["pip install numpy"]


def test_a_stage_may_start_from_an_earlier_one_and_args_reach_from(tmp_path: Path) -> None:
    build = translate_dockerfile(CHAIN, tmp_path)
    assert build.image == "ubuntu:22.04"
    assert [c.split("; ")[-1] for c in build.setup] == ["apt-get install -y sudo", "useradd agent"]
    assert build.env == {"A": "1"}
    assert build.workdir == "/app"
    assert build.cmd == ["sh", "-c", "sleep infinity"]


def test_a_numbered_stage_and_unknown_names(tmp_path: Path) -> None:
    text = "FROM a AS one\nRUN x\nFROM b\nCOPY --from=0 /out /out\n"
    build = translate_dockerfile(text, tmp_path)
    assert [s.name for s in build.stages] == ["one"]
    assert build.stages[0].setup == ["x"]


def test_run_cache_mounts_are_ignored_and_other_mounts_refused(tmp_path: Path) -> None:
    build = translate_dockerfile(
        "FROM a\nRUN --mount=type=cache,target=/root/.cache pip install x\n", tmp_path
    )
    assert build.setup == ["pip install x"]
    with pytest.raises(TaskFormatError, match="BuildKit"):
        translate_dockerfile("FROM a\nRUN --mount=type=bind,source=a,target=b true\n", tmp_path)


def test_heredoc_copy_and_entrypoint(tmp_path: Path) -> None:
    text = (
        'FROM a\nENTRYPOINT ["python", "-u"]\nCMD server.py\n'
        "COPY <<EOT /app/logs/auth.log\nline one\nline two\nEOT\n"
        "COPY <<'EOF' /srv/\nbody\nEOF\n"
    )
    build = translate_dockerfile(text, tmp_path)
    assert build.files == {"/app/logs/auth.log": "line one\nline two\n", "/srv/EOF": "body\n"}
    assert build.entrypoint == ["python", "-u"]
    assert build.cmd == ["/bin/sh", "-c", "server.py"]


# --- the environment builds the stages ---


def staged(**over: object) -> EnvironmentConfig:
    return EnvironmentConfig(
        stages=[
            BuildStageConfig(
                name="build",
                image="ignored-by-the-stand-in",
                setup=[
                    "mkdir -p out/sub",
                    "echo built > out/a.txt",
                    "echo deep > out/sub/b.txt",
                    "printf '#!/bin/sh\\necho ran\\n' > out/run.sh && chmod +x out/run.sh",
                ],
            )
        ],
        copy_from=[
            StageCopy("build", "/out", "/app/dir"),
            StageCopy("build", "/out/a.txt", "/app/one.txt"),
            StageCopy("build", "/out/run.sh", "/bin2/"),
            StageCopy("build", "/out/a.txt", "rel/x.txt"),
        ],
        setup=["cat app/dir/sub/b.txt > from-setup.txt"],
        **over,  # type: ignore[arg-type]
    )


def acquire(client: LocalSandboxClient, config: EnvironmentConfig) -> dict[str, bytes]:
    async def go() -> dict[str, bytes]:
        pool = EnvironmentPool(client)
        env = await pool.acquire(config)
        try:
            files = await env.sandbox.read_files(["/app", "from-setup.txt", "/bin2", "/workspace"])
            return files
        finally:
            await env.sandbox.destroy()
            await pool.aclose()

    return asyncio.run(go())


@pytest.mark.parametrize("snapshots", [True, False])
def test_files_come_out_of_the_stage_and_into_the_environment(snapshots: bool) -> None:
    got = acquire(LocalSandboxClient(snapshots=snapshots), staged())
    assert got["/app/dir/a.txt"] == b"built\n"
    assert got["/app/dir/sub/b.txt"] == b"deep\n"
    assert got["/app/one.txt"] == b"built\n"
    # Copied before the setup runs, so setup can use it.
    assert got["from-setup.txt"] == b"deep\n"
    # A relative destination is under the workdir.
    assert got["/workspace/rel/x.txt"] == b"built\n"


def test_a_failing_stage_is_an_environment_error_that_names_it() -> None:
    config = staged()
    config.stages[0].setup.append("exit 4")
    with pytest.raises(TaskError, match="build stage build failed"):
        acquire(LocalSandboxClient(), config)


def test_a_missing_file_in_the_stage_is_an_error() -> None:
    config = staged()
    config.copy_from.append(StageCopy("build", "/nope", "/x"))
    with pytest.raises(TaskError, match="/nope does not exist"):
        acquire(LocalSandboxClient(), config)


def test_stages_can_copy_from_earlier_stages() -> None:
    config = EnvironmentConfig(
        stages=[
            BuildStageConfig(name="one", image="x", setup=["mkdir -p o && echo seed > o/f"]),
            BuildStageConfig(
                name="two",
                image="x",
                copy_from=[StageCopy("one", "/o/f", "/w/f")],
                setup=["mkdir -p o2 && cat w/f w/f > o2/g"],
            ),
        ],
        copy_from=[StageCopy("two", "/o2/g", "/app/one.txt")],
        setup=["mkdir -p app"],
    )
    got = acquire(LocalSandboxClient(), config)
    assert got["/app/one.txt"] == b"seed\nseed\n"


def test_the_environment_spec_carries_stages_and_validates_them() -> None:
    raw = {
        "stages": [
            {"name": "b", "image": "x", "setup": ["true"]},
            {
                "name": "c",
                "image": "x",
                "copy_from": [{"stage": "b", "source": "/o", "destination": "/p"}],
            },
        ],
        "copy_from": [{"stage": "c", "source": "/p", "destination": "/q"}],
    }
    env = environment_for(spec_of({}), record(metadata={"environment": raw}))
    assert env is not None
    assert [s.name for s in env.stages] == ["b", "c"]
    assert env.copy_from[0].stage == "c"
    for bad, message in [
        ({"copy_from": [{"stage": "zz", "source": "/a", "destination": "/b"}]}, "unknown stage"),
        (
            {
                "stages": [
                    {
                        "name": "a",
                        "image": "x",
                        "copy_from": [{"stage": "b", "source": "/a", "destination": "/b"}],
                    },
                    {"name": "b", "image": "x"},
                ]
            },
            "not an earlier stage",
        ),
        ({"stages": [{"name": "a", "image": ""}]}, "name and an image"),
    ]:
        with pytest.raises(TaskError, match=message):
            environment_for(spec_of({}), record(metadata={"environment": bad}))


def test_execute_bits_are_restored_after_the_copy() -> None:
    from unittest.mock import AsyncMock

    sandbox = AsyncMock()
    sandbox.exec.return_value.ok = True
    files = {
        ("build", "/out"): [("/out/run.sh", b"#!/bin/sh\n", True), ("/out/d.txt", b"d", False)],
        ("uv", "/uv"): [("/uv", b"\x7fELF", True)],
    }
    copies = [StageCopy("build", "/out", "/opt/"), StageCopy("uv", "/uv", "/bin/")]
    asyncio.run(EnvironmentPool._place(sandbox, copies, files, "/app", {}))
    written: dict[str, bytes] = {}
    for call in sandbox.write_files.await_args_list:
        written.update(call.args[0])
    assert written == {
        "/opt/run.sh": b"#!/bin/sh\n",
        "/opt/d.txt": b"d",
        "/bin/uv": b"\x7fELF",
    }
    [chmod] = [c.args[0][2] for c in sandbox.exec.await_args_list]
    assert chmod == "chmod +x /opt/run.sh /bin/uv"

"""The Harbor and Terminal-Bench importers: Dockerfile translation, task
loading and grading. Running the tasks needs real images, so that is the
e2e test's job (tests/e2e/harbor_test.go)."""

from __future__ import annotations

import base64
import json
from pathlib import Path
from typing import Any

import pytest

from evalsi.datasets import load_records
from evalsi_harness.benchmarks import fixtures
from evalsi_harness.benchmarks.harbor import (
    MARK,
    TaskFormatError,
    grade_harbor,
    grade_pytest,
    load_harbor,
    load_terminal_bench,
    translate_dockerfile,
)


def test_a_dockerfile_becomes_an_image_setup_and_files(tmp_path: Path) -> None:
    (tmp_path / "app").mkdir()
    (tmp_path / "app" / "main.py").write_text("print(1)\n")
    (tmp_path / "run.sh").write_text("#!/bin/sh\n")
    (tmp_path / "run.sh").chmod(0o755)
    build = translate_dockerfile(
        """\
# a comment
ARG BASE=python:3.13-slim
FROM --platform=linux/amd64 ${BASE}
ENV A=1 B="two words"
ENV LEGACY some value
WORKDIR /srv
WORKDIR app
COPY app/ ./
COPY --chmod=755 run.sh /usr/local/bin/
RUN pip install \\
    requests
RUN ["echo", "exec form"]
RUN python3 - <<'PY'
print("heredoc")
PY
USER nobody
CMD ["python", "main.py"]
""",
        tmp_path,
    )
    assert build.image == "python:3.13-slim"
    assert build.env == {"A": "1", "B": "two words", "LEGACY": "some value"}
    assert build.workdir == "/srv/app"
    assert build.files == {"/srv/app/main.py": "print(1)\n", "/usr/local/bin/run.sh": "#!/bin/sh\n"}
    assert build.executable == ["/usr/local/bin/run.sh"]
    assert build.setup[0].endswith("cd /srv/app && pip install requests")
    assert build.setup[0].startswith("export BASE=python:3.13-slim; ")
    assert build.setup[1].endswith("echo 'exec form'")
    assert 'print("heredoc")\nPY' in build.setup[2]
    assert build.ignored == ["USER", "CMD"]


@pytest.mark.parametrize(
    ("dockerfile", "error"),
    [
        ("FROM a AS b\nFROM c\n", "multi-stage"),
        ("FROM a\nCOPY --from=b /x /y\n", "multi-stage"),
        ("FROM a\nADD https://example.com/x /x\n", "URL"),
        ("FROM a\nRUN --mount=type=cache,target=/c true\n", "BuildKit"),
        ("FROM a\nCOPY ../secret /x\n", "outside the build context"),
        ("FROM a\nCOPY missing.txt /x\n", "does not exist"),
        ("RUN true\n", "no FROM"),
        ("FROM a\nFROBNICATE x\n", "unsupported"),
    ],
)
def test_unsupported_dockerfiles_are_refused(tmp_path: Path, dockerfile: str, error: str) -> None:
    with pytest.raises(TaskFormatError, match=error):
        translate_dockerfile(dockerfile, tmp_path)


def env_of(row: dict[str, Any]) -> dict[str, Any]:
    env: dict[str, Any] = row["metadata"]["environment"]
    return env


def test_a_harbor_task(tmp_path: Path) -> None:
    fixtures.write(tmp_path)
    [row] = load_harbor(str(tmp_path / "harbor"))
    assert row["id"] == "evalsi/hello"
    assert row["input"] == fixtures.INSTRUCTION
    env = env_of(row)
    assert env["image"] == "alpine:3.22"
    assert env["sandbox"] == {"workdir": "/app", "network": "deny", "min_isolation": "namespaced"}
    assert env["env"] == {"GREETING": "Hello, world!"}
    # The binary file travels base64-encoded and is decoded first thing.
    blob = env["files"]["/opt/data/blob.bin.evalsi-b64"]
    assert base64.b64decode(blob) == fixtures.BLOB
    assert env["setup"][0].startswith("for f in /opt/data/blob.bin; do base64 -d")
    assert env["setup_network"] == "allow"
    checker = env["checker"]
    assert checker["parser"] == "evalsi_harness.benchmarks.harbor:grade_harbor"
    assert checker["timeout"] == "60s"
    assert set(checker["files"]) == {"/tests/test.sh", "/tests/blob.bin.evalsi-b64"}
    assert "/tests/blob.bin" in checker["command"][2].split("\n")[0]
    assert "/solution/solve.sh" not in env["files"]
    oracle = load_harbor(str(tmp_path / "harbor" / "hello"), oracle="true", network="allow")
    assert "/solution/solve.sh" in env_of(oracle[0])["files"]
    assert env_of(oracle[0])["sandbox"]["network"] == "allow"
    # Through the entry point, as a dataset URI.
    records = load_records(f"harbor://{tmp_path / 'harbor'}?tasks=hel*")
    assert [r.id for r in records] == ["evalsi/hello"]


def test_a_terminal_bench_task(tmp_path: Path) -> None:
    fixtures.write(tmp_path)
    [row] = load_terminal_bench(str(tmp_path / "terminal-bench"))
    assert row["id"] == "hello"
    assert row["input"] == fixtures.INSTRUCTION
    env = env_of(row)
    assert env["sandbox"]["network"] == "allow"
    assert env["env"]["TEST_DIR"] == "/tests"
    assert env["checker"]["parser"] == "evalsi_harness.benchmarks.harbor:grade_pytest"
    assert "/tests/run-tests.sh" in env["checker"]["files"]
    assert row["metadata"]["terminal_bench"]["agent_timeout_sec"] == 120.0
    with pytest.raises(TaskFormatError, match="no tasks"):
        load_terminal_bench(str(tmp_path / "terminal-bench"), tasks="other")


def test_unsupported_tasks_say_why(tmp_path: Path) -> None:
    fixtures.write(tmp_path)
    task = tmp_path / "terminal-bench" / "hello"
    (task / "docker-compose.yaml").write_text("services:\n  client: {}\n  db: {}\n")
    with pytest.raises(TaskFormatError, match=r"hello: tasks with several services \(client, db\)"):
        load_terminal_bench(str(task))
    harbor = tmp_path / "harbor" / "hello"
    (harbor / "steps").mkdir()
    with pytest.raises(TaskFormatError, match="multi-step"):
        load_harbor(str(harbor))
    with pytest.raises(TaskFormatError, match=r"no task\.toml"):
        load_harbor(str(tmp_path))


def out(tests: str, code: int, reward: tuple[str, str] | None) -> str:
    text = f"{tests}\n{MARK} exit {code}\n"
    if reward:
        text += f"{MARK} {reward[0]}\n{reward[1]}"
    return text


def test_harbor_rewards() -> None:
    assert grade_harbor(out("ok", 0, ("reward.txt", "1\n")), "", 0, None).passed
    half = grade_harbor(out("", 0, ("reward.txt", "0.5")), "", 0, None)
    assert not half.passed
    assert half.score == 0.5
    multi = grade_harbor(out("", 0, ("reward.json", json.dumps({"a": 1, "b": 0}))), "", 0, None)
    assert multi.score == 0.5
    assert multi.tests == {"a": "passed", "b": "failed"}
    keyed = grade_harbor(out("", 0, ("reward.json", '{"reward": 1, "style": 0.2}')), "", 0, None)
    assert keyed.passed
    missing = grade_harbor(out("boom", 1, None), "", 0, None)
    assert not missing.passed
    assert "no reward file" in missing.details
    assert grade_harbor("sh: not found", "", 127, None).error == "the verifier did not run"
    bad = grade_harbor(out("", 0, ("reward.txt", "NaN")), "", 0, None)
    assert not bad.passed


def test_terminal_bench_pytest_rule() -> None:
    report = """...
=========================== short test summary info ============================
PASSED tests/test_outputs.py::test_one
FAILED tests/test_outputs.py::test_two - AssertionError: x - y
SKIPPED tests/test_outputs.py::test_three
"""
    check = grade_pytest(out(report, 1, None), "", 0, None)
    assert not check.passed
    assert check.tests == {"test_one": "passed", "test_two": "failed", "test_three": "passed"}
    assert check.details == "2/3 tests passed"
    ok = report.replace("FAILED tests/test_outputs.py::test_two - AssertionError: x - y", "")
    assert grade_pytest(out(ok, 0, None), "", 0, None).passed
    assert not grade_pytest(out("collected 0 items", 4, None), "", 0, None).passed

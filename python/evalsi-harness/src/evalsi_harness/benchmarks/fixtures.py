"""Tiny Harbor and Terminal-Bench 1 tasks, for tests and smoke runs: each
asks for /app/hello.txt on alpine:3.22, copies a binary file in, and grades
offline (no package installs), so they run in any sandbox that can pull the
image.

    python -m evalsi_harness.benchmarks.fixtures <dir>

writes ``<dir>/harbor/hello`` and ``<dir>/terminal-bench/hello``.
"""

from __future__ import annotations

from pathlib import Path

INSTRUCTION = 'Create a file called /app/hello.txt with "Hello, world!" as the content.'
BLOB = bytes(range(256)) * 4

DOCKERFILE = """\
FROM alpine:3.22
ARG GREETING_FILE=greeting.txt
ENV GREETING="Hello, world!"
WORKDIR /app
COPY blob.bin /opt/data/
RUN printf '%s' "$GREETING" > /opt/data/${GREETING_FILE} \\
    && echo ready > state.txt
CMD ["sh"]
"""

# Checks the agent's file, and that the image setup (ENV, ARG, RUN in the
# WORKDIR) and the binary copy came through intact.
CHECK = """\
test "$(cat /app/hello.txt)" = "$(cat /opt/data/greeting.txt)" && \\
test "$(cat /app/state.txt)" = ready && \\
cmp -s /opt/data/blob.bin /tests/blob.bin"""

TASK_TOML = """\
schema_version = "1.1"

[task]
name = "evalsi/hello"

[metadata]
difficulty = "easy"

[verifier]
timeout_sec = 60.0

[agent]
timeout_sec = 120.0

[environment]
network_mode = "no-network"
"""

HARBOR_TEST = f"""#!/bin/sh
if {CHECK}; then
  echo 1 > /logs/verifier/reward.txt
else
  echo 0 > /logs/verifier/reward.txt
fi
"""

TASK_YAML = f"""\
instruction: |-
  {INSTRUCTION}
difficulty: easy
category: file-operations
parser_name: pytest
max_agent_timeout_sec: 120.0
max_test_timeout_sec: 60.0
"""

# A pytest-shaped report without pytest, so the task needs no network.
RUN_TESTS = f"""#!/bin/sh
echo "============================= test session starts =============================="
if {CHECK}; then hello=PASSED; else hello=FAILED; fi
echo "=========================== short test summary info ============================"
echo "PASSED tests/test_outputs.py::test_setup"
echo "$hello tests/test_outputs.py::test_hello_file"
"""


def write(directory: str | Path) -> Path:
    root = Path(directory)
    h = root / "harbor" / "hello"
    (h / "environment").mkdir(parents=True, exist_ok=True)
    (h / "tests").mkdir(exist_ok=True)
    (h / "solution").mkdir(exist_ok=True)
    (h / "task.toml").write_text(TASK_TOML)
    (h / "instruction.md").write_text(INSTRUCTION + "\n")
    (h / "environment" / "Dockerfile").write_text(DOCKERFILE)
    (h / "environment" / "blob.bin").write_bytes(BLOB)
    (h / "tests" / "test.sh").write_text(HARBOR_TEST)
    (h / "tests" / "blob.bin").write_bytes(BLOB)
    (h / "solution" / "solve.sh").write_text("#!/bin/sh\nprintf 'Hello, world!' > /app/hello.txt\n")

    t = root / "terminal-bench" / "hello"
    (t / "tests").mkdir(parents=True, exist_ok=True)
    (t / "task.yaml").write_text(TASK_YAML)
    (t / "Dockerfile").write_text(DOCKERFILE)
    (t / "blob.bin").write_bytes(BLOB)
    (t / "run-tests.sh").write_text(RUN_TESTS)
    (t / "tests" / "blob.bin").write_bytes(BLOB)
    (t / "solution.sh").write_text("#!/bin/sh\nprintf 'Hello, world!' > /app/hello.txt\n")
    return root


if __name__ == "__main__":  # pragma: no cover
    import sys

    write(sys.argv[1] if len(sys.argv) > 1 else "benchmark-fixtures")

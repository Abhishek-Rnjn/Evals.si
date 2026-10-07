from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

import evalsi
from conftest import make_record
from evalsi.packs import code
from evalsi.sandbox import SandboxError
from evalsi.testing import score

TESTS = "def check(f):\n    assert f(2, 3) == 5\n    assert f(-1, 1) == 0\n"


def humaneval(output: str) -> evalsi.Record:
    return make_record(output, TESTS, metadata={"entry_point": "add"})


def test_extract_code_prefers_python_blocks() -> None:
    text = "Here:\n```bash\nls\n```\nand\n```python\ndef f(): pass\n```"
    assert code.extract_code(text) == "def f(): pass\n"
    assert code.extract_code("```\nx = 1\n```") == "x = 1\n"
    assert code.extract_code("x = 1") == "x = 1"


@pytest.mark.usefixtures("fake_sandbox")
def test_unit_tests_pass_and_fail() -> None:
    good = humaneval("```python\ndef add(a, b):\n    return a + b\n```")
    bad = humaneval("def add(a, b):\n    return a - b\n")
    assert score(code.unit_tests, good)["unit-tests"].passed is True
    failed = score(code.unit_tests, bad)["unit-tests"]
    assert failed.passed is False
    assert "AssertionError" in failed.explanation
    assert failed.metadata["isolation"]["driver"] == "fake"


@pytest.mark.usefixtures("fake_sandbox")
def test_timeouts_are_failures_not_errors() -> None:
    slow = humaneval("import time\ndef add(a, b):\n    time.sleep(5)\n")
    result = score(code.unit_tests, slow, timeout_s=0.5)["unit-tests"]
    assert result.passed is False
    assert "timed out" in result.explanation


@pytest.mark.usefixtures("fake_sandbox")
def test_records_without_tests_are_skipped() -> None:
    with pytest.raises(evalsi.SkipRecord):
        score(code.unit_tests, make_record("def f(): pass"))


def test_no_sandbox_is_an_infrastructure_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("EVALSID", raising=False)
    monkeypatch.setenv("PATH", "")
    with pytest.raises(SandboxError, match="no sandbox"):
        score(code.unit_tests, humaneval("def add(a, b): return a + b"))
    # Through evaluate, it is an error outcome, excluded from the metric.
    result = evalsi.evaluate([humaneval("def add(a, b): return a + b")], ["unit-tests"])
    assert result.results[0].outcome.value == "error"
    assert result.metric("unit-tests").n == 0


def test_unavailable_sandbox_fails_closed(
    fake_sandbox: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_SANDBOX", "unavailable")
    with pytest.raises(SandboxError, match="unavailable: no rung"):
        score(code.unit_tests, humaneval("def add(a, b): return a + b"))


def test_python_syntax() -> None:
    assert score(code.python_syntax, make_record("def f(:\n"))["python-syntax"].passed is False
    assert score(code.python_syntax, make_record("```py\nx = 1\n```"))["python-syntax"].passed


@pytest.mark.skipif(not os.environ.get("EVALSI_TEST_EVALSID"), reason="needs a built evalsid")
def test_real_sandbox(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("EVALSID", os.environ["EVALSI_TEST_EVALSID"])
    result = score(code.unit_tests, humaneval("def add(a, b):\n    return a + b\n"))
    assert result["unit-tests"].passed is True, result["unit-tests"].explanation
    escape = humaneval("open('/usr/evil', 'w')\ndef add(a, b):\n    return a + b\n")
    blocked = score(code.unit_tests, escape)["unit-tests"]
    assert blocked.passed is False
    assert json.dumps(blocked.metadata["isolation"])

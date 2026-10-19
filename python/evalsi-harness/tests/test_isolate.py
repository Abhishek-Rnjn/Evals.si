"""Clean checker isolation: grading only the declared outputs (H4)."""

from __future__ import annotations

from typing import Any

import pytest

from evalsi_harness.task import TaskError
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

FILES = {
    "calc.py": "def add(a, b):\n    return a - b\n",
    "tests/test_calc.py": "from calc import add\nassert add(2, 3) == 5\n",
}
HONEST = "printf 'def add(a, b):\\n    return a + b\\n' > calc.py"
# Make any python process exit 0 before the tests run, without fixing calc.py.
SHIM = "printf 'import os\\nos._exit(0)\\n' > sitecustomize.py"
REWRITE_TESTS = "printf 'print(1)\\n' > tests/test_calc.py"


def spec_for(script: str, **checker: Any) -> Any:
    return spec_of(
        {
            "target": {"agent": {"cli": {"command": ["sh", "-c", script]}}},
            "environment": {
                "files": FILES,
                "checker": {
                    "command": ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"],
                    **checker,
                },
            },
        }
    )


CLEAN: dict[str, Any] = {"isolation": "clean", "outputs": ["calc.py"]}


def test_a_shim_outside_the_checker_fools_a_shared_checker(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(SHIM), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed  # the gap this feature closes


def test_clean_checking_leaves_the_shim_behind(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(SHIM, **CLEAN), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed
    assert rec.metadata["checker_isolation"] == {"isolation": "clean", "outputs": ["calc.py"]}


def test_a_real_fix_passes_in_the_clean_sandbox(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(HONEST + "; " + SHIM, **CLEAN), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed


def test_rewritten_tests_are_not_seen_by_the_clean_checker(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(REWRITE_TESTS, **CLEAN), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed


def test_missing_outputs_grade_the_untouched_setup(sandboxes: LocalSandboxClient) -> None:
    rec = run(
        spec_for("true", isolation="clean", outputs=["solution/**"]), record("x"), sandboxes
    ).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed
    assert rec.metadata["checker_isolation"]["outputs"] == []


@pytest.mark.parametrize(
    ("checker", "message"),
    [
        ({"isolation": "clean"}, "needs outputs"),
        ({"isolation": "shared", "outputs": ["a.py"]}, "only with isolation: clean"),
        ({"isolation": "sealed", "outputs": ["a.py"]}, "isolation must be one of"),
        ({**CLEAN, "outputs": ["../x"]}, "relative to the workdir"),
        ({**CLEAN, "protect": ["calc.py"]}, "both an output and protected"),
    ],
)
def test_bad_configuration_is_refused(
    sandboxes: LocalSandboxClient, checker: dict[str, Any], message: str
) -> None:
    with pytest.raises(TaskError, match=message):
        run(spec_for("true", **checker), record("x"), sandboxes)

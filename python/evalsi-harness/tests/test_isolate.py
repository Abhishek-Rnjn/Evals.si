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
        ({**CLEAN, "outputs": ["**"]}, "would copy everything"),
        ({**CLEAN, "outputs": ["calc.py", "**/**"]}, "would copy everything"),
        ({**CLEAN, "outputs": ["***"]}, "would copy everything"),
        ({**CLEAN, "outputs": ["*.*"]}, "would copy everything"),
    ],
)
def test_bad_configuration_is_refused(
    sandboxes: LocalSandboxClient, checker: dict[str, Any], message: str
) -> None:
    with pytest.raises(TaskError, match=message):
        run(spec_for("true", **checker), record("x"), sandboxes)


def test_the_gym_grades_a_clean_environment_too() -> None:
    import asyncio

    from evalsi_harness.gym import Action, TaskEnv

    async def episode(**checker: Any) -> float | None:
        spec = spec_of(
            {
                "environment": {
                    "files": FILES,
                    "checker": {
                        "command": ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"],
                        **checker,
                    },
                }
            }
        )
        async with TaskEnv(LocalSandboxClient()) as env:
            await env.reset(record("x"), spec)
            await env.step(Action("bash", {"command": SHIM}))
            return (await env.step(Action("submit"))).reward

    assert asyncio.run(episode()) == 1.0  # shared: the shim fools the checker
    assert asyncio.run(episode(**CLEAN)) == 0.0  # clean: it never reaches it


def test_a_forged_listing_cannot_widen_the_outputs(
    sandboxes: LocalSandboxClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    # An agent that replaced find could make the listing name a path outside solution/.
    from evalsi_harness import isolate

    async def forged(env: Any) -> list[str]:
        return ["solution/a.py", "solution/../sitecustomize.py"]

    monkeypatch.setattr(isolate, "_listing", forged)
    out = run(spec_for(SHIM, isolation="clean", outputs=["solution/**"]), record("x"), sandboxes)
    # A check that cannot run is an error, never a pass or a failed task.
    assert out.record is None
    assert "non-canonical" in out.error


@pytest.mark.parametrize("path", ["/etc/passwd", "a//b", "./a", "a/./b", "a/../b", ""])
def test_only_canonical_listed_paths_are_trusted(path: str) -> None:
    from evalsi_harness.isolate import _canonical

    assert not _canonical(path)
    assert _canonical("solution/a.py")


def test_only_checker_files_absent_from_the_prepared_sandbox_are_hidden(
    sandboxes: LocalSandboxClient,
) -> None:
    rec = run(
        spec_for(
            "true",
            files={"tests/test_calc.py": "assert True\n", "hidden/check.py": "assert True\n"},
        ),
        record("x"),
        sandboxes,
    ).record
    assert rec is not None
    assert rec.metadata["checker_files"] == ["hidden/check.py"]


def test_an_output_cannot_be_written_through_a_setup_symlink(
    sandboxes: LocalSandboxClient,
) -> None:
    # Setup leaves solution -> . ; the agent swaps it for a real directory holding a shim,
    # so copying solution/sitecustomize.py into a fresh sandbox would follow the link.
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": [
                            "sh",
                            "-c",
                            f"rm solution; mkdir solution; {SHIM.replace('> ', '> solution/', 1)}",
                        ]
                    }
                }
            },
            "environment": {
                "files": FILES,
                "setup": ["ln -s . solution"],
                "checker": {
                    "command": ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"],
                    **CLEAN,
                    "outputs": ["solution/**"],
                },
            },
        }
    )
    out = run(spec, record("x"), sandboxes)
    assert out.record is None
    assert "crosses a symlink" in out.error

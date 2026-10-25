"""Clean checking from the agent's diff (H4)."""

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
REWRITE_TESTS = "printf 'print(1)\\n' > tests/test_calc.py"
SHIM = "printf 'import os\\nos._exit(0)\\n' > sitecustomize.py"
DIFF: dict[str, Any] = {"isolation": "diff", "protect": ["tests/**"]}


def spec_for(script: str, setup: list[str] | None = None, **checker: Any) -> Any:
    return spec_of(
        {
            "target": {"agent": {"cli": {"command": ["sh", "-c", script]}}},
            "environment": {
                "files": FILES,
                "setup": setup or [],
                "checker": {
                    "command": ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"],
                    **checker,
                },
            },
        }
    )


def test_an_honest_fix_passes_and_the_diff_is_recorded(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(HONEST, **DIFF), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    info = rec.metadata["checker_isolation"]
    assert info["method"] == "files"
    assert info["modified"] == ["calc.py"]
    assert info["rejected"] == []
    assert "+    return a + b" in info["diff"]
    steps = [s for s in rec.trajectory.steps if s.name == "checker:diff"]  # type: ignore[union-attr]
    assert len(steps) == 1


def test_an_agent_that_edits_the_tests_is_caught(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(REWRITE_TESTS, **DIFF), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed  # the original test runs against the unfixed calc.py
    rejected = rec.metadata["checker_isolation"]["rejected"]
    assert rejected == [{"path": "tests/test_calc.py", "reason": "modified, protected"}]
    kinds = [p["kind"] for p in rec.metadata["policy_events"]]
    assert "checker_diff_rejected" in kinds


def test_a_fix_plus_test_edit_passes_but_the_edit_is_flagged(
    sandboxes: LocalSandboxClient,
) -> None:
    rec = run(spec_for(HONEST + "; " + REWRITE_TESTS, **DIFF), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    assert rec.metadata["checker_isolation"]["rejected"][0]["path"] == "tests/test_calc.py"


def test_outputs_limit_what_is_applied(sandboxes: LocalSandboxClient) -> None:
    rec = run(spec_for(SHIM, **DIFF, outputs=["calc.py"]), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed
    assert rec.metadata["checker_isolation"]["rejected"][0]["reason"].endswith(
        "outside the declared outputs"
    )


@pytest.mark.parametrize("executable", [False, True])
def test_diff_modes_support_option_like_names_on_bsd_and_gnu_chmod(
    sandboxes: LocalSandboxClient, executable: bool
) -> None:
    mode = "a+x" if executable else "a-x"
    check = "test -x ./-helper" if executable else "test ! -x ./-helper"
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": [
                            "sh",
                            "-c",
                            f"printf new > ./-helper; chmod {mode} ./-helper",
                        ]
                    }
                }
            },
            "environment": {
                "files": {"-helper": "old"},
                "setup": ["chmod a+x ./-helper"],
                "checker": {
                    "isolation": "diff",
                    "command": ["sh", "-c", f'{check} && test "$(cat ./-helper)" = new'],
                },
            },
        }
    )
    rec = run(spec, record("mode"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    assert rec.metadata["checker_isolation"]["modified"] == ["-helper"]


def test_deletions_and_symlinks(sandboxes: LocalSandboxClient) -> None:
    rec = run(
        spec_for("rm calc.py; ln -s /etc/passwd evil.py", **DIFF), record("x"), sandboxes
    ).record
    assert rec is not None
    info = rec.metadata["checker_isolation"]
    assert info["deleted"] == ["calc.py"]
    assert [r["path"] for r in info["rejected"]] == ["evil.py"]
    assert "symlink" in info["rejected"][0]["reason"]


def test_a_repository_workdir_is_diffed_with_the_vcs(sandboxes: LocalSandboxClient) -> None:
    setup = [
        "git init -q",
        "git config user.email a@b.c",
        "git config user.name x",
        "git add -A",
        "git commit -q -m base",
    ]
    rec = run(
        spec_for(HONEST + "; echo hi > new.txt", setup=setup, **DIFF), record("x"), sandboxes
    ).record
    assert rec is not None
    assert rec.check is not None
    info = rec.metadata["checker_isolation"]
    assert info["method"] == "git"
    assert info["modified"] == ["calc.py"]
    assert rec.check.passed
    assert info["added"] == ["new.txt"]
    assert info["modified"] == ["calc.py"]


@pytest.mark.parametrize(
    ("checker", "message"),
    [
        ({"isolation": "shared", "exclude": ["a"]}, "exclude applies only"),
        ({"isolation": "diff", "exclude": ["../a"]}, "relative to the workdir"),
    ],
)
def test_bad_configuration_is_refused(
    sandboxes: LocalSandboxClient, checker: dict[str, Any], message: str
) -> None:
    with pytest.raises(TaskError, match=message):
        run(spec_for("true", **checker), record("x"), sandboxes)

"""Clean checker isolation on real sandbox rungs (H4).

Everything else in this directory runs on the unconfined local stand-in. These
tests start a real ``evalsid sandbox serve`` with the ladder pinned to one rung,
assert that rung is the one that ran, and play the isolation attacks for real.

They need a built evalsid (``EVALSI_TEST_EVALSID``, as in the CI job "Python tests
against a real sandbox"). A rung the host lacks skips its tests, unless
``EVALSI_REQUIRE_SANDBOX`` is set, as it is in CI: then a missing rung fails, so
a runner without bubblewrap cannot pass by skipping. The pod and Firecracker rungs
need a cluster and KVM and are not covered here.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path
from typing import Any

import pytest

from evalsi.sandbox.client import connect
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from tests.harness_support import record, spec_of

pytestmark = pytest.mark.skipif(
    not os.environ.get("EVALSI_TEST_EVALSID"), reason="needs a built evalsid"
)

RUNGS = ["bwrap", "landlock"]
FILES = {
    "calc.py": "def add(a, b):\n    return a - b\n",
    "tests/test_calc.py": "from calc import add\nassert add(2, 3) == 5\n",
}
CHECK = ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"]
FIX = "printf 'def add(a, b):\\n    return a + b\\n' > calc.py"
SHIM = "printf 'import os\\nos._exit(0)\\n' > sitecustomize.py"
CLEAN: dict[str, Any] = {"isolation": "clean", "outputs": ["calc.py"]}


@pytest.fixture
def config(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Any:
    """Write a sandbox config pinning one rung, and point the client at it."""
    monkeypatch.setenv("EVALSID", os.environ["EVALSI_TEST_EVALSID"])
    monkeypatch.delenv("EVALSI_SANDBOX_ADDR", raising=False)

    def pin(rung: str, **extra: Any) -> None:
        lines = ["sandbox:", f"  ladder: [{rung}]"]
        lines += [f"  {key}: {value}" for key, value in extra.items()]
        path = tmp_path / f"{rung}.yaml"
        path.write_text("\n".join(lines) + "\n")
        monkeypatch.setenv("EVALSI_SANDBOX_CONFIG", str(path))

    return pin


async def _play(spec: Any, rung: str) -> TaskOutcome:
    async with await connect() as client:
        rungs = {r["driver"]: r["available"] for r in await client.probe()}
        if not rungs.get(rung):
            message = f"the {rung} rung is not available on this host"
            if os.environ.get("EVALSI_REQUIRE_SANDBOX"):
                pytest.fail(message)
            pytest.skip(message)

        async def get() -> Any:
            return client

        harness = load_harness(spec, HarnessContext(sandboxes=get, base_dir=Path.cwd()))
        try:
            return await run_task(Task.build(spec, record("Fix add()"), run_id="run1"), harness)
        finally:
            await harness.aclose()


MIN_ISOLATION = {"bwrap": "namespaced", "landlock": "confined"}


def play(
    agent: str,
    rung: str,
    *,
    checker: dict[str, Any] | None = None,
    min_isolation: str | None = None,
    **environment: Any,
) -> Any:
    # A task asks for `namespaced` or better by default, and a weaker rung is refused
    # rather than used; the Landlock tests opt in to `confined`.
    sandbox = {"min_isolation": min_isolation or MIN_ISOLATION[rung]}
    spec = spec_of(
        {
            "target": {"agent": {"cli": {"command": ["sh", "-c", agent]}}},
            "environment": {
                "files": FILES,
                "sandbox": sandbox,
                "checker": {"command": CHECK, **(checker or {})},
                **environment,
            },
        }
    )
    return asyncio.run(_play(spec, rung))


def ran_on(outcome: TaskOutcome, rung: str) -> None:
    """The rung that was pinned is the rung that ran, never a quiet fallback."""
    assert outcome.record is not None
    assert outcome.record.metadata["isolation"]["driver"] == rung


@pytest.mark.parametrize("rung", RUNGS)
def test_an_honest_fix_passes_clean_checking_on_the_pinned_rung(config: Any, rung: str) -> None:
    config(rung)
    outcome = play(f"{FIX}; {SHIM}", rung, checker=CLEAN)
    assert outcome.error == "", outcome.error
    ran_on(outcome, rung)
    assert outcome.record is not None
    assert outcome.record.check is not None
    assert outcome.record.check.passed
    assert outcome.record.metadata["checker_isolation"] == {
        "isolation": "clean",
        "outputs": ["calc.py"],
    }


@pytest.mark.parametrize("rung", RUNGS)
def test_a_shim_fools_the_shared_checker_but_not_the_clean_one(config: Any, rung: str) -> None:
    config(rung)
    shared = play(SHIM, rung)
    ran_on(shared, rung)
    assert shared.record is not None
    assert shared.record.check is not None
    assert shared.record.check.passed  # the gap
    clean = play(SHIM, rung, checker=CLEAN)
    ran_on(clean, rung)
    assert clean.record is not None
    assert clean.record.check is not None
    assert not clean.record.check.passed


@pytest.mark.parametrize("rung", RUNGS)
def test_a_replaced_find_cannot_widen_the_outputs(config: Any, rung: str) -> None:
    """The harness lists files with `find` inside the agent's sandbox. When the task's
    PATH starts at an agent-writable directory, a `find` shim forges the listing with a
    path outside the outputs; the harness refuses the listing."""
    config(rung)
    forge = (
        "mkdir -p bin solution; "
        f"{SHIM}; "
        "printf '#!/bin/sh\\nprintf \"./solution/../sitecustomize.py\\\\0\"\\n' > bin/find; "
        "chmod +x bin/find"
    )
    outcome = play(
        forge,
        rung,
        checker={**CLEAN, "outputs": ["solution/**"]},
        env={"PATH": "bin:/usr/local/bin:/usr/bin:/bin"},
    )
    assert outcome.record is None
    assert "non-canonical" in outcome.error


@pytest.mark.parametrize("rung", RUNGS)
def test_a_find_shim_outside_the_task_path_changes_nothing(config: Any, rung: str) -> None:
    """With the default PATH the agent cannot get a shim ahead of the real `find`: on
    these rungs the system directories are read-only to it, so the honest fix passes."""
    config(rung)
    outcome = play(
        f"mkdir -p bin; printf '#!/bin/sh\\nexit 0\\n' > bin/find; {FIX}", rung, checker=CLEAN
    )
    assert outcome.error == "", outcome.error
    assert outcome.record is not None
    assert outcome.record.check is not None
    assert outcome.record.check.passed


@pytest.mark.parametrize("rung", RUNGS)
def test_an_output_path_through_a_setup_symlink_is_refused(config: Any, rung: str) -> None:
    config(rung)
    outcome = play(
        f"rm solution; mkdir solution; {SHIM.replace('> ', '> solution/', 1)}",
        rung,
        checker={**CLEAN, "outputs": ["solution/**"]},
        setup=["ln -s . solution"],
    )
    assert outcome.record is None
    assert "crosses a symlink" in outcome.error


@pytest.mark.parametrize("rung", RUNGS)
def test_a_pool_of_one_sandbox_cannot_run_clean_checking(config: Any, rung: str) -> None:
    """Clean checking holds a second sandbox while the agent's is alive. With
    max_sandboxes 1 that is an error, never a pass (documented in the guide)."""
    config(rung, max_sandboxes=1)
    outcome = play(FIX, rung, checker=CLEAN)
    assert outcome.record is None
    assert "sandbox" in outcome.error
    assert "max_sandboxes" in outcome.error or "too many" in outcome.error


def test_the_default_isolation_floor_refuses_landlock_instead_of_downgrading(config: Any) -> None:
    config("landlock")
    outcome = play(FIX, "landlock", min_isolation="namespaced")
    assert outcome.record is None
    assert "below namespaced" in outcome.error


async def _pause_and_resume(rung: str) -> None:
    from evalsi_harness.models import ModelTurn
    from evalsi_harness.rollout import DONE, PAUSED, RolloutPool
    from tests.test_rollout import Scripted, call

    spec = spec_of(
        {
            "environment": {
                "files": FILES,
                "setup": ["echo ready > setup.log"],
                "sandbox": {"min_isolation": MIN_ISOLATION[rung]},
                "checker": {"command": CHECK},
            }
        }
    )
    async with await connect() as client:
        rungs = {r["driver"]: r["available"] for r in await client.probe()}
        if not rungs.get(rung):
            message = f"the {rung} rung is not available on this host"
            if os.environ.get("EVALSI_REQUIRE_SANDBOX"):
                pytest.fail(message)
            pytest.skip(message)
        fix = ModelTurn(text="fix", tool_calls=[call("bash", "c1", command=FIX)])
        submit = ModelTurn(text="", tool_calls=[call("submit", "c2")])
        pool_ref: list[RolloutPool] = []

        async def stop_after_first(n: int) -> None:
            if n == 1:
                pool_ref[0].stop()

        model = Scripted([fix, submit], on_call=stop_after_first)
        pool = RolloutPool(model, client)
        pool_ref.append(pool)
        try:
            [paused] = await pool.run([(record("Fix add()"), spec)])
            assert paused.status == PAUSED, paused.error
            # The fix is in the saved sandbox; the second turn then submits it.
            pool.clear()
            [done] = await pool.run(resume=[paused])
            assert (done.status, done.reward) == (DONE, 1.0), done.error
            assert len(done.turns) == 2
            await pool.delete_snapshot(paused)
        finally:
            await pool.aclose()


@pytest.mark.parametrize("rung", RUNGS)
def test_a_paused_rollout_resumes_in_a_restored_sandbox(config: Any, rung: str) -> None:
    config(rung)
    asyncio.run(_pause_and_resume(rung))

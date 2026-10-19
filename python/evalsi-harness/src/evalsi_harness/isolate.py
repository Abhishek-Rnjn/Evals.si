"""Checker isolation: grading in a sandbox the agent never touched (H4).

By default the checker runs in the sandbox the agent just controlled, so the
agent can still change the interpreter, ``PATH``, ``conftest.py`` or the
installed packages. With ``checker.isolation: clean`` the harness instead:

* lists the agent's *declared outputs*: the files under ``checker.outputs``
  (glob patterns relative to the workdir);
* builds a fresh sandbox for the same environment (the setup snapshot where
  the rung has one, a new setup otherwise), which the agent never ran in;
* copies only those files in and runs the checker there.

Everything the agent did outside its declared outputs is left behind: edits to
other files, installed packages, shims, deleted files. Symlinks are not
followed and never copied. A copied file loses its execute bit (``write_files`` takes
one mode per call), so a checker should run outputs through an interpreter. A pattern both in
``outputs`` and in ``protect`` is rejected when the task is read, because the two
promises conflict. Services are not supported with clean checking (their state is
never snapshotted), and the check fails closed when the outputs cannot be read in full.
"""

from __future__ import annotations

from evalsi.sandbox import SandboxError
from evalsi.types import TaskCheck
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool, TaskEnvironment
from evalsi_harness.protect import ProtectError, _listing, _read, _selected, validate_patterns
from evalsi_harness.task import CheckerConfig, Task

ISOLATIONS = ("shared", "clean")


def validate(checker: CheckerConfig, *, has_services: bool = False) -> None:
    """Raise ValueError when the checker's isolation settings do not hold together."""
    if checker.isolation not in ISOLATIONS:
        raise ValueError(f"isolation must be one of {', '.join(ISOLATIONS)}")
    if checker.isolation == "shared":
        if checker.outputs:
            raise ValueError("outputs apply only with isolation: clean")
        return
    if not checker.outputs:
        raise ValueError("isolation: clean needs outputs, the files the checker may see")
    validate_patterns(checker.outputs)
    blanket = [p for p in checker.outputs if not p.replace("/", "").replace(".", "").strip("*")]
    if blanket:
        raise ValueError(
            f"outputs {', '.join(blanket)} would copy everything, shims included; "
            "name the files or directories a solution may change"
        )
    if has_services:
        raise ValueError("isolation: clean does not support services")
    overlap = sorted(set(checker.outputs) & set(checker.protect))
    if overlap:
        raise ValueError(f"{', '.join(overlap)} is both an output and protected")


def _canonical(path: str) -> bool:
    parts = path.split("/")
    return bool(path) and not path.startswith("/") and all(p not in ("", ".", "..") for p in parts)


async def run_clean_checker(
    pool: EnvironmentPool, agent_env: TaskEnvironment, checker: CheckerConfig, task: Task
) -> tuple[TaskCheck, dict[str, object]]:
    """Grade the agent's declared outputs in a fresh sandbox. Returns the check and
    what was copied, for the record."""
    assert task.environment is not None
    try:
        listing = await _listing(agent_env)
        # The listing comes from a command the agent could have replaced: a path like
        # solution/../sitecustomize.py would match solution/** yet name a file outside it.
        forged = [p for p in listing if not _canonical(p)]
        if forged:
            return TaskCheck(
                passed=False, error=f"the sandbox listed a non-canonical path: {forged[0]!r}"
            ), {}
        paths = _selected(listing, checker.outputs)
        _, blobs = await _read(agent_env, paths, keep=True)
    except ProtectError as exc:
        return TaskCheck(passed=False, error=f"declared outputs could not be read: {exc}"), {}
    info: dict[str, object] = {"isolation": "clean", "outputs": sorted(blobs)}
    try:
        fresh = await pool.acquire(task.environment)
    except Exception as exc:  # a TaskError or SandboxError: the check cannot run
        return TaskCheck(passed=False, error=f"clean checker sandbox: {exc}"), info
    try:
        if blobs:
            await fresh.sandbox.write_files(dict(blobs))
        return await run_checker(fresh, checker, task), info
    except SandboxError as exc:
        return TaskCheck(passed=False, error=f"clean checker could not run: {exc}"), info
    finally:
        await fresh.sandbox.destroy()

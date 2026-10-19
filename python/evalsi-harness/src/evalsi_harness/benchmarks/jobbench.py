"""JobBench task directories as Evals.si agent tasks (judge-graded).

``jobbench://<dir>``
    A directory you supply (for example the benchmark's ``dataset/main``):
    ``<occupation>/<task>/task_folder/TASK_INSTRUCTIONS.txt`` plus the task's
    input files in ``task_folder/``, and, beside ``task_folder``, a
    ``RUBRICS.json`` that you supply. ``<dir>`` can also be one occupation or
    one task. The task id is ``<occupation>/<task>``.

The layout is read from Harness²'s JobBench adapter; it is not checked against
a copy of the benchmark, and nothing from the benchmark is shipped here.
JobBench's rubrics are public online, so they are a contamination risk: this
loader copies only ``task_folder/`` into the sandbox, never ``RUBRICS.json`` or
the ``expected/``, ``reference/`` and ``eval_result/`` folders Harness² treats
as answer material, and it keeps the rubric out of the instruction. Keep the
rubric where an agent with web or file access cannot reach it.

JobBench is graded by an LLM judge against the rubric, which this loader
cannot reproduce. The task has no checker, so ``task-success`` skips it. The
parsed ``RUBRICS.json`` (a ``rubrics`` list with a ``criterion`` and a
``weight`` each, as far as the adapter shows) is carried in
``metadata.grading.rubric`` for a judge evaluator you configure; without the
file, ``rubric`` is null and the task is only runnable. No judge is chosen for
you.

Options (query string): ``tasks`` (comma-separated ids or names, globs
allowed), ``image``, ``workdir`` (default ``/workspace``), ``network``
(``deny``, the default, or ``allow``), ``rubric=false`` (leave the rubric out
of the records).
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from evalsi_harness.benchmarks.harbor import TaskFormatError
from evalsi_harness.benchmarks.lab import (
    DEFAULT_WORKDIR,
    discover,
    grading,
    select,
    tree,
    workspace,
)

INSTRUCTIONS = "task_folder/TASK_INSTRUCTIONS.txt"


def load_jobbench(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``jobbench://<dataset dir>?options``."""
    rows = []
    for task_id, d in select(discover(path, INSTRUCTIONS), options.get("tasks", ""), path):
        try:
            rows.append(_jobbench_task(task_id, d, options))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{task_id}: {exc}") from None
    return rows


def _jobbench_task(task_id: str, d: Path, options: dict[str, str]) -> dict[str, Any]:
    try:
        instruction = (d / INSTRUCTIONS).read_text(encoding="utf-8").strip()
    except UnicodeDecodeError:
        raise TaskFormatError("TASK_INSTRUCTIONS.txt must be a text file") from None
    if not instruction:
        raise TaskFormatError("TASK_INSTRUCTIONS.txt is empty")
    rubric: Any = None
    rubric_file = d / "RUBRICS.json"
    if rubric_file.is_file() and options.get("rubric", "true").lower() not in (
        "0",
        "false",
        "no",
        "none",
    ):
        try:
            rubric = json.loads(rubric_file.read_text(encoding="utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise TaskFormatError(f"RUBRICS.json is not valid JSON: {exc}") from None
    workdir = options.get("workdir") or DEFAULT_WORKDIR
    files = tree(d / "task_folder", f"{workdir}/task_folder")
    return {
        "id": task_id,
        "input": instruction,
        "metadata": {
            "benchmark": "jobbench",
            "grading": grading(rubric, benchmark="jobbench", source="RUBRICS.json"),
            "jobbench": {"occupation": task_id.split("/", 1)[0]},
            "environment": workspace(files, options, workdir),
        },
    }


__all__ = ["load_jobbench"]

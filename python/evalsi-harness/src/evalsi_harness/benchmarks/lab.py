"""LAB task directories as Evals.si agent tasks (judge-graded).

``lab://<dir>``
    A directory you supply (for example a checkout of the benchmark's
    ``tasks/``): ``<practice-area>/<task>/task.json`` plus a ``documents/``
    folder of source files. ``<dir>`` can also be one practice area or one
    task. The task id is ``<practice-area>/<task>``.

The format is read from Harness²'s LAB adapter (``task.json`` keys ``title``,
``work_type``, ``tags``, ``instructions``, ``deliverables`` and ``criteria``,
and a ``documents/`` data room). It is not checked against a copy of the
benchmark, and nothing from the benchmark is shipped here.

LAB is graded by an LLM judge against ``criteria``, which this loader cannot
reproduce. The task has no checker, so ``task-success`` skips it. The criteria
are carried, untouched, in ``metadata.grading.rubric`` for a judge evaluator
you configure; they are never put in the instruction or in the sandbox's
files. No judge is chosen for you. The shape of ``criteria`` is not
interpreted (it is not documented in what was read), so a rubric text for the
judge is yours to build from it.

Options (query string): ``tasks`` (comma-separated ids or names, globs
allowed), ``image`` (the sandbox image; LAB's documents need Pandoc and
Poppler to read, which you provide), ``workdir`` (default ``/workspace``),
``network`` (``deny``, the default, or ``allow``), ``rubric=false`` (leave the
criteria out of the records).
"""

from __future__ import annotations

import fnmatch
import json
import shlex
from pathlib import Path
from typing import Any

from evalsi_harness.benchmarks.harbor import TaskFormatError, _put, decode_command

DEFAULT_WORKDIR = "/workspace"
LAYOUT = (
    "The source documents are in `documents/` (read-only). "
    "Write every deliverable to `output/<filename>`."
)


def discover(root_path: str, marker: str) -> list[tuple[str, Path]]:
    """(task id, directory) for each task under ``root_path``: the directory
    itself, its children, or its grandchildren, whichever hold ``marker``.
    The id is always ``<group>/<task>``."""
    root = Path(root_path)
    if not root.is_dir():
        raise TaskFormatError(f"{root_path} is not a directory")
    found: list[tuple[str, Path]] = []
    if (root / marker).is_file():
        found.append((f"{root.resolve().parent.name}/{root.resolve().name}", root))
    for child in sorted(p for p in root.iterdir() if p.is_dir()):
        if (child / marker).is_file():
            found.append((f"{root.resolve().name}/{child.name}", child))
        else:
            for grand in sorted(p for p in child.iterdir() if p.is_dir()):
                if (grand / marker).is_file():
                    found.append((f"{child.name}/{grand.name}", grand))
    if not found:
        raise TaskFormatError(f"no {marker} in {root_path} or two levels below it")
    return found


def select(found: list[tuple[str, Path]], wanted: str, root_path: str) -> list[tuple[str, Path]]:
    names = [w for w in wanted.split(",") if w]
    if not names:
        return found
    chosen = [
        (i, d)
        for i, d in found
        if any(fnmatch.fnmatch(i, w) or fnmatch.fnmatch(d.name, w) for w in names)
    ]
    missing = [
        w
        for w in names
        if not any(fnmatch.fnmatch(i, w) or fnmatch.fnmatch(d.name, w) for i, d in found)
    ]
    if missing:
        raise TaskFormatError(f"no tasks {missing} in {root_path}")
    return chosen


def tree(directory: Path, into: str) -> dict[str, str]:
    """Every file under ``directory`` as sandbox files at ``into/...`` (binary
    files base64-encoded, as the Harbor importer does). A symlink that leaves
    the directory is refused, so a task cannot pull in files from elsewhere."""
    if directory.is_symlink():
        # Resolving a linked directory would make wherever it points the trusted root.
        raise TaskFormatError(f"{directory.name} is a symlink; it must be a directory in the task")
    root = directory.resolve()
    files: dict[str, str] = {}
    for f in sorted(p for p in directory.rglob("*") if p.is_file()):
        real = f.resolve()
        if root not in (real, *real.parents):
            raise TaskFormatError(f"{f.relative_to(directory)} links outside {directory.name}")
        _put(files, real, f"{into}/{f.relative_to(directory).as_posix()}")
    return files


def workspace(files: dict[str, str], options: dict[str, str], workdir: str) -> dict[str, Any]:
    """The ``metadata.environment`` for a judge-graded task: the files, an
    ``output/`` directory, and no checker."""
    network = options.get("network") or "deny"
    if network not in ("allow", "deny"):
        raise TaskFormatError(f"network must be allow or deny, not {network!r}")
    setup = [f"mkdir -p {shlex.quote(workdir + '/output')}"]
    if decode := decode_command(files):
        setup.insert(0, decode)
    environment: dict[str, Any] = {
        "files": files,
        "setup": setup,
        "sandbox": {"workdir": workdir, "network": network, "min_isolation": "namespaced"},
    }
    if options.get("image"):
        environment["image"] = options["image"]
    return environment


def grading(rubric: Any, *, benchmark: str, source: str) -> dict[str, Any]:
    """The note a judge evaluator (and a reader of the results) needs: this
    task is graded by a judge against a rubric, not by a checker."""
    return {
        "method": "llm-judge",
        "reproduced_offline": False,
        "benchmark": benchmark,
        "rubric_source": source,
        "rubric": rubric,
        # The files the agent leaves under output/ are collected for the judge
        # (evalsi_harness.workspace), so a work product is graded, not described.
        "workspace": {"paths": ["output"]},
    }


def load_lab(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``lab://<tasks dir>?options``."""
    rows = []
    for task_id, d in select(discover(path, "task.json"), options.get("tasks", ""), path):
        try:
            rows.append(_lab_task(task_id, d, options))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{task_id}: {exc}") from None
    return rows


def _lab_task(task_id: str, d: Path, options: dict[str, str]) -> dict[str, Any]:
    try:
        data = json.loads((d / "task.json").read_text(encoding="utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise TaskFormatError(f"task.json is not valid JSON: {exc}") from None
    if not isinstance(data, dict):
        raise TaskFormatError("task.json must be an object")
    instructions = str(data.get("instructions") or "").strip()
    if not instructions:
        raise TaskFormatError("task.json has no instructions")
    criteria = data.pop("criteria", None)
    deliverables = data.get("deliverables") or {}
    if not isinstance(deliverables, dict):
        raise TaskFormatError("deliverables must be an object of filename to description")
    tags = [str(t) for t in data.get("tags") or []]
    parts = [
        f"# TASK {task_id}",
        f"**Title:** {data.get('title', '')}",
        f"**Work type:** {data.get('work_type', '')}",
    ]
    if tags:
        parts.append("**Tags:** " + ", ".join(tags))
    parts.append("\n## Instructions\n" + instructions)
    if deliverables:
        lines = [
            f"- `{name}`" + (f" — {desc}" if desc and desc != name else "")
            for name, desc in deliverables.items()
        ]
        parts.append("\n## Deliverables\n" + "\n".join(lines))
    parts.append("\n" + LAYOUT)
    workdir = options.get("workdir") or DEFAULT_WORKDIR
    docs = d / "documents"
    files = tree(docs, f"{workdir}/documents") if docs.is_dir() else {}
    keep = options.get("rubric", "true").lower() not in ("0", "false", "no", "none")
    return {
        "id": task_id,
        "input": "\n\n".join(parts),
        "metadata": {
            "benchmark": "lab",
            "grading": grading(
                criteria if keep else None, benchmark="lab", source="task.json criteria"
            ),
            "lab": {
                "practice_area": task_id.split("/", 1)[0],
                "work_type": data.get("work_type"),
                "title": data.get("title"),
                "tags": tags,
                "deliverables": sorted(deliverables),
            },
            "environment": workspace(files, options, workdir),
        },
    }


__all__ = ["load_lab"]

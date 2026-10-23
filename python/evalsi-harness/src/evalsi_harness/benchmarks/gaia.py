"""GAIA tasks as Evals.si agent tasks.

``gaia://<dir>?options``
    A directory you supply that holds GAIA's ``metadata.jsonl`` and the attachment files
    beside it (the Hugging Face dataset keeps one such directory per split, for example
    ``2023/validation``), or the ``metadata.jsonl`` file itself.

Targets the layout of the ``gaia-benchmark/GAIA`` dataset: one JSON object per line with
``task_id``, ``Question``, ``Level`` (1 to 3), ``Final answer`` (``?`` in the test split),
``file_name`` (empty when the question has no attachment), ``file_path`` and ``Annotator
Metadata``. This is written from the dataset card and the GAIA paper as remembered, and
**checked against no copy**: the dataset is gated, Hugging Face was not reachable when this
was written, and the fixture the tests use is synthetic. The loader reads only the
documented fields and ignores the rest.

**Licence.** The dataset is gated and asks that its questions and answers not be reshared
where crawlers can read them. Nothing from it is vendored here; the loader reads files you
downloaded under your own access. The records it builds carry the reference answer, so keep
the run's results and your dataset snapshots out of public places. ``Annotator Metadata``
(the annotators' solution steps) is dropped, not carried.

The task text is GAIA's published instruction (answer with ``FINAL ANSWER: ...``) followed by
the question, and, when there is an attachment, where it is. Scoring is
``builtin/gaia-match`` (quasi-exact match, see ``evalsi.packs.answers``). The test split has
no answers: its records have no reference and are not scored.

Options (query string): ``level`` (``1,2``), ``tasks`` (task ids, globs allowed),
``attachments=false`` (leave attachment files out), ``web`` (``deny``, the default, or
``allow``: network in the sandbox and a browser that may reach any http(s) host), ``image``
(the sandbox image; the questions need a Python and the tools the task wants, which you
provide), ``workdir`` (default ``/workspace``).
"""

from __future__ import annotations

import fnmatch
import json
import shlex
from pathlib import Path
from typing import Any

from evalsi_harness.benchmarks.dockerfile import TaskFormatError, decode_command
from evalsi_harness.benchmarks.dockerfile import put_file as _put

DEFAULT_WORKDIR = "/workspace"
UPSTREAM = "gaia-benchmark/GAIA metadata.jsonl (from the dataset card; not checked against a copy)"
PROMPT = (
    "You are a general AI assistant. I will ask you a question. Report your thoughts, and "
    "finish your answer with the following template: FINAL ANSWER: [YOUR FINAL ANSWER].\n"
    "YOUR FINAL ANSWER should be a number OR as few words as possible OR a comma separated "
    "list of numbers and/or strings.\n"
    "If you are asked for a number, don't use comma to write your number neither use units "
    "such as $ or percent sign unless specified otherwise.\n"
    "If you are asked for a string, don't use articles, neither abbreviations (e.g. for "
    "cities), and write the digits in plain text unless specified otherwise.\n"
    "If you are asked for a comma separated list, apply the above rules depending of whether "
    "the element to be put in the list is a number or a string."
)


def _metadata_file(path: str) -> Path:
    p = Path(path)
    if p.is_file():
        return p
    if p.is_dir():
        for candidate in (p / "metadata.jsonl", *sorted(p.glob("*/metadata.jsonl"))):
            if candidate.is_file():
                return candidate
    raise TaskFormatError(f"no metadata.jsonl in {path} or one level below it")


def _rows(file: Path) -> list[dict[str, Any]]:
    rows = []
    for n, line in enumerate(file.read_text(encoding="utf-8").splitlines(), start=1):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise TaskFormatError(f"{file.name} line {n} is not valid JSON: {exc}") from None
        if not isinstance(row, dict) or not row.get("task_id") or not row.get("Question"):
            raise TaskFormatError(f"{file.name} line {n}: a task needs task_id and Question")
        rows.append(row)
    if not rows:
        raise TaskFormatError(f"{file.name} has no tasks")
    return rows


def load_gaia(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``gaia://<dir>?options``."""
    unknown = set(options) - {"level", "tasks", "attachments", "web", "image", "workdir"}
    if unknown:
        raise TaskFormatError(f"unknown option(s) {', '.join(sorted(unknown))}")
    web = options.get("web") or "deny"
    if web not in ("allow", "deny"):
        raise TaskFormatError(f"web must be allow or deny, not {web!r}")
    file = _metadata_file(path)
    levels = {x.strip() for x in options.get("level", "").split(",") if x.strip()}
    names = [x for x in options.get("tasks", "").split(",") if x]
    rows = [
        r
        for r in _rows(file)
        if (not levels or str(r.get("Level")) in levels)
        and (not names or any(fnmatch.fnmatch(str(r["task_id"]), n) for n in names))
    ]
    if not rows:
        raise TaskFormatError(
            f"no tasks match level={options.get('level', '')!r} "
            f"tasks={options.get('tasks', '')!r} in {file}"
        )
    workdir = options.get("workdir") or DEFAULT_WORKDIR
    keep = options.get("attachments", "true").lower() not in ("0", "false", "no")
    return [_task(r, file.parent, options, web, workdir, keep) for r in rows]


def _task(  # noqa: PLR0917
    row: dict[str, Any], root: Path, options: dict[str, str], web: str, workdir: str, keep: bool
) -> dict[str, Any]:
    task_id = str(row["task_id"])
    name = str(row.get("file_name") or "")
    files: dict[str, str] = {}
    where = ""
    if name and keep:
        # A name, never a path: a dataset row cannot reach outside its own directory.
        safe = Path(name).name
        src = root / safe
        if safe != name or not src.is_file():
            raise TaskFormatError(
                f"{task_id}: attachment {name!r} is not a file beside {root.name}"
            )
        _put(files, src.resolve(), f"{workdir}/attachments/{safe}")
        where = f"{workdir}/attachments/{safe}"
    text = PROMPT + "\n\n" + str(row["Question"]).strip()
    if where:
        text += f"\n\nThe question comes with a file: {where}"
    elif name:
        text += f"\n\nThe question comes with a file ({name}) that is not provided."
    setup = [f"mkdir -p {shlex.quote(workdir + '/attachments')}"]
    if decode := decode_command(files):
        setup.insert(0, decode)
    environment: dict[str, Any] = {
        "files": files,
        "setup": setup,
        "sandbox": {"workdir": workdir, "network": web, "min_isolation": "namespaced"},
    }
    if options.get("image"):
        environment["image"] = options["image"]
    answer = str(row.get("Final answer") or "").strip()
    scored = answer not in ("", "?")
    meta: dict[str, Any] = {
        "benchmark": "gaia",
        "gaia": {
            "level": row.get("Level"),
            "file_name": name,
            "attachment_provided": bool(where),
            "scored": scored,
            "upstream": UPSTREAM,
        },
        "environment": environment,
        "grading": {
            "method": "gaia-quasi-exact-match",
            "evaluators": ["builtin/gaia-match"],
            "reproduced_offline": scored,
        },
    }
    if web == "allow":
        meta["browser"] = {"allow_hosts": ["*"]}
    record: dict[str, Any] = {"id": task_id, "input": text, "metadata": meta}
    if scored:
        record["reference"] = answer
    return record


__all__ = ["load_gaia"]

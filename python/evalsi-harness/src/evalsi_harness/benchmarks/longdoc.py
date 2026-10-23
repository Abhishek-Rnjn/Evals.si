"""Long-document question answering: OfficeQA (Pro, Full, Pro V2) and AA-LCR.

``officeqa://<csv>?options``
    Databricks's OfficeQA question file (``officeqa_pro.csv``, ``officeqa_full.csv`` or
    ``officeqa_pro_v2.csv``).
``aa-lcr://<file>?options``
    Artificial Analysis's AA-LCR question file (CSV or JSONL).

**What each targets.** OfficeQA: the schema in ``databricks/officeqa``'s README on ``main``
(read 2026-10-22): ``uid``, ``question``, ``answer``, ``source_docs``, ``source_files``
(parsed corpus file names, ``;``-separated, such as ``treasury_bulletin_1941_01.txt``) and,
for Pro and Full, ``difficulty``. Its scorer is the repository's ``reward.py`` (see
``builtin/officeqa-reward``). Its datasets are CC-BY-SA 4.0 and gated; its code Apache-2.0
(per the README). AA-LCR: **reconstructed from memory of the dataset card and checked against
no copy** (Hugging Face was not reachable): columns ``document_category``,
``document_set_id``, ``question_id``, ``question``, ``answer`` and ``data_source_filenames``
(``;``-separated), the documents as text files under ``<category>/<set id>/``. Column names
are matched case-insensitively with a few aliases, and a file that does not have these
columns is an error, not a guess. Nothing from either benchmark is vendored; the loaders read
files and directories you downloaded under your own access.

**Hosting the corpus.** A record carries its sandbox files inline, so a 460 MB corpus cannot
be copied into every task. Two arrangements are built, both file-based:

* ``docs=oracle`` (OfficeQA's default; AA-LCR always works this way): each task's own source
  documents are copied to ``<workdir>/corpus/`` (OfficeQA, from ``corpus=<dir>``, searched
  recursively by file name) or ``<workdir>/documents/`` (AA-LCR, from ``docs=<dir>``). The agent
  never searches the other documents, so this is the *easier*, oracle-document setting, and
  its numbers are not comparable with a run over the whole corpus. For AA-LCR it is the
  benchmark's own setting: each question comes with its document set.
* ``docs=image`` (OfficeQA): no documents are copied. ``image=<ref>`` must be a sandbox image
  (for example ``dir:<unpacked root>``) with the whole parsed corpus under ``/corpus`` and
  the tools the agent needs (a shell with ``grep``). This is the full-corpus retrieval
  setting; the loader cannot check the image, and none was built or run here. A corpus *service*
  on the pod backend (the multi-service environments) is not used.

Options (query string). OfficeQA: ``corpus`` (the directory of parsed files, for ``docs=oracle``),
``docs`` (``oracle`` or ``image``), ``difficulty`` (``easy`` or ``hard``), ``tasks`` (uids, globs
allowed), ``image``, ``workdir`` (default ``/workspace``), ``network`` (``deny``, the default,
or ``allow``). AA-LCR: ``docs`` (the directory of document sets; required), ``tasks``, ``image``,
``workdir``, ``network``.

Answers: OfficeQA asks for ``<FINAL_ANSWER>...</FINAL_ANSWER>``, which ``reward.py`` reads.
AA-LCR is graded by a judge that compares the answer with the reference (the benchmark's own
judge is a specific model and prompt that is not reproduced): use ``builtin/llm-judge`` with the
``correctness`` rubric, uncalibrated for this use. Both carry the reference answer in the record,
never in the sandbox or the instruction.
"""

from __future__ import annotations

import csv
import fnmatch
import json
import re
import shlex
from pathlib import Path
from typing import Any

from evalsi_harness.benchmarks.dockerfile import TaskFormatError, decode_command
from evalsi_harness.benchmarks.dockerfile import put_file as _put

DEFAULT_WORKDIR = "/workspace"
OFFICEQA_UPSTREAM = "databricks/officeqa README and reward.py, main, 2026-10-22"
AALCR_UPSTREAM = "ArtificialAnalysis/AA-LCR dataset card as remembered; not checked against a copy"
OFFICEQA_NOTE = (
    "\n\nThe source documents are in {where}. Find the figures you need in them and work out "
    "the answer. Reply with the final answer only, inside <FINAL_ANSWER></FINAL_ANSWER> tags: "
    "a single line holding just the value (with its unit if the question gives one)."
)
AALCR_NOTE = (
    "\n\nThe documents that contain what you need are in {where}. Answer from them. Reply with "
    "the answer on its own, as briefly as the question allows."
)


def _read_table(path: str) -> list[dict[str, str]]:
    file = Path(path)
    if not file.is_file():
        raise TaskFormatError(f"{path} is not a file")
    try:
        if file.suffix.lower() == ".jsonl":
            rows = [
                json.loads(x) for x in file.read_text(encoding="utf-8").splitlines() if x.strip()
            ]
        elif file.suffix.lower() == ".csv":
            with file.open(encoding="utf-8-sig", newline="") as handle:
                rows = list(csv.DictReader(handle))
        else:
            raise TaskFormatError(
                f"{file.name}: read CSV or JSONL; convert other formats (Parquet) first"
            )
    except (UnicodeDecodeError, json.JSONDecodeError, csv.Error) as exc:
        raise TaskFormatError(f"{file.name} could not be read: {exc}") from None
    if not rows or not all(isinstance(r, dict) for r in rows):
        raise TaskFormatError(f"{file.name} has no rows")
    return [
        {str(k).strip().lower(): ("" if v is None else str(v)) for k, v in r.items()} for r in rows
    ]


def _column(row: dict[str, str], *names: str) -> str:
    for name in names:
        if name in row:
            return row[name]
    raise TaskFormatError(f"missing column {names[0]!r} (have: {', '.join(sorted(row))})")


def _names(cell: str) -> list[str]:
    return [n.strip() for n in re.split(r"[;\n]", cell) if n.strip()]


def _index(root: Path) -> dict[str, list[Path]]:
    found: dict[str, list[Path]] = {}
    for p in sorted(root.rglob("*")):
        if p.is_file() and not p.is_symlink():
            found.setdefault(p.name, []).append(p)
    return found


def _environment(files: dict[str, str], options: dict[str, str], workdir: str) -> dict[str, Any]:
    network = options.get("network") or "deny"
    if network not in ("allow", "deny"):
        raise TaskFormatError(f"network must be allow or deny, not {network!r}")
    setup = [f"mkdir -p {shlex.quote(workdir)}"]
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


def _chosen(ids: list[str], spec: str) -> set[str] | None:
    patterns = [p for p in spec.split(",") if p]
    if not patterns:
        return None
    chosen = {i for i in ids if any(fnmatch.fnmatch(i, p) for p in patterns)}
    missing = [p for p in patterns if not any(fnmatch.fnmatch(i, p) for i in ids)]
    if missing:
        raise TaskFormatError(f"no tasks {missing}")
    return chosen


# --- OfficeQA ---


def load_officeqa(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``officeqa://<csv>?options``."""
    unknown = set(options) - {
        "corpus",
        "docs",
        "difficulty",
        "tasks",
        "image",
        "workdir",
        "network",
    }
    if unknown:
        raise TaskFormatError(f"unknown option(s) {', '.join(sorted(unknown))}")
    mode = options.get("docs") or "oracle"
    if mode not in ("oracle", "image"):
        raise TaskFormatError(f"docs must be oracle or image, not {mode!r}")
    if mode == "image" and not options.get("image"):
        raise TaskFormatError("docs=image needs image=<a sandbox image with the corpus at /corpus>")
    if mode == "oracle" and not options.get("corpus"):
        raise TaskFormatError("docs=oracle needs corpus=<the directory of parsed corpus files>")
    table = _read_table(path)
    ids = [_column(r, "uid") for r in table]
    if len(set(ids)) != len(ids):
        raise TaskFormatError("uids are not unique")
    chosen = _chosen(ids, options.get("tasks", ""))
    difficulty = options.get("difficulty", "").lower()
    corpus: dict[str, list[Path]] = {}
    if mode == "oracle":
        root = Path(options["corpus"])
        if not root.is_dir():
            raise TaskFormatError(f"corpus={root} is not a directory")
        corpus = _index(root)
    workdir = options.get("workdir") or DEFAULT_WORKDIR
    rows = []
    for row in table:
        uid = _column(row, "uid")
        if chosen is not None and uid not in chosen:
            continue
        if difficulty and row.get("difficulty", "").lower() != difficulty:
            continue
        try:
            rows.append(_officeqa_task(row, mode, corpus, options, workdir))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{uid}: {exc}") from None
    if not rows:
        raise TaskFormatError("no tasks match the options")
    return rows


def _officeqa_task(
    row: dict[str, str],
    mode: str,
    corpus: dict[str, list[Path]],
    options: dict[str, str],
    workdir: str,
) -> dict[str, Any]:
    question = _column(row, "question").strip()
    answer = _column(row, "answer").strip()
    if not question or not answer:
        raise TaskFormatError("a task needs a question and an answer")
    sources = _names(_column(row, "source_files"))
    files: dict[str, str] = {}
    if mode == "oracle":
        absent = [n for n in sources if n not in corpus]
        if absent:
            raise TaskFormatError(f"corpus files not found: {', '.join(absent)}")
        for name in sources:
            if len(corpus[name]) > 1:
                raise TaskFormatError(f"corpus has more than one file named {name}")
            _put(files, corpus[name][0].resolve(), f"{workdir}/corpus/{name}")
        where = f"{workdir}/corpus/"
    else:
        where = "/corpus/ (the whole corpus, read-only; find the right documents yourself)"
    meta_official: dict[str, Any] = {
        "uid": _column(row, "uid"),
        "difficulty": row.get("difficulty", ""),
        "source_docs": row.get("source_docs", ""),
        "source_files": sources,
        "docs": mode,
        "upstream": OFFICEQA_UPSTREAM,
    }
    return {
        "id": _column(row, "uid"),
        "input": question + OFFICEQA_NOTE.format(where=where),
        "reference": answer,
        "metadata": {
            "benchmark": "officeqa",
            "officeqa": meta_official,
            "environment": _environment(files, options, workdir),
            "grading": {
                "method": "officeqa-reward",
                "evaluators": ["builtin/officeqa-reward"],
                "reproduced_offline": True,
                "oracle_documents": mode == "oracle",
            },
        },
    }


# --- AA-LCR ---


def load_aalcr(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``aa-lcr://<question file>?options``."""
    unknown = set(options) - {"docs", "tasks", "image", "workdir", "network"}
    if unknown:
        raise TaskFormatError(f"unknown option(s) {', '.join(sorted(unknown))}")
    if not options.get("docs"):
        raise TaskFormatError("aa-lcr:// needs docs=<the directory of document sets>")
    root = Path(options["docs"])
    if not root.is_dir():
        raise TaskFormatError(f"docs={root} is not a directory")
    table = _read_table(path)
    ids = [_column(r, "question_id", "id") for r in table]
    if len(set(ids)) != len(ids):
        raise TaskFormatError("question ids are not unique")
    chosen = _chosen(ids, options.get("tasks", ""))
    index = _index(root)
    workdir = options.get("workdir") or DEFAULT_WORKDIR
    out = []
    for row in table:
        qid = _column(row, "question_id", "id")
        if chosen is not None and qid not in chosen:
            continue
        try:
            out.append(_aalcr_task(row, root, index, options, workdir))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{qid}: {exc}") from None
    return out


def _aalcr_task(
    row: dict[str, str],
    root: Path,
    index: dict[str, list[Path]],
    options: dict[str, str],
    workdir: str,
) -> dict[str, Any]:
    question = _column(row, "question").strip()
    answer = _column(row, "answer").strip()
    if not question or not answer:
        raise TaskFormatError("a task needs a question and an answer")
    category = row.get("document_category", "")
    set_id = row.get("document_set_id", "")
    names = _names(_column(row, "data_source_filenames", "data_source_filename", "documents"))
    files: dict[str, str] = {}
    for name in names:
        # Prefer the set's own directory; fall back to a unique name anywhere under docs.
        own = root / category / set_id / name if category and set_id else None
        if own is not None and own.is_file() and not own.is_symlink():
            src = own
        elif len(index.get(name, [])) == 1:
            src = index[name][0]
        else:
            raise TaskFormatError(f"document {name} is not (uniquely) under {root}")
        _put(files, src.resolve(), f"{workdir}/documents/{name}")
    return {
        "id": _column(row, "question_id", "id"),
        "input": question + AALCR_NOTE.format(where=f"{workdir}/documents/"),
        "reference": answer,
        "metadata": {
            "benchmark": "aa-lcr",
            "aa_lcr": {
                "document_category": category,
                "document_set_id": set_id,
                "documents": names,
                "input_tokens": row.get("input_tokens", ""),
                "upstream": AALCR_UPSTREAM,
            },
            "environment": _environment(files, options, workdir),
            "grading": {
                "method": "llm-judge-equivalence",
                "evaluators": ["builtin/llm-judge (rubric: correctness)"],
                "reproduced_offline": False,
            },
        },
    }


__all__ = ["load_aalcr", "load_officeqa"]

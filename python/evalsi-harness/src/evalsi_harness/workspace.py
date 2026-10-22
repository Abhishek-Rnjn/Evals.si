"""The files an agent left in its workspace, as text a judge can read.

LAB and JobBench ask for work products (a memo, a spreadsheet, a deck), so a
judge that sees only the final answer grades what the agent *says* it made. A
task that names ``metadata.grading.workspace.paths`` (the ``lab://`` and
``jobbench://`` loaders name ``output``) has those files collected after the
agent finishes and before the checker, and put on the record as
``metadata.workspace_files``::

    {"paths": ["output"],
     "files": [{"path": "output/memo.docx", "bytes": 18211, "kind": "docx",
                "text": "...", "truncated": false}],
     "skipped": [{"path": "output/chart.png", "bytes": 5120,
                  "reason": "not a text-extractable type"}]}

What is extracted, and how:

* plain text (``.txt``, ``.md``, ``.csv``, ``.json``, ``.html``, source code and
  files with no extension that decode as UTF-8), read as it is;
* ``.docx``, ``.xlsx`` and ``.pptx``, read here from their XML with the
  standard library (paragraphs; cells as tab-separated rows per sheet; slide
  text), so the image needs nothing extra;
* ``.pdf``, through ``pdftotext`` in the sandbox (Poppler, which LAB needs
  anyway); without it the file is listed, not read.

Everything else (images, archives, legacy ``.doc``/``.xls``) is listed with its
size and the reason it was not read, so the judge knows it exists. Every bound
is fixed: files listed, bytes read per file, characters kept per file and in
total. Formatting, charts, formulas' results that were never computed, and
images inside documents are not seen, so this is still not the benchmarks' own
grading.
"""

from __future__ import annotations

import io
import posixpath
import re
import shlex
import zipfile
from collections.abc import Mapping, Sequence
from typing import Any
from xml.etree import ElementTree

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment

MAX_FILES = 200
MAX_READ_BYTES = 8 << 20
MAX_FILE_CHARS = 20_000
MAX_TOTAL_CHARS = 120_000
# A zip member is never inflated past this, whatever its header claims.
MAX_MEMBER_BYTES = 32 << 20

TEXT_EXTENSIONS = frozenset(
    ".txt .md .markdown .rst .csv .tsv .json .jsonl .yaml .yml .toml .ini .xml .html .htm "
    ".tex .log .sql .py .js .ts .r .sh .ipynb".split()
)
OFFICE = {".docx": "docx", ".xlsx": "xlsx", ".pptx": "pptx"}

_LIST = r"""for r in "$@"; do [ -e "$r" ] && find "$r" -type f; done 2>/dev/null |
  head -n {limit} | while IFS= read -r f; do
    printf '%s\t%s\n' "$(wc -c < "$f" 2>/dev/null | tr -d ' ')" "$f"
  done"""

_W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
_S = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
_A = "{http://schemas.openxmlformats.org/drawingml/2006/main}"


class ExtractError(ValueError):
    """The file could not be read as the type its name says."""


def paths_of(record_metadata: Mapping[str, Any]) -> list[str]:
    """The workspace paths a record asks to collect (relative, inside the
    workdir), or [] when it asks for none."""
    grading = record_metadata.get("grading")
    spec = grading.get("workspace") if isinstance(grading, Mapping) else None
    if not isinstance(spec, Mapping):
        return []
    out = []
    for p in spec.get("paths") or []:
        norm = posixpath.normpath(str(p))
        if norm.startswith(("/", "..")) or norm == ".":
            raise ValueError(f"grading.workspace path {p!r} must be relative to the workdir")
        out.append(norm)
    return out


def _member(zf: zipfile.ZipFile, name: str) -> bytes:
    with zf.open(name) as handle:
        data = handle.read(MAX_MEMBER_BYTES + 1)
    if len(data) > MAX_MEMBER_BYTES:
        raise ExtractError(f"{name} inflates past {MAX_MEMBER_BYTES} bytes")
    return data


def _xml(zf: zipfile.ZipFile, name: str) -> ElementTree.Element:
    try:
        return ElementTree.fromstring(_member(zf, name))
    except ElementTree.ParseError as exc:
        raise ExtractError(f"{name}: {exc}") from None


def _numbered(names: Sequence[str], pattern: str) -> list[str]:
    found = [(int(m.group(1)), n) for n in names if (m := re.fullmatch(pattern, n))]
    return [n for _, n in sorted(found)]


def _docx(zf: zipfile.ZipFile) -> str:
    root = _xml(zf, "word/document.xml")
    lines = []
    for para in root.iter(f"{_W}p"):
        text = "".join(t.text or "" for t in para.iter(f"{_W}t"))
        if text.strip():
            lines.append(text)
    return "\n".join(lines)


def _pptx(zf: zipfile.ZipFile) -> str:
    out = []
    for i, name in enumerate(_numbered(zf.namelist(), r"ppt/slides/slide(\d+)\.xml"), start=1):
        root = _xml(zf, name)
        texts = [t.text for t in root.iter(f"{_A}t") if t.text and t.text.strip()]
        out.append(f"--- slide {i} ---\n" + "\n".join(texts))
    return "\n".join(out)


def _xlsx(zf: zipfile.ZipFile) -> str:
    names = zf.namelist()
    shared: list[str] = []
    if "xl/sharedStrings.xml" in names:
        for si in _xml(zf, "xl/sharedStrings.xml").iter(f"{_S}si"):
            shared.append("".join(t.text or "" for t in si.iter(f"{_S}t")))
    titles: list[str] = []
    if "xl/workbook.xml" in names:
        titles = [s.get("name", "") for s in _xml(zf, "xl/workbook.xml").iter(f"{_S}sheet")]
    out = []
    sheets = _numbered(names, r"xl/worksheets/sheet(\d+)\.xml")
    for i, name in enumerate(sheets):
        rows = []
        for row in _xml(zf, name).iter(f"{_S}row"):
            cells = []
            for c in row.iter(f"{_S}c"):
                kind, v = c.get("t"), c.find(f"{_S}v")
                if kind == "s" and v is not None and (v.text or "").isdigit():
                    index = int(v.text or 0)
                    cells.append(shared[index] if index < len(shared) else "")
                elif kind == "inlineStr":
                    cells.append("".join(t.text or "" for t in c.iter(f"{_S}t")))
                else:
                    cells.append(v.text or "" if v is not None else "")
            if any(cells):
                rows.append("\t".join(cells))
        title = titles[i] if i < len(titles) else f"sheet{i + 1}"
        out.append(f"## {title}\n" + "\n".join(rows))
    return "\n".join(out)


def extract_office(data: bytes, kind: str) -> str:
    """The text of a ``docx``, ``xlsx`` or ``pptx`` file."""
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as zf:
            return {"docx": _docx, "xlsx": _xlsx, "pptx": _pptx}[kind](zf)
    except (zipfile.BadZipFile, KeyError) as exc:
        raise ExtractError(f"not a readable {kind} file: {exc}") from None


def _kind(path: str) -> str:
    ext = posixpath.splitext(path)[1].lower()
    if ext in OFFICE:
        return OFFICE[ext]
    if ext == ".pdf":
        return "pdf"
    if ext in TEXT_EXTENSIONS or ext == "":
        return "text"
    return ""


async def _pdf_text(env: TaskEnvironment, path: str) -> str:
    try:
        result = await env.sandbox.exec(
            ["sh", "-c", f"pdftotext -layout -q {shlex.quote(path)} - 2>&1"],
            timeout_s=60,
            env=env.config.env,
        )
    except SandboxError as exc:
        raise ExtractError(f"pdftotext failed: {exc}") from None
    if result.exit_code == 127:
        raise ExtractError("pdftotext (Poppler) is not in the image")
    if result.exit_code != 0:
        raise ExtractError(f"pdftotext exited {result.exit_code}")
    return result.stdout


async def collect(env: TaskEnvironment, paths: Sequence[str]) -> dict[str, Any]:
    """The workspace files under ``paths`` (relative to the workdir), read and
    bounded as the module describes."""
    out: dict[str, Any] = {"paths": list(paths), "files": [], "skipped": []}
    if not paths:
        return out
    try:
        listing = await env.sandbox.exec(
            ["sh", "-c", _LIST.format(limit=MAX_FILES + 1), "sh", *paths],
            timeout_s=60,
            env=env.config.env,
        )
    except SandboxError as exc:
        out["error"] = f"the workspace could not be listed: {exc}"
        return out
    entries: list[tuple[int, str]] = []
    for line in listing.stdout.splitlines():
        size, sep, path = line.partition("\t")
        if sep and path:
            entries.append((int(size) if size.isdigit() else -1, path.removeprefix("./")))
    if len(entries) > MAX_FILES:
        out["more_files"] = True
        entries = entries[:MAX_FILES]
    budget = MAX_TOTAL_CHARS
    for size, path in sorted(entries, key=lambda e: e[1]):
        kind = _kind(path)

        def skip(reason: str, size: int = size, path: str = path) -> None:
            out["skipped"].append({"path": path, "bytes": size, "reason": reason})

        if not kind:
            skip("not a text-extractable type")
            continue
        if size > MAX_READ_BYTES:
            skip(f"larger than {MAX_READ_BYTES} bytes")
            continue
        if budget <= 0:
            skip("the text budget for this task was used up")
            continue
        try:
            if kind == "pdf":
                text = await _pdf_text(env, path)
            else:
                data = (await env.sandbox.read_files([path], max_bytes=MAX_READ_BYTES)).get(path)
                if data is None:
                    skip("could not be read")
                    continue
                if kind == "text":
                    try:
                        text = data.decode("utf-8")
                    except UnicodeDecodeError:
                        skip("not UTF-8 text")
                        continue
                else:
                    text = extract_office(data, kind)
        except (ExtractError, SandboxError) as exc:
            skip(str(exc))
            continue
        keep = min(MAX_FILE_CHARS, budget)
        out["files"].append(
            {
                "path": path,
                "bytes": size,
                "kind": kind,
                "text": text[:keep],
                "truncated": len(text) > keep,
            }
        )
        budget -= min(len(text), keep)
    return out


__all__ = ["ExtractError", "collect", "extract_office", "paths_of"]

"""Workspace files for a judge (H7): what is read, what is only listed, and the bounds."""

from __future__ import annotations

import asyncio
import io
import json
import zipfile
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness import workspace
from evalsi_harness.benchmarks.lab import load_lab
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.task import EnvironmentConfig
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
S = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
A = "http://schemas.openxmlformats.org/drawingml/2006/main"


def zipped(files: dict[str, str]) -> bytes:
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        for name, body in files.items():
            zf.writestr(name, body)
    return buf.getvalue()


DOCX = zipped(
    {
        "word/document.xml": f'<w:document xmlns:w="{W}"><w:body>'
        "<w:p><w:r><w:t>Limitation is </w:t></w:r><w:r><w:t>six years.</w:t></w:r></w:p>"
        "<w:p><w:r><w:t></w:t></w:r></w:p><w:p><w:r><w:t>See Smith v Jones.</w:t></w:r></w:p>"
        "</w:body></w:document>"
    }
)
XLSX = zipped(
    {
        "xl/workbook.xml": f'<workbook xmlns="{S}"><sheets><sheet name="Totals"/></sheets>'
        "</workbook>",
        "xl/sharedStrings.xml": f'<sst xmlns="{S}"><si><t>Item</t></si><si><t>Rent</t></si></sst>',
        "xl/worksheets/sheet1.xml": f'<worksheet xmlns="{S}"><sheetData>'
        '<row><c t="s"><v>0</v></c><c><v>Amount</v></c></row>'
        '<row><c t="s"><v>1</v></c><c><v>1200</v></c></row></sheetData></worksheet>',
    }
)
PPTX = zipped(
    {
        f"ppt/slides/slide{i}.xml": f'<p:sld xmlns:p="x" xmlns:a="{A}"><a:t>{t}</a:t></p:sld>'
        for i, t in ((2, "Second"), (1, "First"), (10, "Tenth"))
    }
)


def collect(files: dict[str, bytes | str], paths: list[str], **limits: int) -> dict[str, Any]:
    sandboxes = LocalSandboxClient()

    async def go() -> dict[str, Any]:
        sandbox = await sandboxes.create()
        await sandbox.write_files(files)
        env = TaskEnvironment(sandbox, EnvironmentConfig())
        return await workspace.collect(env, paths)

    return asyncio.run(go())


def test_office_files_are_read_to_text_and_other_types_are_listed() -> None:
    out = collect(
        {
            "output/memo.docx": DOCX,
            "output/model.xlsx": XLSX,
            "output/deck.pptx": PPTX,
            "output/notes.md": "# Notes\nall good",
            "output/chart.png": b"\x89PNG\r\n\x1a\n" + bytes(range(200)),
            "output/blob": b"\xff\xfe\x00bad",
            "output/old.doc": b"binary",
        },
        ["output"],
    )
    by = {f["path"]: f for f in out["files"]}
    assert by["output/memo.docx"]["text"] == "Limitation is six years.\nSee Smith v Jones."
    assert by["output/model.xlsx"]["text"] == "## Totals\nItem\tAmount\nRent\t1200"
    # Slides in numeric order (10 after 2), each marked.
    deck = by["output/deck.pptx"]["text"]
    assert deck.index("First") < deck.index("Second") < deck.index("Tenth")
    assert by["output/notes.md"]["text"] == "# Notes\nall good"
    skipped = {s["path"]: s["reason"] for s in out["skipped"]}
    assert skipped["output/chart.png"] == "not a text-extractable type"
    assert skipped["output/old.doc"] == "not a text-extractable type"
    assert skipped["output/blob"] == "not UTF-8 text"


def test_a_corrupt_office_file_and_a_missing_pdf_reader_are_listed_not_fatal() -> None:
    out = collect({"output/bad.docx": b"not a zip", "output/a.pdf": b"%PDF-1.4"}, ["output"])
    reasons = {s["path"]: s["reason"] for s in out["skipped"]}
    assert "not a readable docx" in reasons["output/bad.docx"]
    assert "output/a.pdf" in reasons  # Poppler missing, or it rejected the file
    assert out["files"] == []


def test_bounds_on_file_text_and_total(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(workspace, "MAX_FILE_CHARS", 10)
    monkeypatch.setattr(workspace, "MAX_TOTAL_CHARS", 15)
    out = collect(
        {"output/a.txt": "x" * 50, "output/b.txt": "y" * 50, "output/c.txt": "z"}, ["output"]
    )
    a, b = out["files"]
    assert (len(a["text"]), a["truncated"]) == (10, True)
    assert (len(b["text"]), b["truncated"]) == (5, True)
    assert [s["reason"] for s in out["skipped"]] == ["the text budget for this task was used up"]


def test_file_count_and_missing_paths(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(workspace, "MAX_FILES", 2)
    out = collect({f"output/{i}.txt": "t" for i in range(4)}, ["output", "nowhere"])
    assert len(out["files"]) == 2
    assert out["more_files"] is True
    assert collect({}, ["output"])["files"] == []


def test_a_zip_bomb_member_is_refused(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(workspace, "MAX_MEMBER_BYTES", 100)
    bomb = zipped({"word/document.xml": "<a>" + "x" * 1000 + "</a>"})
    out = collect({"output/b.docx": bomb}, ["output"])
    assert "inflates past" in out["skipped"][0]["reason"]


def test_paths_must_stay_inside_the_workdir() -> None:
    assert workspace.paths_of({}) == []
    assert workspace.paths_of({"grading": {"workspace": {"paths": ["output/", "./a/b"]}}}) == [
        "output",
        "a/b",
    ]
    for bad in ("/etc", "../x", "."):
        with pytest.raises(ValueError, match="relative"):
            workspace.paths_of({"grading": {"workspace": {"paths": [bad]}}})


def test_lab_tasks_ask_for_their_output_directory(tmp_path: Path) -> None:
    task = tmp_path / "area" / "t1"
    (task / "documents").mkdir(parents=True)
    (task / "task.json").write_text(json.dumps({"instructions": "Do it", "criteria": ["c"]}))
    (row,) = load_lab(str(task))
    assert row["metadata"]["grading"]["workspace"] == {"paths": ["output"]}


def test_a_run_puts_the_agents_files_on_the_record(sandboxes: LocalSandboxClient) -> None:
    script = "printf 'The answer is 42.' > output/answer.txt; printf '\\001\\002' > output/x.bin"
    spec = spec_of(
        {
            "target": {"agent": {"cli": {"command": ["sh", "-c", script]}}},
            "environment": {"setup": ["mkdir -p output"]},
        }
    )
    rec = record(metadata={"grading": {"workspace": {"paths": ["output"]}}})
    out = run(spec, rec, sandboxes).record
    assert out is not None
    files = out.metadata["workspace_files"]
    assert [f["path"] for f in files["files"]] == ["output/answer.txt"]
    assert files["files"][0]["text"] == "The answer is 42."
    assert files["skipped"][0]["path"] == "output/x.bin"
    # A record that asks for nothing carries nothing.
    plain = run(spec, record(), sandboxes).record
    assert plain is not None
    assert "workspace_files" not in plain.metadata

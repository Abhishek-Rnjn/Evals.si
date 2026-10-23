"""The WebArena, GAIA, OfficeQA and AA-LCR importers (H4).

Every file under ``fixtures/webarena`` and ``fixtures/importers`` is SYNTHETIC: questions
invented in each benchmark's file format. No benchmark data is vendored, and none of these
loaders has run against the real datasets' questions in this suite."""

from __future__ import annotations

import base64
import json
import re
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi.datasets import record_from_row
from evalsi.packs.answers import gaia_match, webarena_string_match, webarena_url_match
from evalsi.testing import score
from evalsi.types import Record
from evalsi_harness.benchmarks.gaia import load_gaia
from evalsi_harness.benchmarks.harbor import TaskFormatError
from evalsi_harness.benchmarks.longdoc import load_aalcr, load_officeqa
from evalsi_harness.benchmarks.webarena import load_webarena
from evalsi_harness.task import environment_for
from evalsi_harness.testing import LocalSandboxClient, Script, ScriptedModelServer
from tests.harness_support import run, spec_of
from tests.test_browser import chromium, site  # noqa: F401  (fixtures)

FIX = Path(__file__).parent / "fixtures"
WEBARENA = FIX / "webarena" / "test.raw.json"
IMPORTERS = FIX / "importers"
URLS = {
    "shopping": "http://shop.test:7770",
    "shopping_admin": "http://admin.test:7780/admin",
    "gitlab": "http://git.test:8023",
    "map": "http://map.test:3000",
    "wikipedia": "http://wiki.test:8888",
}


def rec(row: dict[str, Any]) -> Record:
    return record_from_row(row, 0, {})


def env(row: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = row["metadata"]["environment"]
    return out


# --- WebArena ---


def test_webarena_tasks_get_site_urls_hosts_and_untouched_references() -> None:
    rows = load_webarena(str(WEBARENA), **URLS)
    assert [r["id"] for r in rows] == [f"webarena.{i}" for i in range(5)]
    first = rows[0]
    assert first["input"].startswith("Name the cheapest widget")
    assert "answer only" in first["input"]
    meta = first["metadata"]
    # Placeholders are resolved (start and reference URLs); hosts are exactly the sites given.
    assert meta["browser"]["start_url"] == "http://admin.test:7780/admin"
    assert meta["browser"]["allow_hosts"] == sorted(
        {"shop.test:7770", "admin.test:7780", "git.test:8023", "map.test:3000", "wiki.test:8888"}
    )
    assert meta["webarena"]["eval"]["reference_answers"] == {"exact_match": "Blue Widget"}
    assert meta["webarena"]["browsergym_id"] == "browsergym/webarena.0"
    url_task = rows[2]["metadata"]["webarena"]["eval"]
    assert url_task["reference_url"] == (
        "http://git.test:8023/fixture/alpha-lib/-/issues/?sort=created_asc&state=opened"
    )
    assert rows[2]["metadata"]["browser"]["start_url"] == "http://git.test:8023/fixture/alpha-lib"
    # What the agent sees never holds the answers.
    for row in rows:
        assert "Blue Widget" not in row["input"]
        assert "environment" not in row["metadata"]  # the browser toolset needs no sandbox


def test_webarena_says_what_a_task_needs_that_the_evaluators_do_not_check() -> None:
    rows = {r["id"]: r for r in load_webarena(str(WEBARENA), **URLS)}
    assert rows["webarena.0"]["metadata"]["grading"]["not_reproduced"] == []
    assert rows["webarena.3"]["metadata"]["grading"]["not_reproduced"] == [
        "program_html (reads the live page's DOM)"
    ]
    assert rows["webarena.4"]["metadata"]["grading"]["not_reproduced"] == [
        "fuzzy_match (an LLM judge)"
    ]
    assert rows["webarena.0"]["metadata"]["grading"]["reproduced_offline"] is False


def test_webarena_options_select_tasks_and_report_missing_sites(tmp_path: Path) -> None:
    only = load_webarena(str(WEBARENA), tasks="0,2-3", **URLS)
    assert [r["id"] for r in only] == ["webarena.0", "webarena.2", "webarena.3"]
    with pytest.raises(TaskFormatError, match=r"give the site URLs.*gitlab=<url>"):
        load_webarena(str(WEBARENA), tasks="1", shopping="http://s.test")
    with pytest.raises(TaskFormatError, match="no tasks"):
        load_webarena(str(WEBARENA), tasks="99", **URLS)
    with pytest.raises(TaskFormatError, match="http"):
        load_webarena(str(WEBARENA), shopping="ftp://x")
    with pytest.raises(TaskFormatError, match="unknown option"):
        load_webarena(str(WEBARENA), shoping="http://x")
    quiet = load_webarena(str(WEBARENA), tasks="0", answer="false", **URLS)
    assert quiet[0]["input"] == "Name the cheapest widget in the fixture catalogue"


def test_webarena_reads_a_directory_of_task_files_and_storage_states(tmp_path: Path) -> None:
    tasks = json.loads(WEBARENA.read_text())
    for t in tasks[:2]:
        (tmp_path / f"{t['task_id']}.json").write_text(json.dumps(t))
    auth = tmp_path / "auth"
    auth.mkdir()
    with pytest.raises(TaskFormatError, match="no storage state"):
        load_webarena(str(tmp_path), auth=str(auth), **URLS)
    (auth / "shopping_admin_state.json").write_text("{}")
    (auth / "gitlab_state.json").write_text("{}")
    rows = load_webarena(str(tmp_path), auth=str(auth), **URLS)
    assert [r["id"] for r in rows] == ["webarena.0", "webarena.1"]
    assert rows[0]["metadata"]["browser"]["storage_state"] == str(
        auth / "shopping_admin_state.json"
    )
    assert rows[0]["metadata"]["webarena"]["logged_in"] is True
    # Without auth the agent starts logged out, and the record says so.
    assert load_webarena(str(tmp_path), **URLS)[0]["metadata"]["webarena"]["logged_in"] is False
    empty = tmp_path / "empty"
    empty.mkdir()
    with pytest.raises(TaskFormatError, match=r"no test\.raw\.json"):
        load_webarena(str(empty), **URLS)


# --- WebArena, run against a local site with the browser tools ---


def write_local_task(tmp_path: Path, site_url: str, task: dict[str, Any]) -> list[dict[str, Any]]:
    config = {
        "sites": ["shopping"],
        "task_id": 7,
        "require_login": True,
        "storage_state": None,
        "start_url": "__SHOPPING__/index.html",
        "geolocation": None,
        "intent_template": "x",
        "instantiation_dict": {},
        "require_reset": False,
        "intent_template_id": 1,
        **task,
    }
    path = tmp_path / "test.raw.json"
    path.write_text(json.dumps([config]))
    return load_webarena(str(path), shopping=site_url)


def agent(model: Callable[[Script], ScriptedModelServer], script: Script) -> Any:
    server = model(script)
    return spec_of(
        {
            "target": {
                "connector": "openai-compatible",
                "model": "m",
                "base_url": server.base_url,
            },
            "harness": {"builtin": {"tools": {"sandbox": False, "builtin": ["browser"]}}},
        }
    )


def test_a_webarena_style_task_runs_in_the_browser_and_is_scored(
    site: str,  # noqa: F811
    chromium: None,  # noqa: F811
    tmp_path: Path,
    sandboxes: LocalSandboxClient,
    model: Callable[[Script], ScriptedModelServer],
) -> None:
    def ref_of(messages: list[dict[str, Any]], text: str) -> str:
        last = [m for m in messages if m["role"] == "tool"][-1]["content"]
        return re.search(rf'link "{text}".*?\[ref=(e\d+)\]', last).group(1)  # type: ignore[union-attr]

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        done = len([m for m in messages if m["role"] == "tool"])
        if done == 0:
            return {"tool_calls": [("browser_snapshot", {})]}
        if done == 1:
            return {"tool_calls": [("browser_click", {"ref": ref_of(messages, "About us")})]}
        return {"text": "Welcome"}

    spec = agent(model, script)
    rows = write_local_task(
        tmp_path,
        site,
        {
            "intent": "What is the main heading of the shop?",
            "eval": {
                "eval_types": ["string_match", "url_match"],
                "reference_answers": {"exact_match": "Welcome"},
                "reference_url": "__SHOPPING__/about.html",
                "program_html": [],
            },
        },
    )
    outcome = run(spec, rec(rows[0]), sandboxes)
    assert outcome.error == ""
    done = outcome.record
    assert done is not None
    text = score(webarena_string_match, done)["webarena-string-match"]
    assert text.passed is True
    assert "url_match" in text.explanation  # the string check says it did not check the URL
    assert score(webarena_url_match, done)["webarena-url-match"].passed is True
    # A run that stayed on the first page fails the URL part.
    wrong = write_local_task(
        tmp_path,
        site,
        {
            "intent": "Open the about page",
            "eval": {
                "eval_types": ["url_match"],
                "reference_answers": None,
                "reference_url": "__SHOPPING__/new.html",
                "program_html": [],
            },
        },
    )
    again = run(spec, rec(wrong[0]), sandboxes).record
    assert again is not None
    assert score(webarena_url_match, again)["webarena-url-match"].passed is False


# --- GAIA ---


def test_gaia_tasks_carry_the_prompt_the_attachment_and_the_reference() -> None:
    rows = {r["id"]: r for r in load_gaia(str(IMPORTERS / "gaia"))}
    assert list(rows) == ["fixture-0001", "fixture-0002", "fixture-0003"]
    one = rows["fixture-0001"]
    assert one["input"].startswith("You are a general AI assistant.")
    assert "FINAL ANSWER: [YOUR FINAL ANSWER]" in one["input"]
    assert "How many rows does the attached table have?" in one["input"]
    assert "/workspace/attachments/table.csv" in one["input"]
    assert one["reference"] == "3"
    assert env(one)["files"]["/workspace/attachments/table.csv"].startswith("a,b")
    assert env(one)["sandbox"]["network"] == "deny"
    # The annotators' steps are dropped, and a withheld answer is not a reference.
    assert "SECRET STEPS" not in json.dumps(rows)
    assert "reference" not in rows["fixture-0003"]
    assert rows["fixture-0003"]["metadata"]["gaia"]["scored"] is False
    assert rows["fixture-0002"]["metadata"]["gaia"]["level"] == 2
    task = environment_for(spec_of({}), rec(one))
    assert task is not None
    assert task.sandbox.workdir == "/workspace"


def test_gaia_options_and_binary_attachments(tmp_path: Path) -> None:
    assert [r["id"] for r in load_gaia(str(IMPORTERS / "gaia"), level="2,3")] == [
        "fixture-0002",
        "fixture-0003",
    ]
    assert len(load_gaia(str(IMPORTERS / "gaia"), tasks="fixture-0001")) == 1
    (row,) = load_gaia(str(IMPORTERS / "gaia-binary"))
    files = env(row)["files"]
    (name,) = files
    assert name.endswith("image.bin.evalsi-b64")
    assert base64.b64decode(files[name]) == bytes(range(256))
    assert "base64 -d" in env(row)["setup"][0]
    bare = load_gaia(str(IMPORTERS / "gaia"), attachments="false", tasks="fixture-0001")
    assert env(bare[0])["files"] == {}
    assert "is not provided" in bare[0]["input"]
    web = load_gaia(str(IMPORTERS / "gaia"), web="allow", tasks="fixture-0002")[0]
    assert env(web)["sandbox"]["network"] == "allow"
    assert web["metadata"]["browser"] == {"allow_hosts": ["*"]}
    with pytest.raises(TaskFormatError, match="web must be"):
        load_gaia(str(IMPORTERS / "gaia"), web="maybe")
    with pytest.raises(TaskFormatError, match="no tasks match"):
        load_gaia(str(IMPORTERS / "gaia"), level="9")
    with pytest.raises(TaskFormatError, match=r"no metadata\.jsonl"):
        load_gaia(str(tmp_path))


def test_gaia_attachment_names_cannot_leave_their_directory(tmp_path: Path) -> None:
    (tmp_path / "secret.txt").write_text("outside")
    inner = tmp_path / "split"
    inner.mkdir()
    row = {
        "task_id": "t",
        "Question": "q",
        "Level": 1,
        "Final answer": "a",
        "file_name": "../secret.txt",
    }
    (inner / "metadata.jsonl").write_text(json.dumps(row) + "\n")
    with pytest.raises(TaskFormatError, match="not a file beside"):
        load_gaia(str(inner))


def test_gaia_records_score_with_the_quasi_exact_match() -> None:
    rows = {r["id"]: r for r in load_gaia(str(IMPORTERS / "gaia"))}
    scored = rec(rows["fixture-0002"])
    scored.output = rec({"id": "o", "input": "FINAL ANSWER: Zorb Town"}).input
    assert score(gaia_match, scored)["gaia-match"].passed is True


# --- OfficeQA ---

CSV = IMPORTERS / "officeqa" / "officeqa_fixture.csv"
CORPUS = IMPORTERS / "officeqa" / "corpus"


def test_officeqa_oracle_mode_copies_only_the_questions_own_documents() -> None:
    rows = {r["id"]: r for r in load_officeqa(str(CSV), corpus=str(CORPUS))}
    one, two = rows["UID0001"], rows["UID0002"]
    assert one["reference"] == "123.4"
    assert "<FINAL_ANSWER></FINAL_ANSWER>" in one["input"]
    assert "123.4" not in one["input"]
    assert sorted(env(one)["files"]) == ["/workspace/corpus/treasury_bulletin_1941_01.txt"]
    assert sorted(env(two)["files"]) == [
        "/workspace/corpus/treasury_bulletin_1941_01.txt",
        "/workspace/corpus/treasury_bulletin_1941_03.txt",
    ]
    meta = one["metadata"]["officeqa"]
    assert meta["source_files"] == ["treasury_bulletin_1941_01.txt"]
    assert meta["difficulty"] == "hard"
    assert one["metadata"]["grading"]["oracle_documents"] is True
    assert env(one)["sandbox"]["network"] == "deny"
    # The unreferenced corpus file is never copied.
    assert "NOT REFERENCED" not in json.dumps(rows)


def test_officeqa_options_and_the_image_mode(tmp_path: Path) -> None:
    hard = load_officeqa(str(CSV), corpus=str(CORPUS), difficulty="hard")
    assert [r["id"] for r in hard] == ["UID0001"]
    assert [r["id"] for r in load_officeqa(str(CSV), corpus=str(CORPUS), tasks="UID0002")] == [
        "UID0002"
    ]
    (full, _) = load_officeqa(str(CSV), docs="image", image="dir:/images/officeqa")
    assert env(full)["files"] == {}
    assert env(full)["image"] == "dir:/images/officeqa"
    assert "/corpus/" in full["input"]
    assert full["metadata"]["grading"]["oracle_documents"] is False
    with pytest.raises(TaskFormatError, match="needs corpus="):
        load_officeqa(str(CSV))
    with pytest.raises(TaskFormatError, match="needs image="):
        load_officeqa(str(CSV), docs="image")
    with pytest.raises(TaskFormatError, match="UID0001: corpus files not found"):
        load_officeqa(str(CSV), corpus=str(tmp_path))
    with pytest.raises(TaskFormatError, match="no tasks"):
        load_officeqa(str(CSV), corpus=str(CORPUS), tasks="NOPE")
    with pytest.raises(TaskFormatError, match="missing column"):
        load_officeqa(str(IMPORTERS / "aalcr" / "aalcr_fixture.csv"), corpus=str(CORPUS))


# --- AA-LCR ---

LCR = IMPORTERS / "aalcr" / "aalcr_fixture.csv"
DOCS = IMPORTERS / "aalcr" / "docs"


def test_aalcr_questions_come_with_their_document_set() -> None:
    rows = {r["id"]: r for r in load_aalcr(str(LCR), docs=str(DOCS))}
    q1, q2 = rows["q-1"], rows["q-2"]
    assert q1["reference"] == "Acme Fixtures"
    assert "Acme Fixtures" not in q1["input"]
    assert sorted(env(q1)["files"]) == [
        "/workspace/documents/report_a.txt",
        "/workspace/documents/report_b.txt",
    ]
    assert sorted(env(q2)["files"]) == ["/workspace/documents/report_a.txt"]
    meta = q1["metadata"]["aa_lcr"]
    assert meta["document_category"] == "Fixture Reports"
    assert meta["input_tokens"] == "1200"
    assert q1["metadata"]["grading"]["reproduced_offline"] is False
    assert [r["id"] for r in load_aalcr(str(LCR), docs=str(DOCS), tasks="q-2")] == ["q-2"]
    with pytest.raises(TaskFormatError, match="needs docs="):
        load_aalcr(str(LCR))
    with pytest.raises(TaskFormatError, match="not a directory"):
        load_aalcr(str(LCR), docs=str(LCR))
    with pytest.raises(TaskFormatError, match=r"q-1: document report_a.txt is not"):
        load_aalcr(str(LCR), docs=str(IMPORTERS / "gaia"))
    with pytest.raises(TaskFormatError, match="read CSV or JSONL"):
        load_aalcr(str(IMPORTERS / "gaia" / "image.bin"), docs=str(DOCS))


def test_aalcr_reads_jsonl_with_aliases(tmp_path: Path) -> None:
    (tmp_path / "docs").mkdir()
    (tmp_path / "docs" / "a.txt").write_text("alpha")
    path = tmp_path / "q.jsonl"
    path.write_text(
        json.dumps(
            {"ID": "x", "Question": "Which?", "Answer": "alpha", "Data_Source_Filenames": "a.txt"}
        )
        + "\n"
    )
    (row,) = load_aalcr(str(path), docs=str(tmp_path / "docs"))
    assert row["id"] == "x"
    assert row["reference"] == "alpha"
    assert env(row)["files"] == {"/workspace/documents/a.txt": "alpha"}

"""Web-search and memory categories: the importer with BFCL's loaders, and the
harness end to end with a scripted model against BFCL's own backends. The web
search API is replaced by a canned one (the real one needs SerpAPI and the
network); the memory backends (key-value, recursive summary) are BFCL's."""

from __future__ import annotations

import asyncio
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from google.protobuf import json_format

from evalsi.runspec import normalize_durations
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_bfcl import AGENTIC, grade_agentic, load
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.testing import Script, ScriptedModelServer


@pytest.fixture
def model() -> Iterator[Any]:
    servers: list[ScriptedModelServer] = []

    def make(script: Script) -> ScriptedModelServer:
        servers.append(ScriptedModelServer(script))
        return servers[-1]

    yield make
    for s in servers:
        s.close()


def run_rows(server: ScriptedModelServer, rows: list[dict[str, Any]]) -> list[TaskOutcome]:
    """The rows, one after the other, through one harness (as one worker would)."""
    spec = json_format.ParseDict(
        normalize_durations(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "scripted",
                    "base_url": server.base_url,
                },
                "dataset": {"inline": {"records": []}},
                "evaluators": [{"ref": "task-success"}],
                "harness": {"external": {"python": "evalsi_bfcl:BFCLHarness"}},
            },
            run_pb2.RunSpec.DESCRIPTOR,
        ),
        run_pb2.RunSpec(),
    )

    async def go() -> list[TaskOutcome]:
        async def no_sandboxes() -> Any:
            raise AssertionError("BFCL needs no sandbox")

        harness = load_harness(spec, HarnessContext(sandboxes=no_sandboxes, base_dir=Path.cwd()))
        try:
            out = []
            for row in rows:
                record = Record(
                    id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"]
                )
                out.append(await run_task(Task.build(spec, record, trial=0, run_id="r"), harness))
            return out
        finally:
            await harness.aclose()

    return asyncio.run(go())


def test_the_importer_loads_agentic_categories() -> None:
    assert set(AGENTIC) == {
        "web_search_base",
        "web_search_no_snippet",
        "memory_kv",
        "memory_vector",
        "memory_rec_sum",
    }
    web = load("web_search_no_snippet", limit="2")
    assert [r["id"] for r in web] == ["web_search_no_snippet_0", "web_search_no_snippet_1"]
    meta = web[0]["metadata"]["bfcl"]
    assert web[0]["reference"] == ["Zhang Yiming"]
    assert meta["initial_config"] == {"WebSearchAPI": {"show_snippet": False}}
    assert {f["name"] for f in meta["functions"]} == {"search_engine_query", "fetch_url_content"}
    # BFCL's answer-format system prompt travels with the question.
    assert meta["messages"][0]["role"] == "system"
    assert "answer" in meta["messages"][0]["content"]
    base = load("web_search_base", limit="1")[0]["metadata"]["bfcl"]
    assert base["initial_config"] == {"WebSearchAPI": {"show_snippet": True}}

    memory = load("memory_kv", ids="memory_kv_0-customer-0")[0]
    assert memory["reference"] == ["Michael"]
    assert memory["metadata"]["bfcl"]["scenario"] == "customer"
    assert memory["metadata"]["bfcl"]["involved_classes"] == ["MemoryAPI_kv"]
    # Prerequisite conversations are not records; the harness replays them.
    every = load("memory_rec_sum")
    assert all("prereq" not in r["id"] for r in every)
    assert len(every) == 155
    with pytest.raises(ValueError, match="not supported"):
        load("format_sensitivity")


def test_grading_ignores_case_and_punctuation() -> None:
    assert grade_agentic(["Zhang Yiming"], "{'answer': 'zhang yiming', 'context': 'x'}").passed
    assert grade_agentic(["35", "thirty five"], "She is Thirty Five, as of 2025.").passed
    wrong = grade_agentic(["Michael"], "{'answer': 'I do not know'}")
    assert not wrong.passed
    assert "answer_not_found" in wrong.details
    assert not grade_agentic(["x"], None).passed


def test_web_search_runs_tools_and_checks_the_final_answer(
    model: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    from bfcl_eval.eval_checker.multi_turn_eval.func_source_code.web_search import WebSearchAPI

    seen: list[tuple[bool, str]] = []

    def fake_search(self: Any, keywords: str, **_: Any) -> dict[str, Any]:
        seen.append((self.show_snippet, keywords))
        return {"results": [{"title": "Forbes", "url": "https://example.com"}]}

    monkeypatch.setattr(WebSearchAPI, "search_engine_query", fake_search)
    [row] = load("web_search_no_snippet", limit="1")

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if messages[-1]["role"] == "tool":
            return {"text": "{'answer': 'Zhang Yiming', 'context': 'Forbes'}"}
        return {"tool_calls": [("search_engine_query", {"keywords": "richest from China"})]}

    server = model(script)
    [outcome] = run_rows(server, [row])
    rec = outcome.record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed, rec.check.details
    # The category's snippet setting reached the API instance.
    assert seen == [(False, "richest from China")]
    tool = next(m for m in server.requests[1]["messages"] if m["role"] == "tool")
    assert "Forbes" in tool["content"]
    assert server.requests[0]["messages"][0]["role"] == "system"

    # A wrong answer fails, and a model that never stops calling is cut off.
    [wrong] = run_rows(model(lambda m, t: {"text": "I do not know"}), [row])
    assert wrong.record is not None
    assert wrong.record.check is not None
    assert not wrong.record.check.passed
    forever = model(lambda m, t: {"tool_calls": [("search_engine_query", {"keywords": "x"})]})
    [cut] = run_rows(forever, [row])
    assert cut.record is not None
    assert cut.record.check is not None
    assert not cut.record.check.passed
    assert "no_last_message" in cut.record.check.details


def memory_script(remember: bool) -> Script:
    """Writes the customer's first name into core memory during the prerequisite
    conversations (when asked to), and answers from the memory it is shown."""
    wrote = {"done": False}

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        system = messages[0]["content"] if messages[0]["role"] == "system" else ""
        if "Core Memory" in system and "Michael" in system:
            return {"text": "{'answer': 'Michael', 'context': 'it is in my core memory'}"}
        if "core_memory_add" in tools and remember and not wrote["done"]:
            wrote["done"] = True
            return {"tool_calls": [("core_memory_add", {"key": "first_name", "value": "Michael"})]}
        if (
            messages[-1]["role"] == "user"
            and "answer" in system.lower()
            and "Michael" not in system
        ):
            return {"text": "{'answer': 'I do not know', 'context': 'nothing in memory'}"}
        return {"text": "Noted."}

    return script


def bfcl_globals() -> list[str]:
    from bfcl_eval.eval_checker.multi_turn_eval import multi_turn_utils

    return [k for k in vars(multi_turn_utils) if "evalsi_" in k]


def test_memory_questions_see_what_the_prerequisites_stored(model: Any) -> None:
    rows = load("memory_kv", ids="memory_kv_0-customer-0,memory_kv_1-customer-1")
    server = model(memory_script(remember=True))
    first, second = run_rows(server, rows)
    assert first.record is not None
    assert first.record.check is not None
    assert first.record.check.passed, first.record.check.details
    # The prerequisites ran before the question, once: the second question
    # needs one model call, and the answer of the first came from core memory.
    after_first = [
        r
        for r in server.requests
        if "Michael" in (r["messages"][0].get("content") or "") and len(r["messages"]) == 2
    ]
    assert after_first, "no question saw the stored memory"
    question_requests = [
        r for r in server.requests if r["messages"][-1]["content"] == rows[1]["input"]
    ]
    assert len(question_requests) == 1
    assert second.record is not None
    # Question 1 asks the age; the script only knows the name, which is the point:
    # the second question reaches the same memory without replaying anything.
    assert second.record.check is not None
    assert not second.record.check.passed
    assert bfcl_globals() == []


def test_without_stored_memory_the_question_fails(model: Any) -> None:
    [row] = load("memory_kv", ids="memory_kv_0-customer-0")
    [outcome] = run_rows(model(memory_script(remember=False)), [row])
    assert outcome.record is not None
    assert outcome.record.check is not None
    assert not outcome.record.check.passed
    assert "answer_not_found" in outcome.record.check.details


def test_the_recursive_summary_backend_runs(model: Any) -> None:
    [row] = load("memory_rec_sum", ids="memory_rec_sum_0-customer-0")

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        system = messages[0]["content"] if messages[0]["role"] == "system" else ""
        if "Michael" in system and "memory system" in system:
            return {"text": "{'answer': 'Michael', 'context': 'memory'}"}
        if (
            "memory_append" in tools
            and messages[-1]["role"] == "user"
            and "Noted" not in str(messages)
        ):
            return {"tool_calls": [("memory_append", {"text": "The customer is Michael."})]}
        return {"text": "Noted."}

    [outcome] = run_rows(model(script), [row])
    assert outcome.record is not None
    assert outcome.record.check is not None
    assert outcome.record.check.passed, outcome.record.check.details


def test_a_failed_prerequisite_fails_its_questions(model: Any) -> None:
    rows = load("memory_kv", ids="memory_kv_0-customer-0,memory_kv_1-customer-1")
    outcomes = run_rows(model(lambda m, t: {"status": 500}), rows)
    for outcome in outcomes:
        assert outcome.record is None
        assert "prerequisites failed" in outcome.error

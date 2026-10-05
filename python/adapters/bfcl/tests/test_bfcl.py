"""Contract tests against bfcl-eval: the importer with BFCL's loaders, and
the harness end to end with a scripted model, graded by BFCL's AST checker."""

from __future__ import annotations

import asyncio
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from google.protobuf import json_format

from evalsi.datasets import load_records
from evalsi.runspec import normalize_durations
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_bfcl import grade, load, tools_for
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.testing import Script, ScriptedModelServer


def test_the_importer_uses_bfcls_loaders() -> None:
    rows = load("simple_python", limit="3")
    assert [r["id"] for r in rows] == ["simple_python_0", "simple_python_1", "simple_python_2"]
    meta = rows[0]["metadata"]["bfcl"]
    assert meta["functions"][0]["name"] == "calculate_triangle_area"
    assert meta["ground_truth"] == [
        {"calculate_triangle_area": {"base": [10], "height": [5], "unit": ["units", ""]}}
    ]
    assert "triangle" in rows[0]["input"]
    # BFCL's language hints are part of the function docs.
    java = load("simple_java", ids="simple_java_0")[0]["metadata"]["bfcl"]
    assert "Java 8 SDK syntax" in java["functions"][0]["description"]
    irrelevant = load("irrelevance", limit="1")[0]
    assert irrelevant["metadata"]["bfcl"]["ground_truth"] is None
    assert [r.id for r in load_records("bfcl://multiple?limit=2")] == ["multiple_0", "multiple_1"]
    with pytest.raises(ValueError, match="not supported"):
        load("multi_turn_base")
    with pytest.raises(ValueError, match="no entries"):
        load("simple_python", ids="nope")


def test_tools_are_converted_as_bfcl_does() -> None:
    functions = load("simple_python", ids="simple_python_1")[0]["metadata"]["bfcl"]["functions"]
    specs, names = tools_for(functions)
    assert specs[0].name == "math_factorial"
    assert names == {"math_factorial": "math.factorial"}
    assert specs[0].input_schema["type"] == "object"


@pytest.fixture
def model() -> Iterator[Any]:
    servers: list[ScriptedModelServer] = []

    def make(script: Script) -> ScriptedModelServer:
        servers.append(ScriptedModelServer(script))
        return servers[-1]

    yield make
    for s in servers:
        s.close()


def run(server: ScriptedModelServer, row: dict[str, Any]) -> TaskOutcome:
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
    record = Record(id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"])

    async def go() -> TaskOutcome:
        async def no_sandboxes() -> Any:
            raise AssertionError("BFCL needs no sandbox")

        harness = load_harness(spec, HarnessContext(sandboxes=no_sandboxes, base_dir=Path.cwd()))
        try:
            return await run_task(Task.build(spec, record, trial=0, run_id="r"), harness)
        finally:
            await harness.aclose()

    return asyncio.run(go())


def calling(*calls: tuple[str, dict[str, Any]]) -> Script:
    return lambda messages, tools: {"tool_calls": list(calls)}


def test_a_correct_call_passes_and_a_wrong_one_fails(model: Any) -> None:
    row = load("simple_python", ids="simple_python_1")[0]
    good = run(model(calling(("math_factorial", {"number": 5}))), row).record
    assert good is not None
    assert good.check is not None
    assert good.check.passed, good.check.details
    assert good.output is not None
    assert good.output.json == [{"math.factorial": {"number": 5}}]
    server = model(calling(("math_factorial", {"number": 6})))
    bad = run(server, row).record
    assert bad is not None
    assert bad.check is not None
    assert not bad.check.passed
    assert "number" in bad.check.details
    # The model saw the converted tool and the user's question.
    request = server.requests[0]
    assert request["tools"][0]["function"]["name"] == "math_factorial"
    assert "factorial of 5" in request["messages"][-1]["content"]


def test_parallel_calls_in_any_order(model: Any) -> None:
    row = load("parallel", limit="1")[0]
    truth = row["metadata"]["bfcl"]["ground_truth"]
    _, names = tools_for(row["metadata"]["bfcl"]["functions"])
    back = {v: k for k, v in names.items()}
    calls = []
    for answer in reversed(truth):
        [(name, params)] = answer.items()
        calls.append((back[name], {k: v[0] for k, v in params.items() if v[0] != ""}))
    rec = run(model(calling(*calls)), row).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed, rec.check.details


def test_irrelevance_passes_when_nothing_is_called(model: Any) -> None:
    row = load("irrelevance", limit="1")[0]
    quiet = run(model(lambda m, t: {"text": "I cannot help with that."}), row).record
    assert quiet is not None
    assert quiet.check is not None
    assert quiet.check.passed
    fn = row["metadata"]["bfcl"]["functions"][0]["name"]
    eager = run(model(calling((fn, {"weight": 1.0, "height": 1.0}))), row).record
    assert eager is not None
    assert eager.check is not None
    assert not eager.check.passed


def test_grade_without_decodable_calls() -> None:
    row = load("simple_python", ids="simple_python_0")[0]["metadata"]["bfcl"]
    check = grade("simple_python", row["functions"], row["ground_truth"], None)
    assert not check.passed
    assert grade("live_relevance", [], None, [{"f": {}}]).passed
    assert not grade("live_relevance", [], None, []).passed

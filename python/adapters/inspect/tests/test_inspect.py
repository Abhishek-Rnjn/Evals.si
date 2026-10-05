"""Contract tests: real Inspect tasks on Inspect's mock model, no network."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from inspect_ai import Task
from inspect_ai import eval as inspect_eval
from inspect_ai.dataset import Sample
from inspect_ai.log import EvalLog
from inspect_ai.model import Model, ModelOutput, ModelUsage, get_model
from inspect_ai.scorer import Scorer, match
from inspect_ai.solver import generate, use_tools
from inspect_ai.tool import Tool, tool

import evalsi
from evalsi.datasets import load_records
from evalsi.judges import JudgeClient
from evalsi.testing import TEST_JUDGE, ScriptedJudge
from evalsi_inspect import evalsi_scorer, read_log

MODEL = "mockllm/model"


@tool
def lookup() -> Tool:
    async def execute(order_id: int) -> str:
        """Look up an order.

        Args:
            order_id: The order id.
        """
        return "shipped" if order_id == 42 else "unknown"

    return execute


def mock_model(*outputs: ModelOutput) -> Model:
    for output in outputs:  # explicit usage keeps the mock from downloading a tokenizer
        output.usage = ModelUsage(input_tokens=10, output_tokens=5, total_tokens=15)
    return get_model(MODEL, custom_outputs=list(outputs))


def run_task(tmp_path: Path, scorers: list[Scorer], *, target: str, epochs: int = 1) -> EvalLog:
    outputs = []
    for _ in range(epochs):
        outputs += [
            ModelOutput.for_tool_call(MODEL, "lookup", {"order_id": 42}),
            ModelOutput.from_content(MODEL, "It ships tomorrow."),
        ]
    task = Task(
        dataset=[
            Sample(id="s1", input="Where is order 42?", target=target, metadata={"tier": "gold"})
        ],
        solver=[use_tools(lookup()), generate()],
        scorer=scorers,
        epochs=epochs,
    )
    (log,) = inspect_eval(
        task, model=mock_model(*outputs), log_dir=str(tmp_path / "logs"), display="none"
    )
    assert log.status == "success", log.error
    return log


def test_inspect_log_imports_as_records(tmp_path: Path) -> None:
    log = run_task(tmp_path, [match()], target="It ships tomorrow.")
    (record,) = load_records(f"inspect://{log.location}")
    assert record.id == "s1"
    assert record.input is not None
    assert record.input.as_text() == "Where is order 42?"
    assert record.output is not None
    assert record.output.as_text() == "It ships tomorrow."
    assert record.reference is not None
    assert record.reference.as_text() == "It ships tomorrow."
    assert record.metadata["tier"] == "gold"
    assert record.metadata["inspect_scores"] == {"match": "C"}
    assert record.metadata["inspect_model"] == MODEL
    assert record.usage is not None
    assert record.usage.input_tokens == 20
    assert record.trajectory is not None
    (tool_use,) = record.trajectory.tool_uses()
    assert tool_use.name == "lookup"
    assert json.loads(tool_use.arguments) == {"order_id": 42}
    assert tool_use.result == "shipped"
    # The imported records score like any dataset.
    result = evalsi.evaluate([record], ["exact-match", "tool-errors"])
    assert result.metric("exact-match").mean == 1.0
    assert result.metric("tool-errors").mean == 0.0


def test_epochs_get_distinct_ids_and_scores_can_be_left_out(tmp_path: Path) -> None:
    log = run_task(tmp_path, [match()], target="It ships tomorrow.", epochs=2)
    records = read_log(str(log.location), scores="false")
    assert [r.id for r in records] == ["s1#1", "s1#2"]
    assert "inspect_scores" not in records[0].metadata


def test_evalsi_evaluators_score_inspect_tasks(tmp_path: Path) -> None:
    target = json.dumps({"tool_calls": [{"name": "lookup", "arguments": {"order_id": 42}}]})
    log = run_task(
        tmp_path,
        [
            evalsi_scorer("builtin/tool-call-accuracy"),
            evalsi_scorer("builtin/loop-detection"),
            evalsi_scorer("builtin/faithfulness"),  # needs context: no score, not zero
        ],
        target=target,
    )
    assert log.samples is not None
    scores = log.samples[0].scores or {}
    assert scores["evalsi_tool_call_accuracy"].value == {"recall": 1.0, "precision": 1.0}
    assert scores["evalsi_loop_detection"].value == 1.0
    assert "evalsi_faithfulness" not in scores or scores["evalsi_faithfulness"].value is None
    assert log.results is not None
    names = {s.name for s in log.results.scores}
    assert "evalsi_loop_detection" in names


def test_judge_based_scorer_uses_the_given_judge(tmp_path: Path) -> None:
    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        return {"reasoning": "On topic.", "score": 5}

    backend = ScriptedJudge(reply)
    judge = JudgeClient(TEST_JUDGE, backend)
    log = run_task(
        tmp_path, [evalsi_scorer("answer-relevance", judge=judge)], target="It ships tomorrow."
    )
    assert log.samples is not None
    assert (log.samples[0].scores or {})["evalsi_answer_relevance"].value == pytest.approx(1.0)
    assert len(backend.calls) == 1
    assert "Where is order 42?" in backend.calls[0][0]

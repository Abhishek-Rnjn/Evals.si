"""Contract tests: real DeepEval metrics, scripted judge, no network."""

from __future__ import annotations

import json
from typing import Any

import pytest

import evalsi
from evalsi import Content, Record
from evalsi.judges import JudgeClient
from evalsi.testing import TEST_JUDGE, ScriptedJudge, score, scripted_judge
from evalsi.types import Step, Trajectory
from evalsi_deepeval import G_EVAL, PACK

EVALUATORS = {d.spec.short_name: d for d in PACK.evaluators}


def rag_record(**overrides: Any) -> Record:
    fields: dict[str, Any] = {
        "id": "r1",
        "input": Content(text="What is the capital of France?"),
        "output": Content(text="Paris is the capital of France. It has 50 million people."),
        "context": [Content(text="Paris is the capital and largest city of France.")],
    }
    fields.update(overrides)
    return Record(**fields)


def keys(schema: dict[str, Any]) -> set[str]:
    return set(schema.get("properties", {}))


def faithfulness_reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
    k = keys(schema)
    if k == {"truths"}:
        return {"truths": ["Paris is the capital of France."]}
    if k == {"claims"}:
        return {"claims": ["Paris is the capital of France.", "Paris has 50 million people."]}
    if k == {"verdicts"}:
        return {
            "verdicts": [
                {"verdict": "yes", "reason": None},
                {"verdict": "no", "reason": "The context gives no population."},
            ]
        }
    if k == {"reason"}:
        return {"reason": "One of two claims is unsupported."}
    raise AssertionError(f"unexpected schema {schema}")


def test_pack_covers_the_metrics() -> None:
    assert set(EVALUATORS) == {
        "faithfulness",
        "answer-relevancy",
        "contextual-precision",
        "contextual-recall",
        "contextual-relevancy",
        "hallucination",
        "bias",
        "toxicity",
        "summarization",
        "tool-correctness",
        "argument-correctness",
        "g-eval",
    }
    assert all(d.spec.requires.judge for d in PACK.evaluators)
    assert EVALUATORS["hallucination"].spec.outputs[0].higher_is_better is False


def test_faithfulness_runs_deepeval_through_the_judge() -> None:
    ctx, judge = scripted_judge(faithfulness_reply)
    result = score(EVALUATORS["faithfulness"], rag_record(), ctx)["faithfulness"]
    assert result.number == pytest.approx(0.5)
    assert result.explanation == "One of two claims is unsupported."
    assert result.metadata["success"] is True  # 0.5 meets the default threshold
    assert result.metadata["judge_calls"] == len(judge.calls) == 4
    assert result.cost is not None
    assert result.cost.input_tokens == 40
    # Every schema reaching the judge is strict: closed objects, all fields required.
    verdicts = next(s for _, s in judge.calls if keys(s) == {"verdicts"})
    item = verdicts["properties"]["verdicts"]["items"]
    item = verdicts["$defs"][item["$ref"].rsplit("/", 1)[-1]] if "$ref" in item else item
    assert item["additionalProperties"] is False
    assert set(item["required"]) == {"verdict", "reason"}


def test_threshold_and_options_reach_the_metric() -> None:
    ctx, _ = scripted_judge(faithfulness_reply)
    result = score(
        EVALUATORS["faithfulness"],
        rag_record(),
        ctx,
        threshold=0.8,
        include_reason=False,
        options={"truths_extraction_limit": 3},
    )["faithfulness"]
    assert result.metadata["success"] is False
    assert result.metadata["threshold"] == 0.8


def test_g_eval_with_criteria() -> None:
    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        if keys(schema) == {"steps"}:
            return {"steps": ["Check the answer names the right city."]}
        if {"score", "reason"} <= keys(schema):
            return {"score": 8, "reason": "Correct city, wrong population."}
        raise AssertionError(f"unexpected schema {schema}")

    ctx, _ = scripted_judge(reply)
    result = score(G_EVAL, rag_record(), ctx, criteria="Is the answer correct?")["g-eval"]
    assert result.number == pytest.approx(0.8)
    assert "wrong population" in result.explanation


def test_g_eval_skips_records_missing_a_field() -> None:
    ctx, judge = scripted_judge(lambda p, s: {})
    with pytest.raises(evalsi.SkipRecord, match="no reference"):
        score(
            G_EVAL,
            rag_record(),
            ctx,
            criteria="Matches the expected answer?",
            evaluation_params=["actual_output", "expected_output"],
        )
    assert not judge.calls


def agent_record() -> Record:
    steps = [
        Step(type="llm", name="chat"),
        Step(type="tool", name="lookup_order", input=Content(text=json.dumps({"id": 42}))),
        Step(type="tool", name="send_email", input=Content(text="{}")),
    ]
    return Record(
        id="a1",
        input=Content(text="Where is order 42?"),
        output=Content(text="It ships tomorrow."),
        reference=Content.from_value({"tool_calls": ["lookup_order", "cancel_order"]}),
        trajectory=Trajectory(steps=steps),
    )


def test_tool_correctness_maps_the_trajectory() -> None:
    ctx, _ = scripted_judge(lambda p, s: {"reason": "Missed cancel_order."})
    result = score(EVALUATORS["tool-correctness"], agent_record(), ctx)["tool-correctness"]
    assert result.number == pytest.approx(0.5)


def test_evaluate_end_to_end_with_skips() -> None:
    rows = [rag_record(), rag_record(id="r2", context=[])]
    judge = JudgeClient(TEST_JUDGE, ScriptedJudge(faithfulness_reply))
    result = evalsi.evaluate(rows, ["deepeval/faithfulness"], judge=judge)
    metric = result.metric("faithfulness")
    assert metric.n == 1
    assert metric.mean == pytest.approx(0.5)
    outcomes = sorted(r.outcome.value for r in result.results)
    assert outcomes == ["scored", "skipped"]

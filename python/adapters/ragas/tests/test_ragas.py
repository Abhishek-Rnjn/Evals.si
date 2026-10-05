"""Contract tests: real RAGAS metrics, scripted judge, no network."""

from __future__ import annotations

import json
from typing import Any

import pytest

import evalsi
from evalsi import Content, Record
from evalsi.judges import JudgeClient
from evalsi.testing import TEST_JUDGE, ScriptedJudge, score, scripted_judge
from evalsi.types import Message, Step, ToolCall, Trajectory
from evalsi_ragas import PACK, to_messages

EVALUATORS = {d.spec.short_name: d for d in PACK.evaluators}


def keys(schema: dict[str, Any]) -> set[str]:
    return set(schema.get("properties", {}))


def rag_record(**overrides: Any) -> Record:
    fields: dict[str, Any] = {
        "id": "r1",
        "input": Content(text="What is the capital of France?"),
        "output": Content(text="Paris is the capital of France. It has 50 million people."),
        "reference": Content(text="Paris is the capital of France."),
        "context": [Content(text="Paris is the capital and largest city of France.")],
    }
    fields.update(overrides)
    return Record(**fields)


def faithfulness_reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
    # Both steps reply with "statements": first a list of strings, then verdicts.
    if schema["properties"]["statements"]["items"].get("type") == "string":
        return {"statements": ["Paris is the capital of France.", "Paris has 50 million people."]}
    return {
        "statements": [
            {"statement": "Paris is the capital of France.", "reason": "stated", "verdict": 1},
            {"statement": "Paris has 50 million people.", "reason": "absent", "verdict": 0},
        ]
    }


def test_pack_requirements() -> None:
    assert EVALUATORS["faithfulness"].spec.requires.context
    assert EVALUATORS["tool-call-f1"].spec.requires.judge is False
    assert EVALUATORS["tool-call-f1"].spec.requires.trajectory
    assert EVALUATORS["noise-sensitivity"].spec.outputs[0].higher_is_better is False
    assert "reference_topics" in EVALUATORS["topic-adherence"].spec.params


def test_faithfulness_runs_ragas_through_the_judge() -> None:
    ctx, judge = scripted_judge(faithfulness_reply)
    result = score(EVALUATORS["faithfulness"], rag_record(), ctx)["faithfulness"]
    assert result.number == pytest.approx(0.5)
    assert result.metadata["judge_calls"] == len(judge.calls) == 2
    assert all(s.get("additionalProperties") is False for _, s in judge.calls)


def test_answer_accuracy_normalizes_ratings() -> None:
    ctx, judge = scripted_judge(lambda prompt, schema: {"rating": 4})
    result = score(EVALUATORS["answer-accuracy"], rag_record(), ctx)["answer-accuracy"]
    assert result.number == pytest.approx(1.0)
    # RAGAS asks twice, swapping the roles of response and reference.
    assert len(judge.calls) == 2


def agent_record() -> Record:
    steps = [
        Step(type="llm", name="chat", output=Content(text="Let me look that up.")),
        Step(
            type="tool",
            name="lookup_order",
            input=Content(text=json.dumps({"id": 42})),
            output=Content(text="shipped"),
        ),
        Step(type="llm", name="chat", output=Content(text="It ships tomorrow.")),
    ]
    return Record(
        id="a1",
        input=Content(text="Where is order 42?"),
        output=Content(text="It ships tomorrow."),
        reference=Content.from_value(
            {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, "send_email"]}
        ),
        trajectory=Trajectory(steps=steps),
    )


def test_trajectory_becomes_ragas_messages() -> None:
    messages = to_messages(agent_record())
    kinds = [type(m).__name__ for m in messages]
    assert kinds == ["HumanMessage", "AIMessage", "ToolMessage", "AIMessage"]
    assert messages[1].tool_calls[0].name == "lookup_order"
    assert messages[1].tool_calls[0].args == {"id": 42}
    assert messages[2].content == "shipped"


def test_tool_calls_requested_by_the_model_are_used_without_tool_spans() -> None:
    call = ToolCall(name="search", arguments='{"q": "x"}')
    record = Record(
        id="m",
        input=Content(text="find x"),
        output=Content(text="done"),
        trajectory=Trajectory(
            steps=[
                Step(
                    type="llm",
                    output=Content(messages=[Message(role="assistant", tool_calls=[call])]),
                )
            ]
        ),
    )
    messages = to_messages(record)
    assert messages[1].tool_calls[0].args == {"q": "x"}
    assert messages[-1].content == "done"


def test_tool_call_f1_needs_no_judge() -> None:
    result = score(EVALUATORS["tool-call-f1"], agent_record())["tool-call-f1"]
    assert result.number == pytest.approx(2 / 3, abs=1e-3)
    assert result.cost is None


def test_topic_adherence_without_topics_is_skipped() -> None:
    ctx, judge = scripted_judge(lambda prompt, schema: {})
    with pytest.raises(evalsi.SkipRecord, match="reference_topics"):
        score(EVALUATORS["topic-adherence"], agent_record(), ctx)
    assert not judge.calls


def test_evaluate_end_to_end() -> None:
    rows = [rag_record(), rag_record(id="r2", context=[])]
    judge = JudgeClient(TEST_JUDGE, ScriptedJudge(faithfulness_reply))
    result = evalsi.evaluate(rows, ["ragas/faithfulness", "ragas/tool-call-f1"], judge=judge)
    assert result.metric("faithfulness").mean == pytest.approx(0.5)
    assert result.metric("faithfulness").n == 1
    assert result.metric("tool-call-f1").n == 0  # no trajectories: skipped, not zero

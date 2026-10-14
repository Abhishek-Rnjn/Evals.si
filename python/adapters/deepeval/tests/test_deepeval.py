"""Contract tests: real DeepEval metrics, scripted judge, no network."""

from __future__ import annotations

import inspect
import json
from typing import Any

import pytest

import evalsi
from evalsi import Content, Record
from evalsi.evaluator import EvaluatorConfigError
from evalsi.judges import JudgeClient, JudgeFatalError
from evalsi.testing import TEST_JUDGE, ScriptedJudge, score, scripted_judge
from evalsi.types import Message, Step, ToolCall, Trajectory
from evalsi_deepeval import (
    G_EVAL,
    METRICS,
    PACK,
    jev_questions,
    to_conversation,
    to_trace,
)

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
        "task-completion",
        "step-efficiency",
        "plan-quality",
        "plan-adherence",
        "prompt-alignment",
        "pii-leakage",
        "misuse",
        "non-advice",
        "role-violation",
        "knowledge-retention",
        "conversation-completeness",
        "turn-relevancy",
        "role-adherence",
        "topic-adherence",
        "goal-accuracy",
        "g-eval",
        "jev-eval",
        "conversational-g-eval",
        "conversational-jev-eval",
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


def test_every_metric_takes_the_judge_as_both_models() -> None:
    import deepeval.metrics

    for info in METRICS:
        params = inspect.signature(getattr(deepeval.metrics, info.cls).__init__).parameters
        assert {"model", "system_one_model", "eval_mode"} <= set(params), info.cls


def test_required_config_is_a_required_param() -> None:
    with pytest.raises(EvaluatorConfigError, match="prompt_instructions"):
        EVALUATORS["prompt-alignment"].bind()

    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        if keys(schema) == {"verdicts"}:
            assert "Answer in one word." in prompt
            return {"verdicts": [{"verdict": "no", "reason": "Two sentences."}]}
        return {"reason": "The instruction was not followed."}

    ctx, _ = scripted_judge(reply)
    result = score(
        EVALUATORS["prompt-alignment"],
        rag_record(),
        ctx,
        prompt_instructions=["Answer in one word."],
    )["prompt-alignment"]
    assert result.number == 0.0


def nested_agent_record() -> Record:
    steps = [
        Step(type="agent", name="planner", span_id="a"),
        Step(type="llm", name="chat", span_id="b", parent_span_id="a"),
        Step(type="tool", name="lookup_order", input=Content(text="{}"), parent_span_id="a"),
        # A parent that only appears later is not trusted, so a bad trace cannot cycle.
        Step(type="retrieval", name="search", span_id="c", parent_span_id="d"),
        Step(type="generic", name="late", span_id="d", parent_span_id="c"),
    ]
    return Record(
        id="n1",
        input=Content(text="Where is order 42?"),
        output=Content(text="It ships tomorrow."),
        trajectory=Trajectory(steps=steps),
    )


def test_trajectory_becomes_a_nested_trace() -> None:
    trace = to_trace(nested_agent_record())
    assert trace["type"] == "agent"
    assert trace["input"] == "Where is order 42?"
    planner, search = trace["children"]
    assert [c["name"] for c in planner["children"]] == ["chat", "lookup_order"]
    assert [c["type"] for c in planner["children"]] == ["llm", "tool"]
    assert search["type"] == "retriever"
    assert [c["name"] for c in search["children"]] == ["late"]


def test_trace_metrics_read_the_trajectory() -> None:
    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        if keys(schema) == {"task"}:
            assert "lookup_order" in prompt
            return {"task": "Find order 42."}
        return {"score": 0.25, "reason": "Two needless steps."}

    ctx, _ = scripted_judge(reply)
    result = score(EVALUATORS["step-efficiency"], nested_agent_record(), ctx)["step-efficiency"]
    assert result.number == pytest.approx(0.25)
    assert EVALUATORS["step-efficiency"].spec.requires.trajectory


def jev_reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
    assert set(keys(schema)) == {"q_0", "q_1", "q_2"}
    level = schema["properties"]["q_1"]["properties"]["probabilities"]
    assert set(level["properties"]) == {"level_0", "level_1", "level_2"}
    return {
        "q_0": {"reasoning": "", "probability_true": 0.9},
        "q_1": {"reasoning": "", "probabilities": {"level_0": 0, "level_1": 1, "level_2": 1}},
        "q_2": {"reasoning": "", "probabilities": {"option_0": 3, "option_1": 1}},
    }


QUESTIONS = [
    {"statement": "The output names the capital correctly."},
    {"question": "How concise is it?", "levels": ["rambling", "ok", "tight"]},
    {"question": "Tone", "options": {"polite": 1.0, "rude": 0.0}, "weight": 2},
]


def test_jev_eval_answers_through_the_judge() -> None:
    ctx, judge = scripted_judge(jev_reply)
    result = score(EVALUATORS["jev-eval"], rag_record(), ctx, questions=QUESTIONS)["jev-eval"]
    # Weighted mean of 0.9, 0.75 (expected level 1.5 of 2) and 0.75 (P(polite)) x2.
    assert result.number == pytest.approx((0.9 + 0.75 + 2 * 0.75) / 4)
    assert len(judge.calls) == 1
    prompt, _ = judge.calls[0]
    assert "Paris is the capital of France" in prompt
    assert "option_0: polite" in prompt
    assert result.metadata["confidence"] == pytest.approx(0.25)
    assert [o["type"] for o in result.metadata["score_breakdown"]] == ["noul", "score", "choice"]
    assert "evalsi judge (scripted)" in result.explanation


def test_jev_eval_on_the_trace() -> None:
    ctx, judge = scripted_judge(jev_reply)
    result = score(
        EVALUATORS["jev-eval"],
        nested_agent_record(),
        ctx,
        questions=QUESTIONS,
        evaluation_params=["trace"],
    )["jev-eval"]
    assert result.number == pytest.approx((0.9 + 0.75 + 2 * 0.75) / 4)
    assert "lookup_order" in judge.calls[0][0]
    with pytest.raises(evalsi.SkipRecord, match="no trajectory"):
        score(
            EVALUATORS["jev-eval"],
            rag_record(),
            ctx,
            questions=QUESTIONS,
            evaluation_params=["trace"],
        )


def test_jev_questions_are_validated() -> None:
    assert [type(q).__name__ for q in jev_questions(QUESTIONS)] == ["Noul", "Score", "Choice"]
    with pytest.raises(ValueError, match="non-empty"):
        jev_questions([])
    with pytest.raises(ValueError, match="question 0"):
        jev_questions([{"question": "Scale", "levels": ["only one"]}])
    with pytest.raises(ValueError, match="unknown type"):
        jev_questions([{"type": "essay", "question": "Why?"}])
    with pytest.raises(EvaluatorConfigError, match="questions"):
        EVALUATORS["jev-eval"].bind()


def test_system_one_mode_never_reaches_typesafe(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TYPESAFE_API_KEY", raising=False)

    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        return {
            k: {"reasoning": "", "probability_true": 1.0}
            if "probability_true" in v["properties"]
            else {
                "reasoning": "",
                "probabilities": dict.fromkeys(v["properties"]["probabilities"]["properties"], 1),
            }
            for k, v in schema["properties"].items()
        }

    ctx, judge = scripted_judge(reply)
    result = score(EVALUATORS["faithfulness"], rag_record(), ctx, eval_mode="system_one")[
        "faithfulness"
    ]
    assert result.number is not None
    assert 0.0 <= result.number <= 1.0
    assert len(judge.calls) == 1
    with pytest.raises(ValueError, match="eval_mode"):
        score(EVALUATORS["faithfulness"], rag_record(), ctx, eval_mode="jev")


def chat_record(**overrides: Any) -> Record:
    fields: dict[str, Any] = {
        "id": "c1",
        "input": Content(
            messages=[
                Message(role="system", content="You are a support bot."),
                Message(role="user", content="My order number is 42."),
                Message(
                    role="assistant",
                    content="",
                    tool_calls=[ToolCall(name="lookup_order", arguments='{"id": 42}', id="t1")],
                ),
                Message(role="tool", content="ships tomorrow", tool_call_id="t1"),
                Message(role="assistant", content="It ships tomorrow."),
                Message(role="user", content="Which order was that?"),
            ]
        ),
        "output": Content(text="Order 42."),
    }
    fields.update(overrides)
    return Record(**fields)


def test_chat_messages_become_turns() -> None:
    case = to_conversation(chat_record(), chatbot_role="support bot")
    assert [t.role for t in case.turns] == ["user", "assistant", "assistant", "user", "assistant"]
    assert case.turns[-1].content == "Order 42."
    call = case.turns[1].tools_called[0]
    assert (call.name, call.input_parameters, call.output) == (
        "lookup_order",
        {"id": 42},
        "ships tomorrow",
    )
    assert case.chatbot_role == "support bot"
    # An output that repeats the input is the whole transcript, not a continuation.
    before = chat_record().input
    assert before is not None
    assert before.messages is not None
    reply = Message(role="assistant", content="Order 42.")
    whole = chat_record(output=Content(messages=[*before.messages, reply]))
    assert len(to_conversation(whole).turns) == 5


def test_conversational_metric_runs_and_skips() -> None:
    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        if keys(schema) == {"reason", "verdict"}:
            return {"verdict": "yes", "reason": None}
        return {"reason": "Every turn is relevant."}

    ctx, _ = scripted_judge(reply)
    result = score(EVALUATORS["turn-relevancy"], chat_record(), ctx)["turn-relevancy"]
    assert result.number == pytest.approx(1.0)
    no_user = chat_record(input=Content(messages=[Message(role="system", content="Hi")]))
    with pytest.raises(evalsi.SkipRecord, match="no conversation"):
        score(EVALUATORS["turn-relevancy"], no_user, ctx)


def test_conversational_jev_eval() -> None:
    def reply(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        assert "Which order was that?" in prompt
        return {"q_0": {"reasoning": "", "probability_true": 0.8}}

    ctx, _ = scripted_judge(reply)
    result = score(
        EVALUATORS["conversational-jev-eval"],
        chat_record(),
        ctx,
        questions=[{"statement": "The assistant remembered the order number."}],
    )["conversational-jev-eval"]
    assert result.number == pytest.approx(0.8)


def letter_logprobs(prompt: str) -> dict[str, float]:
    # A self-hosted model's first token: mostly the first letter, some mass elsewhere.
    import math

    if "Is this statement true?" in prompt:
        return {"A": math.log(0.6), " A": math.log(0.2), "B": math.log(0.2)}
    if "rambling" in prompt:
        return {"A": math.log(0.1), "B": math.log(0.1), "C": math.log(0.6), "The": math.log(0.2)}
    return {"A": math.log(0.9), "B": math.log(0.1)}


def test_jev_eval_reads_probabilities_off_logprobs() -> None:
    ctx, judge = scripted_judge(lambda p, s: {}, logprobs=letter_logprobs)
    result = score(
        EVALUATORS["jev-eval"], rag_record(), ctx, questions=QUESTIONS, decisions="logprobs"
    )["jev-eval"]
    # P(true)=0.8; expected level (0.125 + 2 * 0.75) / 2; P(polite)=0.9 weighted 2.
    assert result.number == pytest.approx((0.8 + 0.8125 + 2 * 0.9) / 4)
    assert len(judge.calls) == 3  # one first-token call per question
    assert all(schema == {"top_logprobs": 20} for _, schema in judge.calls)
    assert "C: tight" in judge.calls[1][0]
    assert result.metadata["min_label_mass"] == pytest.approx(0.8)
    assert "logprobs" in result.explanation


def test_logprob_decisions_need_a_judge_with_logprobs() -> None:
    ctx, _ = scripted_judge(lambda p, s: {})  # like Claude: no logprobs
    with pytest.raises(JudgeFatalError, match="no logprobs"):
        score(EVALUATORS["jev-eval"], rag_record(), ctx, questions=QUESTIONS, decisions="logprobs")
    with pytest.raises(ValueError, match="decisions"):
        score(EVALUATORS["jev-eval"], rag_record(), ctx, questions=QUESTIONS, decisions="vibes")


def test_builtin_metrics_take_logprob_decisions() -> None:
    ctx, judge = scripted_judge(lambda p, s: {}, logprobs=letter_logprobs)
    result = score(
        EVALUATORS["answer-relevancy"],
        rag_record(),
        ctx,
        eval_mode="system_one",
        decisions="logprobs",
    )["answer-relevancy"]
    assert result.number is not None
    assert 0.0 <= result.number <= 1.0
    assert judge.calls
    assert all(schema == {"top_logprobs": 20} for _, schema in judge.calls)

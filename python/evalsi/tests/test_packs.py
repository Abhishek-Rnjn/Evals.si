from __future__ import annotations

import asyncio
import json
from typing import Any

import pytest
from test_judge import OPENAI

import evalsi
from conftest import make_record
from evalsi import (
    Content,
    EvalContext,
    Message,
    Record,
    Score,
    SkipRecord,
    Step,
    ToolCall,
    Trajectory,
)
from evalsi.datasets import load_records
from evalsi.evaluator import EvaluatorDef
from evalsi.judges import JudgeClient, JudgeError, JudgeResponse
from evalsi.packs import agent, rag, safety, text


def run(
    definition: EvaluatorDef, record: Record, ctx: EvalContext | None = None, **params: Any
) -> dict[str, Score]:
    scores = asyncio.run(definition.bind(params).run(record, ctx or EvalContext()))
    return {s.name: s for s in scores}


# --- text: golden values computed with NLTK, rouge-score and sacreBLEU ---

GOLDEN = [
    # hypothesis, reference, bleu (nltk method2), rouge1, rouge2, rougeL, chrF
    ("the cat sat on the mat", "the cat is on the mat", 0.4855, 0.8333, 0.6000, 0.8333, 0.6458),
    ("there is a cat on the mat", "the cat is on the mat", 0.3780, 0.7692, 0.3636, 0.6154, 0.5375),
    (
        "a quick brown fox jumps over the lazy dog today",
        "the quick brown fox jumped over the lazy dog",
        0.4648,
        0.7368,
        0.5882,
        0.7368,
        0.7730,
    ),
]


@pytest.mark.parametrize("case", GOLDEN)
def test_text_metrics_match_reference_implementations(
    case: tuple[str, str, float, float, float, float, float],
) -> None:
    hyp, ref, b, r1, r2, rl, c = case
    record = make_record(hyp, ref)
    assert run(text.bleu, record)["bleu"].number == pytest.approx(b, abs=1e-4)
    rouge = run(text.rouge, record)
    assert rouge["rouge1"].number == pytest.approx(r1, abs=1e-4)
    assert rouge["rouge2"].number == pytest.approx(r2, abs=1e-4)
    assert rouge["rougeL"].number == pytest.approx(rl, abs=1e-4)
    assert run(text.chrf, record)["chrf"].number == pytest.approx(c, abs=1e-4)


def test_corpus_bleu_matches_nltk() -> None:
    records = [make_record(h, r, id=str(i)) for i, (h, r, *_rest) in enumerate(GOLDEN)]
    (score,) = asyncio.run(text.corpus_bleu.bind().run(records, EvalContext()))
    assert score.number == pytest.approx(0.296839, abs=1e-6)


def test_token_f1_and_edge_cases() -> None:
    assert (
        run(text.token_f1, make_record("The Eiffel Tower!", "eiffel tower"))["token-f1"].number
        == 1.0
    )
    assert run(text.token_f1, make_record("Paris", ["London", "Paris"]))["token-f1"].number == 1.0
    assert run(text.bleu, make_record("", "something"))["bleu"].number == 0.0


def test_corpus_bleu_through_evaluate() -> None:
    rows = [{"output": h, "reference": r} for h, r, *_ in GOLDEN] + [{"output": "x"}]
    result = evalsi.evaluate(rows, ["corpus-bleu", "rouge"])
    assert result.metric("corpus-bleu").mean == pytest.approx(0.296839, abs=1e-6)
    assert result.metric("rouge.rougeL").n == 3


# --- rag, with a scripted judge ---


class ScriptedJudge:
    def __init__(self, data: dict[str, Any]) -> None:
        self.data = data
        self.prompts: list[str] = []

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        self.prompts.append(prompt)
        return JudgeResponse(data=self.data, model="j", input_tokens=10, output_tokens=5)

    async def aclose(self) -> None:
        return None


def judged(data: dict[str, Any]) -> tuple[EvalContext, ScriptedJudge]:
    backend = ScriptedJudge(data)
    return EvalContext(judge=JudgeClient(OPENAI, backend)), backend


def test_faithfulness() -> None:
    ctx, judge = judged(
        {"claims": [{"claim": "a", "supported": True}, {"claim": "b", "supported": False}]}
    )
    record = make_record("A and B.", context=["doc about A"])
    score = run(rag.faithfulness, record, ctx)["faithfulness"]
    assert score.number == 0.5
    assert "unsupported: b" in score.explanation
    assert '<context index="1">' in judge.prompts[0]
    ctx, _ = judged({"claims": []})
    with pytest.raises(SkipRecord):
        run(rag.faithfulness, record, ctx)


def test_context_precision_and_recall() -> None:
    record = make_record(None, "Paris is the capital. It is in France.", context=["x", "y", "z"])
    ctx, _ = judged({"relevant": [False, True, True]})
    # Average precision: (1/2 + 2/3) / 2.
    assert run(rag.context_precision, record, ctx)["context-precision"].number == pytest.approx(
        7 / 12
    )
    ctx, _ = judged({"relevant": [True]})
    with pytest.raises(JudgeError, match="1 verdicts for 3"):
        run(rag.context_precision, record, ctx)
    ctx, judge = judged({"attributable": [True, False]})
    assert run(rag.context_recall, record, ctx)["context-recall"].number == 0.5
    assert "2. It is in France." in judge.prompts[0]


def test_citation_accuracy() -> None:
    record = make_record("See [1] and [2].", context=["a", "b"])
    assert run(rag.citation_accuracy, record)["citation-accuracy"].passed
    assert not run(rag.citation_accuracy, make_record("See [3].", context=["a"]))[
        "citation-accuracy"
    ].passed
    assert not run(rag.citation_accuracy, make_record("No cites.", context=["a"]))[
        "citation-accuracy"
    ].passed
    assert run(
        rag.citation_accuracy, make_record("No cites.", context=["a"]), require_citation=False
    )["citation-accuracy"].passed


# --- safety ---


@pytest.mark.parametrize(
    ("output", "found"),
    [
        ("Contact jane.doe@example.com", "email"),
        ("Card 4111 1111 1111 1111 on file", "credit-card"),
        ("SSN 123-45-6789", "us-ssn"),
        ("Server at 10.0.0.12", "ipv4"),
        ("Call +1 415 555 0134", "phone"),
    ],
)
def test_pii_leak_detects(output: str, found: str) -> None:
    score = run(safety.pii_leak, make_record(output))["pii-leak"]
    assert score.passed is False
    assert found in score.metadata["found"]
    assert output.rsplit(maxsplit=1)[-1] not in score.explanation  # values are never copied


def test_pii_leak_avoids_false_positives_and_allows() -> None:
    assert run(safety.pii_leak, make_record("Order 4111 1111 1111 1112 shipped."))[
        "pii-leak"
    ].passed  # fails Luhn
    assert run(safety.pii_leak, make_record("We shipped 3 orders in 2026."))["pii-leak"].passed
    assert run(safety.pii_leak, make_record("Mail a@b.co"), allow=["email"])["pii-leak"].passed


def test_secret_and_canary_leaks() -> None:
    assert not run(safety.secret_leak, make_record("key AKIAABCDEFGHIJKLMNOP"))[
        "secret-leak"
    ].passed
    assert not run(safety.secret_leak, make_record("-----BEGIN RSA PRIVATE KEY-----"))[
        "secret-leak"
    ].passed
    assert run(safety.secret_leak, make_record("the sky is blue"))["secret-leak"].passed
    leaked = make_record("The secret word is ZEBRA-42.", metadata={"canary": "zebra-42"})
    assert not run(safety.canary_leak, leaked)["canary-leak"].passed
    assert run(safety.canary_leak, make_record("nothing here"), canary="ZEBRA-42")[
        "canary-leak"
    ].passed
    with pytest.raises(SkipRecord):
        run(safety.canary_leak, make_record("x"))


@pytest.mark.parametrize(
    ("output", "refused"),
    [
        ("I'm sorry, but I can't help with that.", True),
        ("I cannot provide instructions for that.", True),
        ("I must decline this request.", True),
        ("Sure! Here is a recipe for bread.", False),
        ("You can't go wrong with Paris in spring.", False),
    ],
)
def test_refusal(output: str, refused: bool) -> None:
    assert run(safety.refusal, make_record(output))["refusal"].passed is refused


# --- agent ---


def agent_record(reference: Any = None, *, loop: bool = False, error: bool = False) -> Record:
    steps = [
        Step(type="llm", name="chat", output=Content(text="thinking")),
        Step(
            type="tool",
            name="lookup_order",
            input=Content(json={"id": 42}),
            output=Content(text="shipping"),
            error="timeout" if error else "",
        ),
        Step(type="tool", name="send_email", input=Content(json={"to": "a@b.co"})),
        Step(type="llm", name="chat", output=Content(text="It ships tomorrow and I emailed you.")),
    ]
    if loop:
        steps += [Step(type="tool", name="send_email", input=Content(json={"to": "a@b.co"}))] * 2
    return Record(
        id="t1",
        input=Content(text="Where is order 42? Email me."),
        reference=None if reference is None else Content.from_value(reference),
        trajectory=Trajectory(steps=steps),
    )


def test_tool_call_accuracy() -> None:
    ref = {
        "tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, "send_email", "refund"]
    }
    scores = run(agent.tool_call_accuracy, agent_record(ref))
    assert scores["recall"].number == pytest.approx(2 / 3)
    assert scores["precision"].number == 1.0
    assert scores["recall"].metadata["missing"] == ["refund"]
    wrong_args = {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 7}}]}
    assert run(agent.tool_call_accuracy, agent_record(wrong_args))["recall"].number == 0.0
    with pytest.raises(SkipRecord):
        run(agent.tool_call_accuracy, agent_record("not json"))


def test_trajectory_match_modes() -> None:
    record = agent_record(["lookup_order", "send_email"])
    assert run(agent.trajectory_match, record)["trajectory-match"].passed
    swapped = agent_record(["send_email", "lookup_order"])
    assert not run(agent.trajectory_match, swapped)["trajectory-match"].passed
    assert run(agent.trajectory_match, swapped, mode="unordered")["trajectory-match"].passed
    assert run(agent.trajectory_match, agent_record(["lookup_order"]), mode="superset")[
        "trajectory-match"
    ].passed
    assert not run(agent.trajectory_match, agent_record(["lookup_order"]), mode="subset")[
        "trajectory-match"
    ].passed


def test_tool_errors_loops_and_budget() -> None:
    assert run(agent.tool_errors, agent_record(error=True))["tool-errors"].number == 0.5
    assert run(agent.loop_detection, agent_record())["loop-detection"].passed
    looped = run(agent.loop_detection, agent_record(loop=True))["loop-detection"]
    assert not looped.passed
    assert looped.metadata["longest_repeat"] == 3
    budget = run(agent.step_budget, agent_record(), max_tool_calls=1)
    assert (budget["steps"].number, budget["tool_calls"].number) == (4, 2)
    assert budget["within-budget"].passed is False
    assert "within-budget" not in run(agent.step_budget, agent_record())


def test_tool_calls_from_llm_messages_when_no_tool_spans() -> None:
    trajectory = Trajectory(
        steps=[
            Step(
                type="llm",
                output=Content(
                    messages=[
                        Message(
                            role="assistant",
                            tool_calls=[ToolCall(name="search", arguments='{"q": "x"}')],
                        )
                    ]
                ),
            )
        ]
    )
    record = Record(id="1", reference=Content(json=["search"], is_json=True), trajectory=trajectory)
    assert run(agent.trajectory_match, record)["trajectory-match"].passed


def test_goal_completion_sees_the_trajectory() -> None:
    ctx, judge = judged({"reasoning": "done", "score": 5})
    record = agent_record()
    record.output = None
    score = run(agent.goal_completion, record, ctx)["goal-completion"]
    assert score.number == 1.0
    prompt = judge.prompts[0]
    assert "<trajectory>" in prompt
    assert "tool lookup_order" in prompt
    assert "It ships tomorrow" in prompt.split("<response>")[1]


def test_promoted_trace_rows_load_with_trajectories() -> None:
    # The shape evalsid writes when it promotes a trace (protojson, proto names).
    row = {
        "id": "abc",
        "input": {"messages": {"messages": [{"role": "user", "content": "Where is my order?"}]}},
        "output": {
            "messages": {"messages": [{"role": "assistant", "content": "It ships tomorrow."}]}
        },
        "usage": {"input_tokens": "320", "latency": "0.900s"},
        "trajectory": {
            "trace_id": "abc",
            "steps": [
                {"type": "STEP_TYPE_AGENT", "name": "invoke_agent"},
                {
                    "type": "STEP_TYPE_TOOL",
                    "name": "lookup_order",
                    "input": {"json": {"id": 42}},
                    "usage": {"latency": "0.090s"},
                },
            ],
        },
        "provenance": {"trace": {"trace_id": "abc"}},
        "metadata": {"service": "support-agent", "online_scores": {"llm-judge": 0.25}},
    }
    (record,) = load_records([json.loads(json.dumps(row))])
    assert record.input is not None
    assert record.input.as_text() == "Where is my order?"
    assert record.output is not None
    assert record.output.as_text() == "It ships tomorrow."
    assert record.usage is not None
    assert (record.usage.input_tokens, record.usage.latency_ms) == (320, 900.0)
    assert record.trajectory is not None
    assert [s.type for s in record.trajectory.steps] == ["agent", "tool"]
    assert record.trajectory.steps[1].duration_ms == pytest.approx(90.0)
    assert "trajectory" not in record.metadata
    assert "provenance" not in record.metadata
    result = evalsi.evaluate([record], ["trajectory-match", "step-budget"], params={})
    assert result.metric("step-budget.tool_calls").mean == 1

from __future__ import annotations

import asyncio
import json
import struct
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import httpx
import pytest
from test_judge import FakeBackend

from evalsi import Content, Record, Score, Usage, evaluator
from evalsi.cli import main
from evalsi.client import Client, ServerError
from evalsi.evaluator import MetricSpec, ScoreType
from evalsi.judges import JudgeClient, JudgeConfig
from evalsi.registry import Registry, discover_packs
from evalsi.results import summarize
from evalsi.run import BudgetExceeded, execute
from evalsi.runner import bind_evaluators
from evalsi.runspec import SpecError, check_gates, load_spec, parse_spec
from evalsi.targets import (
    AnthropicTarget,
    Generation,
    OpenAICompatibleTarget,
    TargetConfig,
    generate_all,
)
from evalsi.types import EvaluationResult, Outcome
from evalsi.v1alpha1 import run_pb2

OPENAI_TARGET = TargetConfig(connector="openai-compatible", model="m", base_url="http://t/v1")


# --- targets ---


def test_openai_target_sends_system_and_records_usage() -> None:
    seen: dict[str, Any] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.update(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "choices": [{"message": {"content": "Paris"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 9, "completion_tokens": 1},
            },
        )

    config = TargetConfig(
        connector="openai-compatible", model="m", base_url="http://t/v1", system_prompt="Be brief."
    )
    target = OpenAICompatibleTarget(
        config, httpx.AsyncClient(transport=httpx.MockTransport(handler))
    )
    gen = asyncio.run(target.generate(Record(id="1", input=Content(text="Capital of France?"))))
    assert gen.output == Content(text="Paris")
    assert (gen.usage.input_tokens, gen.usage.output_tokens) == (9, 1)
    assert gen.usage.latency_ms is not None
    assert seen["messages"] == [
        {"role": "system", "content": "Be brief."},
        {"role": "user", "content": "Capital of France?"},
    ]


def test_target_failures_become_generation_errors() -> None:
    target = OpenAICompatibleTarget(
        OPENAI_TARGET,
        httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(400, text="bad"))),
    )
    (gen,) = asyncio.run(generate_all(target, [Record(id="1", input=Content(text="q"))]))
    assert gen.output is None
    assert "HTTP 400" in gen.error
    (missing,) = asyncio.run(generate_all(target, [Record(id="2")]))
    assert "no input" in missing.error


def test_anthropic_target_keeps_refusals_as_outputs() -> None:
    message = SimpleNamespace(
        content=[SimpleNamespace(type="text", text="I can't help with that.")],
        stop_reason="refusal",
        stop_details=SimpleNamespace(category="cyber"),
        usage=SimpleNamespace(input_tokens=5, output_tokens=6),
    )
    calls: dict[str, Any] = {}

    async def create(**kwargs: Any) -> Any:
        calls.update(kwargs)
        return message

    client = SimpleNamespace(messages=SimpleNamespace(create=create))
    config = TargetConfig(connector="anthropic", model="claude-opus-5-5", effort="low")
    gen = asyncio.run(
        AnthropicTarget(config, client=client).generate(Record(id="1", input=Content(text="q")))
    )
    assert gen.output == Content(text="I can't help with that.")
    assert "refusal" in gen.error
    assert calls["output_config"] == {"effort": "low"}
    assert "fallbacks" not in calls


def test_target_config_validation() -> None:
    with pytest.raises(ValueError, match="base_url"):
        TargetConfig(connector="openai-compatible", model="m")
    with pytest.raises(ValueError, match="unknown target connector"):
        TargetConfig(connector="x", model="m")


# --- specs and gates ---

SPEC: dict[str, Any] = {
    "apiVersion": "evals.si/v1alpha1",
    "kind": "EvalRun",
    "metadata": {"name": "nightly", "project": "qa"},
    "spec": {
        "dataset": {
            "inline": {
                "records": [
                    {"id": "a", "input": {"text": "q1"}, "reference": {"text": "Paris"}},
                    {"id": "b", "input": {"text": "q2"}, "reference": {"text": "Rome"}},
                ]
            }
        },
        "target": {"connector": "openai-compatible", "model": "m", "base_url": "http://t/v1"},
        "evaluators": [{"ref": "exact-match"}],
        "trials": 3,
        "gates": [{"metric": "exact-match", "min": 0.5}],
    },
}


def test_parse_spec_and_validation() -> None:
    run = parse_spec(SPEC)
    assert (run.name, run.project, run.spec.trials) == ("nightly", "qa", 3)
    assert run.labels == {}
    labeled = parse_spec({**SPEC, "metadata": {**SPEC["metadata"], "labels": {"app": "checkout"}}})
    assert labeled.labels == {"app": "checkout"}
    bad = [
        ({**SPEC, "metadata": {"labels": ["x"]}}, "metadata.labels"),
        ({**SPEC, "kind": "Other"}, "kind must be"),
        ({**SPEC, "spec": {**SPEC["spec"], "evaluators": []}}, "at least one evaluator"),
        ({**SPEC, "spec": {**SPEC["spec"], "bogus": 1}}, "invalid spec"),
        ({**SPEC, "spec": {**SPEC["spec"], "target": None}}, "needs a target"),
        ({**SPEC, "spec": {**SPEC["spec"], "gates": [{"metric": "x"}]}}, "needs min or max"),
    ]
    for document, message in bad:
        if document["spec"].get("target", 1) is None:
            document["spec"] = {k: v for k, v in document["spec"].items() if k != "target"}
        with pytest.raises(SpecError, match=message):
            parse_spec(document)


def test_check_gates() -> None:
    em = bind_evaluators(["exact-match"])
    results = [
        EvaluationResult(
            "a", "exact-match", "x", Outcome.SCORED, [Score(passed=True, name="exact-match")]
        ),
        EvaluationResult(
            "b", "exact-match", "x", Outcome.SCORED, [Score(passed=False, name="exact-match")]
        ),
    ]
    summaries = summarize(em, results, [Record(id="a"), Record(id="b")])
    gates = [
        run_pb2.Gate(metric="exact-match", min=0.5),
        run_pb2.Gate(metric="exact-match", stat=run_pb2.GATE_STAT_CI_LOW, min=0.5),
        run_pb2.Gate(metric="missing", max=1),
    ]
    first, second, third = check_gates(gates, summaries)
    assert first.passed
    assert first.value == 0.5
    assert not second.passed
    assert "below" in second.reason
    assert not third.passed
    assert "no such metric" in third.reason


def test_trials_add_pass_at_k_and_cluster_by_record() -> None:
    em = bind_evaluators(["exact-match"])
    outcomes = {"a": [True, True, True], "b": [True, False, False], "c": [False, False, False]}
    results = [
        EvaluationResult(
            rid, "exact-match", "x", Outcome.SCORED, [Score(passed=p, name="exact-match")], trial=t
        )
        for rid, passes in outcomes.items()
        for t, p in enumerate(passes)
    ]
    summaries = {
        s.metric: s for s in summarize(em, results, [Record(id=r) for r in outcomes], trials=3)
    }
    assert summaries["exact-match"].mean == pytest.approx(4 / 9)
    assert summaries["exact-match"].clusters == 3
    assert summaries["exact-match"].ci is not None
    assert summaries["exact-match"].ci.method == "clustered-t"
    assert summaries["exact-match.pass@3"].mean == pytest.approx(2 / 3)
    assert summaries["exact-match.pass^3"].mean == pytest.approx(1 / 3)


# --- embedded execution ---


class FakeTarget:
    """Answers "Paris" on odd calls and "Rome" on even calls, so trials differ."""

    def __init__(self) -> None:
        self.calls = 0
        self.config = OPENAI_TARGET

    async def generate(self, record: Record) -> Generation:
        self.calls += 1
        if record.id == "boom":
            raise RuntimeError("unreachable")
        text = "Paris" if self.calls % 2 else "Rome"
        return Generation(output=Content(text=text), usage=Usage(input_tokens=10, output_tokens=2))

    async def aclose(self) -> None:
        return None


def test_execute_runs_trials_and_gates(monkeypatch: pytest.MonkeyPatch) -> None:
    target = FakeTarget()
    monkeypatch.setattr("evalsi.run.create_target", lambda config: target)
    outcome = asyncio.run(execute(parse_spec(SPEC)))
    assert target.calls == 6
    metrics = {s.metric for s in outcome.result.summaries}
    assert {"exact-match", "exact-match.pass@3", "exact-match.pass^3"} <= metrics
    assert outcome.result.manifest["trials"] == 3
    assert outcome.target_usage.input_tokens == 60
    assert sorted({r.trial for r in outcome.result.results}) == [0, 1, 2]
    assert len(outcome.gates) == 1


def test_execute_enforces_budgets(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("evalsi.run.create_target", lambda config: FakeTarget())
    spec = json.loads(json.dumps(SPEC))
    spec["spec"]["budget"] = {"max_target_tokens": 30}
    with pytest.raises(BudgetExceeded, match="target used 48 tokens"):
        asyncio.run(execute(parse_spec(spec)))


def test_execute_without_target_uses_dataset_outputs_and_judge(tmp_path: Path) -> None:
    data = tmp_path / "d.jsonl"
    data.write_text('{"id": "1", "output": "Paris", "reference": "Paris"}\n')
    spec_file = tmp_path / "run.yaml"
    spec_file.write_text(
        "spec:\n  dataset: {path: d.jsonl}\n"
        "  evaluators: [{ref: exact-match}, {ref: llm-judge}]\n"
        "  budget: {max_judge_tokens: 1000}\n"
    )
    judge = JudgeClient(
        JudgeConfig(provider="openai-compatible", model="j", base_url="http://j/v1"),
        FakeBackend({"reasoning": "", "score": 5}),
    )
    outcome = asyncio.run(execute(load_spec(spec_file), judge=judge))
    assert {s.metric: s.mean for s in outcome.result.summaries} == {
        "exact-match": 1.0,
        "llm-judge": 1.0,
    }
    assert outcome.judge_usage.input_tokens == 100
    assert outcome.passed


def test_cli_run_embedded_exit_codes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr("evalsi.run.create_target", lambda config: FakeTarget())
    spec = json.loads(json.dumps(SPEC))
    path = tmp_path / "run.json"
    path.write_text(json.dumps(spec))  # JSON is valid YAML
    assert main(["run", "-f", str(path), "--quiet", "--output", str(tmp_path / "out.json")]) in (
        0,
        3,
    )
    out = capsys.readouterr().out
    assert "x 3 trials" in out
    assert "gates:" in out
    assert json.loads((tmp_path / "out.json").read_text())["gates"]
    spec["spec"]["gates"] = [{"metric": "exact-match", "max": 0.5}]  # the fake is always right
    path.write_text(json.dumps(spec))
    assert main(["run", "-f", str(path), "--quiet"]) == 3


# --- the server client ---


def frame(message: dict[str, Any], flags: int = 0) -> bytes:
    payload = json.dumps(message).encode()
    return struct.pack(">BI", flags, len(payload)) + payload


def test_client_unary_and_streaming() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/CreateRun"):
            body = json.loads(request.content)
            return httpx.Response(200, json={"run": {"id": "r1", "name": body["name"]}})
        if request.url.path.endswith("/WatchRun"):
            assert request.headers["content-type"] == "application/connect+json"
            _flags, size = struct.unpack(">BI", request.content[:5])
            assert json.loads(request.content[5 : 5 + size]) == {
                "id": "r1",
                "includeResults": False,
            }
            body = (
                frame({"run": {"status": "RUN_STATUS_RUNNING"}})
                + frame({"progress": {"done": "1"}})
                + frame({}, 2)
            )
            return httpx.Response(200, content=body)
        return httpx.Response(404, json={"code": "not_found", "message": "no such run"})

    client = Client("http://s", transport=httpx.MockTransport(handler))
    assert client.create_run({}, name="n") == {"id": "r1", "name": "n"}
    events = list(client.watch_run("r1"))
    assert events == [{"run": {"status": "RUN_STATUS_RUNNING"}}, {"progress": {"done": "1"}}]
    with pytest.raises(ServerError, match="not_found: no such run"):
        client.get_run("nope")


def test_client_stream_error_trailer() -> None:
    body = frame({"error": {"code": "invalid_argument", "message": "bad"}}, 2)
    client = Client(
        "http://s", transport=httpx.MockTransport(lambda r: httpx.Response(200, content=body))
    )
    with pytest.raises(ServerError, match="invalid_argument"):
        list(client.watch_run("r1"))


def test_custom_registry_is_respected_by_execute(monkeypatch: pytest.MonkeyPatch) -> None:
    @evaluator(
        name="test/always", version="1.0.0", outputs=[MetricSpec("always", ScoreType.PASSED)]
    )
    def always(record: Record) -> Score:
        return Score(passed=True)

    registry = Registry(discover_packs())
    registry.add(always)
    spec = json.loads(json.dumps(SPEC))
    spec["spec"]["evaluators"] = [{"ref": "test/always"}]
    spec["spec"]["trials"] = 1
    monkeypatch.setattr("evalsi.run.create_target", lambda config: FakeTarget())
    outcome = asyncio.run(execute(parse_spec(spec), registry=registry))
    assert outcome.result.metric("always").mean == 1.0

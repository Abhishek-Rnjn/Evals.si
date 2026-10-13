"""``Client.log_run``: a result evaluated in this process becomes an ImportRun
request the server stores as a finished run (PRD E6)."""

from __future__ import annotations

from typing import Any

import pytest
from google.protobuf import json_format

import evalsi
from evalsi import evaluator
from evalsi.client import Client, import_run_body
from evalsi.convert import summary_from_proto, summary_to_proto
from evalsi.types import Score
from evalsi.v1alpha1 import run_service_pb2


@evaluator(name="local/short-answer", version="1.0.0")
def short_answer(record: Any) -> Score:
    return Score(passed=len(record.output.as_text()) <= 10)


ROWS = [
    {"id": "a", "input": "capital of France?", "output": "Paris", "reference": "Paris"},
    {"id": "b", "input": "2 + 2?", "output": "4", "reference": "4"},
    {"id": "c", "input": "sky color?", "output": "green, I think, mostly", "reference": "blue"},
]


def test_the_body_is_an_import_request_with_everything_evaluated() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match", short_answer])
    body = import_run_body(
        result, name="epoch-3", project="studio", labels={"loop": "train"},
        gates=[{"metric": "exact-match", "min": 0.9}],
    )  # fmt: skip
    # The server's own message parses it: field names and types are right.
    msg = json_format.ParseDict(body, run_service_pb2.ImportRunRequest())
    assert (msg.name, msg.project, dict(msg.labels)) == ("epoch-3", "studio", {"loop": "train"})
    assert [e.name for e in msg.spec.evaluators] == ["exact-match", "short-answer"]
    assert msg.spec.evaluators[1].ref.startswith("local/short-answer")
    assert msg.spec.gates[0].metric == "exact-match"
    assert [r.id for r in msg.records] == ["a", "b", "c"]
    assert len(msg.results) == 6
    assert {s.metric: round(s.mean, 4) for s in msg.summaries} == {
        "exact-match": 0.6667,
        "short-answer": 0.6667,
    }
    assert msg.HasField("started_at")
    assert msg.HasField("finished_at")


def test_summaries_round_trip() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    (summary,) = result.summaries
    back = summary_from_proto(summary_to_proto(summary))
    assert (back.metric, back.kind, back.n, back.mean) == (
        summary.metric,
        summary.kind,
        summary.n,
        summary.mean,
    )
    assert back.ci == summary.ci


class FakeClient(Client):
    def __init__(self) -> None:
        super().__init__("http://evalsi.test")
        self.sent: list[tuple[str, str, dict[str, Any], str | None]] = []

    def call(self, service: str, method: str, body: dict[str, Any], **kw: Any) -> dict[str, Any]:
        self.sent.append((service, method, body, kw.get("idempotency_key")))
        return {"run": {"id": "run-1", "status": "RUN_STATUS_SUCCEEDED", "imported": True}}


def test_log_run_sends_import_run() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    client = FakeClient()
    try:
        run = client.log_run(result, name="epoch-3", idempotency_key="epoch-3")
    finally:
        client.close()
    ((service, method, body, key),) = client.sent
    assert (service, method, key) == ("RunService", "ImportRun", "epoch-3")
    assert body["name"] == "epoch-3"
    assert run["imported"] is True


def test_a_param_json_cannot_hold_is_recorded_as_text() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    result.manifest["evaluators"][0]["params"] = {"normalize": str.lower}
    body = import_run_body(result, name="x")
    assert isinstance(body["spec"]["evaluators"][0]["params"]["normalize"], str)


def test_a_bad_gate_is_refused_before_sending() -> None:
    result = evalsi.evaluate(ROWS, ["exact-match"])
    with pytest.raises(ValueError, match="cannot be stored"):
        import_run_body(result, name="x", gates=[{"metric": "exact-match", "min": "high"}])

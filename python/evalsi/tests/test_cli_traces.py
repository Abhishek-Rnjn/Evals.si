"""``evalsi traces``: listing, showing and exporting a server's traces."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, ClassVar

import pytest
from google.protobuf import json_format

from evalsi.cli import main
from evalsi.convert import record_to_proto
from evalsi.datasets import load_records
from evalsi.types import Content, Record, Step, Trajectory


def _record(trace_id: str) -> dict[str, Any]:
    record = Record(
        id=trace_id,
        input=Content.from_value("Research RAG evaluation."),
        output=Content.from_value("RAG is evaluated in two parts [1]."),
        trajectory=Trajectory(
            steps=[
                Step(type="tool", name="search_docs", input=Content.from_value({"query": "rag"}))
            ]
        ),
        metadata={"service": "research-assistant"},
    )
    out: dict[str, Any] = json_format.MessageToDict(record_to_proto(record))
    return out


def _summary(trace_id: str, version: str) -> dict[str, Any]:
    return {
        "traceId": trace_id,
        "project": "studio",
        "service": "research-assistant",
        "startTime": "2026-10-13T09:42:12.483Z",
        "results": 1,
        "labels": {"workflow": "research-assistant", "version": version},
    }


class FakeClient:
    calls: ClassVar[list[tuple[str, str, dict[str, Any]]]] = []
    # Three scored traces, served two to a page.
    ids: ClassVar[list[str]] = ["a" * 32, "b" * 32, "c" * 32]

    def __init__(self, *_: Any, **__: Any) -> None:
        pass

    def __enter__(self) -> FakeClient:
        return self

    def __exit__(self, *_: Any) -> None:
        pass

    def call(self, service: str, method: str, body: dict[str, Any]) -> dict[str, Any]:
        FakeClient.calls.append((service, method, body))
        start = int(body.get("pageToken") or 0)
        page = self.ids[start : start + min(2, body.get("pageSize", 2))]
        end = start + len(page)
        more = {"nextPageToken": str(end)} if end < len(self.ids) else {}
        if method == "ListTraces":
            return {"traces": [_summary(i, "v1") for i in page], **more}
        if method == "ListScores":
            return {
                "traces": [{"trace": _summary(i, "v3"), "policies": []} for i in page],
                **more,
            }
        if method == "GetTrace":
            policies = [{"policy": "studio-default", "results": [{"evaluator": "tool-errors"}]}]
            return {"record": _record(body["traceId"]), "policies": policies}
        raise AssertionError(method)


@pytest.fixture
def fake(monkeypatch: pytest.MonkeyPatch) -> type[FakeClient]:
    import evalsi.client

    FakeClient.calls = []
    monkeypatch.setattr(evalsi.client, "Client", FakeClient)
    return FakeClient


SERVER = ["--server", "http://evalsi.test"]


def test_export_by_label_writes_a_dataset_with_the_scores(
    tmp_path: Path, fake: type[FakeClient]
) -> None:
    out = tmp_path / "traces.jsonl"
    argv = ["traces", "export", "-o", str(out), "--project", "studio"]
    code = main([*argv, "--labels", "workflow=research-assistant", "version=v3", *SERVER])
    assert code == 0
    # Labels select through ListScores, page by page, then each trace is read whole.
    scores = [b for _, m, b in fake.calls if m == "ListScores"]
    assert scores[0]["labels"] == ["workflow=research-assistant", "version=v3"]
    assert [b.get("pageToken", "") for b in scores] == ["", "2"]
    assert [b["traceId"] for _, m, b in fake.calls if m == "GetTrace"] == fake.ids

    records = load_records(out)
    assert [r.id for r in records] == fake.ids
    first = records[0]
    assert first.output is not None
    assert "[1]" in first.output.as_text()
    assert first.trajectory is not None
    assert first.trajectory.steps[0].name == "search_docs"
    assert first.metadata["service"] == "research-assistant"
    trace = first.metadata["evalsi_trace"]
    assert trace["labels"] == {"workflow": "research-assistant", "version": "v3"}
    assert trace["project"] == "studio"
    assert first.metadata["evalsi_scores"][0]["policy"] == "studio-default"


def test_export_without_filters_lists_traces_and_stops_at_the_limit(
    tmp_path: Path, fake: type[FakeClient]
) -> None:
    out = tmp_path / "traces.jsonl"
    assert main(["traces", "export", "-o", str(out), "--limit", "2", *SERVER]) == 0
    assert {m for _, m, _ in fake.calls} == {"ListTraces", "GetTrace"}
    assert len(out.read_text().splitlines()) == 2


def test_list_and_get(fake: type[FakeClient], capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["traces", "list", *SERVER]) == 0
    table = capsys.readouterr().out
    assert "version=v1,workflow=research-assistant" in table
    assert len(table.splitlines()) == 1 + len(fake.ids)

    assert main(["traces", "get", "a" * 32, "--project", "studio", *SERVER]) == 0
    got = json.loads(capsys.readouterr().out)
    assert got["policies"][0]["policy"] == "studio-default"
    assert fake.calls[-1][2] == {"traceId": "a" * 32, "project": "studio"}


def test_export_refuses_a_label_without_a_value(
    tmp_path: Path, fake: type[FakeClient], capsys: pytest.CaptureFixture[str]
) -> None:
    argv = ["traces", "export", "-o", str(tmp_path / "x.jsonl"), "--labels", "workflow"]
    assert main([*argv, *SERVER]) == 1
    assert "KEY=VALUE" in capsys.readouterr().err
    assert fake.calls == []

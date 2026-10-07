from __future__ import annotations

import json
import sys
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi.cli import main
from evalsi.cli_annotate import annotate_loop, ask, queue_from_doc, render_content
from evalsi.client import Client

QUEUE = {
    "name": "helpfulness",
    "project": "support",
    "instructions": "Judge the answer.",
    "annotations_per_item": 2,
    "questions": [
        {"name": "correct", "kind": "pass_fail", "compare_metric": "exact-match"},
        {"name": "quality", "kind": "score", "min": 1, "max": 5},
        {"name": "tone", "kind": "label", "options": ["polite", "rude"]},
        {"name": "notes", "kind": "text", "optional": True},
    ],
}


def test_queue_from_doc() -> None:
    q = queue_from_doc(QUEUE)
    assert q["annotationsPerItem"] == 2
    assert [x["kind"] for x in q["questions"]] == [
        "QUESTION_KIND_PASS_FAIL",
        "QUESTION_KIND_SCORE",
        "QUESTION_KIND_LABEL",
        "QUESTION_KIND_TEXT",
    ]
    assert q["questions"][0]["compareMetric"] == "exact-match"
    with pytest.raises(Exception, match="nope"):
        queue_from_doc({"name": "x", "nope": 1})


def _prompter(answers: list[str]) -> Any:
    it: Iterator[str] = iter(answers)

    def prompt(_: str) -> str:
        return next(it)

    return prompt


def test_ask_validates_and_retries() -> None:
    q = queue_from_doc(QUEUE)["questions"]
    assert ask(q[0], _prompter(["maybe", "y"])) == {"question": "correct", "passed": True}
    assert ask(q[1], _prompter(["9", "x", "4.5"])) == {"question": "quality", "number": 4.5}
    assert ask(q[2], _prompter(["3", "2"])) == {"question": "tone", "label": "rude"}
    assert ask(q[2], _prompter(["polite"])) == {"question": "tone", "label": "polite"}
    assert ask(q[3], _prompter([""])) is None
    assert ask(q[0], _prompter(["", "n"])) == {"question": "correct", "passed": False}


def test_render_content() -> None:
    assert render_content({"text": "hi"}) == "hi"
    msgs = {"messages": {"messages": [{"role": "user", "content": "q"}]}}
    assert render_content(msgs) == "[user] q"
    assert render_content({"json": {"a": 1}}) == '{\n  "a": 1\n}'
    assert render_content(None) == ""


class FakeServer:
    """AnnotationService over httpx.MockTransport, enough for the CLI."""

    def __init__(self) -> None:
        self.items = [
            {"id": f"i{n}", "record": {"id": f"r{n}", "input": {"text": "Q"}}} for n in (1, 2)
        ]
        self.submitted: list[dict[str, Any]] = []
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        method = request.url.path.rsplit("/", 1)[-1]
        body = json.loads(request.content)
        self.calls.append((method, body))
        if method == "CreateQueue":
            return httpx.Response(200, json=body)
        if method == "GetQueue":
            return httpx.Response(200, json={"queue": queue_from_doc(QUEUE)})
        if method == "NextItem":
            answered = {s["itemId"] for s in self.submitted}
            left = [i for i in self.items if i["id"] not in answered]
            if not left:
                return httpx.Response(200, json={})
            return httpx.Response(200, json={"item": left[0], "remaining": str(len(left))})
        if method == "SubmitAnnotation":
            self.submitted.append(body)
            return httpx.Response(200, json={"annotation": {"itemId": body["itemId"]}})
        if method == "AddItems":
            return httpx.Response(200, json={"added": "3"})
        if method == "SummarizeQueue":
            return httpx.Response(
                200,
                json={
                    "items": "2",
                    "done": "2",
                    "annotations": "4",
                    "annotators": {"key:a": "2"},
                    "questions": [
                        {
                            "question": "correct",
                            "summary": {"n": "2", "mean": 0.5, "ci": {"low": 0.1, "high": 0.9}},
                            "interAnnotatorAlpha": 1.0,
                            "metricAgreement": {"n": "2", "accuracy": 0.5, "cohenKappa": 0.0},
                        }
                    ],
                },
            )
        if method == "ListAnnotations":
            if body.get("pageToken"):
                return httpx.Response(
                    200, json={"annotations": [{"itemId": "i2"}], "items": [self.items[1]]}
                )
            return httpx.Response(
                200,
                json={
                    "annotations": [{"itemId": "i1"}],
                    "items": [self.items[0]],
                    "nextPageToken": "p2",
                },
            )
        return httpx.Response(404, json={"code": "not_found", "message": method})


def test_annotate_loop_answers_skips_and_stops(capsys: pytest.CaptureFixture[str]) -> None:
    fake = FakeServer()
    client = Client("http://s", transport=httpx.MockTransport(fake))
    # First item: answered (notes left blank); second: skipped; then nothing.
    prompt = _prompter(["y", "4", "1", "", "fine", "s"])
    # A skip marks the item answered in the fake, so the loop ends.
    n = annotate_loop(client, "support", "helpfulness", prompt=prompt)
    assert n == 1
    first, second = fake.submitted
    assert first["itemId"] == "i1"
    assert first["comment"] == "fine"
    assert first["answers"] == [
        {"question": "correct", "passed": True},
        {"question": "quality", "number": 4.0},
        {"question": "tone", "label": "polite"},
    ]
    assert second == {**second, "itemId": "i2", "skip": True, "answers": []}
    out = capsys.readouterr().out
    assert "Judge the answer." in out
    assert "input:\nQ" in out
    assert "nothing left" in out


def test_annotate_loop_quit_keeps_the_rest() -> None:
    fake = FakeServer()
    client = Client("http://s", transport=httpx.MockTransport(fake))
    assert annotate_loop(client, "support", "helpfulness", prompt=_prompter(["q"])) == 0
    assert fake.submitted == []


def test_cli_add_stats_export(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    fake = FakeServer()
    real = Client.__init__

    def init(self: Client, base_url: str, **kw: Any) -> None:
        kw["transport"] = httpx.MockTransport(fake)
        real(self, base_url, **kw)

    monkeypatch.setattr(Client, "__init__", init)
    monkeypatch.setenv("EVALSI_API_KEY", "evk_test")
    common = ["--project", "support", "--server", "http://s"]

    data = tmp_path / "recs.jsonl"
    data.write_text('{"id": "a", "input": "q", "output": "x"}\n')
    assert main(["annotate", "add", "helpfulness", "--records", str(data), *common]) == 0
    method, body = fake.calls[-1]
    assert method == "AddItems"
    assert body["records"]["records"][0]["id"] == "a"
    assert main(["annotate", "add", "helpfulness", "--run", "run1", "--when", "true", *common]) == 0
    assert fake.calls[-1][1]["run"] == {"runId": "run1", "when": "true", "allTrials": False}

    assert main(["annotate", "stats", "helpfulness", *common]) == 0
    out = capsys.readouterr().out
    assert "2/2 items done" in out
    assert "accuracy=0.500" in out
    assert "[0.100, 0.900]" in out

    dest = tmp_path / "out.jsonl"
    assert main(["annotate", "export", "helpfulness", "-o", str(dest), *common]) == 0
    rows = [json.loads(line) for line in dest.read_text().splitlines()]
    assert [r["item"]["id"] for r in rows] == ["i1", "i2"]

    qf = tmp_path / "q.yaml"
    qf.write_text(json.dumps(QUEUE))
    assert main(["annotate", "create", "-f", str(qf), "--server", "http://s"]) == 0
    assert fake.calls[-1][1]["queue"]["questions"][1]["kind"] == "QUESTION_KIND_SCORE"


def test_show_renders_trajectory_steps(capsys: pytest.CaptureFixture[str]) -> None:
    from google.protobuf import json_format

    from evalsi.cli_annotate import _show
    from evalsi.convert import record_to_proto
    from evalsi.types import Content, Record, Step, Trajectory

    rec = Record(
        id="r1",
        input=Content.from_value("Refund order 42"),
        trajectory=Trajectory(
            steps=[
                Step(
                    type="tool",
                    name="lookup_order",
                    input=Content.from_value({"id": 42}),
                    output=Content.from_value("shipped"),
                ),
                Step(
                    type="llm",
                    name="answer",
                    output=Content.from_value("It shipped."),
                    error="slow",
                ),
            ]
        ),
    )
    item = {"id": "i1", "record": json_format.MessageToDict(record_to_proto(rec))}
    _show(item, False, sys.stdout)
    out = capsys.readouterr().out
    assert "1. tool lookup_order" in out
    assert '"id": 42' in out
    assert "output: shipped" in out
    assert "2. llm answer" in out
    assert "error: slow" in out

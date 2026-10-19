"""``contamination-fetch`` (H7). URLs are synthetic; no real benchmark content."""

from __future__ import annotations

import asyncio
from typing import Any

from evalsi.canaries import make_canaries
from evalsi.evaluator import EvalContext, SkipRecord
from evalsi.packs.contamination import contamination_fetch
from evalsi.types import Content, Record, Step, Trajectory


def tool(name: str, text: str, output: str = "") -> Step:
    return Step(
        type="tool",
        name=name,
        input=Content(text=text),
        output=Content(text=output) if output else None,
    )


class Skipped:
    """What the runner reports for a record the evaluator cannot score."""

    skipped = True

    def __init__(self, reason: str) -> None:
        self.reason = reason


def run_def(definition: Any, record: Record, **params: Any) -> Any:
    missing = definition.spec.requires.missing(record)
    if missing:
        return Skipped(missing)
    try:
        return asyncio.run(definition.bind(params).run(record, EvalContext()))[0]
    except SkipRecord as exc:
        return Skipped(str(exc))


def run(steps: list[Step], **params: Any) -> Any:
    record = Record(id="r", trajectory=Trajectory(steps=steps))
    return run_def(contamination_fetch, record, **params)


def test_rubric_and_answer_paths_on_github_are_flagged() -> None:
    for url in (
        "https://raw.githubusercontent.com/org/bench/main/tasks/RUBRICS.json",
        "https://github.com/org/bench/blob/main/data/gold_answers.jsonl",
        "https://github.com/org/bench/tree/main/GoldAnswers",
        "raw.githubusercontent.com/org/bench/main/solution/t1.py",
    ):
        score = run([tool("web_fetch", f'{{"url":"{url}"}}')])
        assert score.number == 1.0, url
        assert score.metadata["evidence"][0]["step"] == 0


def test_curl_and_wget_commands_are_read() -> None:
    score = run([tool("bash", "curl -sL https://github.com/org/bench/raw/main/rubric.md -o r.md")])
    assert score.number == 1.0
    assert (
        run([tool("bash", "wget -q https://raw.githubusercontent.com/o/b/m/answers.csv")]).number
        == 1.0
    )


def test_ordinary_documentation_is_clean() -> None:
    steps = [
        tool("web_fetch", "https://docs.python.org/3/library/answer.html"),  # not a GitHub host
        tool("web_fetch", "https://github.com/org/bench/blob/main/README.md"),
        tool("bash", "curl https://raw.githubusercontent.com/org/lib/main/src/golden.py"),
        tool("web_search", "how to parse csv in python"),
    ]
    score = run(steps)
    assert score.number == 0.0
    assert score.metadata["evidence"] == []


def test_known_false_positive_is_documented_behavior() -> None:
    # An architecture doc with "solutions" in the path is flagged; the evidence shows why.
    score = run(
        [tool("web_fetch", "https://github.com/o/r/blob/main/docs/solutions-architecture.md")]
    )
    assert score.number == 1.0
    assert score.metadata["evidence"][0]["reason"].startswith("rubric or answer path")


def test_words_and_hosts_can_be_replaced() -> None:
    step = tool("web_fetch", "https://github.com/org/bench/blob/main/rubric.md")
    assert run([step], words=["checklist"]).number == 0.0
    other = tool("web_fetch", "https://git.example.org/org/bench/blob/main/rubric.md")
    assert run([other]).number == 0.0
    assert run([other], hosts=["git.example.org"]).number == 1.0


def test_huggingface_datasets_need_configuration() -> None:
    step = tool("web_fetch", "https://huggingface.co/datasets/acme/jobbench/resolve/main/test.json")
    assert run([step]).number == 0.0
    assert run([step], datasets=["acme/jobbench"]).number == 1.0
    assert run([step], datasets=["jobbench"]).number == 0.0  # name match is on org/name or org
    assert (
        run(
            [tool("web_fetch", "https://huggingface.co/datasets/acme/other")],
            datasets=["acme/jobbench"],
        ).number
        == 0.0
    )
    assert (
        run(
            [tool("python", "load('hf://datasets/acme/jobbench/x.parquet')")],
            datasets=["ACME/JobBench"],
        ).number
        == 1.0
    )
    api = "https://datasets-server.huggingface.co/rows?dataset=acme%2Fjobbench"
    assert run([tool("web_fetch", api)], datasets=["acme/jobbench"]).number == 1.0


def test_extra_patterns_and_canaries() -> None:
    step = tool("web_fetch", "https://bench.example.com/leaderboard/answers/t1")
    assert run([step]).number == 0.0
    assert run([step], patterns=["bench.example.com/leaderboard/*"]).number == 1.0
    leak = tool("web_fetch", "https://example.com/p", output="header SECRET-TOKEN-123 footer")
    score = run([leak], canaries=["SECRET-TOKEN-123"])
    assert score.metadata["evidence"][0]["kind"] == "canary"
    assert run([leak]).number == 0.0


def test_evalsi_canaries_are_recognized_in_tool_output() -> None:
    canary = make_canaries(1)[0].input.as_text().rstrip(":")  # type: ignore[union-attr]
    score = run([tool("web_fetch", "https://example.com/p", output=f"page {canary} end")])
    assert score.number == 1.0
    assert score.metadata["evidence"][0]["where"] == "output"


def test_only_tool_steps_are_read() -> None:
    llm = Step(
        type="llm", name="m", input=Content(text="see https://github.com/o/b/blob/main/rubric.md")
    )
    assert run([llm, tool("bash", "ls")]).number == 0.0


def test_no_steps_or_no_trajectory_is_skipped() -> None:
    assert run([]).skipped
    record = Record(id="r")
    assert run_def(contamination_fetch, record).skipped

from __future__ import annotations

import copy
import json
import os
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

import pytest

from evalsi.calibration_sets import (
    annotation_rows,
    canonical,
    decode,
    digest,
    freeze_set,
    read_artifact,
    read_jsonl,
    validate_set,
    write_artifact,
)


def labelled_rows() -> list[dict[str, Any]]:
    return [
        {
            "record": {
                "id": "a",
                "input": "question a",
                "output": "answer",
                "reference": "task reference",
            },
            "split": "tuning",
            "ratings": [
                {"annotator": "one", "passed": True},
                {"annotator": "two", "passed": False},
            ],
        },
        {
            "record": {"id": "b", "input": "question b", "output": "answer b"},
            "split": "held-out",
            "ratings": [
                {"annotator": "one", "passed": False},
                {"annotator": "two", "passed": True},
                {"annotator": "three", "passed": False},
            ],
        },
    ]


def frozen() -> dict[str, Any]:
    return freeze_set(labelled_rows(), name="answers", version="labels-1", question="Correct?")


def test_freeze_retains_dissent_without_overwriting_reference(tmp_path: Path) -> None:
    rows = labelled_rows()
    dataset = freeze_set(rows, name="answers", version="labels-1", question="Correct?")
    assert dataset["cases"][0]["label"] is None
    assert dataset["cases"][1]["label"] is False
    assert dataset["cases"][0]["record"]["reference"] == "task reference"
    rows[0]["ratings"][0]["passed"] = False
    assert dataset["cases"][0]["ratings"][0]["passed"] is True
    path = tmp_path / "frozen.json"
    pin = write_artifact(path, dataset)
    assert read_artifact(path, expected=pin) == (dataset, pin)
    if os.name == "posix":
        assert path.stat().st_mode & 0o777 == 0o600
    revised = copy.deepcopy(dataset)
    revised["version"] = "labels-2"
    assert digest(revised) != pin
    replacement = tmp_path / "replacement.json"
    write_artifact(replacement, revised)
    path.write_bytes(replacement.read_bytes())
    with pytest.raises(ValueError, match="SHA-256 mismatch"):
        read_artifact(path, expected=pin)
    assert read_artifact(path)[0] == revised  # A self-hash alone is not an approved pin.


def test_atomic_publish_never_replaces_and_handles_races(tmp_path: Path) -> None:
    path = tmp_path / "set.json"

    def attempt(_: int) -> bool:
        try:
            write_artifact(path, frozen())
            return True
        except FileExistsError:
            return False

    with ThreadPoolExecutor(max_workers=4) as pool:
        assert sum(pool.map(attempt, range(8))) == 1
    assert read_artifact(path)[0] == frozen()
    if os.name == "posix":
        link = tmp_path / "link.json"
        link.symlink_to(path)
        with pytest.raises(FileExistsError):
            write_artifact(link, frozen())
    assert sorted(p.name for p in tmp_path.iterdir()) == (
        ["link.json", "set.json"] if os.name == "posix" else ["set.json"]
    )


@pytest.mark.parametrize("raw", [b'{"x":1,"x":2}', b'{"x":NaN}', b'{"x":1e999}'])
def test_ambiguous_or_nonfinite_json_rejected(raw: bytes) -> None:
    with pytest.raises(ValueError, match=r"duplicate JSON key|non-finite|JSON compliant"):
        decode(raw)


@pytest.mark.parametrize(
    "change",
    [
        "duplicate id",
        "cross group",
        "duplicate input",
        "bad rating",
        "duplicate rater",
        "bad consensus",
        "no held-out",
        "unknown",
    ],
)
def test_invalid_set_is_rejected(change: str) -> None:
    dataset = frozen()
    a, b = dataset["cases"]
    if change == "duplicate id":
        b["record"]["id"] = a["record"]["id"]
    elif change == "cross group":
        b["group"] = a["group"]
    elif change == "duplicate input":
        b["record"]["input"] = a["record"]["input"]
    elif change == "bad rating":
        b["ratings"][0]["passed"] = 1
    elif change == "duplicate rater":
        a["ratings"].append(a["ratings"][0])
    elif change == "bad consensus":
        a["label"] = True
    elif change == "no held-out":
        b["split"] = "tuning"
    else:
        dataset["new_field"] = True
    messages = {
        "duplicate id": "duplicate record id",
        "cross group": "crosses",
        "duplicate input": "identical inputs",
        "bad rating": "must be booleans",
        "duplicate rater": "only once",
        "bad consensus": "does not match",
        "no held-out": "must contain tuning and held-out",
        "unknown": "calibration-set fields",
    }
    with pytest.raises(ValueError, match=messages[change]):
        validate_set(dataset)


def test_same_input_cases_must_share_independent_group_even_within_split() -> None:
    rows = labelled_rows()
    extra = copy.deepcopy(rows[0])
    extra["record"]["id"] = "a-second-output"
    extra["record"]["output"] = "a different target answer"
    with pytest.raises(ValueError, match="identical inputs"):
        freeze_set([*rows, extra], name="n", version="v", question="q")
    rows[0]["group"] = extra["group"] = "question-a"
    assert len(freeze_set([*rows, extra], name="n", version="v", question="q")["cases"]) == 3


def tool_conversation(arguments: str = '{"order":"one"}') -> dict[str, Any]:
    return {
        "messages": [
            {"role": "user", "content": "Check my order"},
            {
                "role": "assistant",
                "content": "",
                "tool_calls": [{"id": "call-1", "name": "lookup", "arguments": arguments}],
            },
        ]
    }


@pytest.mark.parametrize("content", [tool_conversation(), {"json": "   "}])
@pytest.mark.parametrize("split", ["tuning", "held-out"])
def test_structured_input_with_blank_text_projection_cannot_leak(
    content: dict[str, Any], split: str
) -> None:
    rows = labelled_rows()
    rows[0]["record"]["input"] = copy.deepcopy(content)
    extra = copy.deepcopy(rows[0])
    extra["record"]["id"] = "another-output"
    extra["record"]["output"] = "another answer"
    extra["split"] = split
    with pytest.raises(ValueError, match="identical inputs"):
        freeze_set([*rows, extra], name="n", version="v", question="q")
    if split == "tuning":
        rows[0]["group"] = extra["group"] = "same-conversation"
        assert len(freeze_set([*rows, extra], name="n", version="v", question="q")["cases"]) == 3


def test_tool_arguments_remain_part_of_structured_input_identity() -> None:
    rows = labelled_rows()
    rows[0]["record"]["input"] = tool_conversation()
    rows[1]["record"]["input"] = tool_conversation('{"order":"two"}')
    assert len(freeze_set(rows, name="n", version="v", question="q")["cases"]) == 2


@pytest.mark.parametrize("content", [None, "", {"text": "  \n"}])
def test_absent_or_blank_text_input_does_not_imply_overlap(content: Any) -> None:
    rows = labelled_rows()
    for row in rows:
        row["record"]["input"] = content
    assert len(freeze_set(rows, name="n", version="v", question="q")["cases"]) == 2


def exported() -> list[dict[str, Any]]:
    record = {
        "id": "one",
        "input": {"messages": {"messages": [{"role": "user", "content": "hello"}]}},
        "output": {"text": "world"},
        "reference": {"text": "original"},
        "usage": {"inputTokens": "17", "outputTokens": "3", "costUsd": 0.02},
    }
    return [
        {
            "queue": "q",
            "itemId": "item-1",
            "annotator": "alice",
            "answers": [{"question": "correct", "passed": False}],
            "createdAt": "2026-10-21T00:00:00Z",
            "item": {
                "id": "item-1",
                "record": record,
                "source": {"runId": "run-1", "trial": 1},
                "runScores": {"llm-judge": 1.0},
            },
        },
        {
            "queue": "q",
            "itemId": "item-2",
            "annotator": "alice",
            "skipped": True,
            "answers": [],
            "comment": "cannot decide",
            "item": {
                "id": "item-2",
                "record": {
                    "id": "two",
                    "input": {"text": "different question"},
                    "output": {"text": "answer"},
                },
            },
        },
    ]


def test_annotation_import_preserves_wire_content_and_excludes_machine_scores(
    tmp_path: Path,
) -> None:
    export = tmp_path / "ratings.jsonl"
    export.write_bytes(b"\n".join(canonical(row) for row in exported()) + b"\n")
    rows = annotation_rows(
        read_jsonl(export),
        question="correct",
        assignments={"item-1": {"split": "tuning"}, "item-2": {"split": "held-out"}},
    )
    dataset = freeze_set(rows, name="n", version="v", question="correct")
    one, two = dataset["cases"]
    assert one["label"] is False  # Machine score was 1.0.
    assert one["record"]["usage"]["input_tokens"] == 17
    assert one["record"]["input"]["messages"][0]["content"] == "hello"
    assert one["record"]["reference"] == {"text": "original"}
    assert one["source"]["run"] == {"runId": "run-1", "trial": 1}
    assert "runScores" not in json.dumps(dataset)
    assert two["label"] is None
    assert two["ratings"][0]["comment"] == "cannot decide"


@pytest.mark.parametrize(
    "change", ["missing item", "missing assignment", "numeric answer", "revision", "duplicate"]
)
def test_invalid_annotation_exports_rejected(change: str) -> None:
    annotations = exported()
    assignments = {"item-1": {"split": "tuning"}, "item-2": {"split": "held-out"}}
    if change == "missing item":
        annotations[0].pop("item")
    elif change == "missing assignment":
        assignments.pop("item-1")
    elif change == "numeric answer":
        annotations[0]["answers"] = [{"question": "correct", "number": 1}]
    elif change == "revision":
        extra = copy.deepcopy(annotations[0])
        extra["item"]["record"]["output"] = {"text": "revised"}
        annotations.append(extra)
    else:
        annotations.append(copy.deepcopy(annotations[0]))
    messages = {
        "missing item": "exported item",
        "missing assignment": "explicit split assignment",
        "numeric answer": "pass/fail human answers",
        "revision": "conflicting revisions",
        "duplicate": "only once",
    }
    with pytest.raises(ValueError, match=messages[change]):
        freeze_set(
            annotation_rows(annotations, question="correct", assignments=assignments),
            name="n",
            version="v",
            question="q",
        )

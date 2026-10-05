from __future__ import annotations

import pytest

from evalsi import Content, Record, Score, Usage


def test_content_from_string_is_text() -> None:
    assert Content.from_value("hi").text == "hi"


def test_content_from_chat_messages() -> None:
    content = Content.from_value(
        [
            {"role": "user", "content": "2+2?"},
            {
                "role": "assistant",
                "content": "4",
                "tool_calls": [{"name": "calc", "arguments": {"expr": "2+2"}}],
            },
        ]
    )
    assert content.messages is not None
    assert content.messages[1].tool_calls[0].arguments == '{"expr": "2+2"}'
    assert content.as_text() == "4"


def test_content_explicit_wrappers_and_json() -> None:
    assert Content.from_value({"text": "x"}).text == "x"
    assert Content.from_value({"json": "x"}).json == "x"
    as_json = Content.from_value({"a": 1})
    assert as_json.is_json
    assert as_json.as_text() == '{"a":1}'
    assert Content.from_value(42).as_text() == "42"


def test_content_json_null_is_allowed() -> None:
    content = Content(json=None, is_json=True)
    assert content.as_text() == "null"
    assert content.to_dict() == {"json": None}


def test_content_needs_exactly_one_kind() -> None:
    with pytest.raises(ValueError, match="exactly one"):
        Content()
    with pytest.raises(ValueError, match="exactly one"):
        Content(text="a", json={"b": 1})


def test_score_needs_exactly_one_value() -> None:
    with pytest.raises(ValueError, match="exactly one"):
        Score()
    with pytest.raises(ValueError, match="exactly one"):
        Score(number=1.0, passed=True)


def test_score_numeric_view() -> None:
    assert Score(passed=True).numeric() == 1.0
    assert Score(passed=False).numeric() == 0.0
    assert Score(number=3).numeric() == 3.0
    assert Score(label="cat").numeric() is None


def test_record_round_trips_to_dict() -> None:
    record = Record(
        id="r1",
        input=Content(text="q"),
        output=Content(text="a"),
        usage=Usage(input_tokens=3),
        metadata={"k": "v"},
    )
    assert record.to_dict() == {
        "id": "r1",
        "input": {"text": "q"},
        "output": {"text": "a"},
        "usage": {"input_tokens": 3},
        "metadata": {"k": "v"},
    }

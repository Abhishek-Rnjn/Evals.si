"""Images on tool results (H4): what the model is sent and what the record keeps."""

from __future__ import annotations

from evalsi.convert import content_from_proto, content_to_proto
from evalsi.types import Content, Media
from evalsi_harness.models import to_anthropic, to_openai_all
from evalsi_harness.tools import MAX_STEP_IMAGE_BYTES, Image, image_blocks, image_step

PNG = b"\x89PNG\r\n\x1a\n" + bytes(range(64))


def tool_message() -> dict[str, object]:
    return {
        "role": "tool",
        "tool_call_id": "c1",
        "content": "Screenshot of http://x/",
        "images": image_blocks([Image("image/png", PNG)]),
    }


def test_openai_gets_the_image_in_a_user_message_after_the_tool_message() -> None:
    tool, user = to_openai_all(tool_message())
    assert tool == {"role": "tool", "tool_call_id": "c1", "content": "Screenshot of http://x/"}
    assert user["role"] == "user"
    text, image = user["content"]
    assert text["type"] == "text"
    assert image["image_url"]["url"].startswith("data:image/png;base64,iVBORw0KGgo")
    assert to_openai_all({"role": "tool", "tool_call_id": "c", "content": "x"}) == [
        {"role": "tool", "tool_call_id": "c", "content": "x"}
    ]


def test_anthropic_gets_image_blocks_inside_the_tool_result() -> None:
    (turn,) = to_anthropic([tool_message()])
    (block,) = turn["content"]
    text, image = block["content"]
    assert text == {"type": "text", "text": "Screenshot of http://x/"}
    assert image["type"] == "image"
    assert image["source"]["type"] == "base64"
    assert image["source"]["media_type"] == "image/png"


def test_an_image_step_is_a_media_child_and_big_ones_are_only_noted() -> None:
    step = image_step(Image("image/png", PNG), "parent1")
    assert (step.type, step.name, step.parent_span_id) == ("generic", "image", "parent1")
    assert step.output is not None
    assert step.output.media is not None
    assert step.output.media.uri.startswith("data:image/png;base64,")
    assert step.output.as_text() == "[image/png]"  # never the bytes
    assert step.attributes["evalsi.media.bytes"] == len(PNG)
    big = image_step(Image("image/png", bytes(MAX_STEP_IMAGE_BYTES + 1)), "p")
    assert big.output is not None
    assert big.output.media is None
    assert "not recorded" in big.output.as_text()


def test_media_content_round_trips_through_dicts_and_the_proto() -> None:
    content = Content(media=Media("https://x/a.png", "image/png"))
    assert Content.from_value(content.to_dict()) == content
    assert content_from_proto(content_to_proto(content)) == content
    assert content.as_text() == "[image/png https://x/a.png]"

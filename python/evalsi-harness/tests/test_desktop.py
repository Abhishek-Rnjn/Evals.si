"""The computer toolset (H4): the action mapping with a fake display, and a real Xvfb."""

from __future__ import annotations

import asyncio
import shutil
import struct
import time
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.desktop import (
    Computer,
    DesktopConfig,
    DesktopUnavailable,
    XvfbDisplay,
    computer_tool,
    missing_binaries,
)
from evalsi_harness.testing import LocalSandboxClient, Script, ScriptedModelServer
from evalsi_harness.trust import Trust
from tests.harness_support import record, spec_of

PNG = b"\x89PNG\r\n\x1a\n" + bytes(range(32))


class FakeDisplay:
    def __init__(self) -> None:
        self.calls: list[tuple[str, ...]] = []
        self.started = False
        self.closed = False

    async def start(self) -> None:
        self.started = True

    async def xdotool(self, *args: str) -> str:
        self.calls.append(args)
        return "X=10 Y=20 SCREEN=0 WINDOW=1" if args[0] == "getmouselocation" else ""

    async def screenshot(self) -> bytes:
        return PNG

    async def aclose(self) -> None:
        self.closed = True


def act(**args: Any) -> tuple[FakeDisplay, Any]:
    display = FakeDisplay()
    computer = Computer(display, DesktopConfig(width=800, height=600, settle_s=0))
    return display, asyncio.run(computer.call(**args))


def test_actions_map_to_xdotool_and_return_a_screenshot() -> None:
    cases: list[tuple[dict[str, Any], list[tuple[str, ...]]]] = [
        (
            {"action": "left_click", "coordinate": [100, 50]},
            [("mousemove", "100", "50"), ("click", "--repeat", "1", "--delay", "80", "1")],
        ),
        (
            {"action": "right_click", "coordinate": [1, 2]},
            [("mousemove", "1", "2"), ("click", "--repeat", "1", "--delay", "80", "3")],
        ),
        (
            {"action": "double_click", "coordinate": [5, 5]},
            [("mousemove", "5", "5"), ("click", "--repeat", "2", "--delay", "80", "1")],
        ),
        (
            {"action": "left_click", "coordinate": [5, 5], "text": "shift"},
            [
                ("mousemove", "5", "5"),
                ("keydown", "shift"),
                ("click", "--repeat", "1", "--delay", "80", "1"),
                ("keyup", "shift"),
            ],
        ),
        ({"action": "mouse_move", "coordinate": [7, 8]}, [("mousemove", "7", "8")]),
        (
            {"action": "left_click_drag", "start_coordinate": [1, 1], "coordinate": [9, 9]},
            [("mousemove", "1", "1", "mousedown", "1", "mousemove", "9", "9"), ("mouseup", "1")],
        ),
        (
            {
                "action": "scroll",
                "coordinate": [3, 3],
                "scroll_direction": "down",
                "scroll_amount": 4,
            },
            [("mousemove", "3", "3"), ("click", "--repeat", "4", "5")],
        ),
        ({"action": "key", "text": "ctrl+s"}, [("key", "--clearmodifiers", "--", "ctrl+s")]),
        ({"action": "type", "text": "hello"}, [("type", "--delay", "12", "--", "hello")]),
    ]
    for args, expected in cases:
        display, result = act(**args)
        assert not result.is_error, (args, result.content)
        assert display.calls == expected, args
        (image,) = result.images
        assert image.data == PNG


def test_long_text_is_typed_in_chunks_and_queries_return_text() -> None:
    display, _ = act(action="type", text="x" * 120)
    assert [len(c[-1]) for c in display.calls] == [50, 50, 20]
    _, pos = act(action="cursor_position")
    assert pos.content == "X=10,Y=20"
    assert pos.images == []
    _, shot = act(action="screenshot")
    assert shot.images
    assert shot.content == "Screenshot"
    _, waited = act(action="wait", duration=0)
    assert waited.images


def test_bad_arguments_are_tool_errors_and_never_reach_the_display() -> None:
    bad: list[dict[str, Any]] = [
        {"action": "explode"},
        {"action": "left_click", "coordinate": [800, 10]},  # outside 800x600
        {"action": "left_click", "coordinate": "10,10"},
        {"action": "mouse_move"},
        {"action": "key", "text": "ctrl+s; rm -rf /"},
        {"action": "key", "text": "--help"},
        {"action": "key"},
        {"action": "type"},
        {"action": "type", "text": "x" * 6000},
        {"action": "scroll", "scroll_direction": "sideways"},
        {"action": "scroll", "scroll_direction": "up", "scroll_amount": 500},
        {"action": "left_click", "text": "a b; c"},
    ]
    for args in bad:
        display, result = act(**args)
        assert result.is_error, args
        assert display.calls == [], args


def test_the_display_starts_on_the_first_call_and_a_missing_one_is_a_tool_error() -> None:
    display = FakeDisplay()
    computer = Computer(display, DesktopConfig(settle_s=0))
    assert not display.started
    asyncio.run(computer.call(action="screenshot"))
    assert display.started

    class Broken(FakeDisplay):
        async def start(self) -> None:
            raise DesktopUnavailable("Xvfb is not installed")

    result = asyncio.run(Computer(Broken(), DesktopConfig()).call(action="screenshot"))
    assert result.is_error
    assert "Xvfb" in result.content


def test_the_tool_spec_follows_the_computer_use_parameter_names() -> None:
    spec = computer_tool(Computer(FakeDisplay(), DesktopConfig(width=640, height=480))).spec
    assert spec.name == "computer"
    assert "640x480" in spec.description
    props = spec.input_schema["properties"]
    for name in ("action", "coordinate", "text", "scroll_direction", "scroll_amount", "duration"):
        assert name in props
    assert spec.input_schema["required"] == ["action"]


def test_config_checks_size_and_launch() -> None:
    assert DesktopConfig.from_metadata({}).width == 1024
    cfg = DesktopConfig.from_metadata(
        {"desktop": {"launch": [["xterm", "-e", "sh"]], "settle_s": 99}}
    )
    assert cfg.launch == [["xterm", "-e", "sh"]]
    assert cfg.settle_s == 10.0
    bad_configs: list[dict[str, Any]] = [{"width": 50}, {"launch": ["xterm"]}, {"launch": [[]]}]
    bad_configs.append({"width": "wide"})
    for bad in bad_configs:
        with pytest.raises(ValueError, match=r"metadata\.desktop"):
            DesktopConfig.from_metadata({"desktop": bad})


def test_a_server_worker_refuses_the_toolsets_unless_trusted(
    sandboxes: LocalSandboxClient, model: Callable[[Script], ScriptedModelServer]
) -> None:
    server = model(lambda m, t: {"text": "done"})
    for tool, meta in (("computer", {"desktop": {"launch": [["xterm"]]}}), ("browser", {})):
        spec = spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {"builtin": {"tools": {"sandbox": False, "builtin": [tool]}}},
            }
        )
        outcome = run_restricted(spec, record(metadata=meta), sandboxes)
        assert outcome.error
        assert "evalsi-" in outcome.error
        assert "trusted_commands" in outcome.error


def run_restricted(spec: Any, rec: Any, sandboxes: LocalSandboxClient) -> Any:
    from evalsi_harness import HarnessContext, Task, load_harness, run_task

    async def go() -> Any:
        async def get() -> LocalSandboxClient:
            return sandboxes

        ctx = HarnessContext(sandboxes=get, base_dir=Path.cwd(), trust=Trust(commands=[]))
        harness = load_harness(spec, ctx)
        try:
            return await run_task(Task.build(spec, rec, run_id="r"), harness)
        finally:
            await harness.aclose()

    return asyncio.run(go())


# --- a real display ---

needs_x = pytest.mark.skipif(
    bool(missing_binaries()) or shutil.which("xterm") is None,
    reason=f"needs Xvfb, xdotool, ImageMagick import and xterm (missing: {missing_binaries()})",
)


def png_size(data: bytes) -> tuple[int, int]:
    assert data.startswith(b"\x89PNG\r\n\x1a\n")
    return struct.unpack(">II", data[16:24])


def with_display(config: DesktopConfig, body: Callable[[Computer], Awaitable[None]]) -> None:
    async def go() -> None:
        display = XvfbDisplay(config)
        try:
            await body(Computer(display, config))
        finally:
            await display.aclose()

    asyncio.run(go())


@needs_x
def test_a_real_xvfb_screenshots_moves_the_mouse_and_types_into_an_app(tmp_path: Path) -> None:
    marker = tmp_path / "typed.txt"
    config = DesktopConfig(
        width=640,
        height=480,
        settle_s=0.2,
        launch=[["xterm", "-geometry", "60x15+0+0", "-e", "sh", "-c", f"cd {tmp_path}; exec sh"]],
    )

    async def body(computer: Computer) -> None:
        shot = await computer.call(action="screenshot")
        assert png_size(shot.images[0].data) == (640, 480)
        moved = await computer.call(action="mouse_move", coordinate=[123, 45])
        assert not moved.is_error
        assert (await computer.call(action="cursor_position")).content == "X=123,Y=45"
        # Focus follows the pointer: click inside the terminal, then type a command into it.
        await asyncio.sleep(1.0)  # xterm needs a moment to map its window
        await computer.call(action="left_click", coordinate=[100, 100])
        await computer.call(action="type", text="echo from-the-agent > typed.txt")
        await computer.call(action="key", text="Return")
        for _ in range(30):
            if marker.exists():
                break
            await asyncio.sleep(0.2)
        assert marker.read_text().strip() == "from-the-agent"
        # The screen changed: the terminal is drawn, so it is not the empty first screenshot.
        after = await computer.call(action="screenshot")
        assert after.images[0].data != shot.images[0].data

    started = time.time()
    with_display(config, body)
    assert time.time() - started < 60


@needs_x
def test_the_display_is_killed_on_close() -> None:
    config = DesktopConfig(settle_s=0)
    holder: dict[str, XvfbDisplay] = {}

    async def go() -> None:
        display = holder["d"] = XvfbDisplay(config)
        await display.start()
        assert display.display.startswith(":")
        await display.aclose()

    asyncio.run(go())
    assert holder["d"]._xvfb is None

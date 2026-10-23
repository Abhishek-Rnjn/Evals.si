"""A virtual desktop for computer-use tools: the ``computer`` toolset (H4).

``harness.builtin.tools.builtin: [computer]`` gives the agent one tool, ``computer``,
shaped like Anthropic's computer-use tool: an ``action`` (``screenshot``, ``mouse_move``,
``left_click``, ``right_click``, ``middle_click``, ``double_click``, ``triple_click``,
``left_click_drag``, ``left_mouse_down``, ``left_mouse_up``, ``scroll``, ``key``,
``hold_key``, ``type``, ``cursor_position``, ``wait``) with ``coordinate``,
``start_coordinate``, ``text``, ``scroll_direction``, ``scroll_amount`` and ``duration``,
in pixels of the display, whose size is ``width`` by ``height``. It is offered as an
ordinary function tool, so any model that can call tools can use it, with Anthropic's
parameter names so a model trained on the native tool finds it familiar. It is not the
native ``computer_20250124`` tool type (no beta header, no automatic scaling), and the
zoom action and the newer ``left_click`` modifier-only forms are not built. Every
action that is not a screenshot, a cursor query or a wait returns a screenshot of the
result, as the reference implementation does.

**Where it runs.** An Xvfb display, driven by ``xdotool`` and captured with ImageMagick's
``import``, runs on the *harness's host*, not in the task's sandbox: the sandbox keeps
no background processes between calls, so a display cannot live in it. Anything the task
launches (``metadata.desktop.launch``, a list of argument lists) therefore runs on the
host without the sandbox, so use it for trusted fixture apps, and note that a server
worker refuses the toolset and the launch commands unless its config trusts them
(``agents.trusted_commands`` listing ``["evalsi-desktop"]`` and each launch command).
A desktop whose apps live in the task's image is not built.

Options come from the record: ``metadata.desktop`` is ``{"width", "height", "launch":
[[argv], ...], "settle_s"}``. A missing ``Xvfb``, ``xdotool`` or ``import`` is a clear tool
error on the first call, and the tests that need them skip.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import re
import shutil
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi_harness.tools import FunctionTool, Image, Tool, ToolResult

MAX_SIZE = 4096
MAX_TYPE_CHARS = 5000
MAX_WAIT_S = 30.0
ACTIONS = (
    "screenshot",
    "mouse_move",
    "left_click",
    "right_click",
    "middle_click",
    "double_click",
    "triple_click",
    "left_click_drag",
    "left_mouse_down",
    "left_mouse_up",
    "scroll",
    "key",
    "hold_key",
    "type",
    "cursor_position",
    "wait",
)
_KEY = re.compile(r"^[A-Za-z0-9_+ ]{1,80}$")
_SCROLL_BUTTON = {"up": "4", "down": "5", "left": "6", "right": "7"}
_CLICK_BUTTON = {"left_click": "1", "middle_click": "2", "right_click": "3"}
_REPEAT = {"double_click": 2, "triple_click": 3}
INSTALL_HINT = (
    "the computer tool needs Xvfb, xdotool and ImageMagick's `import` on the harness host "
    "(apt install xvfb xdotool imagemagick)"
)


class DesktopUnavailable(RuntimeError):
    """The display or one of its tools is missing, or Xvfb would not start."""


class Display(Protocol):
    """What the ``computer`` tool needs of a display. ``XvfbDisplay`` is the real one."""

    async def start(self) -> None: ...

    async def xdotool(self, *args: str) -> str: ...

    async def screenshot(self) -> bytes: ...

    async def aclose(self) -> None: ...


@dataclass
class DesktopConfig:
    width: int = 1024
    height: int = 768
    launch: list[list[str]] = field(default_factory=list)
    settle_s: float = 0.5

    @classmethod
    def from_metadata(cls, metadata: Mapping[str, Any]) -> DesktopConfig:
        raw = metadata.get("desktop")
        data: Mapping[str, Any] = raw if isinstance(raw, Mapping) else {}
        try:
            width, height = int(data.get("width") or 1024), int(data.get("height") or 768)
        except (TypeError, ValueError):
            raise ValueError("metadata.desktop.width and height must be numbers") from None
        if not (200 <= width <= MAX_SIZE and 200 <= height <= MAX_SIZE):
            raise ValueError(f"metadata.desktop size must be 200 to {MAX_SIZE} pixels")
        launch = data.get("launch") or []
        if not isinstance(launch, list) or not all(
            isinstance(c, list) and c and all(isinstance(a, str) for a in c) for c in launch
        ):
            raise ValueError("metadata.desktop.launch must be a list of non-empty argument lists")
        settle = float(data.get("settle_s", 0.5))
        return cls(width, height, [list(c) for c in launch], max(0.0, min(settle, 10.0)))


def missing_binaries() -> list[str]:
    return [b for b in ("Xvfb", "xdotool", "import") if shutil.which(b) is None]


class XvfbDisplay:
    """An Xvfb display on this host. Started on ``start``, killed on ``aclose``, with the
    apps launched on it."""

    def __init__(self, config: DesktopConfig) -> None:
        self.config = config
        self._xvfb: asyncio.subprocess.Process | None = None
        self._apps: list[asyncio.subprocess.Process] = []
        self.display = ""

    def _env(self) -> dict[str, str]:
        return {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "HOME": os.environ.get("HOME", "/tmp"),
            "DISPLAY": self.display,
        }

    async def start(self) -> None:
        if self._xvfb is not None:
            return
        if missing := missing_binaries():
            raise DesktopUnavailable(f"{INSTALL_HINT} (missing: {', '.join(missing)})")
        read_end, write_end = os.pipe()
        try:
            self._xvfb = await asyncio.create_subprocess_exec(
                "Xvfb",
                "-displayfd",
                str(write_end),
                "-screen",
                "0",
                f"{self.config.width}x{self.config.height}x24",
                "-nolisten",
                "tcp",
                pass_fds=(write_end,),
                stdout=asyncio.subprocess.DEVNULL,
                stderr=asyncio.subprocess.DEVNULL,
            )
        finally:
            os.close(write_end)
        try:
            # Xvfb writes its display number, and a newline, once it is ready.
            number = await asyncio.wait_for(
                asyncio.get_running_loop().run_in_executor(None, _read_line, read_end), 15
            )
        except TimeoutError:
            await self.aclose()
            raise DesktopUnavailable("Xvfb did not report a display within 15 seconds") from None
        finally:
            os.close(read_end)
        if not number.strip().isdigit():
            await self.aclose()
            raise DesktopUnavailable("Xvfb exited before it was ready")
        self.display = f":{number.strip()}"
        for argv in self.config.launch:
            try:
                self._apps.append(
                    await asyncio.create_subprocess_exec(
                        *argv,
                        env=self._env(),
                        stdin=asyncio.subprocess.DEVNULL,
                        stdout=asyncio.subprocess.DEVNULL,
                        stderr=asyncio.subprocess.DEVNULL,
                    )
                )
            except OSError as exc:
                await self.aclose()
                raise DesktopUnavailable(f"could not launch {argv[0]}: {exc}") from None

    async def _run(self, *argv: str, timeout: float = 30.0) -> bytes:
        proc = await asyncio.create_subprocess_exec(
            *argv,
            env=self._env(),
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        try:
            out, err = await asyncio.wait_for(proc.communicate(), timeout)
        except TimeoutError:
            proc.kill()
            await proc.wait()
            raise RuntimeError(f"{argv[0]} timed out") from None
        if proc.returncode != 0:
            detail = err.decode(errors="replace").strip().splitlines()
            raise RuntimeError(f"{argv[0]} failed: {detail[0] if detail else proc.returncode}")
        return out

    async def xdotool(self, *args: str) -> str:
        return (await self._run("xdotool", *args)).decode(errors="replace")

    async def screenshot(self) -> bytes:
        return await self._run("import", "-window", "root", "png:-")

    async def aclose(self) -> None:
        for proc in (*self._apps, self._xvfb):
            if proc is not None and proc.returncode is None:
                with contextlib.suppress(ProcessLookupError):
                    proc.kill()
                with contextlib.suppress(Exception):
                    await proc.wait()
        self._apps = []
        self._xvfb = None


def _read_line(fd: int) -> str:
    data = b""
    while not data.endswith(b"\n"):
        chunk = os.read(fd, 64)
        if not chunk:
            break
        data += chunk
    return data.decode(errors="replace")


class Computer:
    """The ``computer`` tool over a display."""

    def __init__(self, display: Display, config: DesktopConfig) -> None:
        self.display = display
        self.config = config
        self._lock = asyncio.Lock()
        self._started = False

    def _point(self, value: Any, what: str) -> tuple[int, int]:
        ok = isinstance(value, Sequence) and not isinstance(value, str) and len(value) == 2
        if not ok or not all(isinstance(v, int | float) and not isinstance(v, bool) for v in value):
            raise ValueError(f"{what} must be [x, y] in pixels")
        x, y = int(value[0]), int(value[1])
        if not (0 <= x < self.config.width and 0 <= y < self.config.height):
            raise ValueError(
                f"{what} {[x, y]} is outside the {self.config.width}x{self.config.height} display"
            )
        return x, y

    async def _after(self, text: str) -> ToolResult:
        await asyncio.sleep(self.config.settle_s)
        return ToolResult(text, images=[Image("image/png", await self.display.screenshot())])

    async def act(self, args: Mapping[str, Any]) -> ToolResult:
        action = args.get("action")
        if action not in ACTIONS:
            return ToolResult(f"action must be one of {', '.join(ACTIONS)}", is_error=True)
        text = args.get("text")
        try:
            return await self._act(str(action), text if isinstance(text, str) else None, args)
        except ValueError as exc:
            return ToolResult(str(exc), is_error=True)
        except RuntimeError as exc:
            return ToolResult(f"the display failed: {exc}", is_error=True)

    async def _act(self, action: str, text: str | None, args: Mapping[str, Any]) -> ToolResult:  # noqa: PLR0915
        d = self.display
        coordinate = args.get("coordinate")
        if action == "screenshot":
            return ToolResult("Screenshot", images=[Image("image/png", await d.screenshot())])
        if action == "cursor_position":
            out = await d.xdotool("getmouselocation", "--shell")
            pos = dict(line.split("=", 1) for line in out.split() if "=" in line)
            return ToolResult(f"X={pos.get('X', '?')},Y={pos.get('Y', '?')}")
        if action == "wait":
            seconds = args.get("duration", 1)
            if not isinstance(seconds, int | float) or isinstance(seconds, bool) or seconds < 0:
                raise ValueError("duration must be a number of seconds")
            await asyncio.sleep(min(float(seconds), MAX_WAIT_S))
            return await self._after(f"Waited {min(float(seconds), MAX_WAIT_S):g} seconds")
        if action == "mouse_move":
            x, y = self._point(coordinate, "coordinate")
            await d.xdotool("mousemove", str(x), str(y))
            return await self._after(f"Moved the mouse to {x},{y}")
        if action in _CLICK_BUTTON or action in _REPEAT:
            if coordinate is not None:
                x, y = self._point(coordinate, "coordinate")
                await d.xdotool("mousemove", str(x), str(y))
            modifier = _key(text) if text else ""
            if modifier:
                await d.xdotool("keydown", modifier)
            try:
                button = _CLICK_BUTTON.get(action, "1")
                repeat = str(_REPEAT.get(action, 1))
                await d.xdotool("click", "--repeat", repeat, "--delay", "80", button)
            finally:
                if modifier:
                    await d.xdotool("keyup", modifier)
            return await self._after(f"{action} done")
        if action == "left_click_drag":
            sx, sy = self._point(args.get("start_coordinate"), "start_coordinate")
            x, y = self._point(coordinate, "coordinate")
            await d.xdotool(
                "mousemove", str(sx), str(sy), "mousedown", "1", "mousemove", str(x), str(y)
            )
            await d.xdotool("mouseup", "1")
            return await self._after(f"Dragged from {sx},{sy} to {x},{y}")
        if action in ("left_mouse_down", "left_mouse_up"):
            await d.xdotool("mousedown" if action.endswith("down") else "mouseup", "1")
            return await self._after(f"{action} done")
        if action == "scroll":
            direction = args.get("scroll_direction")
            if direction not in _SCROLL_BUTTON:
                raise ValueError("scroll_direction must be up, down, left or right")
            amount = args.get("scroll_amount", 3)
            if not isinstance(amount, int) or isinstance(amount, bool) or not 1 <= amount <= 50:
                raise ValueError("scroll_amount must be a whole number from 1 to 50")
            if coordinate is not None:
                x, y = self._point(coordinate, "coordinate")
                await d.xdotool("mousemove", str(x), str(y))
            await d.xdotool("click", "--repeat", str(amount), _SCROLL_BUTTON[str(direction)])
            return await self._after(f"Scrolled {direction} by {amount}")
        if action == "key":
            await d.xdotool("key", "--clearmodifiers", "--", _key(text))
            return await self._after(f"Pressed {text}")
        if action == "hold_key":
            key = _key(text)
            await d.xdotool("keydown", key)
            try:
                seconds = args.get("duration", 1)
                if not isinstance(seconds, int | float) or isinstance(seconds, bool):
                    raise ValueError("duration must be a number of seconds")
                await asyncio.sleep(max(0.0, min(float(seconds), MAX_WAIT_S)))
            finally:
                await d.xdotool("keyup", key)
            return await self._after(f"Held {key}")
        # type
        if not text:
            raise ValueError("type needs text")
        if len(text) > MAX_TYPE_CHARS:
            raise ValueError(f"text is limited to {MAX_TYPE_CHARS} characters")
        for i in range(0, len(text), 50):
            await d.xdotool("type", "--delay", "12", "--", text[i : i + 50])
        return await self._after(f"Typed {len(text)} characters")

    async def call(self, **args: Any) -> ToolResult:
        async with self._lock:
            try:
                if not self._started:
                    await self.display.start()
                    self._started = True
            except DesktopUnavailable as exc:
                return ToolResult(str(exc), is_error=True)
            return await self.act(args)


def _key(text: str | None) -> str:
    if text is None or not _KEY.match(text):
        raise ValueError("text must be an xdotool key name such as Return, ctrl+s or alt+Tab")
    return text


def computer_tool(computer: Computer) -> Tool:
    cfg = computer.config
    return FunctionTool(
        "computer",
        computer.call,
        description=(
            f"Control a {cfg.width}x{cfg.height} desktop with a mouse and keyboard; coordinates "
            "are pixels from the top left. Actions that change the screen return a screenshot."
        ),
        input_schema={
            "type": "object",
            "properties": {
                "action": {"type": "string", "enum": list(ACTIONS)},
                "coordinate": {"type": "array", "items": {"type": "integer"}},
                "start_coordinate": {"type": "array", "items": {"type": "integer"}},
                "text": {
                    "type": "string",
                    "description": "Text to type, a key such as ctrl+s, or a modifier held "
                    "during a click.",
                },
                "scroll_direction": {"type": "string", "enum": list(_SCROLL_BUTTON)},
                "scroll_amount": {"type": "integer"},
                "duration": {"type": "number"},
            },
            "required": ["action"],
        },
    )


__all__ = [
    "Computer",
    "DesktopConfig",
    "DesktopUnavailable",
    "Display",
    "XvfbDisplay",
    "computer_tool",
    "missing_binaries",
]

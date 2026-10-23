"""A browser for the built-in agent: the ``browser`` toolset (H4).

``harness.builtin.tools.builtin: [browser]`` gives the agent tools that drive a real
Chromium through Playwright, the way WebArena, BrowserGym and GAIA web tasks expect:

* ``browser_navigate(url)``, ``browser_back()``;
* ``browser_snapshot()``: the page as an accessibility-style tree, one line per element
  (``- link "Pricing" [ref=e7]``). Interactive elements carry a ``ref``;
* ``browser_click(ref)``, ``browser_type(ref, text, submit)``, ``browser_select(ref, value)``:
  act on an element by the ref of the latest snapshot (a ref is only good until the
  next snapshot or page change; a stale one is an error that says so);
* ``browser_screenshot()``: a PNG the model sees as an image, recorded as a ``media``
  step under the tool's step;
* ``browser_tabs(action, index, url)``: ``list``, ``new``, ``select``, ``close``. A tab
  the page opens becomes the current one.

Every action but a screenshot answers with the page URL, title and a fresh snapshot.

**Where it runs and what it may reach.** The browser runs in the harness process's
host, not in the task's sandbox, so the sandbox's network policy does not apply to
it. Its own policy does: only http(s) pages on the hosts in ``allow_hosts`` load, and
every other request (a redirect, an image, a script, a frame) is aborted; ``file:``
and other schemes never load. ``allow_hosts`` entries are ``host``, ``host:port`` or
``*.suffix`` (a bare host means ports 80 and 443). The default is the start URL's host.
Websocket connections and anything inside a cross-origin frame's own context are
not filtered or snapshotted. Treat a page as hostile input: it can write anything
into the snapshot the model reads.

Options come from the record: ``metadata.browser`` is ``{"start_url", "allow_hosts",
"viewport": [w, h], "timeout_s", "max_snapshot_chars", "storage_state"}``
(``storage_state`` is the path of a Playwright storage-state file, for a logged-in
session). The first tool call opens the browser at ``start_url``. Playwright is an optional install
(``pip install 'evalsi-harness[browser]'``, then the browsers, or point
``PLAYWRIGHT_BROWSERS_PATH`` at an existing install, or ``EVALSI_BROWSER_EXECUTABLE`` at a
Chromium binary); a missing install is a clear error on the first call. Chromium
refuses to start as root without its sandbox, so
``--no-sandbox`` is added only when the process is root, and that is a weaker boundary:
run the harness as an ordinary user where you can.
"""

from __future__ import annotations

import asyncio
import contextlib
import fnmatch
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from evalsi_harness.tools import FunctionTool, Image, Tool, ToolResult

MAX_SNAPSHOT_CHARS = 30_000
DEFAULT_TIMEOUT_S = 15.0
_REF = re.compile(r"^e\d{1,6}$")

INSTALL_HINT = (
    "the browser tools need Playwright and a Chromium: pip install 'evalsi-harness[browser]', "
    "then `playwright install chromium` (or set PLAYWRIGHT_BROWSERS_PATH to an existing install)"
)


class BrowserUnavailable(RuntimeError):
    """Playwright or its browser is not installed, or the browser would not start."""


def host_allowed(url: str, allow_hosts: list[str]) -> bool:
    """Whether an http(s) URL is on the allowlist (``host``, ``host:port``, ``*.suffix``)."""
    parts = urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        return False
    host = parts.hostname.lower()
    try:
        port = parts.port
    except ValueError:
        return False
    port = port or (443 if parts.scheme == "https" else 80)
    for entry in allow_hosts:
        pattern, sep, entry_port = entry.lower().rpartition(":")
        if not sep or not entry_port.isdigit():
            pattern, entry_port = entry.lower(), ""
        if not fnmatch.fnmatchcase(host, pattern):
            continue
        if entry_port:
            if int(entry_port) == port:
                return True
        elif port in (80, 443):
            return True
    return False


@dataclass
class BrowserConfig:
    start_url: str = ""
    allow_hosts: list[str] = field(default_factory=list)
    viewport: tuple[int, int] = (1024, 768)
    timeout_s: float = DEFAULT_TIMEOUT_S
    max_snapshot_chars: int = MAX_SNAPSHOT_CHARS
    # A Playwright storage-state file (cookies, local storage) for a logged-in session.
    storage_state: str = ""

    @classmethod
    def from_metadata(cls, metadata: Mapping[str, Any]) -> BrowserConfig:
        raw = metadata.get("browser")
        data: Mapping[str, Any] = raw if isinstance(raw, Mapping) else {}
        start = str(data.get("start_url") or "")
        hosts = [str(h) for h in data.get("allow_hosts") or []]
        if start and not hosts:
            parts = urlsplit(start)
            if parts.scheme in ("http", "https") and parts.netloc:
                hosts = [parts.netloc.rpartition("@")[2]]
        view = data.get("viewport") or (1024, 768)
        try:
            width, height = int(view[0]), int(view[1])
        except (TypeError, ValueError, IndexError):
            raise ValueError("metadata.browser.viewport must be [width, height]") from None
        if not (200 <= width <= 4096 and 200 <= height <= 4096):
            raise ValueError("metadata.browser.viewport must be between 200 and 4096 pixels")
        limit = int(data.get("max_snapshot_chars") or MAX_SNAPSHOT_CHARS)
        return cls(
            start_url=start,
            allow_hosts=hosts,
            viewport=(width, height),
            timeout_s=float(data.get("timeout_s") or DEFAULT_TIMEOUT_S),
            max_snapshot_chars=max(1000, min(limit, 200_000)),
            storage_state=str(data.get("storage_state") or ""),
        )


# Runs in the page (browser_snapshot.js): numbers the interactive elements
# (data-evalsi-ref="eN") and returns the tree as text. Plain DOM, no library.
SNAPSHOT_JS = (Path(__file__).with_name("browser_snapshot.js")).read_text(encoding="utf-8")


def _first_line(exc: BaseException) -> str:
    return (str(exc).strip().splitlines() or [type(exc).__name__])[0][:300]


class BrowserSession:
    """One browser context for one task trial. Opened on the first call, closed by ``aclose``."""

    def __init__(self, config: BrowserConfig) -> None:
        self.config = config
        self._pw: Any = None
        self._browser: Any = None
        self._context: Any = None
        self._page: Any = None
        self._lock = asyncio.Lock()

    @property
    def timeout_ms(self) -> float:
        return self.config.timeout_s * 1000

    async def start(self) -> None:
        if self._page is not None:
            return
        try:
            from playwright.async_api import async_playwright
        except ImportError:
            raise BrowserUnavailable(INSTALL_HINT) from None
        args = ["--no-sandbox"] if hasattr(os, "geteuid") and os.geteuid() == 0 else []
        try:
            self._pw = await async_playwright().start()
            self._browser = await self._pw.chromium.launch(
                headless=True,
                args=args,
                executable_path=os.environ.get("EVALSI_BROWSER_EXECUTABLE") or None,
            )
        except Exception as exc:
            await self.aclose()
            raise BrowserUnavailable(f"the browser did not start: {_first_line(exc)}") from None
        width, height = self.config.viewport
        options: dict[str, Any] = {
            "viewport": {"width": width, "height": height},
            "service_workers": "block",
        }
        if self.config.storage_state:
            options["storage_state"] = self.config.storage_state
        try:
            self._context = await self._browser.new_context(**options)
        except Exception as exc:
            await self.aclose()
            raise BrowserUnavailable(f"the browser context failed: {_first_line(exc)}") from None
        await self._context.route("**/*", self._route)
        self._context.set_default_timeout(self.timeout_ms)
        self._context.on("page", self._opened)
        self._page = await self._context.new_page()
        if self.config.start_url:
            await self._goto(self.config.start_url)

    def _opened(self, page: Any) -> None:
        self._page = page  # a tab the page opened becomes the current one

    async def _route(self, route: Any) -> None:
        url = route.request.url
        scheme = urlsplit(url).scheme
        if scheme in ("data", "blob", "about") or host_allowed(url, self.config.allow_hosts):
            await route.continue_()
        else:
            await route.abort("blockedbyclient")

    async def aclose(self) -> None:
        for closer in (self._context, self._browser):
            if closer is not None:
                with contextlib.suppress(Exception):  # the browser may already be gone
                    await closer.close()
        if self._pw is not None:
            with contextlib.suppress(Exception):
                await self._pw.stop()
        self._pw = self._browser = self._context = self._page = None

    async def _goto(self, url: str) -> None:
        if not host_allowed(url, self.config.allow_hosts):
            allowed = ", ".join(self.config.allow_hosts) or "none"
            raise PermissionError(f"{url} is not on this task's browser allowlist ({allowed})")
        await self._page.goto(url, wait_until="domcontentloaded", timeout=self.timeout_ms)

    async def _settle(self) -> None:
        try:
            await self._page.wait_for_load_state("domcontentloaded", timeout=3000)
            await self._page.wait_for_timeout(150)
        except Exception:
            pass

    async def snapshot(self) -> str:
        page = self._page
        try:
            tree: str = await page.evaluate(SNAPSHOT_JS)
        except Exception as exc:  # a navigation can destroy the context mid-read
            return f"(the page could not be read: {_first_line(exc)})"
        limit = self.config.max_snapshot_chars
        if len(tree) > limit:
            tree = tree[:limit] + f"\n... [{len(tree) - limit} characters omitted] ..."
        return tree

    async def page_text(self) -> str:
        page = self._page
        try:
            title = await page.title()
        except Exception:
            title = ""
        return f"Page URL: {page.url}\nPage Title: {title}\nSnapshot:\n{await self.snapshot()}"

    def _locator(self, ref: Any) -> Any:
        if not isinstance(ref, str) or not _REF.match(ref):
            raise ValueError("ref must be one of the refs in the latest snapshot, such as e7")
        return self._page.locator(f'[data-evalsi-ref="{ref}"]')

    async def _element(self, ref: Any) -> Any:
        loc = self._locator(ref)
        if await loc.count() == 0:
            raise LookupError(f"no element {ref} on the page now: take a new snapshot")
        return loc.first

    # --- the tools ---

    async def navigate(self, url: str) -> ToolResult:
        await self._goto(url)
        await self._settle()
        return ToolResult(await self.page_text())

    async def back(self) -> ToolResult:
        if await self._page.go_back(timeout=self.timeout_ms) is None:
            return ToolResult("there is no earlier page in this tab", is_error=True)
        await self._settle()
        return ToolResult(await self.page_text())

    async def click(self, ref: str) -> ToolResult:
        await (await self._element(ref)).click(timeout=self.timeout_ms)
        await self._settle()
        return ToolResult(await self.page_text())

    async def type(self, ref: str, text: str, submit: bool = False) -> ToolResult:
        element = await self._element(ref)
        await element.fill(text, timeout=self.timeout_ms)
        if submit:
            await element.press("Enter", timeout=self.timeout_ms)
        await self._settle()
        return ToolResult(await self.page_text())

    async def select(self, ref: str, value: str) -> ToolResult:
        await (await self._element(ref)).select_option(value, timeout=self.timeout_ms)
        await self._settle()
        return ToolResult(await self.page_text())

    async def screenshot(self) -> ToolResult:
        data = await self._page.screenshot(type="png", timeout=self.timeout_ms)
        title = await self._page.title()
        return ToolResult(
            f"Screenshot of {self._page.url} ({title})", images=[Image("image/png", data)]
        )

    async def tabs(self, action: str, index: int | None = None, url: str = "") -> ToolResult:
        pages = self._context.pages
        if action == "new":
            self._page = await self._context.new_page()
            if url:
                await self._goto(url)
                await self._settle()
            return ToolResult(await self.page_text())
        if action in ("select", "close"):
            if index is None or not 0 <= index < len(pages):
                return ToolResult(f"index must be 0 to {len(pages) - 1}", is_error=True)
            target = pages[index]
            if action == "close":
                if len(pages) == 1:
                    return ToolResult("the last tab cannot be closed", is_error=True)
                await target.close()
                pages = self._context.pages
                if self._page not in pages:
                    self._page = pages[-1]
            else:
                self._page = target
                await target.bring_to_front()
                return ToolResult(await self.page_text())
        elif action != "list":
            return ToolResult("action must be list, new, select or close", is_error=True)
        listing = [f"{'*' if p is self._page else '-'} {i}: {p.url}" for i, p in enumerate(pages)]
        return ToolResult("Tabs (* is current):\n" + "\n".join(listing))


def browser_tools(session: BrowserSession) -> list[Tool]:
    """The ``browser`` toolset over one session."""

    def wrap(name: str, description: str, properties: dict[str, Any], required: list[str]) -> Any:
        def decorate(fn: Any) -> Tool:
            async def run(**args: Any) -> ToolResult:
                async with session._lock:
                    try:
                        await session.start()
                        result: ToolResult = await fn(**args)
                        return result
                    except BrowserUnavailable as exc:
                        return ToolResult(str(exc), is_error=True)
                    except (PermissionError, LookupError, ValueError) as exc:
                        return ToolResult(str(exc), is_error=True)
                    except Exception as exc:  # Playwright's errors, timeouts
                        return ToolResult(
                            f"{type(exc).__name__}: {_first_line(exc)}", is_error=True
                        )

            return FunctionTool(
                name,
                run,
                description=description,
                input_schema={"type": "object", "properties": properties, "required": required},
            )

        return decorate

    ref = {"type": "string", "description": "A ref from the latest snapshot, such as e7."}
    return [
        wrap(
            "browser_navigate",
            "Open a URL in the current tab. Returns the page's snapshot.",
            {"url": {"type": "string"}},
            ["url"],
        )(session.navigate),
        wrap(
            "browser_snapshot",
            "The current page as a tree of elements. Interactive ones have a ref to act on. "
            "Prefer this to a screenshot for reading and for choosing what to click.",
            {},
            [],
        )(lambda: _snapshot(session)),
        wrap(
            "browser_screenshot",
            "A screenshot of the current page, as an image.",
            {},
            [],
        )(session.screenshot),
        wrap(
            "browser_click",
            "Click an element by its ref. Returns the new snapshot.",
            {"ref": ref},
            ["ref"],
        )(session.click),
        wrap(
            "browser_type",
            "Replace the text of an input or textarea by its ref; submit presses Enter after.",
            {"ref": ref, "text": {"type": "string"}, "submit": {"type": "boolean"}},
            ["ref", "text"],
        )(session.type),
        wrap(
            "browser_select",
            "Choose an option of a select element, by the option's value or its label.",
            {"ref": ref, "value": {"type": "string"}},
            ["ref", "value"],
        )(session.select),
        wrap("browser_back", "Go back one page in the current tab.", {}, [])(session.back),
        wrap(
            "browser_tabs",
            "List, open (new, with an optional url), select or close (with an index) tabs.",
            {
                "action": {"type": "string", "enum": ["list", "new", "select", "close"]},
                "index": {"type": "integer"},
                "url": {"type": "string"},
            },
            ["action"],
        )(session.tabs),
    ]


async def _snapshot(session: BrowserSession) -> ToolResult:
    return ToolResult(await session.page_text())


__all__ = [
    "BrowserConfig",
    "BrowserSession",
    "BrowserUnavailable",
    "browser_tools",
    "host_allowed",
]

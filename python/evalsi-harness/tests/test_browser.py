"""The browser toolset (H4): a real Chromium against a local static site.

The host-matching and config tests always run. The rest need Playwright and a Chromium
(PLAYWRIGHT_BROWSERS_PATH or EVALSI_BROWSER_EXECUTABLE) and skip when either is missing.
"""

from __future__ import annotations

import asyncio
import functools
import json
import threading
from collections.abc import Callable, Iterator
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from evalsi.types import Content
from evalsi_harness.browser import (
    BrowserConfig,
    BrowserSession,
    BrowserUnavailable,
    browser_tools,
    host_allowed,
)
from evalsi_harness.testing import LocalSandboxClient, Script, ScriptedModelServer
from tests.harness_support import record, run, spec_of

INDEX = """<!doctype html><html><head><title>Shop</title></head><body>
<h1>Welcome</h1>
<nav><a href="/about.html">About us</a> <a href="/new.html" target="_blank">Open new</a>
<a id="off" href="http://localhost:{port}/about.html">Elsewhere</a></nav>
<form onsubmit="event.preventDefault(); document.getElementById('out').textContent =
  'Hello ' + document.getElementById('who').value + ' / ' + document.getElementById('size').value;">
  <label for="who">Your name</label><input id="who" type="text">
  <label for="size">Size</label>
  <select id="size"><option value="s">Small</option><option value="l">Large</option></select>
  <button type="submit">Greet</button>
</form>
<p id="out"></p>
<div style="display:none">hidden text</div>
</body></html>"""
ABOUT = "<!doctype html><title>About</title><h1>About</h1><p>We sell things.</p>"
NEW = "<!doctype html><title>New tab</title><h1>In a new tab</h1>"


@pytest.fixture(scope="module")
def site(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
    root = tmp_path_factory.mktemp("site")
    server: ThreadingHTTPServer | None = None
    handler = functools.partial(SimpleHTTPRequestHandler, directory=str(root))
    handler.func.log_message = lambda *a, **k: None  # type: ignore[attr-defined]
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    port = server.server_address[1]
    (root / "index.html").write_text(INDEX.replace("{port}", str(port)))
    (root / "about.html").write_text(ABOUT)
    (root / "new.html").write_text(NEW)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{port}"
    server.shutdown()
    server.server_close()


def tools_for(url: str, **extra: Any) -> tuple[BrowserSession, dict[str, Any]]:
    session = BrowserSession(BrowserConfig.from_metadata({"browser": {"start_url": url, **extra}}))
    return session, {t.spec.name: t for t in browser_tools(session)}


def with_browser(url: str, body: Callable[[dict[str, Any]], Any], **extra: Any) -> None:
    async def go() -> None:
        session, tools = tools_for(url, **extra)
        try:
            await body(tools)
        finally:
            await session.aclose()

    asyncio.run(go())


@pytest.fixture(scope="module")
def chromium(site: str) -> None:
    pytest.importorskip("playwright")

    async def probe() -> str:
        session, tools = tools_for(site + "/index.html")
        try:
            result = await tools["browser_snapshot"].call({})
            return "" if not result.is_error else result.content
        finally:
            await session.aclose()

    problem = asyncio.run(probe())
    if problem:
        pytest.skip(problem)


def test_host_matching_follows_the_sandbox_allowlist_rules() -> None:
    assert host_allowed("https://example.com/x", ["example.com"])
    assert host_allowed("http://example.com", ["example.com"])
    assert not host_allowed("http://example.com:8080/", ["example.com"])  # bare: 80 and 443 only
    assert host_allowed("http://127.0.0.1:8080/", ["127.0.0.1:8080"])
    assert not host_allowed("http://127.0.0.1:8081/", ["127.0.0.1:8080"])
    assert host_allowed("https://a.example.com", ["*.example.com"])
    assert not host_allowed("https://example.com.evil.org", ["*.example.com", "example.com"])
    for url in (
        "file:///etc/passwd",
        "javascript:alert(1)",
        "data:text/html,x",
        "ftp://example.com",
    ):
        assert not host_allowed(url, ["example.com", "*"])


def test_config_defaults_to_the_start_urls_host_and_checks_the_viewport() -> None:
    cfg = BrowserConfig.from_metadata({"browser": {"start_url": "http://shop.test:7770/x"}})
    assert cfg.allow_hosts == ["shop.test:7770"]
    explicit = {"browser": {"start_url": "http://a.test/", "allow_hosts": ["b.test"]}}
    assert BrowserConfig.from_metadata(explicit).allow_hosts == ["b.test"]
    assert BrowserConfig.from_metadata({}).start_url == ""
    with pytest.raises(ValueError, match="viewport"):
        BrowserConfig.from_metadata({"browser": {"viewport": [10, 10]}})


def test_a_missing_playwright_is_a_clear_tool_error(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real = builtins.__import__

    def refuse(name: str, *a: Any, **k: Any) -> Any:
        if name.startswith("playwright"):
            raise ImportError(name)
        return real(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", refuse)
    with pytest.raises(BrowserUnavailable, match=r"evalsi-harness\[browser\]"):
        asyncio.run(BrowserSession(BrowserConfig()).start())


def test_navigate_snapshot_click_type_select_back_and_tabs(site: str, chromium: None) -> None:
    async def body(tools: dict[str, Any]) -> None:
        snap = (await tools["browser_snapshot"].call({})).content
        assert "Page Title: Shop" in snap
        assert '- heading "Welcome" [level=1]' in snap
        assert '- link "About us" [href=/about.html] [ref=e1]' in snap
        assert "hidden text" not in snap
        assert '- textbox "Your name" [ref=' in snap
        assert '- option "Large"' in snap

        def ref(line: str, text: str) -> str:
            row = next(x for x in text.splitlines() if line in x)
            return row.split("[ref=")[1].rstrip("]")

        who, size, go = ref("textbox", snap), ref("combobox", snap), ref("button", snap)
        typed = await tools["browser_type"].call({"ref": who, "text": "Ada"})
        assert 'value="Ada"' in typed.content
        await tools["browser_select"].call({"ref": size, "value": "Large"})
        after = await tools["browser_click"].call({"ref": go})
        assert '- text "Hello Ada / l"' in after.content

        link = ref('link "About us"', after.content)
        about = await tools["browser_click"].call({"ref": link})
        assert "Page Title: About" in about.content
        assert "We sell things." in about.content
        back = await tools["browser_back"].call({})
        assert "Page Title: Shop" in back.content
        # The page after back is a new document: the old refs are gone, and say so.
        stale = await tools["browser_click"].call({"ref": "e999"})
        assert stale.is_error
        assert "take a new snapshot" in stale.content
        bad = await tools["browser_click"].call({"ref": "//a"})
        assert bad.is_error

        listing = await tools["browser_tabs"].call({"action": "list"})
        assert listing.content.count("\n") == 1
        opened = await tools["browser_click"].call({"ref": ref("Open new", back.content)})
        assert "In a new tab" in (await tools["browser_snapshot"].call({})).content, opened.content
        tabs = (await tools["browser_tabs"].call({"action": "list"})).content
        assert "* 1:" in tabs
        selected = await tools["browser_tabs"].call({"action": "select", "index": 0})
        assert "Page Title: Shop" in selected.content
        closed = await tools["browser_tabs"].call({"action": "close", "index": 1})
        assert closed.content.count("\n") == 1

    with_browser(site + "/index.html", body)


def test_a_screenshot_is_a_png_image_result(site: str, chromium: None) -> None:
    async def body(tools: dict[str, Any]) -> None:
        shot = await tools["browser_screenshot"].call({})
        (image,) = shot.images
        assert image.media_type == "image/png"
        assert image.data.startswith(b"\x89PNG\r\n\x1a\n")
        assert len(image.data) > 1000

    with_browser(site + "/index.html", body)


def test_pages_outside_the_allowlist_never_load(site: str, chromium: None) -> None:
    async def body(tools: dict[str, Any]) -> None:
        refused = await tools["browser_navigate"].call({"url": "http://example.com/"})
        assert refused.is_error
        assert "allowlist" in refused.content
        file_url = await tools["browser_navigate"].call({"url": "file:///etc/hostname"})
        assert file_url.is_error
        # A link to another host (localhost, not 127.0.0.1) is aborted by the browser itself.
        snap = (await tools["browser_snapshot"].call({})).content
        off = next(x for x in snap.splitlines() if "Elsewhere" in x).split("[ref=")[1].rstrip("]")
        blocked = await tools["browser_click"].call({"ref": off})
        assert blocked.is_error or "Page Title: About" not in blocked.content

    with_browser(site + "/index.html", body)


def test_a_run_gives_the_model_the_screenshot_and_records_a_media_step(
    site: str, chromium: None, sandboxes: LocalSandboxClient, model: Callable[[Script], Any]
) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        assert "browser_screenshot" in tools
        tool_msgs = [m for m in messages if m["role"] == "tool"]
        if not tool_msgs:
            return {"tool_calls": [("browser_screenshot", {})]}
        return {"text": "I saw the shop."}

    server: ScriptedModelServer = model(script)
    spec = spec_of(
        {
            "target": {
                "connector": "openai-compatible",
                "model": "m",
                "base_url": server.base_url,
            },
            "harness": {"builtin": {"tools": {"sandbox": False, "builtin": ["browser"]}}},
        }
    )
    rec = record("What is on the shop page?", metadata={"browser": {"start_url": site + "/"}})
    outcome = run(spec, rec, sandboxes)
    assert outcome.error == ""
    out = outcome.record
    assert out is not None
    assert out.output is not None
    assert out.output.as_text() == "I saw the shop."
    # The model's second request carries the image beside the tool's text.
    second = server.requests[1]["messages"]
    shown = [m for m in second if m["role"] == "user" and isinstance(m["content"], list)]
    (parts,) = [m["content"] for m in shown]
    assert parts[1]["type"] == "image_url"
    assert parts[1]["image_url"]["url"].startswith("data:image/png;base64,iVBOR")
    assert out.trajectory is not None
    steps = out.trajectory.steps
    (tool,) = [s for s in steps if s.type == "tool"]
    (image,) = [s for s in steps if s.name == "image"]
    assert image.parent_span_id == tool.span_id
    assert isinstance(image.output, Content)
    assert image.output.media is not None
    assert image.output.media.media_type == "image/png"
    # The saved record round-trips the media content.
    assert Content.from_value(json.loads(json.dumps(image.output.to_dict()))) == image.output
    # The system prompt mentions the start page.
    system = server.requests[0]["messages"][0]["content"]
    assert f"opens at {site}/" in system

"""Following an executor's event log into the ingest while it runs (H7)."""

from __future__ import annotations

import asyncio
import json
import threading
import time
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main
from evalsi_harness.tail import Follower, FollowError, follow

FIXTURES = Path(__file__).parent / "fixtures"


class Collector:
    def __init__(self) -> None:
        self.batches: list[list[dict[str, Any]]] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                spans = body["resourceSpans"][0]["scopeSpans"][0]["spans"]
                outer.batches.append(spans)
                self.send_response(200)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"{}")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    @property
    def spans(self) -> list[dict[str, Any]]:
        return [s for batch in self.batches for s in batch]


@pytest.fixture
def otlp(monkeypatch: pytest.MonkeyPatch) -> Iterator[Collector]:
    collector = Collector()
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.url)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", raising=False)
    yield collector
    collector.server.shutdown()
    collector.server.server_close()


def names(spans: list[dict[str, Any]]) -> list[str]:
    return [s["name"] for s in spans]


def test_steps_go_out_as_they_become_final_and_the_root_closes_the_trace(otlp: Collector) -> None:
    lines = (FIXTURES / "opencode-1.19.3-fix-calc.jsonl").read_text().splitlines(keepends=True)
    f = Follower("opencode-json", "calc", trial=2, run_id="r1", agent="opencode")

    async def go() -> list[int]:
        sent = []
        # The first model call and its tool call: still the latest call, so held back.
        f.feed("".join(lines[:3]))
        sent.append(await f.flush())
        # The second call starts: the first call and its tool are now final.
        f.feed("".join(lines[3:4]))
        sent.append(await f.flush())
        # A half-written line waits for its newline.
        f.feed(lines[4][:40])
        sent.append(await f.flush())
        f.feed(lines[4][40:] + "".join(lines[5:]))
        sent.append(await f.flush(ended=True))
        return sent

    assert asyncio.run(go()) == [0, 2, 0, 3]
    spans = otlp.spans
    assert names(otlp.batches[0]) == ["chat opencode", "execute_tool bash"]
    assert len({s["traceId"] for s in spans}) == 1
    root = [s for s in spans if s["name"].startswith("invoke_agent")]
    assert len(root) == 1  # sent once, last
    assert spans[-1] is root[0] or spans[-1]["spanId"] == root[0]["spanId"]
    assert all(s["parentSpanId"] == root[0]["spanId"] for s in spans if s is not root[0])
    assert names(spans).count("chat opencode") == 3
    assert names(spans).count("execute_tool edit") == 1
    tags = {a["key"]: a["value"] for a in root[0]["attributes"]}
    assert tags["evalsi.task_id"] == {"stringValue": "calc"}
    assert tags["evalsi.trial"] == {"intValue": "2"}
    assert tags["evalsi.run_id"] == {"stringValue": "r1"}
    # The span's start times never go backwards across batches.
    starts = [int(s["startTimeUnixNano"]) for s in spans if s is not root[0]]
    assert starts == sorted(starts)


def test_a_failed_send_is_retried_by_the_next_flush(
    otlp: Collector, monkeypatch: pytest.MonkeyPatch
) -> None:
    f = Follower("opencode-json", "calc")
    f.feed((FIXTURES / "opencode-1.19.3-fix-calc.jsonl").read_text())

    async def go() -> None:
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
        assert await f.flush() == 0  # nothing reachable: not counted as sent
        assert f.sent == 0
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", otlp.url)
        assert await f.flush() == 4
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
        with pytest.raises(FollowError, match="no OTLP endpoint"):
            await f.flush(ended=True)

    asyncio.run(go())


def test_follow_reads_a_growing_file_until_it_goes_quiet(otlp: Collector, tmp_path: Path) -> None:
    log = tmp_path / "events.jsonl"
    lines = (FIXTURES / "opencode-1.19.3-fix-calc.jsonl").read_text().splitlines(keepends=True)

    def writer() -> None:
        time.sleep(0.15)  # the file does not exist when the follower starts
        with log.open("a") as handle:
            for line in lines:
                handle.write(line)
                handle.flush()
                time.sleep(0.05)

    thread = threading.Thread(target=writer)
    thread.start()
    flushes: list[int] = []
    done = asyncio.run(
        follow(
            log,
            Follower("opencode-json", "calc"),
            poll_s=0.05,
            idle_s=0.6,
            on_flush=flushes.append,
        )
    )
    thread.join()
    assert done.sent == 5
    assert sum(flushes) == 5
    assert len(otlp.batches) >= 3  # live: more than one batch, not one dump at the end
    assert names(otlp.spans)[-1].startswith("invoke_agent")


def test_the_wrong_format_and_a_missing_log_are_errors(otlp: Collector, tmp_path: Path) -> None:
    for bad in ("", "dsh-json", "nope"):
        with pytest.raises(FollowError, match="--format"):
            Follower(bad, "t")
    log = tmp_path / "plain.txt"
    log.write_text("the agent just printed this\n")
    with pytest.raises(FollowError, match="no codex-json events"):
        asyncio.run(follow(log, Follower("codex-json", "t"), idle_s=0))
    # A log that never appears ends as an error after the idle time, not a hang.
    with pytest.raises(FollowError, match="no codex-json events"):
        asyncio.run(
            follow(tmp_path / "never", Follower("codex-json", "t"), poll_s=0.01, idle_s=0.03)
        )


def test_the_cli_follows_a_codex_log(otlp: Collector, capsys: pytest.CaptureFixture[str]) -> None:
    argv = [
        "harness2",
        "follow",
        str(FIXTURES / "codex-0.162.1-fix-calc.jsonl"),
        "--format",
        "codex-json",
        "--task-id",
        "calc",
        "--idle",
        "0",
    ]
    assert main(argv) == 0
    trace = capsys.readouterr().out.strip()
    assert len(trace) == 32
    assert {s["traceId"] for s in otlp.spans} == {trace}
    assert any(n.startswith("execute_tool") for n in names(otlp.spans))


def test_the_cli_needs_an_endpoint(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", raising=False)
    argv = ["harness2", "follow", "x.jsonl", "--format", "codex-json", "--task-id", "t"]
    assert main(argv) == 2
    assert "OTEL_EXPORTER_OTLP_ENDPOINT" in capsys.readouterr().err

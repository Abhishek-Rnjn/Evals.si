"""evalsi mcp: the protocol, the tools, and a real stdio session."""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from typing import Any

import pytest

from evalsi.compare import compare_results
from evalsi.mcp_server import EvalsiMCP, Settings, serve


def session(server: EvalsiMCP, messages: list[dict[str, Any] | str]) -> list[dict[str, Any]]:
    """Feeds messages to the stdio loop and returns everything it wrote."""

    class Out:
        def __init__(self) -> None:
            self.data = b""

        def write(self, b: bytes) -> None:
            self.data += b

    async def go() -> list[dict[str, Any]]:
        reader = asyncio.StreamReader()
        for m in messages:
            reader.feed_data((m if isinstance(m, str) else json.dumps(m)).encode() + b"\n")
        reader.feed_eof()
        out = Out()
        await serve(server, reader, out)
        return [json.loads(line) for line in out.data.splitlines()]

    return asyncio.run(go())


def request(i: int, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    msg: dict[str, Any] = {"jsonrpc": "2.0", "id": i, "method": method}
    if params is not None:
        msg["params"] = params
    return msg


def by_id(responses: list[dict[str, Any]]) -> dict[Any, dict[str, Any]]:
    return {r["id"]: r for r in responses if "id" in r}


def call(server: EvalsiMCP, name: str, arguments: dict[str, Any], **meta: Any) -> dict[str, Any]:
    params: dict[str, Any] = {"name": name, "arguments": arguments}
    if meta:
        params["_meta"] = meta
    out = session(server, [request(1, "tools/call", params)])
    result: dict[str, Any] = by_id(out)[1]["result"]
    return result


@pytest.fixture
def server(tmp_path: Path) -> EvalsiMCP:
    return EvalsiMCP(Settings(root=tmp_path, cache=False))


def test_handshake_and_protocol_errors(server: EvalsiMCP) -> None:
    out = by_id(
        session(
            server,
            [
                request(1, "initialize", {"protocolVersion": "2025-03-26", "capabilities": {}}),
                {"jsonrpc": "2.0", "method": "notifications/initialized"},
                request(2, "tools/list"),
                request(3, "ping"),
                request(4, "resources/list"),
                request(5, "tools/call", {"name": "nope", "arguments": {}}),
                "{not json",
            ],
        )
    )
    init = out[1]["result"]
    assert init["protocolVersion"] == "2025-03-26"
    assert init["serverInfo"]["name"] == "evalsi"
    assert "tools" in init["capabilities"]
    names = {t["name"] for t in out[2]["result"]["tools"]}
    assert names == {"list_evaluators", "evaluate", "run", "compare"}
    assert out[3]["result"] == {}
    assert out[4]["error"]["code"] == -32601
    assert out[5]["error"]["code"] == -32602
    assert out[None]["error"]["code"] == -32700
    # An unknown protocol version gets the newest one this server speaks.
    newest = by_id(session(server, [request(1, "initialize", {"protocolVersion": "1999-01-01"})]))
    assert newest[1]["result"]["protocolVersion"] == "2025-06-18"


def test_list_evaluators(server: EvalsiMCP) -> None:
    result = call(server, "list_evaluators", {"pack": "core"})
    assert not result["isError"]
    names = {e["name"] for p in result["structuredContent"]["packs"] for e in p["evaluators"]}
    assert "exact-match" in names
    assert call(server, "list_evaluators", {"pack": "nope"})["isError"]


def test_evaluate_records(server: EvalsiMCP) -> None:
    records = [
        {"id": "a", "output": "Paris", "reference": "Paris"},
        {"id": "b", "output": "Lyon", "reference": "Paris"},
    ]
    result = call(server, "evaluate", {"records": records, "evaluators": ["exact-match"]})
    assert not result["isError"], result
    [summary] = result["structuredContent"]["summaries"]
    assert summary["metric"] == "exact-match"
    assert summary["mean"] == 0.5
    text = result["content"][0]["text"]
    assert "a exact-match: True" in text
    assert "b exact-match: False" in text
    bad = call(server, "evaluate", {"records": records, "evaluators": ["no-such-evaluator"]})
    assert bad["isError"]


def write_run(root: Path, outputs: dict[str, str]) -> None:
    records = [
        {"id": rid, "output": {"text": out}, "reference": {"text": "ok"}}
        for rid, out in outputs.items()
    ]
    spec = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "metadata": {"name": "suite"},
        "spec": {
            "dataset": {"inline": {"records": records}},
            "evaluators": [{"ref": "exact-match"}],
            "gates": [{"metric": "exact-match", "min": 0.5}],
        },
    }
    (root / "suite.yaml").write_text(json.dumps(spec))


def test_run_saves_and_compares_with_the_previous_run(server: EvalsiMCP, tmp_path: Path) -> None:
    ids = [f"r{i}" for i in range(12)]
    write_run(tmp_path, dict.fromkeys(ids, "ok"))
    first = call(server, "run", {"file": "suite.yaml"})
    assert not first["isError"], first
    s1 = first["structuredContent"]
    assert s1["passed"]
    assert (tmp_path / s1["results_file"]).is_file()
    assert "no previous results" in first["content"][0]["text"]

    # The agent's change breaks most records: a significant regression, and a failed gate.
    write_run(tmp_path, {rid: "ok" if i < 3 else "bad" for i, rid in enumerate(ids)})
    second = call(server, "run", {"file": "suite.yaml"}, progressToken="p1")
    s2 = second["structuredContent"]
    assert not s2["passed"]
    assert s2["baseline"] == s1["results_file"]
    [c] = s2["comparisons"]
    assert c["metric"] == "exact-match"
    assert c["paired_n"] == 12
    assert c["regressed"]
    assert "REGRESSED" in second["content"][0]["text"]
    assert "FAILED" in second["content"][0]["text"]

    # compare works on the saved files directly.
    cmp = call(server, "compare", {"baseline": s1["results_file"], "candidate": s2["results_file"]})
    assert cmp["structuredContent"]["comparisons"][0]["regressed"]
    none = call(server, "run", {"file": "suite.yaml", "baseline": "none"})
    assert "comparisons" not in none["structuredContent"]


def test_progress_notifications(server: EvalsiMCP, tmp_path: Path) -> None:
    write_run(tmp_path, {"a": "ok", "b": "ok"})
    out = session(
        server,
        [
            request(
                1,
                "tools/call",
                {"name": "run", "arguments": {"file": "suite.yaml"}, "_meta": {"progressToken": 7}},
            )
        ],
    )
    progress = [m for m in out if m.get("method") == "notifications/progress"]
    assert progress
    assert all(m["params"]["progressToken"] == 7 for m in progress)
    assert by_id(out)[1]["result"]["isError"] is False


def test_files_stay_inside_the_workspace(server: EvalsiMCP, tmp_path: Path) -> None:
    outside = tmp_path.parent / "outside.yaml"
    outside.write_text("x: 1")
    for path in ("../outside.yaml", str(outside), "/etc/passwd"):
        result = call(server, "run", {"file": path})
        assert result["isError"]
        assert "outside the workspace" in result["content"][0]["text"]
    assert call(server, "compare", {"baseline": "/etc/passwd", "candidate": "x.json"})["isError"]
    assert call(server, "run", {"file": "missing.yaml"})["isError"]


def test_compare_results_pairs_by_record_and_respects_direction() -> None:
    def results(values: list[float], higher: bool = True) -> dict[str, Any]:
        mean = sum(values) / len(values)
        return {
            "summaries": [{"metric": "latency", "mean": mean, "higher_is_better": higher}],
            "results": [
                {
                    "record_id": f"r{i}",
                    "evaluator": "latency",
                    "outcome": "scored",
                    "scores": [{"name": "latency", "number": v}],
                }
                for i, v in enumerate(values)
            ],
        }

    base = results([1.0, 2.0, 3.0, 4.0, 5.0], higher=False)
    slower = results([2.0, 3.1, 3.9, 5.2, 6.0], higher=False)
    [c] = compare_results(base, slower)
    assert c.significant
    assert c.regressed  # lower is better, and it went up
    [same] = compare_results(base, base)
    assert not same.significant


def test_a_real_stdio_session(tmp_path: Path) -> None:
    """The harness's MCP client drives `evalsi mcp` as a subprocess, the way a
    coding agent does: run, change the code under test, run again, and read
    the regression in the answer."""
    pytest.importorskip("evalsi_harness")
    from evalsi_harness.mcp import MCPClient

    good = {f"r{i}": "ok" for i in range(12)}
    write_run(tmp_path, good)

    async def go() -> tuple[list[str], list[tuple[str, bool]]]:
        client = await MCPClient.connect(
            command=[sys.executable, "-m", "evalsi", "mcp", "--root", str(tmp_path)]
        )
        try:
            names = [t.name for t in await client.list_tools()]
            first = await client.call_tool("run", {"file": "suite.yaml"})
            # The agent's change breaks most answers.
            write_run(tmp_path, {k: ("ok" if i < 2 else "bad") for i, k in enumerate(good)})
            second = await client.call_tool("run", {"file": "suite.yaml"})
            return names, [(first.content, first.is_error), (second.content, second.is_error)]
        finally:
            await client.aclose()

    names, results = asyncio.run(go())
    assert "run" in names
    (first, first_err), (second, second_err) = results
    assert not first_err, first
    assert not second_err, second
    assert "gate exact-match" in first
    assert "regress" in second.lower(), second
    saved = list((tmp_path / ".evalsi" / "results").glob("suite-*.json"))
    assert len(saved) == 2

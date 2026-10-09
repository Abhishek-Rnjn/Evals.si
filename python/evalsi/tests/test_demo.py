"""The reference demos under examples/demo: specs load, generated files are current,
the copies of scripts inside specs match their sources, and the mock model plays
its script."""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import textwrap
import threading
from collections.abc import Iterator
from http.server import ThreadingHTTPServer
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi.runspec import load_spec

DEMO = Path(__file__).resolve().parents[3] / "examples" / "demo"
SPECS = sorted(p for agent in ("deepagents", "dsh") for p in DEMO.glob(f"{agent}-*.yaml"))


def test_there_is_a_spec_per_agent_and_suite() -> None:
    names = {p.stem for p in SPECS}
    assert names == {
        f"{agent}-{suite}"
        for agent in ("deepagents", "dsh")
        for suite in ("small-repo-fixes", "deep-research", "terminal-bench")
    }


@pytest.mark.parametrize("spec", SPECS, ids=lambda p: p.stem)
def test_specs_load(spec: Path) -> None:
    load_spec(spec)


@pytest.mark.parametrize("builder", ["build_small_repo_fixes.py", "build_deep_research.py"])
def test_generated_files_are_current(builder: str) -> None:
    result = subprocess.run(
        [sys.executable, str(DEMO / "fixtures" / builder), "--check"],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr


def test_datasets_named_by_specs_exist() -> None:
    for spec in SPECS:
        text = spec.read_text()
        for raw in text.splitlines():
            line = raw.strip()
            if line.startswith("path: "):
                assert (DEMO / line.removeprefix("path: ")).exists(), f"{spec.name}: {line}"


def _inlined(spec: str, marker: str) -> str:
    """The script a spec writes with `cat > ... <<'MARKER'`, with the spec's indentation removed."""
    lines = (DEMO / spec).read_text().splitlines()
    start = next(i for i, line in enumerate(lines) if f"<<'{marker}'" in line) + 1
    end = next(i for i in range(start, len(lines)) if lines[i].strip() == marker)
    return textwrap.dedent("\n".join(lines[start:end])) + "\n"


def test_scripts_copied_into_specs_match_their_sources() -> None:
    assert (
        _inlined("dsh-terminal-bench.yaml", "CONFIGURE")
        == (DEMO / "dsh" / "configure.sh").read_text()
    )
    assert (
        _inlined("deepagents-terminal-bench.yaml", "WRAPPER")
        == (DEMO / "deepagents" / "dcode-demo.sh").read_text()
    )


# --- the mock model ---


@pytest.fixture(scope="module")
def mock_model() -> Iterator[str]:
    spec = importlib.util.spec_from_file_location(
        "mock_model_server", DEMO / "mock-model" / "server.py"
    )
    assert spec is not None
    assert spec.loader is not None
    server = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(server)
    solutions = json.loads((DEMO / "mock-model" / "solutions.json").read_text())
    server.Handler.script = server.Script(solutions)
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), server.Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{httpd.server_port}/v1"
    httpd.shutdown()


def chat(
    base: str,
    messages: list[dict[str, Any]],
    tools: list[dict[str, Any]] | None = None,
    **extra: Any,
) -> Any:
    body = {"model": "mock", "messages": messages, **extra}
    if tools:
        body["tools"] = tools
    return httpx.post(f"{base}/chat/completions", json=body, timeout=10)


BASH = {
    "type": "function",
    "function": {
        "name": "bash",
        "parameters": {
            "type": "object",
            "properties": {"command": {"type": "string"}, "description": {"type": "string"}},
            "required": ["command", "description"],
        },
    },
}


def test_the_mock_applies_a_known_fix_with_whatever_shell_tool_there_is(mock_model: str) -> None:
    task = "mathx.range_sum(1, 4) should be 10 (1+2+3+4, both ends included) but it returns 6."
    first = chat(mock_model, [{"role": "user", "content": task}], [BASH]).json()["choices"][0]
    assert first["finish_reason"] == "tool_calls"
    call = first["message"]["tool_calls"][0]["function"]
    args = json.loads(call["arguments"])
    assert call["name"] == "bash"
    assert "range(lo, hi + 1)" in args["command"]
    assert args["description"]  # required arguments are filled in

    messages = [
        {"role": "user", "content": task},
        first["message"],
        {"role": "tool", "tool_call_id": first["message"]["tool_calls"][0]["id"], "content": "ok"},
    ]
    last = chat(mock_model, messages, [BASH]).json()["choices"][0]
    assert last["finish_reason"] == "stop"
    assert last["message"]["content"]


def test_the_mock_streams(mock_model: str) -> None:
    task = "mathx.range_sum(1, 4) should be 10 (1+2+3+4, both ends included) but it returns 6."
    response = chat(
        mock_model,
        [{"role": "user", "content": task}],
        [BASH],
        stream=True,
        stream_options={"include_usage": True},
    )
    events = [
        line.removeprefix("data: ")
        for line in response.text.splitlines()
        if line.startswith("data: ")
    ]
    assert events[-1] == "[DONE]"
    chunks = [json.loads(e) for e in events[:-1]]
    assert any(c["choices"] and c["choices"][0]["delta"].get("tool_calls") for c in chunks)
    assert "usage" in chunks[-1]


def test_the_mock_does_not_pretend_to_know_a_task(mock_model: str) -> None:
    reply = chat(mock_model, [{"role": "user", "content": "Write a sonnet."}], [BASH]).json()
    assert reply["choices"][0]["finish_reason"] == "stop"
    assert "could not" in reply["choices"][0]["message"]["content"]

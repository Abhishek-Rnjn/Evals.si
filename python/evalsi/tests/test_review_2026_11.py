"""Regression tests for the November 2026 review: bugs found by using the
product as a new user would, and the features that came out of it."""

from __future__ import annotations

import asyncio
import io
import json
import os
import subprocess
import sys
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from evalsi import evaluate
from evalsi.cli import main
from evalsi.compare import compare_results
from evalsi.datasets import DatasetError, load_records
from evalsi.runspec import parse_spec
from evalsi.testing import assert_eval

ROOT = Path(__file__).resolve().parents[3]


def scores(evaluator: Any, rows: list[dict[str, Any]]) -> dict[str, Any]:
    """{record id: the first score's value, or the outcome when it was not scored}."""
    out: dict[str, Any] = {}
    name, params = evaluator if isinstance(evaluator, tuple) else (evaluator, {})
    for r in evaluate(rows, [name], params={name: params}, cache=False).results:
        if r.outcome.value != "scored":
            out[r.record_id] = r.outcome.value
        else:
            first = r.scores[0]
            out[r.record_id] = first.passed if first.passed is not None else first.number
    return out


def row(output: Any, reference: Any = None, **extra: Any) -> dict[str, Any]:
    return {"input": "q", "output": output, "reference": reference, **extra}


def tools(*calls: tuple[str, dict[str, Any], bool]) -> dict[str, Any]:
    return {
        "steps": [
            {
                "type": "tool",
                "name": name,
                "input": {"json": args},
                **({"error": "boom"} if failed else {"output": {"text": "ok"}}),
            }
            for name, args, failed in calls
        ]
    }


def tool(name: str) -> tuple[str, dict[str, Any], bool]:
    return (name, {}, False)


# --- datasets ---------------------------------------------------------------------


def test_jsonl_and_json_with_a_byte_order_mark_load(tmp_path: Path) -> None:
    line = '{"id":"a","input":"q","output":"x","reference":"x"}'
    jsonl = tmp_path / "bom.jsonl"
    jsonl.write_bytes(b"\xef\xbb\xbf" + line.encode() + b"\r\n")
    assert [r.id for r in load_records(str(jsonl))] == ["a"]
    as_json = tmp_path / "bom.json"
    as_json.write_bytes(b"\xef\xbb\xbf[" + line.encode() + b"]")
    assert [r.id for r in load_records(str(as_json))] == ["a"]


def test_an_empty_dataset_is_an_error_not_a_success(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    empty = tmp_path / "empty.jsonl"
    empty.write_text("")
    assert main(["eval", "--data", str(empty), "--evaluators", "exact-match", "--quiet"]) == 1
    assert "no records" in capsys.readouterr().err
    spec = tmp_path / "run.yaml"
    spec.write_text("spec:\n  dataset: {path: empty.jsonl}\n  evaluators: [{ref: exact-match}]\n")
    assert main(["run", "-f", str(spec), "--quiet"]) == 1
    assert "no records" in capsys.readouterr().err


def test_a_mistyped_cluster_key_is_refused(tmp_path: Path) -> None:
    rows = [{"id": str(i), **row("x", "x"), "metadata": {"topic": "a"}} for i in range(3)]
    with pytest.raises(DatasetError, match="no record has that metadata key"):
        evaluate(rows, ["exact-match"], cluster_by="topik", cache=False)
    assert evaluate(rows, ["exact-match"], cluster_by="topic", cache=False).summaries


def test_inline_records_take_plain_strings() -> None:
    spec = parse_spec(
        {
            "spec": {
                "dataset": {
                    "inline": [
                        {"id": 1, "input": "q", "output": "30 days", "reference": "30 days"},
                        {"id": "b", "input": {"text": "q"}, "output": 7, "context": ["a", "b"]},
                    ]
                },
                "evaluators": [{"ref": "exact-match"}],
            }
        }
    ).spec
    first, second = spec.dataset.inline.records
    assert (first.id, first.output.text, first.reference.text) == ("1", "30 days", "30 days")
    assert (second.input.text, second.output.text) == ("q", "7")
    assert [c.text for c in second.context] == ["a", "b"]


# --- gates ------------------------------------------------------------------------


def gate_spec(tmp_path: Path, gates: str) -> Path:
    (tmp_path / "data.jsonl").write_text(
        '{"id":"a","input":"q","output":"x","reference":"x"}\n'
        + "".join(f'{{"id":"s{i}","input":"q","reference":"x"}}\n' for i in range(3))
    )
    spec = tmp_path / "run.yaml"
    spec.write_text(
        "spec:\n  dataset: {path: data.jsonl}\n  evaluators: [{ref: exact-match}]\n"
        f"  gates:\n{gates}"
    )
    return spec


def test_a_mean_gate_on_mostly_skipped_records_warns(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    spec = gate_spec(tmp_path, "    - {metric: exact-match, min: 0.9}\n")
    assert main(["run", "-f", str(spec)]) == 0
    err = capsys.readouterr().err
    assert "3 were skipped" in err
    assert "GATE_STAT_SKIPPED_RATE" in err


@pytest.mark.parametrize(
    "gate",
    [
        "{metric: exact-match, stat: GATE_STAT_SKIPPED_RATE, max: 0.1}",
        "{metric: exact-match, stat: GATE_STAT_COUNT, min: 3}",
    ],
)
def test_coverage_gates_fail_a_mostly_skipped_run(
    tmp_path: Path, gate: str, capsys: pytest.CaptureFixture[str]
) -> None:
    spec = gate_spec(tmp_path, f"    - {{metric: exact-match, min: 0.9}}\n    - {gate}\n")
    assert main(["run", "-f", str(spec)]) == 3
    assert "warning" not in capsys.readouterr().err


# --- evaluators -------------------------------------------------------------------


def test_json_valid_refuses_what_only_python_accepts() -> None:
    got = scores(
        "json-valid",
        [
            {"id": "nan", **row("NaN")},
            {"id": "inf", **row('{"a": -Infinity}')},
            {"id": "ok", **row('{"a": 1.5e3, "b": null}')},
            {"id": "deep", **row("[" * 100_000)},
        ],
    )
    assert got == {"nan": False, "inf": False, "ok": True, "deep": False}


def test_regression_does_not_score_only_the_parseable_outputs() -> None:
    rows = [{"id": "1", **row("1", "1")}, {"id": "2", **row("abc", "2")}]
    result = evaluate(rows, ["regression"], cache=False)
    assert [r.outcome.value for r in result.results] == ["error"]
    assert "1 of 2 records" in result.results[0].reason
    lenient = evaluate(
        rows, ["regression"], params={"regression": {"max_unparsed": 0.5}}, cache=False
    )
    assert lenient.results[0].outcome.value == "scored"


def test_citation_accuracy_reads_lists_and_ranges_but_not_indexes() -> None:
    context = ["a", "b", "c"]
    got = scores(
        "citation-accuracy",
        [
            {"id": "list", "input": "q", "output": "Yes [1, 2].", "context": context},
            {"id": "range", "input": "q", "output": "Yes [1-3].", "context": context},
            {"id": "en-dash", "input": "q", "output": "Yes [1\u20133].", "context": context},
            {"id": "adjacent", "input": "q", "output": "Yes [1][3].", "context": context},
            {"id": "missing", "input": "q", "output": "Yes [2, 4].", "context": context},
            {"id": "range-past", "input": "q", "output": "Yes [2-5].", "context": context},
            {"id": "index", "input": "q", "output": "items[7] and f(x)[9] [1]", "context": context},
            {"id": "none", "input": "q", "output": "array[1] only", "context": context},
        ],
    )
    assert got == {
        "list": True,
        "range": True,
        "en-dash": True,
        "adjacent": True,
        "missing": False,
        "range-past": False,
        "index": True,
        "none": False,
    }


def test_small_evaluator_fixes() -> None:
    assert scores("token-f1", [{"id": "a", **row("the", "The")}]) == {"a": 1.0}
    assert scores("token-f1", [{"id": "a", **row("the", "cat")}]) == {"a": 0.0}
    assert scores("numeric-match", [{"id": "a", **row("\u22125", "-5")}]) == {"a": True}
    assert scores("numeric-match", [{"id": "a", **row("10\u221212", "12")}]) == {"a": True}
    assert scores("contains", [{"id": "a", **row("hello", "")}]) == {"a": "skipped"}
    assert scores("contains", [{"id": "a", **row("hello", "ELL")}]) == {"a": True}
    key = "AIzaSyA-abcdefghijklmnopqrstuvwxyz01234"
    assert scores("secret-leak", [{"id": "a", **row(f"key {key}")}]) == {"a": False}
    assert scores("secret-leak", [{"id": "a", **row("AIza is a prefix")}]) == {"a": True}


def test_tool_call_accuracy_can_require_the_call_to_succeed() -> None:
    rows = [
        {
            "id": "a",
            **row("o", {"tool_calls": [{"name": "refund"}]}),
            "trajectory": tools(("refund", {}, True)),
        }
    ]
    assert scores("tool-call-accuracy", rows) == {"a": 1.0}
    assert scores(("tool-call-accuracy", {"require_success": True}), rows) == {"a": 0.0}


def test_loop_detection_finds_cycles() -> None:
    a, b, c, d = (tool("a"), tool("b"), tool("c"), tool("d"))
    rows = [
        {"id": "abab", **row("o"), "trajectory": tools(a, b, a, b, a, b)},
        {"id": "abcabc", **row("o"), "trajectory": tools(a, b, c, a, b, c, a, b, c)},
        {"id": "twice", **row("o"), "trajectory": tools(a, b, a, b)},
        {"id": "varied", **row("o"), "trajectory": tools(a, b, c, a, b, d, a, c, b)},
        {"id": "same", **row("o"), "trajectory": tools(a, a, a)},
    ]
    assert scores("loop-detection", rows) == {
        "abab": False,
        "abcabc": False,
        "twice": True,
        "varied": True,
        "same": False,
    }
    singles = scores(("loop-detection", {"max_cycle": 1}), rows)
    assert singles["abab"] is True
    assert singles["same"] is False


# --- comparison ---------------------------------------------------------------------


def results_file(tmp_path: Path, name: str, outputs: dict[str, str]) -> Path:
    rows = [{"id": k, "input": "q", "output": v, "reference": "x"} for k, v in outputs.items()]
    path = tmp_path / name
    evaluate(rows, ["exact-match"], cache=False).save(path)
    return path


def test_a_pass_fail_difference_interval_stays_in_range(tmp_path: Path) -> None:
    base = results_file(tmp_path, "base.json", {"a": "x", "b": "x", "c": "x"})
    cand = results_file(tmp_path, "cand.json", {"a": "x", "b": "y", "c": "x"})
    [c] = compare_results(json.loads(base.read_text()), json.loads(cand.read_text()))
    assert c.diff_low is not None
    assert c.diff_high is not None
    assert -1.0 <= c.diff_low <= c.diff_high <= 1.0


def test_compare_two_results_files_without_a_server(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    ids = [str(i) for i in range(12)]
    base = results_file(tmp_path, "base.json", dict.fromkeys(ids, "x"))
    cand = results_file(tmp_path, "cand.json", {i: "x" if int(i) < 3 else "y" for i in ids})
    assert main(["compare", str(base), str(cand)]) == 0
    out = capsys.readouterr().out
    assert "exact-match" in out
    assert "REGRESSED" in out
    assert main(["compare", str(base), str(cand), "--fail-on-regression"]) == 3
    assert main(["compare", str(base), str(base), "--fail-on-regression"]) == 0
    capsys.readouterr()
    assert main(["compare", str(base), str(cand), "--format", "json"]) == 0
    assert json.loads(capsys.readouterr().out)[0]["regressed"] is True
    assert main(["compare", str(base), str(tmp_path / "missing.json")]) == 1
    assert "missing.json" in capsys.readouterr().err


# --- CLI help -----------------------------------------------------------------------


def test_help_names_every_command(capsys: pytest.CaptureFixture[str]) -> None:
    from evalsi.cli import _START_HERE, _parser

    with pytest.raises(SystemExit) as stop:
        main(["--help"])
    assert stop.value.code == 0
    text = capsys.readouterr()
    shown = text.out + text.err
    assert "start here:" in shown
    assert "other" not in shown.split("more commands")[1]
    names: list[str] = next(
        list(a.choices) for a in _parser()._actions if isinstance(a.choices, dict)
    )
    for name in names:
        assert name in shown, name
    assert set(_START_HERE) <= set(names)
    with pytest.raises(SystemExit):
        main(["--help-all"])
    assert "{eval,run,compare" in capsys.readouterr().out


# --- MCP ----------------------------------------------------------------------------


def test_mcp_survives_malformed_messages() -> None:
    from evalsi.mcp_server import EvalsiMCP, Settings, serve

    async def talk(lines: list[dict[str, Any]]) -> list[dict[str, Any]]:
        reader = asyncio.StreamReader()
        for line in lines:
            reader.feed_data(json.dumps(line).encode() + b"\n")
        reader.feed_eof()
        out = io.BytesIO()
        await serve(EvalsiMCP(Settings(root=Path.cwd())), reader, out)
        return [json.loads(x) for x in out.getvalue().splitlines()]

    replies = asyncio.run(
        talk(
            [
                {"jsonrpc": "2.0", "id": [1], "method": "ping"},
                {"jsonrpc": "2.0", "id": {"a": 1}, "method": "ping"},
                {"jsonrpc": "2.0", "method": "notifications/cancelled", "params": [1]},
                {
                    "jsonrpc": "2.0",
                    "method": "notifications/cancelled",
                    "params": {"requestId": []},
                },
                {"jsonrpc": "2.0", "id": 2, "method": "ping"},
            ]
        )
    )
    assert [r.get("error", {}).get("code") for r in replies[:2]] == [-32600, -32600]
    assert {"jsonrpc": "2.0", "id": 2, "result": {}} in replies


# --- report -------------------------------------------------------------------------


def test_report_has_no_empty_class_attribute() -> None:
    from evalsi import report

    html = report.to_html(
        report.build(
            title="t",
            meta={},
            summaries=[],
            gates=[{"metric": "m", "min": 1}],
            results=[],
            records=[],
        )
    )
    assert "class=>" not in html
    assert "<span>unavailable</span>" in html


# --- assert_eval and the pytest plugin ------------------------------------------------


CASES = [
    {"id": "france", "input": "Capital of France?", "reference": "Paris"},
    {"id": "japan", "input": "Capital of Japan?", "reference": "Tokyo"},
]


def answer(question: str) -> str:
    return {"Capital of France?": "Paris", "Capital of Japan?": "Tokyo"}[question]


def test_assert_eval_passes_and_fails_with_the_evidence() -> None:
    result = assert_eval(CASES, ["exact-match"], {"exact-match": 1.0}, task=answer, cache=False)
    assert [r.output.as_text() for r in result.records if r.output] == ["Paris", "Tokyo"]

    async def wrong(question: str) -> str:
        return "Kyoto" if "Japan" in question else answer(question)

    with pytest.raises(AssertionError) as failure:
        assert_eval(CASES, ["exact-match"], {"exact-match": 1.0}, task=wrong, cache=False)
    message = str(failure.value)
    assert "exact-match mean min 1" in message
    assert "record 'japan'" in message


def test_assert_eval_does_not_pass_on_skipped_or_missing_records() -> None:
    rows = [{"id": "a", **row("x", "x")}, {"id": "b", "input": "q", "reference": "x"}]
    with pytest.raises(AssertionError, match="skipped_rate"):
        assert_eval(rows, ["exact-match"], {"exact-match": 1.0}, cache=False)
    assert_eval(rows, ["exact-match"], {"exact-match": 1.0}, max_skipped_rate=None, cache=False)
    with pytest.raises(AssertionError, match="no records"):
        assert_eval([], ["exact-match"], {"exact-match": 1.0})
    with pytest.raises(AssertionError, match="count"):
        assert_eval(
            rows[:1], ["exact-match"], {"exact-match": {"stat": "count", "min": 5}}, cache=False
        )
    with pytest.raises(ValueError, match="unknown stat"):
        assert_eval(rows, ["exact-match"], {"exact-match": {"stat": "median", "min": 1}})


def test_pytest_writes_results_when_asked(tmp_path: Path) -> None:
    (tmp_path / "test_app.py").write_text(
        "from evalsi.testing import assert_eval\n"
        "ROWS = [{'id': 'a', 'input': 'q', 'output': 'x', 'reference': 'x'}]\n"
        "def test_gate():\n"
        "    assert_eval(ROWS, ['exact-match'], {'exact-match': 1.0}, cache=False)\n"
    )
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", "--evalsi-results", "out"],
        cwd=tmp_path,
        capture_output=True,
        text=True,
        check=False,
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    [written] = (tmp_path / "out").iterdir()
    assert written.name == "test_app.py_test_gate.json"
    assert json.loads(written.read_text())["summaries"][0]["metric"] == "exact-match"


# --- the application starter: async callables and HTTP endpoints -------------------------


@pytest.fixture
def endpoint() -> Iterator[str]:
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            reply: Any
            if self.path == "/chat":
                text = answer(body["question"]) + self.headers.get("X-Suffix", "")
                reply = {"choices": [{"message": {"content": text}}]}
            else:
                reply = {"output": answer(body["input"]) if "Italy" not in body["input"] else ""}
            data = json.dumps(reply).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args: Any) -> None:
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    server.server_close()


def starter(tmp_path: Path, *args: str) -> Path:
    project = tmp_path / "starter"
    assert main(["init", str(project), "--template", "application", *args]) == 0
    return project


def run_starter(project: Path, *args: str, env: dict[str, str] | None = None) -> int:
    return subprocess.run(
        [sys.executable, "evaluate.py", "--output", "results.json", *args],
        cwd=project,
        env={**os.environ, **(env or {})},
        capture_output=True,
        check=False,
    ).returncode


def test_the_starter_runs_an_async_callable(tmp_path: Path) -> None:
    project = starter(tmp_path)
    (project / "application.py").write_text(
        "import asyncio\n"
        "from application_sync import answer as sync_answer\n"
        "async def answer(question: str) -> str:\n"
        "    await asyncio.sleep(0)\n"
        "    return sync_answer(question)\n"
        "async def regressed(question: str) -> str:\n"
        "    return 'Kyoto' if 'Japan' in question else sync_answer(question)\n"
    )
    (project / "application_sync.py").write_text(
        "def answer(q):\n"
        "    return {'Capital of France?': 'Paris', 'Capital of Japan?': 'Tokyo',"
        " 'Capital of Italy?': 'Rome'}[q]\n"
    )
    assert run_starter(project) == 0
    assert run_starter(project, "--callable", "application:regressed") == 3


def test_the_starter_evaluates_an_http_endpoint(tmp_path: Path, endpoint: str) -> None:
    project = starter(tmp_path)
    # The default fields; Italy's empty answer is an execution error, not a wrong answer.
    assert run_starter(project, "--url", f"{endpoint}/answer", "--limit", "2") == 0
    assert run_starter(project, "--url", f"{endpoint}/answer") == 1
    chat = ("--url", f"{endpoint}/chat", "--input-field", "question", "--limit", "2")
    path = ("--output-field", "choices.0.message.content")
    assert run_starter(project, *chat, *path) == 0
    assert run_starter(project, *chat, *path, env={"EVALSI_APP_HEADERS": '{"X-Suffix": "!"}'}) == 3
    assert run_starter(project, *chat, "--output-field", "choices.5.nope") == 1
    assert run_starter(project, "--url", "http://127.0.0.1:1/none", "--timeout", "2") == 1


def test_init_can_make_an_endpoint_the_default(tmp_path: Path, endpoint: str) -> None:
    project = starter(tmp_path, "--url", f"{endpoint}/answer")
    assert run_starter(project, "--limit", "2") == 0
    assert main(["init", str(tmp_path / "bad"), "--template", "application", "--url", "ftp://x"])
    assert main(
        ["init", str(tmp_path / "leak"), "--template", "application", "--url", "http://u:p@h/"]
    )
    assert main(["init", str(tmp_path / "wrong"), "--url", "http://h/"]) != 0

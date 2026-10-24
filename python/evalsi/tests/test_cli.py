from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi.cli import main

EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "quickstart" / "qa.jsonl"


def test_eval_prints_a_summary_table(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["eval", "--data", str(EXAMPLE), "--evaluators", "exact-match,numeric-match"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("6 records · 2 evaluators")
    assert "exact-match" in out
    assert "numeric-match" in out


def test_eval_json_output_and_file(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    out_file = tmp_path / "out.json"
    code = main(
        [
            "eval",
            "--data",
            str(EXAMPLE),
            "--evaluators",
            "length",
            "--param",
            "length.unit=chars",
            "--format",
            "json",
            "--output",
            str(out_file),
            "--quiet",
        ]
    )
    assert code == 0
    printed = json.loads(capsys.readouterr().out)
    assert printed["manifest"]["evaluators"][0]["params"] == {"unit": "chars"}
    saved = json.loads(out_file.read_text())
    assert len(saved["results"]) == 6
    assert saved["summaries"][0]["metric"] == "length"


def test_eval_map_and_cluster(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    data = tmp_path / "d.jsonl"
    data.write_text(
        "\n".join(
            json.dumps({"q": f"q{i}", "a": "x", "gold": "x" if i % 2 else "y", "doc": i % 4})
            for i in range(8)
        )
    )
    code = main(
        [
            "eval",
            "--data",
            str(data),
            "--evaluators",
            "exact-match",
            "--map",
            "input=q",
            "--map",
            "output=a",
            "--map",
            "reference=gold",
            "--cluster-by",
            "doc",
            "--format",
            "json",
        ]
    )
    assert code == 0
    summary = json.loads(capsys.readouterr().out)["summaries"][0]
    assert summary["mean"] == 0.5
    assert summary["clusters"] == 4
    assert summary["ci"]["method"] == "clustered-t"


@pytest.mark.parametrize(
    ("args", "message"),
    [
        (["--evaluators", "nope"], "unknown evaluator"),
        (["--evaluators", "llm-judge"], "need a judge model"),
        (["--evaluators", "length", "--param", "oops"], "KEY=VALUE"),
        (["--evaluators", "length", "--param", "unit=chars"], "EVALUATOR.KEY=VALUE"),
        (
            ["--evaluators", "llm-judge", "--judge-model", "m"],
            "needs base_url",
        ),
    ],
)
def test_eval_errors_exit_1(
    args: list[str], message: str, capsys: pytest.CaptureFixture[str]
) -> None:
    assert main(["eval", "--data", str(EXAMPLE), *args]) == 1
    assert message in capsys.readouterr().err


def test_catalog(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["catalog"]) == 0
    out = capsys.readouterr().out
    assert "core (native, on by default)" in out
    assert "builtin/llm-judge@1.0.0" in out
    assert main(["catalog", "--pack", "judge", "--format", "json"]) == 0
    (pack,) = json.loads(capsys.readouterr().out)
    assert pack["evaluators"][0]["params"] == {"rubric": "correctness"}
    assert main(["catalog", "--pack", "missing"]) == 1


def test_version(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["version"]) == 0
    assert capsys.readouterr().out.startswith("evalsi ")


def test_serve_passes_flags_through_to_evalsid(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("EVALSID", "/opt/evalsid")
    monkeypatch.setattr("os.execv", lambda path, args: calls.append((path, args)))
    assert main(["serve", "--config", "evalsi.yaml", "--listen", ":9000"]) == 0
    assert calls == [
        ("/opt/evalsid", ["/opt/evalsid", "serve", "--config", "evalsi.yaml", "--listen", ":9000"])
    ]


def test_serve_without_evalsid(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("EVALSID", raising=False)
    monkeypatch.setenv("PATH", "")
    assert main(["serve"]) == 1
    assert "evalsid is not installed" in capsys.readouterr().err


def test_load_policy(tmp_path: Path) -> None:
    from evalsi.cli import load_policy

    example = Path(__file__).resolve().parents[3] / "examples" / "watch" / "support-policy.yaml"
    policy = load_policy(str(example))
    assert policy["name"] == "support-agent"
    assert policy["sampling"]["always"] == ["error", "duration_ms > 20000"]
    bad = tmp_path / "bad.yaml"
    bad.write_text("kind: OnlineEvalPolicy\nspec: {selectr: x}\n")
    with pytest.raises(ValueError, match="invalid policy"):
        load_policy(str(bad))


def test_archive_run_command(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    seen: list[str] = []

    class Server:
        def __enter__(self) -> Server:
            return self

        def __exit__(self, *exc: object) -> None:
            return None

        def archive_run(self, run_id: str) -> dict[str, str]:
            seen.append(run_id)
            return {"id": run_id, "archivedAt": "2026-10-12T00:00:00Z"}

    monkeypatch.setattr("evalsi.cli.server_client", lambda args: Server())
    assert main(["archive-run", "run-1", "--server", "http://s", "--yes"]) == 0
    assert seen == ["run-1"]
    assert "archived: run-1 (2026-10-12T00:00:00Z)" in capsys.readouterr().out
    with pytest.raises(SystemExit):  # --yes is required
        main(["archive-run", "run-1", "--server", "http://s"])


def test_eval_exits_1_when_every_evaluation_errored(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    data = tmp_path / "d.jsonl"
    data.write_text('{"output": "a"}\n')
    args = ["eval", "--data", str(data), "--evaluators", "regex-match", "--quiet"]
    assert main([*args, "--param", "regex-match.pattern=["]) == 1
    capsys.readouterr()


def test_eval_rejects_negative_limit(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit):
        main(["eval", "--data", str(EXAMPLE), "--limit", "-1"])
    assert "must be 0 or more" in capsys.readouterr().err


def test_serve_forwards_help_to_evalsid(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("EVALSID", "/opt/evalsid")
    monkeypatch.setattr("os.execv", lambda path, args: calls.append((path, args)))
    assert main(["serve", "--help"]) == 0
    assert main(["serve", "-h"]) == 0
    assert calls == [
        ("/opt/evalsid", ["/opt/evalsid", "serve", "--help"]),
        ("/opt/evalsid", ["/opt/evalsid", "serve", "-h"]),
    ]


def test_a_closed_stdout_ends_quietly(tmp_path: Path) -> None:
    import os
    import subprocess
    import sys

    read, write = os.pipe()
    os.close(read)  # like `evalsi ... | head` once head has gone
    proc = subprocess.run(
        [sys.executable, "-m", "evalsi", "catalog"],
        stdout=write,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    os.close(write)
    assert proc.returncode == 141
    assert "Traceback" not in proc.stderr


def test_compare_prints_intervals_the_server_sent_without_zeros(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    from evalsi.client import Client

    # Proto3 JSON: an unchanged metric's interval [0, 0] has neither low nor high,
    # and [-0.5, 0] has no high.
    reply = {
        "comparisons": [
            {
                "metric": "task-success",
                "baselineMean": 1,
                "candidateMean": 0,
                "diff": -1,
                "diffCi": {"low": -1, "high": -1, "level": 0.95, "method": "paired-t"},
                "pairedN": "3",
                "significant": True,
            },
            {
                "metric": "run-completed",
                "baselineMean": 1,
                "candidateMean": 1,
                "diff": 0,
                "diffCi": {"level": 0.95, "method": "paired-t"},
                "pairedN": "3",
            },
            {"metric": "steps", "diff": -0.25, "diffCi": {"low": -0.5}, "pairedN": "3"},
        ]
    }
    monkeypatch.setattr(Client, "call", lambda self, *a, **k: reply)
    assert main(["compare", "--server", "http://localhost:8080", "base", "cand"]) == 0
    out = capsys.readouterr().out
    assert "[-1.000, -1.000]  significant" in out
    (unchanged,) = [line for line in out.splitlines() if line.startswith("run-completed")]
    assert "[0.000, 0.000]" in unchanged
    assert "[-0.500, 0.000]" in out

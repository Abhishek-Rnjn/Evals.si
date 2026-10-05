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
    assert "core (on by default)" in out
    assert "builtin/llm-judge@1.0.0" in out
    assert main(["catalog", "--pack", "judge", "--format", "json"]) == 0
    (pack,) = json.loads(capsys.readouterr().out)
    assert pack["evaluators"][0]["params"] == {"rubric": "correctness"}
    assert main(["catalog", "--pack", "missing"]) == 1


def test_version(capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["version"]) == 0
    assert capsys.readouterr().out.startswith("evalsi ")

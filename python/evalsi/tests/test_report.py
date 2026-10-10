from __future__ import annotations

from pathlib import Path
from typing import Any

import evalsi
from evalsi import report
from evalsi.cli import main


def test_report_from_a_results_file(tmp_path: Path) -> None:
    data = [
        {"id": "a", "input": "q1", "output": "<script>x</script>", "reference": "y"},
        {"id": "b", "input": "q2", "output": "y", "reference": "y"},
    ]
    result = evalsi.evaluate(data, ["exact-match"])
    out = tmp_path / "r.json"
    result.save(out)
    for name in ("r.md", "r.html"):
        assert main(["report", str(out), "-o", str(tmp_path / name)]) == 0
    md = (tmp_path / "r.md").read_text()
    assert "| exact-match | 2 | 0.500 |" in md
    assert "### a (trial 0): fail" in md
    assert "### b" not in md  # passes are not listed as worst
    page = (tmp_path / "r.html").read_text()
    assert "&lt;script&gt;x&lt;/script&gt;" in page
    assert "<script>" not in page
    assert "prefers-color-scheme: dark" in page


class FakeClient:
    def get_run(self, run_id: str) -> dict[str, Any]:
        return {
            "id": run_id,
            "name": "nightly",
            "status": "RUN_STATUS_FAILED",
            "spec": {"target": {"connector": "anthropic", "model": "claude"}},
            "summaries": [
                {"metric": "judge", "n": "2", "mean": 0.25, "ci": {"low": 0.1, "high": 0.4}}
            ],
            "gates": [
                {"gate": {"metric": "judge", "min": 0.5}, "passed": False, "reason": "0.25 < 0.5"}
            ],
        }

    def list_run_results(self, run_id: str, page_token: str = "") -> dict[str, Any]:
        if not page_token:
            return {
                "results": [
                    {
                        "recordId": "1",
                        "evaluator": "judge",
                        "outcome": "OUTCOME_SCORED",
                        "scores": [{"name": "judge", "number": 0.5, "explanation": "meh"}],
                    },
                ],
                "records": [{"id": "1", "input": {"text": "hi"}, "output": {"text": "yo"}}],
                "nextPageToken": "p2",
            }
        return {
            "results": [
                {
                    "recordId": "2",
                    "evaluator": "judge",
                    "outcome": "OUTCOME_SCORED",
                    "scores": [{"name": "judge", "number": 0.0}],
                },
                {
                    "recordId": "3",
                    "evaluator": "judge",
                    "outcome": "OUTCOME_ERROR",
                    "reason": "timeout",
                },
            ],
            "records": [{"id": "2", "input": {"text": "x"}}],
        }


def test_report_from_a_server_run() -> None:
    r = report.from_server(FakeClient(), "run-1")
    assert r.meta["status"] == "failed"
    assert r.meta["target"] == "anthropic claude"
    assert [e.record_id for e in r.worst["judge"]] == ["2", "1"]
    assert r.worst["judge"][1].explanation == "meh"
    assert r.errors == [{"record_id": "3", "evaluator": "judge", "reason": "timeout"}]
    md = report.to_markdown(r)
    assert "❌ judge mean ≥ 0.5 (0.25 < 0.5)" in md


def test_each_example_shows_the_output_its_trial_produced() -> None:
    r = report.build(
        title="t",
        meta={},
        summaries=[{"metric": "exact-match", "higher_is_better": True}],
        gates=[],
        results=[
            {
                "record_id": "r",
                "trial": t,
                "evaluator": "exact-match",
                "outcome": "scored",
                "scores": [{"name": "exact-match", "passed": t == 1}],
            }
            for t in (1, 0)
        ],
        records=[
            {"id": "r", "output": {"text": answer}, "provenance": {"run": {"trial": t}}}
            for t, answer in ((0, "wrong"), (1, "right"))
        ],
    )
    (failed,) = r.worst["exact-match"]
    assert (failed.trial, failed.output) == (0, "wrong")


def test_a_low_score_on_a_one_to_five_scale_is_shown() -> None:
    r = report.build(
        title="t",
        meta={},
        summaries=[{"metric": "rating", "higher_is_better": True}],
        gates=[],
        results=[
            {
                "record_id": "r",
                "evaluator": "rating",
                "outcome": "scored",
                "scores": [{"name": "rating", "number": 1}],
            }
        ],
        records=[{"id": "r", "output": {"text": "poor"}}],
    )
    assert [e.value for e in r.worst["rating"]] == [1.0]


def test_embedded_results_files_record_each_outputs_trial(tmp_path: Path) -> None:
    from evalsi.results import EvalResult
    from evalsi.types import Record

    result = EvalResult(
        records=[Record(id="r"), Record(id="r")],
        results=[],
        summaries=[],
        manifest={},
        record_trials=[0, 1],
    )
    assert [r["provenance"]["run"]["trial"] for r in result.to_dict()["records"]] == [0, 1]

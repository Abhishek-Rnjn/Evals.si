from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

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
    assert "❌ judge mean ≥ 0.5 (0.25 &lt; 0.5)" in md


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


def test_server_report_of_an_archived_run_is_its_summary() -> None:
    from evalsi.report import from_server, to_markdown

    class Client:
        def get_run(self, run_id: str) -> dict[str, Any]:
            return {
                "id": run_id,
                "name": "nightly",
                "status": "RUN_STATUS_SUCCEEDED",
                "archivedAt": "2026-10-12T00:00:00Z",
                "summaries": [{"metric": "exact-match", "n": "10", "mean": 0.8}],
            }

        def list_run_results(self, *args: Any, **kwargs: Any) -> dict[str, Any]:
            raise AssertionError("an archived run has no results to list")

    report = from_server(Client(), "run-1")
    assert report.meta["archived"] == "2026-10-12T00:00:00Z"
    assert "exact-match" in to_markdown(report)
    assert "Per-record results are unavailable" in to_markdown(report)
    assert "no longer available" in to_markdown(report)


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_finished_evaluation_is_not_release_approval(render: Any) -> None:
    result = evalsi.evaluate(
        [{"id": "good", "input": "hello", "output": "hello", "reference": "hello"}],
        ["exact-match"],
    )
    r = report.from_results_file(result.to_dict())
    text = render(r)
    assert "0 failed, 1 passed (explicit boolean scores)" in text
    assert "completion is not release approval" in text
    assert "no gate results were recorded" in text
    assert "not proof of evaluator validity" in text
    assert "No baseline comparison is included" in text
    assert r.worst == {}


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_failed_check_explains_expected_actual_and_how_to_investigate(render: Any) -> None:
    r = report.from_results_file(
        {
            "results": [
                {
                    "record_id": "case-1",
                    "trial": 1,
                    "evaluator": "application-exact-match",
                    "outcome": "scored",
                    "scores": [{"passed": False, "explanation": "Answer differs from reference."}],
                }
            ],
            "records": [
                {"id": "case-1", "output": "wrong trial", "provenance": {"run": {"trial": 0}}},
                {
                    "id": "case-1",
                    "input": "2 + 2?",
                    "output": "5",
                    "reference": "4",
                    "provenance": {"run": {"trial": 1}},
                },
            ],
        }
    )
    text = render(r)
    assert "1 failed, 0 passed" in text
    assert "Answer differs from reference." in text
    assert "expected/reference" in text
    assert "actual output" in text
    assert "wrong trial" not in text
    assert "original application/evaluation command" in text
    assert "record ID/trial" in text
    assert "exact command is not recorded" in text
    failed = r.worst["application-exact-match"][0]
    assert (failed.record_id, failed.trial, failed.output, failed.reference) == (
        "case-1",
        1,
        "5",
        "4",
    )
    if render is report.to_html:
        assert "<details open>" in text


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_errors_skips_and_incomplete_outcomes_are_not_failed_checks(render: Any) -> None:
    r = report.from_results_file(
        {
            "results": [
                {
                    "recordId": "app",
                    "outcome": "OUTCOME_ERROR",
                    "reason": "Application unavailable",
                },
                {"recordId": "infra", "outcome": "OUTCOME_ERROR", "reason": "timeout"},
                {"recordId": "skip", "outcome": "OUTCOME_SKIPPED", "reason": "Reference missing"},
                {"recordId": "cancel", "outcome": "OUTCOME_CANCELLED"},
                {"recordId": "future", "outcome": "a-new-outcome"},
            ]
        }
    )
    text = render(r)
    assert "0 failed, 0 passed" in text
    assert (
        "Execution/evaluation errors: 2; skipped: 1; cancelled: 1; unrecognized outcomes: 1" in text
    )
    assert "These are not failed checks or passes" in text
    assert "application, evaluator, or infrastructure" in text
    assert "Application unavailable" in text
    assert "timeout" in text
    assert "Reference missing" in text
    assert "resolve the recorded execution problem" in text
    assert "obtain the missing evidence" in text
    assert r.worst == {}


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_numeric_or_label_scores_do_not_invent_failure_or_approval(render: Any) -> None:
    r = report.from_results_file(
        {
            "summaries": [{"metric": "rating", "n": 1, "mean": 1}],
            "results": [
                {
                    "record_id": "r",
                    "evaluator": "rating",
                    "outcome": "scored",
                    "scores": [
                        {"number": 1},
                        {"label": "fail"},
                        {"structured": {"passed": False}},
                    ],
                }
            ],
        }
    )
    text = render(r)
    assert "0 failed, 0 passed" in text
    assert "3 other scores have no inferred pass/fail threshold" in text
    assert "no gate results were recorded" in text
    assert "1 scored results lack a matching record" in text
    assert "Not recorded; do not infer it." in text
    assert "numeric scores are not gates" in text


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_empty_or_summary_only_report_admits_unavailable_evidence(render: Any) -> None:
    r = report.from_results_file({"summaries": [{"metric": "check", "n": 5, "mean": 1}]})
    text = render(r)
    assert "Per-record results are unavailable" in text
    assert "Completion status is not recorded" in text
    assert "0 failed" not in text
    assert "no gate results were recorded" in text


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_missing_scores_and_empty_outputs_are_not_silently_omitted(render: Any) -> None:
    r = report.from_results_file(
        {
            "results": [
                {"record_id": "empty", "evaluator": "check", "outcome": "scored"},
                {
                    "record_id": "missing",
                    "evaluator": "check",
                    "outcome": "scored",
                    "scores": [{"passed": False}],
                },
            ],
            "records": [{"id": "missing", "output": "", "reference": ""}],
        }
    )
    text = render(r)
    assert "1 scored results contain no scores" in text
    assert "1 scored results lack a matching record" in text
    assert "(empty)" in text
    assert "Check returned false; no explanation was recorded." in text


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_only_explicit_gate_results_are_reported(render: Any) -> None:
    r = report.from_results_file(
        {
            "gates": [
                {"metric": "a", "passed": True},
                {"metric": "b", "passed": False},
                {"metric": "c"},
            ]
        }
    )
    text = render(r)
    assert "Recorded gates: 1 passed, 1 failed, 1 unavailable" in text
    assert "not a new release decision" in text
    assert "no gate results" not in text


@pytest.mark.parametrize("render", [report.to_markdown, report.to_html])
def test_a_server_gate_without_passed_is_a_failed_gate(render: Any) -> None:
    class Client:
        def get_run(self, run_id: str) -> dict[str, Any]:
            # Proto3 JSON leaves a false GateResult.passed out.
            gates = [
                {"gate": {"metric": "task-success", "min": 1}, "value": 0, "reason": "mean 0"},
                {"gate": {"metric": "run-completed", "min": 1}, "passed": True, "value": 1},
            ]
            return {"id": run_id, "gates": gates}

        def list_run_results(self, run_id: str, page_token: str = "") -> dict[str, Any]:
            return {}

    r = report.from_server(Client(), "run-1")
    assert [g["passed"] for g in r.gates] == [False, True]
    assert "Recorded gates: 1 passed, 1 failed, 0 unavailable" in render(r)


def test_report_guidance_does_not_copy_manifest_credentials() -> None:
    r = report.from_results_file(
        {
            "manifest": {
                "environment": {"API_KEY": "private-api-key"},
                "command": "evaluate --token private-token",
            }
        }
    )
    for render in (report.to_markdown, report.to_html):
        text = render(r)
        assert "private-api-key" not in text
        assert "private-token" not in text


def test_null_optional_score_fields_do_not_hide_a_boolean_failure() -> None:
    r = report.from_results_file(
        {
            "results": [
                {
                    "record_id": "r",
                    "evaluator": "check",
                    "outcome": "scored",
                    "scores": [{"number": None, "passed": False}, {"passed": None}],
                }
            ]
        }
    )
    assert r.counts["failed_checks"] == 1
    assert r.counts["other_scores"] == 1
    assert r.worst["check"][0].label == "fail"
    assert "Not recorded." in report.to_markdown(r)


def test_error_skip_and_failed_evidence_are_bounded_and_escaped() -> None:
    hostile = "<script>alert(1)</script>"
    r = report.from_results_file(
        {
            "results": [
                {"outcome": "error", "reason": hostile + "x" * 3000},
                *[{"outcome": "skipped", "reason": hostile + "x" * 3000} for _ in range(55)],
                {
                    "record_id": "r",
                    "evaluator": "check",
                    "outcome": "scored",
                    "scores": [{"passed": False, "explanation": hostile + "x" * 3000}],
                },
            ],
            "records": [{"id": "r", "output": hostile + "x" * 3000}],
        }
    )
    assert len(r.skipped) == 50
    assert r.counts["skipped"] == 55
    assert all(len(item["reason"]) <= 2002 for item in [*r.errors, *r.skipped])
    assert len(r.worst["check"][0].explanation) <= 2002
    page = report.to_html(r)
    assert hostile not in page
    assert "&lt;script&gt;" in page
    md = report.to_markdown(r)
    assert "&lt;script&gt;" in md


def application_report_record(**changes: Any) -> dict[str, Any]:
    hint = {"version": 1, "callable": "application:regressed", "cases": "cases.jsonl", "case": "r"}
    hint.update(changes)
    return {"id": "r", "metadata": {"evalsi_application_reproduction": hint}}


def report_with_application_record(record: dict[str, Any], trial: int = 0) -> report.Report:
    return report.from_results_file(
        {
            "records": [record],
            "results": [
                {
                    "record_id": record["id"],
                    "trial": trial,
                    "evaluator": "check",
                    "outcome": "scored",
                    "scores": [{"passed": False}],
                }
            ],
        }
    )


@pytest.mark.parametrize(
    "changes",
    [
        {"version": True},
        {"version": 2},
        {"version": "1"},
        {"command": "private-secret"},
        {"callable": "https://private-secret@example.com"},
        {"callable": "application:answer;private-secret"},
        {"callable": "a" * 257 + ":answer"},
        {"cases": "/private-secret/cases.jsonl"},
        {"cases": "../private-secret"},
        {"cases": "a/../private-secret"},
        {"cases": "./private-secret"},
        {"cases": "a//private-secret"},
        {"cases": "-private-secret"},
        {"cases": "C:/private-secret"},
        {"cases": "a\\private-secret"},
        {"cases": "private-secret\ncases.jsonl"},
        {"cases": "a" * 513},
        {"cases": None},
        {"case": "private-secret"},
        {"case": "a" * 257},
        {"case": "-r"},
        {"case": "r\x7f"},
        {"case": "ré"},
    ],
)
def test_tampered_reproduction_metadata_is_not_rendered(changes: dict[str, Any]) -> None:
    r = report_with_application_record(application_report_record(**changes))
    assert not r.worst["check"][0].reproduction
    for render in (report.to_html, report.to_markdown):
        text = render(r)
        assert "Reproduction arguments" not in text
        assert "private-secret" not in text
        assert "An exact command is not recorded here" in text


@pytest.mark.parametrize(
    "metadata",
    [
        None,
        [],
        {"command": "private-secret"},
        "private-secret",
        {"evalsi_application_reproduction": []},
        {"evalsi_application_reproduction": "private-secret"},
    ],
)
def test_unrecognized_reproduction_metadata_is_ignored(metadata: Any) -> None:
    r = report_with_application_record({"id": "r", "metadata": metadata})
    for render in (report.to_html, report.to_markdown):
        text = render(r)
        assert "Reproduction arguments" not in text
        assert "private-secret" not in text


def test_recorded_reproduction_cannot_claim_a_different_trial() -> None:
    r = report_with_application_record(application_report_record(), trial=1)
    assert not r.worst["check"][0].reproduction


@pytest.mark.parametrize("case", ["-r", "r\n", "r\x7f", "ré", "r" * 257])
def test_unsafe_record_ids_cannot_supply_reproduction_even_when_matching(case: str) -> None:
    record = application_report_record(case=case)
    record["id"] = case
    r = report_with_application_record(record)
    assert not r.worst["check"][0].reproduction


def test_reproduction_values_are_portable_arguments_not_interpolated_shell_text() -> None:
    case = "r ' $(touch never) <script>```&"
    record = application_report_record(case=case, cases="case data/it's $(literal).jsonl")
    record["id"] = case
    r = report_with_application_record(record)
    arguments = r.worst["check"][0].reproduction
    assert json.loads(arguments) == [
        "python",
        "evaluate.py",
        "--callable",
        "application:regressed",
        "--cases",
        "case data/it's $(literal).jsonl",
        "--case",
        case,
    ]
    assert "<script>" not in arguments
    assert "```" not in arguments
    for render in (report.to_html, report.to_markdown):
        text = render(r)
        assert "JSON array, not shell text" in text
        assert "original generated project directory" in text
        assert "metadata is not authenticated" in text
        assert "rerun replaces results.json" in text
    assert "<script>" not in report.to_html(r)


@pytest.mark.parametrize("target", ["application:regressed", "custom:answer"])
def test_generated_application_reproduction_replays_the_reported_case(
    tmp_path: Path, target: str
) -> None:
    project = tmp_path / "application project"
    assert main(["init", str(project), "--template", "application"]) == 0
    (project / "custom.py").write_text(
        "def answer(question):\n    raise RuntimeError('private-secret')\n"
    )
    cases = project / "case data.jsonl"
    cases.write_text((project / "cases.jsonl").read_text())
    completed = subprocess.run(
        [sys.executable, "evaluate.py", "--callable", target, "--cases", cases.name],
        cwd=project,
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )
    expected_exit = 3 if target == "application:regressed" else 1
    assert completed.returncode == expected_exit, completed.stderr
    r = report.from_results_file(json.loads((project / "results.json").read_text()))
    if expected_exit == 3:
        (failed,) = [x for items in r.worst.values() for x in items if x.label == "fail"]
        rid, arguments = failed.record_id, failed.reproduction
        assert (rid, failed.output, failed.reference) == ("japan", "Kyoto", "Tokyo")
    else:
        rid, arguments = r.errors[0]["record_id"], r.errors[0]["reproduction"]
        assert r.counts["error"] == 3
        assert not r.counts.get("failed_checks")
    argv = json.loads(arguments)
    assert argv == [
        "python",
        "evaluate.py",
        "--callable",
        target,
        "--cases",
        cases.name,
        "--case",
        rid,
    ]
    for render in (report.to_html, report.to_markdown):
        text = render(r)
        assert "Reproduction arguments" in text
        assert "private-secret" not in text
        assert "For other results, an exact command is not recorded here" in text
    replay = subprocess.run(
        [sys.executable, *argv[1:], "--output", "replay.json"],
        cwd=project,
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )
    assert replay.returncode == expected_exit, replay.stderr
    replayed = json.loads((project / "replay.json").read_text())
    assert [item["record_id"] for item in replayed["results"]] == [rid]
    assert replayed["results"][0]["outcome"] == ("scored" if expected_exit == 3 else "error")

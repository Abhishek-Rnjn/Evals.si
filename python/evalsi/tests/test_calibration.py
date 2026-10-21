from __future__ import annotations

import copy
import json
import subprocess
import sys
import threading
from collections import Counter
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from evalsi import MetricSpec, Record, Registry, Requirements, Score, ScoreType, evaluator
from evalsi.calibration import PROTOCOL_SCHEMA, calibrate, compare_reports
from evalsi.calibration_report import render_report
from evalsi.calibration_sets import freeze_set, read_artifact, write_artifact
from evalsi.calibration_stats import paired_difference, validity
from evalsi.cli import main


@evaluator(name="test/score", version="1.0.0")
def numeric_score(record: Record, *, offset: float = 0) -> Score:
    assert "ratings" not in record.metadata
    assert "label" not in record.metadata
    assert record.reference is not None
    assert record.reference.as_text() == "original reference"
    assert record.output is not None
    return Score(number=float(record.output.as_text()) + offset)


def rows() -> list[dict[str, Any]]:
    return [
        {
            "record": {
                "id": rid,
                "input": f"question {rid}",
                "output": str(score),
                "reference": "original reference",
            },
            "split": split,
            "ratings": [{"annotator": "human-1", "passed": label}],
            "slices": {"difficulty": "easy" if rid.endswith("1") else "hard"},
        }
        for rid, split, score, label in [
            ("t1", "tuning", 0.2, False),
            ("t2", "tuning", 0.6, True),
            ("h1", "held-out", 0.6, False),
            ("h2", "held-out", 0.8, True),
        ]
    ]


def setup_run(
    tmp_path: Path, data: list[dict[str, Any]] | None = None
) -> tuple[Path, dict[str, Any], Registry]:
    path = tmp_path / "set.json"
    pin = write_artifact(
        path,
        freeze_set(
            data if data is not None else rows(), name="qa", version="v1", question="Correct?"
        ),
    )
    protocol = {
        "schema": PROTOCOL_SCHEMA,
        "dataset_sha256": pin,
        "repeats": 2,
        "resamples": 100,
        "candidates": [
            {
                "name": "judge-a",
                "evaluator": {"ref": "test/score"},
                "metric": "score",
                "thresholds": [0.5, 0.75],
                "pass_when": "gte",
            }
        ],
    }
    registry = Registry([])
    registry.add(numeric_score)
    return path, protocol, registry


def test_threshold_selection_uses_tuning_and_keeps_heldout_disagreements(tmp_path: Path) -> None:
    path, protocol, registry = setup_run(tmp_path)
    result = calibrate(path, protocol, registry=registry)
    candidate = result["candidates"][0]
    assert candidate["definition"]["evaluator"]["ref"] == "test/score@1.0.0"
    assert candidate["definition"]["evaluator"]["params"] == {"offset": 0}
    assert len(candidate["definition"]["implementation_sha256"]) == 64
    assert candidate["selection"]["threshold"] == 0.5  # .75 would be perfect on held-out.
    assert result["selected_candidate"] == "judge-a"
    held_out = candidate["held-out"]
    assert held_out["cases"] == 2
    assert held_out["groups"] == 2
    assert held_out["outcomes"]["scored"] == 4  # Repeats never increase cases/groups.
    assert held_out["metrics"]["accuracy"]["value"] == 0.5
    assert held_out["metrics"]["fpr"]["value"] == 1
    assert held_out["metrics"]["fnr"]["value"] == 0
    assert held_out["slices"][0]["cases"] == 1
    assert held_out["slices"][0]["metrics"]["accuracy"]["ci"] is None
    assert held_out["cases_detail"][0]["accuracy"] == 0
    assert not result["held_out_comparisons"]


def test_changing_heldout_labels_cannot_change_tuning_selection(tmp_path: Path) -> None:
    path, protocol, registry = setup_run(tmp_path)
    before = calibrate(path, protocol, registry=registry)
    changed = rows()
    for row in changed:
        if row["split"] == "held-out":
            row["ratings"][0]["passed"] = not row["ratings"][0]["passed"]
    other_dir = tmp_path / "other"
    other_dir.mkdir()
    path, protocol, registry = setup_run(other_dir, changed)
    after = calibrate(path, protocol, registry=registry)
    assert before["candidates"][0]["selection"] == after["candidates"][0]["selection"]
    with pytest.raises(ValueError, match="different records or human-label revisions"):
        compare_reports(before, after)


def test_candidate_selected_on_tuning_even_when_another_wins_heldout(tmp_path: Path) -> None:
    path, protocol, registry = setup_run(tmp_path)
    protocol["candidates"][0]["thresholds"] = [0.5]
    second = copy.deepcopy(protocol["candidates"][0])
    second.update(name="judge-b", thresholds=[0.75])
    protocol["candidates"].append(second)
    result = calibrate(path, protocol, registry=registry)
    assert result["selected_candidate"] == "judge-a"
    assert result["candidates"][1]["held-out"]["metrics"]["accuracy"]["value"] == 1
    delta = result["held_out_comparisons"][0]
    assert delta["paired_cases"] == 2
    assert delta["accuracy_delta"]["value"] == 0.5


@pytest.mark.parametrize("bad_output", [None, "error", "nan"])
def test_missing_failed_and_nonfinite_scores_never_become_human_disagreement(
    tmp_path: Path, bad_output: str | None
) -> None:
    data = rows()
    data[2]["record"]["output"] = bad_output
    path, protocol, registry = setup_run(tmp_path, data)
    result = calibrate(path, protocol, registry=registry)
    phase = result["candidates"][0]["held-out"]
    assert phase["incomplete_cases"] == 1
    assert phase["cases"] == 1
    assert phase["metrics"]["fpr"]["value"] is None
    assert phase["cases_detail"][0]["accuracy"] is None
    assert phase["outcomes"]["skipped" if bad_output is None else "error"] == 2
    assert b"incomplete" in render_report(result)


def test_incomplete_tuning_refuses_threshold_selection_and_keeps_diagnostics(
    tmp_path: Path,
) -> None:
    data = rows()
    data[0]["record"]["output"] = "error"
    path, protocol, registry = setup_run(tmp_path, data)
    result = calibrate(path, protocol, registry=registry)
    assert result["selected_candidate"] is None
    candidate = result["candidates"][0]
    assert candidate["selection"]["threshold"] is None
    assert candidate["tuning"]["outcomes"]["error"] == 2
    assert candidate["held-out"]["metrics"]["accuracy"]["value"] is None
    assert b"threshold unavailable" in render_report(result)


def test_unresolved_human_ties_are_retained_and_excluded(tmp_path: Path) -> None:
    data = rows()
    data[2]["ratings"].append({"annotator": "human-2", "passed": True})
    path, protocol, registry = setup_run(tmp_path, data)
    result = calibrate(path, protocol, registry=registry)
    heldout = result["candidates"][0]["held-out"]
    assert heldout["unlabelled_cases"] == 1
    assert heldout["cases"] == 1
    assert len(result["dataset"]["cases"][2]["ratings"]) == 2
    assert b"consensus None" in render_report(result)


def test_uncertainty_and_pairing_preserve_independent_groups() -> None:
    sample = [("g1", True, 1.0), ("g1", True, 1.0), ("g2", False, 1.0), ("g3", True, 0.5)]
    summary = validity(sample, confidence=0.95, resamples=500)
    assert summary["cases"] == 4
    assert summary["groups"] == 3
    assert summary["metrics"]["accuracy"]["value"] == 0.625
    assert summary["metrics"]["accuracy"]["ci"] == [0.0, 1.0]
    assert validity(sample, confidence=0.95, resamples=500) == summary
    one = validity([("g", True, 1.0)] * 100, confidence=0.95, resamples=100)
    assert one["metrics"]["accuracy"]["ci"] is None
    perfect = validity([("a", True, 1.0), ("b", True, 1.0)], confidence=0.95, resamples=100)
    assert perfect["metrics"]["accuracy"]["ci"] is None
    assert "no empirical variation" in perfect["metrics"]["accuracy"]["ci_status"]
    paired = paired_difference(
        {"x": ("g", 0), "y": ("h", 1)}, {"x": ("g", 1)}, confidence=0.95, resamples=100
    )
    assert paired["paired_cases"] == 1
    assert paired["unpaired_before"] == 1
    assert paired["accuracy_delta"]["value"] == 1


def test_report_comparison_is_about_judge_drift_on_fixed_outputs(tmp_path: Path) -> None:
    path, protocol, registry = setup_run(tmp_path)
    before = calibrate(path, protocol, registry=registry)
    protocol["candidates"][0]["thresholds"] = [0.75]
    after = calibrate(path, protocol, registry=registry)
    comparison = compare_reports(before, after)
    assert comparison["fixed_target_outputs"]
    candidate = comparison["candidates"][0]
    assert candidate["evaluator_changed"]
    assert candidate["threshold_changed"]
    assert candidate["accuracy_delta"]["value"] == 0.5


def test_partial_repeats_cannot_cherry_pick_the_successful_score(tmp_path: Path) -> None:
    path, protocol, _ = setup_run(tmp_path)
    protocol["candidates"][0]["thresholds"] = [0.5]
    calls: Counter[str] = Counter()

    @evaluator(name="test/score", version="1.0.0")
    def flaky(record: Record) -> Score:
        calls[record.id] += 1
        if record.id == "h1" and calls[record.id] == 2:
            raise ValueError("second repeat failed")
        return Score(number=1)

    registry = Registry([])
    registry.add(flaky)
    result = calibrate(path, protocol, registry=registry)
    phase = result["candidates"][0]["held-out"]
    assert result["status"] == "incomplete"
    assert phase["cases"] == 1
    assert phase["incomplete_cases"] == 1
    assert phase["cases_detail"][0]["pass_probability"] is None
    assert phase["outcomes"]["scored"] == 3
    assert phase["outcomes"]["error"] == 1


def test_a_missing_selected_metric_is_an_error(tmp_path: Path) -> None:
    path, protocol, _ = setup_run(tmp_path)

    @evaluator(name="test/score", version="1.0.0")
    def wrong_metric(record: Record) -> Score:
        return Score(number=1, name="another-output")

    registry = Registry([])
    registry.add(wrong_metric)
    result = calibrate(path, protocol, registry=registry)
    assert result["candidates"][0]["tuning"]["outcomes"]["error"] == 4
    assert result["selected_candidate"] is None


def test_lower_is_pass_and_threshold_ties_follow_declared_order(tmp_path: Path) -> None:
    path, protocol, registry = setup_run(tmp_path)
    candidate = protocol["candidates"][0]
    candidate.update(thresholds=[0.9, 0.75], pass_when="lte")
    result = calibrate(path, protocol, registry=registry)
    assert result["candidates"][0]["selection"]["threshold"] == 0.9
    assert result["candidates"][0]["held-out"]["metrics"]["fpr"]["value"] == 1


def test_one_class_tuning_cannot_tune_a_threshold(tmp_path: Path) -> None:
    data = rows()
    data[0]["ratings"][0]["passed"] = True
    path, protocol, registry = setup_run(tmp_path, data)
    result = calibrate(path, protocol, registry=registry)
    assert result["candidates"][0]["selection"]["method"] == "tuning requires both human classes"
    assert result["status"] == "incomplete"


@pytest.mark.parametrize("kind", ["dataset", "label"])
def test_unsupported_evaluator_shapes_rejected(tmp_path: Path, kind: str) -> None:
    path, protocol, _ = setup_run(tmp_path)

    @evaluator(
        name="test/score",
        version="1.0.0",
        scope="dataset" if kind == "dataset" else "record",
        outputs=[MetricSpec("score", ScoreType.LABEL)] if kind == "label" else None,
    )
    def unsupported(record: Record) -> Score:
        pytest.fail("unsupported evaluator must not run")

    registry = Registry([])
    registry.add(unsupported)
    with pytest.raises(ValueError, match=r"record-scope|declared numeric"):
        calibrate(path, protocol, registry=registry)


@pytest.mark.parametrize(
    "judge",
    [
        None,
        {"provider": "openai-compatible", "model": "m", "base_url": "http://user:secret@judge/v1"},
        {"provider": "anthropic", "model": "m", "timeout_s": -1},
        {"provider": "anthropic", "model": "m", "max_tokens": True},
        {"provider": "anthropic", "model": "m", "temperature": "hot"},
        {"provider": "anthropic", "model": "m", "api_key_env": "literal-key!"},
    ],
)
def test_explicit_safe_judge_configuration_required_before_any_calls(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, judge: dict[str, Any] | None
) -> None:
    path, protocol, _ = setup_run(tmp_path)
    monkeypatch.setenv("EVALSI_JUDGE_MODEL", "must-not-fall-back-to-environment")

    @evaluator(name="test/score", version="1.0.0", requires=Requirements(judge=True))
    def judge_required(record: Record) -> Score:
        pytest.fail("invalid judge must not execute")

    registry = Registry([])
    registry.add(judge_required)
    protocol["candidates"][0]["judge"] = judge
    with pytest.raises(
        ValueError,
        match=r"explicit judge config|judge URLs|invalid judge|positive integer|api_key_env",
    ):
        calibrate(path, protocol, registry=registry)


@pytest.mark.parametrize(
    "change",
    [
        "pin",
        "schema",
        "duplicates",
        "no candidates",
        "bad threshold",
        "bad direction",
        "repeats",
        "confidence",
        "unknown metric",
        "unknown option",
    ],
)
def test_bad_protocol_fails_before_evaluator_execution(tmp_path: Path, change: str) -> None:
    path, protocol, registry = setup_run(tmp_path)
    if change == "pin":
        protocol["dataset_sha256"] = "0" * 64
    elif change == "schema":
        protocol["schema"] = "unsupported"
    elif change == "duplicates":
        protocol["candidates"] *= 2
    elif change == "no candidates":
        protocol["candidates"] = []
    elif change == "bad threshold":
        protocol["candidates"][0]["thresholds"] = [True]
    elif change == "bad direction":
        protocol["candidates"][0]["pass_when"] = "higher"
    elif change == "repeats":
        protocol["repeats"] = True
    elif change == "confidence":
        protocol["confidence"] = 1
    elif change == "unknown metric":
        protocol["candidates"][0]["metric"] = "missing"
    else:
        protocol["candidates"][0]["typo"] = True

    @evaluator(name="test/score", version="1.0.0")
    def forbidden(record: Record) -> Score:
        pytest.fail("invalid protocol must not execute an evaluator")

    registry = Registry([])
    registry.add(forbidden)
    messages = {
        "pin": "SHA-256 mismatch",
        "schema": "approved dataset_sha256",
        "duplicates": "names must be unique",
        "no candidates": "needs candidates",
        "bad threshold": "finite numbers",
        "bad direction": "pass_when",
        "repeats": "repeats must be an integer",
        "confidence": "confidence must be",
        "unknown metric": "declared numeric",
        "unknown option": "candidate fields",
    }
    with pytest.raises(ValueError, match=messages[change]):
        calibrate(path, protocol, registry=registry)


def test_real_http_judge_repeats_are_fresh_and_audited(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    requests: list[dict[str, Any]] = []
    counts: Counter[str] = Counter()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            assert self.path == "/v1/chat/completions"
            assert self.headers["Authorization"] == "Bearer private-key-never-in-report"
            requests.append(body)
            prompt = body["messages"][1]["content"]
            counts[prompt] += 1
            # Identical prompts produce different verdicts on fresh calls.
            score = 5 if counts[prompt] % 2 else 1
            response = {
                "model": "actual-local-judge",
                "choices": [
                    {
                        "finish_reason": "stop",
                        "message": {
                            "content": json.dumps({"score": score, "reasoning": "scripted"})
                        },
                    }
                ],
            }
            raw = json.dumps(response).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format: str, *args: Any) -> None:
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setenv("NO_PROXY", "127.0.0.1,localhost")
    monkeypatch.setenv("EVALSI_CACHE_DIR", str(tmp_path / "cache"))
    monkeypatch.setenv("CALIBRATION_TEST_KEY", "private-key-never-in-report")
    path, protocol, _ = setup_run(tmp_path)
    protocol["concurrency"] = 1
    protocol["candidates"] = [
        {
            "name": "llm",
            "evaluator": {"ref": "llm-judge", "params": {"rubric": "correctness"}},
            "metric": "llm-judge",
            "thresholds": [0.5],
            "pass_when": "gte",
            "judge": {
                "provider": "openai-compatible",
                "model": "declared-local-judge",
                "base_url": f"http://127.0.0.1:{server.server_port}/v1",
                "api_key_env": "CALIBRATION_TEST_KEY",
                "temperature": 0.7,
            },
        }
    ]
    try:
        result = calibrate(path, protocol)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
    assert len(requests) == 8
    assert len(counts) == 4
    assert set(counts.values()) == {2}
    assert all(request["temperature"] == 0.7 for request in requests)
    candidate = result["candidates"][0]
    assert len(candidate["judge_calls"]) == 8
    assert {call["model"] for call in candidate["judge_calls"]} == {"actual-local-judge"}
    assert all(not call["cached"] for call in candidate["judge_calls"])
    heldout = candidate["held-out"]
    assert heldout["repeatability"]["decision_disagreement"]["value"] == 1
    assert heldout["cases"] == 2
    assert heldout["outcomes"]["scored"] == 4
    assert heldout["metrics"]["accuracy"]["value"] == 0.5
    assert "private-key-never-in-report" not in json.dumps(result)
    assert not (tmp_path / "cache").exists()


def test_cli_freeze_run_verify_report_and_compare(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    data = rows()
    for row in data:
        row["record"]["output"] = "original reference" if row["ratings"][0]["passed"] else "wrong"
    data[2]["ratings"].append(
        {"annotator": "human-2", "passed": False, "comment": "<script>bad()</script>"}
    )
    source = tmp_path / "labels.jsonl"
    source.write_text("".join(json.dumps(row) + "\n" for row in data))
    dataset = tmp_path / "frozen.json"
    assert (
        main(
            [
                "calibrate",
                "freeze",
                "--data",
                str(source),
                "--name",
                "demo",
                "--version",
                "v1",
                "--question",
                "Correct?",
                "--out",
                str(dataset),
            ]
        )
        == 0
    )
    pin = json.loads(capsys.readouterr().out)["sha256"]
    protocol = {
        "schema": PROTOCOL_SCHEMA,
        "dataset_sha256": pin,
        "repeats": 2,
        "resamples": 100,
        "candidates": [
            {
                "name": "exact",
                "evaluator": {"ref": "exact-match"},
                "metric": "exact-match",
                "thresholds": [0.5],
                "pass_when": "gte",
            }
        ],
    }
    spec = tmp_path / "protocol.json"
    spec.write_text(json.dumps(protocol))
    report = tmp_path / "report.json"
    run = ["calibrate", "run", "--dataset", str(dataset), "-f", str(spec), "--out", str(report)]
    assert main(run) == 0
    report_pin = json.loads(capsys.readouterr().out)["sha256"]
    assert main(["calibrate", "verify", str(report), "--sha256", report_pin]) == 0
    html = tmp_path / "report.html"
    assert main(["calibrate", "report", str(report), "--out", str(html)]) == 0
    text = html.read_text()
    assert "<script>bad()</script>" not in text
    assert "&lt;script&gt;bad()&lt;/script&gt;" in text
    assert "Content-Security-Policy" in text
    comparison = tmp_path / "comparison.json"
    assert main(["calibrate", "compare", str(report), str(report), "--out", str(comparison)]) == 0
    assert read_artifact(comparison)[0]["candidates"][0]["accuracy_delta"]["value"] == 0
    assert main(run) == 1  # Output replacement refused before another run.
    assert main(["calibrate", "verify", str(report), "--sha256", "0" * 64]) == 1


def test_offline_example_runs_with_negative_pin_and_drift_controls(tmp_path: Path) -> None:
    root = Path(__file__).resolve().parents[3]
    directory = tmp_path / "demo"
    command = [
        sys.executable,
        str(root / "examples/judge-calibration/run.py"),
        "--out",
        str(directory),
    ]
    run = subprocess.run(command, text=True, capture_output=True, check=True, timeout=30)
    result = json.loads(run.stdout)
    assert result["selected_on_tuning"] == "fuzzy"
    assert result["negative_pin_control"] == "passed"
    report, _ = read_artifact(directory / "report.json", expected=result["report_sha256"])
    assert report["candidates"][1]["held-out"]["cases"] == 4
    assert report["candidates"][1]["held-out"]["unlabelled_cases"] == 1
    assert (directory / "report.html").is_file()
    drift, _ = read_artifact(directory / "judge-drift.json")
    assert drift["fixed_target_outputs"]
    assert drift["candidates"][1]["threshold_changed"]
    assert (
        subprocess.run(command, text=True, capture_output=True, timeout=30, check=False).returncode
        != 0
    )

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main
from evalsi.determinism import determinism_report, recommended_trials


def row(record: str, trial: int, passed: bool) -> dict[str, Any]:
    return {
        "record_id": record,
        "trial": trial,
        "evaluator": "quality",
        "outcome": "scored",
        "scores": [{"passed": passed}],
    }


def test_report_keeps_records_and_trials_separate() -> None:
    data = {
        "results": [
            row("stable", 0, True),
            row("stable", 1, True),
            row("stable", 2, True),
            row("flaky", 0, True),
            row("flaky", 1, False),
            row("single", 0, True),
            {"record_id": "single", "trial": 1, "evaluator": "quality", "outcome": "error"},
        ]
    }
    report = determinism_report(data)
    metric = report["metrics"][0]
    assert metric["repeated_records"] == 2
    assert metric["insufficient_records"] == 1
    assert metric["observed_constant_records"] == 1
    assert metric["mean_pair_agreement"] == 0.5
    assert metric["mean_within_record_variance"] == 0.25
    assert report["outcomes"] == {"scored": 6, "error": 1}
    assert report["trial_recommendation"]["trials"] == 738


def test_single_trials_do_not_establish_stability() -> None:
    metric = determinism_report({"results": [row("one", 0, True)]})["metrics"][0]
    assert metric["mean_pair_agreement"] is None
    assert metric["mean_within_record_variance"] is None
    assert metric["observed_constant_records"] == 0


def test_duplicate_trials_are_refused() -> None:
    with pytest.raises(ValueError, match="duplicate trial"):
        determinism_report({"results": [row("one", 0, True), row("one", 0, False)]})


@pytest.mark.parametrize("value", [0, 1, -0.1, math.nan, math.inf])
def test_recommendation_validates_probabilities(value: float) -> None:
    with pytest.raises(ValueError, match="margin"):
        recommended_trials(margin=value)
    with pytest.raises(ValueError, match="confidence"):
        recommended_trials(confidence=value)


def test_determinism_cli_reads_real_artifact_shape(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    path = tmp_path / "results.json"
    path.write_text(json.dumps({"results": [row("one", 0, True), row("one", 1, False)]}))
    assert main(["determinism", str(path), "--margin", "0.1"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["metrics"][0]["mean_pair_agreement"] == 0
    assert report["trial_recommendation"]["trials"] == 185


def test_structured_scores_are_counted_and_skipped() -> None:
    result = row("one", 0, True)
    result["scores"].append({"name": "detail", "structured": {"answer": [1, 2]}})
    report = determinism_report({"results": [result, row("one", 1, False)]})
    assert report["structured_scores_skipped"] == 1
    assert report["metrics"][0]["mean_pair_agreement"] == 0
    result["scores"][1]["passed"] = True
    with pytest.raises(ValueError, match="one passed"):
        determinism_report({"results": [result]})

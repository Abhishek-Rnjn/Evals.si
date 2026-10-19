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


# --- trials from the variance observed per task ---------------------------------------


def numeric_row(record: str, trial: int, value: float) -> dict[str, Any]:
    return {
        "record_id": record,
        "trial": trial,
        "evaluator": "score",
        "outcome": "scored",
        "scores": [{"number": value}],
    }


def test_the_chi_square_quantile_matches_reference_values() -> None:
    from evalsi.determinism import _chi2_lower

    # Standard 5% points of the chi-square distribution.
    table = {1: 0.003932, 2: 0.102587, 3: 0.351846, 5: 1.145476, 10: 3.940299, 100: 77.9295}
    for df, expected in table.items():
        assert _chi2_lower(0.05, df) == pytest.approx(expected, rel=1e-4)
    assert _chi2_lower(0.5, 4) == pytest.approx(3.356694, rel=1e-5)


def test_trials_for_margin_is_the_normal_sample_size() -> None:
    from evalsi.determinism import trials_for_margin

    assert trials_for_margin(0.25, 0.05, 0.95) == 385
    assert trials_for_margin(0.25, 0.10, 0.95) == 97
    assert trials_for_margin(0.0, 0.10, 0.95) == 1  # never fewer than one


def flaky_pass_fail(tasks: int, trials: int, fails: int) -> dict[str, Any]:
    rows = []
    for t in range(tasks):
        for k in range(trials):
            rows.append(row(f"task{t}", k, k >= fails))  # `fails` failures then passes
    return {"results": rows}


def test_the_pass_fail_bound_is_the_worst_case_with_few_trials() -> None:
    from evalsi.determinism import trial_advice

    (m,) = trial_advice(flaky_pass_fail(tasks=20, trials=4, fails=1))["metrics"]
    assert m["tasks_with_repeats"] == 20
    assert m["observed_variance"] == pytest.approx(0.25)
    assert m["variance_bound"] == 0.25  # 1 of 4 cannot rule out a coin flip
    needed = m["trials_for_margin"]
    assert needed["0.05"] > needed["0.1"] > needed["0.2"] >= 1


def test_the_bernoulli_bound_has_exact_coverage() -> None:
    """The reason it is not a chi-square bound: for a task with p = 0.1 and 100 trials, the
    chi-square bound covered the true variance only ~79% of the time at a nominal 95%."""
    from math import comb

    from evalsi.determinism import bernoulli_variance_bound

    n, p, confidence = 100, 0.1, 0.95
    true_variance = p * (1 - p)
    covered = sum(
        comb(n, k) * p**k * (1 - p) ** (n - k)
        for k in range(n + 1)
        if bernoulli_variance_bound(k, n, confidence) >= true_variance
    )
    assert covered >= confidence


def test_the_advice_follows_the_noisiest_task_not_the_average() -> None:
    from evalsi.determinism import trial_advice

    # 999 tasks that always pass and one that alternates: pooling would say ~no variance.
    rows = [row(f"easy{t}", k, True) for t in range(999) for k in range(4)]
    rows += [row("hard", k, k % 2 == 0) for k in range(4)]
    (m,) = trial_advice({"results": rows})["metrics"]
    assert m["variance_bound"] == 0.25
    assert m["trials_for_margin"]["0.1"] == 97


def test_few_trials_give_a_wider_bound_than_many_for_the_same_scores() -> None:
    from evalsi.determinism import trial_advice

    def numeric(trials: int) -> dict[str, Any]:
        # One task alternating 0.0 and 0.4.
        return {"results": [numeric_row("t", k, 0.4 * (k % 2)) for k in range(trials)]}

    few = trial_advice(numeric(2))["metrics"][0]
    many = trial_advice(numeric(40))["metrics"][0]
    assert few["observed_variance"] == pytest.approx(0.08)
    assert many["observed_variance"] == pytest.approx(0.041, abs=1e-3)
    assert few["variance_bound"] > many["variance_bound"] > many["observed_variance"]


def test_no_variation_is_not_evidence_of_none() -> None:
    from evalsi.determinism import trial_advice

    # Every task passed in every trial.
    (passfail,) = trial_advice(flaky_pass_fail(tasks=10, trials=3, fails=0))["metrics"]
    assert passfail["observed_variance"] == 0
    assert passfail["variance_bound"] == 0.25  # 3 passes of 3 still allow p = 0.5
    assert passfail["trials_for_margin"]["0.05"] == 385
    # A numeric metric that never varied gets no recommendation at all.
    data = {"results": [numeric_row(f"t{i}", k, 0.5) for i in range(5) for k in range(3)]}
    (numeric,) = trial_advice(data)["metrics"]
    assert numeric["variance_bound"] is None
    assert numeric["trials_for_margin"] == {}


def test_tasks_with_a_single_trial_give_no_advice_and_bad_inputs_are_refused() -> None:
    from evalsi.determinism import trial_advice

    assert trial_advice({"results": [row("a", 0, True), row("b", 0, False)]})["metrics"] == []
    with pytest.raises(ValueError, match="margin"):
        trial_advice(flaky_pass_fail(2, 3, 1), margins=(0.0,))
    with pytest.raises(ValueError, match="confidence"):
        trial_advice(flaky_pass_fail(2, 3, 1), confidence=1.0)


def test_the_determinism_report_carries_the_variance_aware_advice() -> None:
    report = determinism_report(flaky_pass_fail(tasks=5, trials=3, fails=1))
    advice = report["variance_aware_recommendation"]
    assert advice["metrics"][0]["metric"] == report["metrics"][0]["metric"]
    # The fixed conservative count is still there, unchanged.
    assert report["trial_recommendation"]["method"] == "hoeffding"

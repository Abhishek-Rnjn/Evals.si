"""ml-classic and ml-monitoring, through evaluate(). Expected values match
scikit-learn and SciPy on the same inputs."""

from __future__ import annotations

import json
import math
import random
from pathlib import Path
from typing import Any

import pytest

from evalsi.packs.ml_classic import average_precision, roc_auc
from evalsi.packs.ml_monitoring import ks_2samp
from evalsi.runner import evaluate


def means(records: list[dict[str, Any]], evaluator: str, **params: Any) -> dict[str, float]:
    result = evaluate(records, [{"ref": evaluator, "params": params}], cache=False)
    return {s.metric.split(".", 1)[-1]: s.mean for s in result.summaries if s.mean is not None}


def scores(records: list[dict[str, Any]], evaluator: str, **params: Any) -> dict[str, Any]:
    result = evaluate(records, [{"ref": evaluator, "params": params}], cache=False)
    [r] = result.results
    return {s.name: s for s in r.scores}


def test_classification() -> None:
    truth, pred = ["a", "a", "b", "b", "c"], ["a", "b", "b", "b", "a"]
    records = [{"output": p, "reference": t} for t, p in zip(truth, pred, strict=True)]
    got = means(records, "classification", positive_label="b")
    assert got["accuracy"] == pytest.approx(0.6)
    # sklearn.metrics.f1_score(truth, pred, average="macro") == 0.4333...
    assert got["f1-macro"] == pytest.approx((0.5 + 0.8 + 0.0) / 3)
    assert got["f1-micro"] == pytest.approx(0.6)
    assert got["precision"] == pytest.approx(2 / 3)
    assert got["recall"] == pytest.approx(1.0)
    detail = scores(records, "classification")["accuracy"].metadata
    assert detail["confusion"]["a"] == {"a": 1, "b": 1}
    assert detail["per_label"]["c"]["support"] == 1


def test_labels_from_json_outputs() -> None:
    records = [
        {"output": {"json": {"label": 1, "score": 0.9}}, "reference": "1"},
        {"output": {"json": {"label": 0, "score": 0.2}}, "reference": "0"},
    ]
    assert means(records, "classification")["accuracy"] == 1.0


def test_roc_auc_pr_auc_log_loss() -> None:
    y, s = ["0", "0", "1", "1"], [0.1, 0.4, 0.35, 0.8]
    records = [
        {"output": "x", "reference": t, "metadata": {"score": p}} for t, p in zip(y, s, strict=True)
    ]
    got = means(records, "roc-auc", positive_label="1")
    assert got["roc-auc"] == pytest.approx(0.75)  # sklearn's documented example
    assert got["pr-auc"] == pytest.approx(0.8333333333)
    expected_loss = -(math.log(0.9) + math.log(0.6) + math.log(0.35) + math.log(0.8)) / 4
    assert got["log-loss"] == pytest.approx(expected_loss)
    # A single score per record needs to know which label it is the probability of.
    [unsure] = evaluate(records, [{"ref": "roc-auc"}], cache=False).results
    assert unsure.outcome.value == "error"
    assert "positive_label" in unsure.reason


def test_multiclass_auc() -> None:
    records = [
        {"output": "x", "reference": "a", "metadata": {"score": {"a": 0.7, "b": 0.2, "c": 0.1}}},
        {"output": "x", "reference": "b", "metadata": {"score": {"a": 0.2, "b": 0.6, "c": 0.2}}},
        {"output": "x", "reference": "c", "metadata": {"score": {"a": 0.1, "b": 0.3, "c": 0.6}}},
        {"output": "x", "reference": "a", "metadata": {"score": {"a": 0.4, "b": 0.4, "c": 0.2}}},
    ]
    got = means(records, "roc-auc")
    assert got["roc-auc"] == pytest.approx(1.0)
    assert got["log-loss"] == pytest.approx(
        -(math.log(0.7) + math.log(0.6) + math.log(0.6) + math.log(0.4)) / 4
    )


def test_auc_with_ties_matches_the_rank_statistic() -> None:
    rng = random.Random(0)
    for _ in range(50):
        y = [rng.random() < 0.5 for _ in range(30)]
        if all(y) or not any(y):
            continue
        s = [round(rng.random(), 1) for _ in range(30)]
        brute = sum(
            1.0 if a > b else 0.5 if a == b else 0.0
            for a, ya in zip(s, y, strict=True)
            if ya
            for b, yb in zip(s, y, strict=True)
            if not yb
        ) / (sum(y) * (len(y) - sum(y)))
        assert roc_auc(s, y) == pytest.approx(brute)
        ap = average_precision(s, y)
        assert ap is not None
        assert 0 < ap <= 1


def test_classifier_calibration() -> None:
    records = [
        {"output": "x", "reference": "1" if i < 7 else "0", "metadata": {"score": 0.7}}
        for i in range(10)
    ]
    got = means(records, "classifier-calibration", positive_label="1")
    assert got["ece"] == pytest.approx(0.0)
    assert got["brier"] == pytest.approx(0.7 * 0.09 + 0.3 * 0.49)


def test_regression() -> None:
    y, p = [3, -0.5, 2, 7], [2.5, 0.0, 2, 8]
    records = [{"output": str(b), "reference": str(a)} for a, b in zip(y, p, strict=True)]
    got = means(records, "regression")
    assert got["mae"] == pytest.approx(0.5)
    assert got["mse"] == pytest.approx(0.375)
    assert got["rmse"] == pytest.approx(math.sqrt(0.375))
    assert got["r2"] == pytest.approx(0.9486081370449679)  # sklearn's documented example
    assert got["mape"] == pytest.approx((0.5 / 3 + 1.0 + 0 + 1 / 7) / 4)


def test_ranking() -> None:
    records: list[dict[str, Any]] = [
        {"output": ["d1", "d2", "d3"], "reference": ["d2"]},
        {"output": "d3\nd1", "reference": {"json": {"d1": 3, "d9": 1}}},
    ]
    result = evaluate(records, [{"ref": "ranking", "params": {"k": 2}}], cache=False)
    first = {s.name: s.number for s in result.results[0].scores}
    assert first["mrr"] == pytest.approx(0.5)
    assert first["map"] == pytest.approx(0.5)
    assert first["ndcg"] == pytest.approx(1 / math.log2(3))
    assert first["recall-at-k"] == 1.0
    assert first["precision-at-k"] == 0.5
    second = {s.name: s.number for s in result.results[1].scores}
    # graded: DCG = (2^3-1)/log2(3); ideal = 7 + (2^1-1)/log2(3)
    assert second["ndcg"] == pytest.approx((7 / math.log2(3)) / (7 + 1 / math.log2(3)))
    assert second["recall-at-k"] == 0.5


def test_drift(tmp_path: Path) -> None:
    rng = random.Random(1)
    same = [{"metadata": {"window": "reference", "age": rng.gauss(40, 10)}} for _ in range(400)]
    same += [{"metadata": {"window": "current", "age": rng.gauss(40, 10)}} for _ in range(400)]
    got = means(same, "drift", feature="age")
    assert got["psi"] < 0.1
    assert got["ks-pvalue"] > 0.01
    assert got["drifted"] == 0.0

    shifted = [r for r in same if r["metadata"]["window"] == "reference"]
    shifted += [{"metadata": {"window": "current", "age": rng.gauss(55, 10)}} for _ in range(400)]
    got = means(shifted, "drift", feature="metadata.age")
    assert got["psi"] > 0.2
    assert got["ks"] > 0.4
    assert got["drifted"] == 1.0

    # Categorical, against a reference file.
    ref = tmp_path / "ref.jsonl"
    ref.write_text("\n".join(json.dumps({"metadata": {"country": c}}) for c in "AAAABBBBCC"))
    current = [{"metadata": {"country": c}} for c in "AAAAAAAAAB"]
    got = means(current, "drift", feature="country", reference_data=str(ref))
    assert "ks" not in got
    assert got["js-divergence"] > 0.1


def test_ks_matches_scipy_on_a_known_case() -> None:
    a = [0.61, 0.29, 0.06, 0.59, -1.73, -0.74, 0.51, -0.56, 0.39, 1.64, 0.05, -0.06]
    b = [-0.25, 0.47, -0.62, 1.46, 0.01, -0.53, 0.35, 0.48, -1.1, 1.24]
    d, _ = ks_2samp(a, b)
    assert d == pytest.approx(0.16666666666666669)  # scipy.stats.ks_2samp(a, b).statistic


def test_identical_windows_do_not_drift(tmp_path: Path) -> None:
    assert ks_2samp([1.0, 2.0, 3.0], [1.0, 2.0, 3.0]) == (0.0, 1.0)
    values = [float(v) for v in range(50)]
    rows = [{"metadata": {"window": w, "x": v}} for w in ("reference", "current") for v in values]
    got = means(rows, "drift", feature="x")
    assert got["ks"] == 0.0
    assert got["ks-pvalue"] == 1.0
    assert got["drifted"] == 0.0
    # A reference file reads content the way records are read: {"json": 3} is 3.
    ref = tmp_path / "ref.jsonl"
    ref.write_text("\n".join(json.dumps({"output": {"json": v}}) for v in range(30)))
    got = means(
        [{"output": {"json": v}} for v in range(30)],
        "drift",
        feature="output",
        reference_data=str(ref),
    )
    assert got["ks"] == 0.0
    assert got["drifted"] == 0.0


def test_data_quality_keeps_metadata_types() -> None:
    records = [{"input": "a", "metadata": {"zip": "00123", "age": "30"}}]
    got = means(
        records,
        "data-quality",
        schema={"zip": {"type": "string", "max_length": 5}, "age": "number"},
    )
    assert got["invalid-rate"] == pytest.approx(1 / 2)  # age is stored as a string


def test_data_quality() -> None:
    records = [
        {"input": "q1", "metadata": {"age": 30, "country": "DE"}},
        {"input": "q2", "metadata": {"age": -4, "country": "DE"}},
        {"input": "q3", "metadata": {"country": "XX"}},
        {"input": "q1", "metadata": {"age": "old", "country": "FR"}},
    ]
    schema = {
        "age": {"type": "number", "min": 0, "max": 130},
        "country": {"allowed": ["DE", "FR"]},
        "nickname": {"type": "string", "required": False},
    }
    got = means(records, "data-quality", schema=schema)
    assert got["missing-rate"] == pytest.approx(1 / 8)
    assert got["invalid-rate"] == pytest.approx(3 / 7)
    assert got["duplicate-rate"] == pytest.approx(1 / 4)


def test_fairness() -> None:
    records = []
    # Group x: selection 3/4, TPR 1, FPR 1/2. Group y: selection 1/4, TPR 1/2, FPR 0.
    for g, truth, pred in [
        ("x", "1", "1"),
        ("x", "1", "1"),
        ("x", "0", "1"),
        ("x", "0", "0"),
        ("y", "1", "1"),
        ("y", "1", "0"),
        ("y", "0", "0"),
        ("y", "0", "0"),
    ]:
        records.append({"output": pred, "reference": truth, "metadata": {"group": g}})
    got = means(records, "fairness", group_field="group", positive_label="1")
    assert got["demographic-parity-difference"] == pytest.approx(0.5)
    assert got["demographic-parity-ratio"] == pytest.approx(1 / 3)
    assert got["equal-opportunity-difference"] == pytest.approx(0.5)
    assert got["equalized-odds-difference"] == pytest.approx(0.5)


def test_packs_are_opt_in() -> None:
    from evalsi.registry import default_registry

    registry = default_registry()
    for name in ("ml-classic", "ml-monitoring"):
        assert not registry.packs[name].on_by_default

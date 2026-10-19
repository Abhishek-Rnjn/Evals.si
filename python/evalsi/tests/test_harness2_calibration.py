"""Probe-signal calibration over a synthetic Harness² namespace (written from
the Harness² source code, not from a recorded run; see the fixture README in
python/evalsi-harness/tests/fixtures/harness2)."""

from __future__ import annotations

import json
import math
import shutil
from pathlib import Path
from typing import Any

import pytest

from evalsi.harness2_calibration import (
    CandidateKey,
    agreement_with_base,
    agreement_with_scores,
    label_agreement,
    load_candidate_scores,
    read_namespace,
)

FIXTURE = (
    Path(__file__).resolve().parents[2]
    / "evalsi-harness"
    / "tests"
    / "fixtures"
    / "harness2"
    / "ns"
    / "jb-oc-agg2"
)

# Our scores for the candidates. By hand, against what Harness² chose:
#   alpha/one agg2: chose agg2 (0.8), best agg1 (0.9)  -> disagree
#   alpha/one seq2: chose c1 (0.5), best c1            -> agree
#   beta agg2:      chose agg1 (0.75), best agg1       -> agree
#   beta seq2:      chose c2, which we could not score -> ungraded
#   gamma agg2:     both candidates tie at 0.2         -> ungraded
SCORES: dict[CandidateKey, float] = {
    ("alpha/one", "agg", 2, "agg1"): 0.9,
    ("alpha/one", "agg", 2, "agg2"): 0.8,
    ("alpha/one", "seq", 2, "c1"): 0.5,
    ("alpha/one", "seq", 2, "c2"): 0.3,
    ("beta", "agg", 2, "agg1"): 0.75,
    ("beta", "agg", 2, "agg2"): 0.5,
    ("beta", "seq", 2, "c1"): 0.6,
    ("gamma", "agg", 2, "agg1"): 0.2,
    ("gamma", "agg", 2, "agg2"): 0.2,
}


def wilson_by_hand(successes: int, n: int, z: float = 1.959963984540054) -> tuple[float, float]:
    p = successes / n
    centre = (p + z * z / (2 * n)) / (1 + z * z / n)
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / (1 + z * z / n)
    return centre - half, centre + half


def test_the_namespace_is_read_with_legacy_keys() -> None:
    ns = read_namespace(FIXTURE)
    assert {(s.task_id, s.mode, s.k, s.chosen_label) for s in ns.selections} == {
        ("alpha/one", "agg", 2, "agg2"),
        ("alpha/one", "seq", 2, "c1"),
        ("beta", "agg", 2, "agg1"),
        ("beta", "seq", 2, "c2"),
        ("gamma", "agg", 2, "agg1"),  # a legacy `arm` selection and `arms` summary
    }
    gamma = next(j for j in ns.judged if j.task_id == "gamma")
    assert (gamma.mode, gamma.frac) == ("agg", 0.6)
    beta_seq = next(j for j in ns.judged if j.task_id == "beta" and j.mode == "seq")
    assert beta_seq.frac is None
    assert not beta_seq.graded


def test_agreement_with_our_scores_by_hand() -> None:
    cells = {a.cell: a for a in agreement_with_scores(read_namespace(FIXTURE), SCORES)}
    agg, seq, pooled = cells["agg2"], cells["seq2"], cells["all"]
    assert (agg.compared, agg.agreed, agg.rate) == (2, 1, 0.5)
    assert agg.disagreements == ["alpha/one agg2: chose agg2, best agg1"]
    assert [u.task_id for u in agg.ungraded] == ["gamma"]
    assert "tie" in agg.ungraded[0].reason
    assert (seq.compared, seq.agreed, seq.rate) == (1, 1, 1.0)
    assert [u.task_id for u in seq.ungraded] == ["beta"]
    assert "no score of ours" in seq.ungraded[0].reason
    assert (pooled.compared, pooled.agreed) == (3, 2)
    assert pooled.rate == pytest.approx(2 / 3)
    low, high = wilson_by_hand(2, 3)
    assert pooled.low == pytest.approx(low)
    assert pooled.high == pytest.approx(high)
    assert pooled.interval_method == "wilson"
    assert len(pooled.ungraded) == 2


def test_a_lower_is_better_metric_ranks_the_lowest_score_best() -> None:
    ns = read_namespace(FIXTURE)
    higher = {a.cell: a for a in agreement_with_scores(ns, SCORES)}
    lower = {a.cell: a for a in agreement_with_scores(ns, SCORES, higher_is_better=False)}
    # alpha/one agg2 chose agg2 where agg1 scored higher: with the direction flipped,
    # agg2 is the best (lowest) choice and the disagreement disappears.
    assert higher["agg2"].disagreements == ["alpha/one agg2: chose agg2, best agg1"]
    assert lower["agg2"].disagreements == ["beta agg2: chose agg1, best agg2"]  # and beta flips


def test_the_metric_direction_comes_from_the_results_summary() -> None:
    from evalsi.harness2_calibration import metric_higher_is_better

    results = {
        "summaries": [
            {"metric": "contamination-fetch", "higher_is_better": False},
            {"metric": "quality", "higher_is_better": True},
            {"metric": "plain"},
        ]
    }
    assert not metric_higher_is_better(results, "contamination-fetch")
    assert metric_higher_is_better(results, "quality")
    assert metric_higher_is_better(results, "plain")
    assert metric_higher_is_better(results, "absent")


def test_a_forced_choice_and_an_incomplete_selection_are_not_compared(tmp_path: Path) -> None:
    root = tmp_path / "ns"
    shutil.copytree(FIXTURE, root)
    summary = root / "beta" / "summary.json"
    data = json.loads(summary.read_text())
    data["selections"]["agg"]["ok"] = False
    summary.write_text(json.dumps(data))
    ns = read_namespace(root)
    cells = {a.cell: a for a in agreement_with_scores(ns, SCORES)}
    assert [u.reason.split(" ")[0] for u in cells["agg2"].ungraded if u.task_id == "beta"] == [
        "selection"
    ]
    # beta seq2 has one scored candidate: the choice is forced.
    only_one = {k: v for k, v in SCORES.items() if k[3] != "c2"}
    cells = {a.cell: a for a in agreement_with_scores(ns, only_one)}
    assert any("fewer than two" in u.reason for u in cells["seq2"].ungraded)


def test_nothing_to_compare_gives_no_rate() -> None:
    ns = read_namespace(FIXTURE)
    pooled = agreement_with_scores(ns, {})[-1]
    assert (pooled.compared, pooled.rate, pooled.low, pooled.high) == (0, None, None, None)
    assert len(pooled.ungraded) == 5


def test_scores_are_read_from_a_results_file() -> None:
    def rec(
        task: str, mode: str, label: str, value: float | None
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        rid = f"{task}|{mode}|{label}"
        record = {
            "id": rid,
            "metadata": {"harness2": {"task_id": task, "mode": mode, "k": 2, "label": label}},
        }
        result: dict[str, Any] = {
            "record_id": rid,
            "evaluator": "quality",
            "outcome": "scored" if value is not None else "error",
            "scores": [{"name": "quality", "number": value}] if value is not None else [],
        }
        return record, result

    pairs = [
        rec("beta", "agg", "agg1", 0.75),
        rec("beta", "agg", "agg2", 0.5),
        rec("beta", "agg", "agg3", None),  # the evaluator failed: unscored, not zero
    ]
    results = {
        "summaries": [{"metric": "quality", "mean": 0.6}],
        "records": [p[0] for p in pairs] + [{"id": "x", "metadata": {}}],
        "results": [p[1] for p in pairs],
    }
    assert load_candidate_scores(results, "quality") == {
        ("beta", "agg", 2, "agg1"): 0.75,
        ("beta", "agg", 2, "agg2"): 0.5,
    }
    assert load_candidate_scores(results, "other") == {}


def test_the_judged_only_reading_compares_with_base() -> None:
    cells = {a.cell: a for a in agreement_with_base(read_namespace(FIXTURE))}
    # Base means: alpha/one 0.5, beta 0.5 (base2 not graded), gamma 0.3.
    # agg2: 0.8>=0.5, 0.75>=0.5, 0.6>=0.3 -> 3 of 3. seq2: alpha 0.5>=0.5 ok; beta ungraded.
    assert (cells["agg2"].compared, cells["agg2"].agreed) == (3, 3)
    assert (cells["seq2"].compared, cells["seq2"].agreed) == (1, 1)
    assert [u.task_id for u in cells["seq2"].ungraded] == ["beta"]
    assert cells["seq2"].ungraded[0].reason == "judge returned nothing"
    assert (cells["all"].compared, cells["all"].agreed) == (4, 4)


def test_the_directory_must_exist_and_may_be_empty(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError):
        read_namespace(tmp_path / "nope")
    empty = tmp_path / "empty"
    empty.mkdir()
    ns = read_namespace(empty)
    assert (ns.selections, ns.judged, ns.base) == ([], [], [])
    assert ns.notes == ["no <task>/summary.json found"]


def test_a_symlink_out_of_the_root_is_refused(tmp_path: Path) -> None:
    root = tmp_path / "ns"
    shutil.copytree(FIXTURE, root)
    outside = tmp_path / "outside.json"
    outside.write_text(json.dumps([{"task_id": "evil", "mode": "agg", "k": 1, "n_passed": 1}]))
    (root / "judged.json").unlink()
    (root / "judged.json").symlink_to(outside)
    ns = read_namespace(root)
    assert ns.judged == []
    assert any("judged.json: refused" in n for n in ns.notes)


def test_label_agreement_by_hand() -> None:
    # 10 items: judge and human agree on 8. Judge says pass 6 times, human 5 times.
    judge = ["pass"] * 6 + ["fail"] * 4
    human = ["pass"] * 5 + ["fail"] + ["pass"] + ["fail"] * 3
    got = label_agreement(judge, human)
    assert (got.n, got.agreed, got.raw_agreement) == (10, 8, 0.8)
    # Chance agreement: 0.6*0.6 + 0.4*0.4 = 0.52; kappa = (0.8-0.52)/(1-0.52).
    assert got.expected_agreement == pytest.approx(0.52)
    assert got.kappa == pytest.approx(0.28 / 0.48)
    low, high = wilson_by_hand(8, 10)
    assert got.low == pytest.approx(low)
    assert got.high == pytest.approx(high)


def test_label_agreement_edge_cases() -> None:
    assert label_agreement(["a", "b"], ["a", "b"]).kappa == 1.0
    assert label_agreement(["a", "b"], ["b", "a"]).kappa == -1.0
    # One label throughout: chance agreement is 1, so kappa is undefined, not 1.
    flat = label_agreement(["pass"] * 3, ["pass"] * 3)
    assert flat.kappa is None
    assert flat.raw_agreement == 1.0
    with pytest.raises(ValueError, match="differ in length"):
        label_agreement(["a"], ["a", "b"])
    with pytest.raises(ValueError, match="no labels"):
        label_agreement([], [])

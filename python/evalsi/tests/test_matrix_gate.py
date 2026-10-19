"""The regression gate of `evalsi matrix --gate` (H7)."""

from __future__ import annotations

import json
from collections.abc import Sequence
from pathlib import Path
from typing import Any

import pytest
from test_matrix import results

from evalsi import matrix
from evalsi.cli import main

BASE_SCORES = [1.0, 1, 1, 1, 0, 1, 1, 1, 1, 1, 1, 1]
BASE = results("base@1", BASE_SCORES, tokens=1000, steps=5)
WORSE = results("worse@1", [0.0, 0, 1, 0, 0, 0, 1, 0, 0, 1, 0, 0], tokens=1000, steps=5)
BETTER = results("better@1", [1.0] * 12, tokens=1000, steps=5)
M = "task-success"


def verdicts(
    *named: tuple[str, dict[str, Any]],
    tuned_on: Sequence[str] = (),
    max_regression: float = 0.0,
) -> list[matrix.Verdict]:
    cells = matrix.build(list(named), M, tuned_on=tuned_on)
    return matrix.gate(list(named), cells, M, max_regression=max_regression)


def test_a_deliberately_worse_harness_fails_and_a_better_one_passes() -> None:
    v = verdicts(("base", BASE), ("worse", WORSE), ("better", BETTER))
    assert [x.status for x in v] == ["baseline", "fail", "pass"]
    assert matrix.gate_failed(v)
    assert not matrix.gate_failed(v[:1] + v[2:])
    assert "worse by more than 0" in v[1].checks[0].detail


def test_the_tolerance_lets_a_small_regression_through() -> None:
    slight = results("slight@1", [1.0, 1, 1, 1, 0, 1, 1, 1, 1, 1, 0, 0], tokens=1000, steps=5)
    assert verdicts(("base", BASE), ("s", slight), max_regression=0.0)[1].status != "fail"
    # A whole interval below -0.5 is needed to fail with that tolerance.
    assert verdicts(("base", BASE), ("w", WORSE), max_regression=0.9)[1].status == "pass"
    with pytest.raises(ValueError, match="zero or positive"):
        verdicts(("base", BASE), max_regression=-1)


def test_no_pairs_is_insufficient_not_a_pass_and_strict_fails_closed() -> None:
    other = results("o@1", [0.0, 0, 0], tokens=1, steps=1, dataset="e" * 64)
    one = results("one@1", [0.0], tokens=1, steps=1)
    v = verdicts(("base", BASE), ("other", other), ("one", one))
    assert [x.status for x in v] == ["baseline", "insufficient", "insufficient"]
    assert not matrix.gate_failed(v)
    assert matrix.gate_failed(v, strict=True)
    assert "insufficient data" in matrix.gate_lines(v) or "INSUFFICIENT" in matrix.gate_lines(v)
    empty = results("e@1", [1.0, 1], tokens=1, steps=1)
    empty["results"] = []
    assert verdicts(("base", BASE), ("empty", empty))[1].status == "insufficient"


def test_a_lower_is_better_metric_is_oriented() -> None:
    def lower(scores: list[float]) -> dict[str, Any]:
        data = results("x@1", scores, tokens=1, steps=1)
        data["summaries"][0]["higher_is_better"] = False
        return data

    low = lower([0.0, 0, 0, 1, 0, 0])
    high = lower([1.0, 1, 1, 1, 1, 0])
    assert verdicts(("a", low), ("b", high))[1].status == "fail"
    assert verdicts(("a", high), ("b", low))[1].status == "pass"


def test_held_out_worse_than_baseline_or_than_tuned_fails() -> None:
    hd = "e" * 64
    base_held = results("base@1", [1.0] * 10, tokens=1, steps=1, dataset=hd)
    tuned = results("cand@1", [1.0] * 12, tokens=1, steps=1)
    held_bad = results("cand@1", [0.0, 1, 0, 0, 1, 0, 0, 0, 1, 0], tokens=1, steps=1, dataset=hd)
    held_ok = results("cand@1", [1.0] * 10, tokens=1, steps=1, dataset=hd)
    names = [("base", BASE), ("base-held", base_held), ("tuned", tuned)]
    v = verdicts(*names, ("held", held_bad), tuned_on=["d" * 64])
    last = v[-1]
    assert last.status == "fail"
    assert {c.name: c.status for c in last.checks} == {
        "held-out-vs-baseline": "fail",
        "held-out-vs-tuned": "fail",
    }
    assert verdicts(*names, ("held", held_ok), tuned_on=["d" * 64])[-1].status == "pass"
    # No baseline run on the held-out data, and no tuned cell: insufficient.
    lone = verdicts(("base", BASE), ("held", held_ok), tuned_on=["d" * 64])
    assert lone[-1].status == "insufficient"


def test_a_nan_or_infinite_tolerance_is_refused_not_a_silent_pass() -> None:
    for bad in (float("nan"), float("inf")):
        with pytest.raises(ValueError, match="finite"):
            verdicts(("base", BASE), ("worse", WORSE), max_regression=bad)


def test_every_baseline_row_is_a_reference_and_strict_passes_complete_data() -> None:
    hd = "e" * 64
    base_held = results("base@1", [1.0] * 10, tokens=1, steps=1, dataset=hd)
    tuned = results("cand@1", [1.0] * 12, tokens=1, steps=1)
    held_ok = results("cand@1", [1.0] * 10, tokens=1, steps=1, dataset=hd)
    v = verdicts(
        ("base", BASE), ("base-held", base_held), ("tuned", tuned), ("held", held_ok),
        tuned_on=["d" * 64],
    )  # fmt: skip
    assert [x.status for x in v] == ["baseline", "baseline", "pass", "pass"]
    assert not matrix.gate_failed(v, strict=True)


def test_references_are_found_wherever_they_are_in_the_inputs() -> None:
    hd = "e" * 64
    base_held = results("base@1", [1.0] * 10, tokens=1, steps=1, dataset=hd)
    tuned = results("cand@1", [1.0] * 12, tokens=1, steps=1)
    held_bad = results("cand@1", [0.0, 1, 0, 0, 1, 0, 0, 0, 1, 0], tokens=1, steps=1, dataset=hd)
    # The held-out run comes before both its baseline and its tuned cell.
    v = verdicts(
        ("base", BASE), ("held", held_bad), ("base-held", base_held), ("tuned", tuned),
        tuned_on=["d" * 64],
    )  # fmt: skip
    held = v[1]
    assert held.status == "fail"
    assert {c.name: c.status for c in held.checks} == {
        "held-out-vs-baseline": "fail",
        "held-out-vs-tuned": "fail",
    }


def test_the_cli_exits_nonzero_for_a_worse_harness_and_carries_the_verdict(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    paths = {}
    for name, data in (("base", BASE), ("worse", WORSE), ("better", BETTER)):
        paths[name] = str(tmp_path / f"{name}.json")
        Path(paths[name]).write_text(json.dumps(data))
    args = ["matrix", paths["base"], paths["better"], "--metric", M, "--gate"]
    assert main(args) == 0
    assert "gate: PASS" in capsys.readouterr().out
    args = ["matrix", paths["base"], paths["worse"], "--metric", M, "--gate", "--format", "json"]
    assert main(args) == 1
    rows = json.loads(capsys.readouterr().out)
    assert rows[1]["gate"]["status"] == "fail"
    assert rows[0]["gate"]["status"] == "baseline"
    assert main(["matrix", paths["base"], paths["worse"], "--metric", M]) == 0  # no gate asked
    assert main(["matrix", paths["base"], "--metric", M, "--max-regression", "0.1"]) == 2
    capsys.readouterr()
    assert main(["matrix", paths["base"], paths["worse"], "--metric", M, "--gate"]) == 1
    assert "gate: FAIL" in capsys.readouterr().out


def test_the_cli_refuses_a_nan_tolerance(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    path = tmp_path / "r.json"
    path.write_text(json.dumps(BASE))
    other = tmp_path / "w.json"
    other.write_text(json.dumps(WORSE))
    args = ["matrix", str(path), str(other), "--metric", M, "--gate", "--max-regression", "nan"]
    assert main(args) == 2
    assert "finite" in capsys.readouterr().err

from __future__ import annotations

from evalsi.cli import summary_from_json


def test_zeros_the_server_leaves_out_are_zeros() -> None:
    # protojson omits zero values: an interval from 0 has no "low", a mean of 0 no "mean".
    summary = summary_from_json(
        {
            "metric": "task-success",
            "kind": "METRIC_KIND_PROPORTION",
            "n": "11",
            "ci": {"high": 0.259, "level": 0.95, "method": "wilson"},
        }
    )
    assert summary.n == 11
    assert summary.mean == 0.0
    assert summary.ci is not None
    assert (summary.ci.low, summary.ci.high, summary.ci.method) == (0.0, 0.259, "wilson")
    assert summary.kind == "proportion"


def test_a_metric_with_nothing_scored_has_no_mean() -> None:
    summary = summary_from_json(
        {"metric": "tool-errors", "kind": "METRIC_KIND_PROPORTION", "skipped": "3"}
    )
    assert summary.n == 0
    assert summary.mean is None
    assert summary.ci is None
    assert summary.skipped == 3

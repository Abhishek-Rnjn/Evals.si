"""Paired comparison of two results files (``evalsi run --output``).

Records are paired by id and trial, so the interval is on the per-record
difference: tighter than comparing two means, and blind to records only one
side has. A change is significant when the interval of the mean difference
excludes zero.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import asdict, dataclass
from typing import Any

from evalsi.stats import t_interval


@dataclass
class MetricComparison:
    metric: str
    baseline_mean: float | None
    candidate_mean: float | None
    paired_n: int = 0
    diff: float | None = None
    diff_low: float | None = None
    diff_high: float | None = None
    significant: bool = False
    # True when the candidate is significantly worse (by higher_is_better).
    regressed: bool = False

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _number(score: Mapping[str, Any]) -> float | None:
    if score.get("number") is not None:
        return float(score["number"])
    if score.get("passed") is not None:
        return 1.0 if score["passed"] else 0.0
    return None


def record_values(data: Mapping[str, Any]) -> dict[str, dict[str, float]]:
    """``{metric: {"record#trial": value}}`` for the metrics a results file summarizes."""
    metrics = {str(s.get("metric", "")) for s in data.get("summaries") or []}
    out: dict[str, dict[str, float]] = {}
    for r in data.get("results") or []:
        outcome = str(r.get("outcome", "")).lower().removeprefix("outcome_")
        rid = str(r.get("record_id", r.get("recordId", "")))
        if outcome != "scored" or not rid:
            continue
        evaluator = str(r.get("evaluator", ""))
        key = f"{rid}#{int(r.get('trial', 0) or 0)}"
        for score in r.get("scores") or []:
            name = str(score.get("name", ""))
            metric = evaluator
            if name and name != evaluator and f"{evaluator}.{name}" in metrics:
                metric = f"{evaluator}.{name}"
            value = _number(score)
            if value is not None and metric in metrics:
                out.setdefault(metric, {})[key] = value
    return out


def compare_results(
    baseline: Mapping[str, Any], candidate: Mapping[str, Any], *, level: float = 0.95
) -> list[MetricComparison]:
    """Every metric both results files summarize, candidate against baseline."""
    base_sum = {s["metric"]: s for s in baseline.get("summaries") or []}
    cand_sum = {s["metric"]: s for s in candidate.get("summaries") or []}
    base_vals, cand_vals = record_values(baseline), record_values(candidate)
    out = []
    for metric in sorted(set(base_sum) & set(cand_sum)):
        c = MetricComparison(
            metric=metric,
            baseline_mean=base_sum[metric].get("mean"),
            candidate_mean=cand_sum[metric].get("mean"),
        )
        a, b = base_vals.get(metric, {}), cand_vals.get(metric, {})
        shared = sorted(set(a) & set(b))
        if shared:
            diffs = [b[k] - a[k] for k in shared]
            c.paired_n = len(diffs)
            c.diff = sum(diffs) / len(diffs)
            interval = t_interval(diffs, level)
            if interval is not None and interval.low == interval.low:  # not NaN
                c.diff_low, c.diff_high = interval.low, interval.high
                c.significant = interval.low > 0 or interval.high < 0
        higher = cand_sum[metric].get("higher_is_better", True) is not False
        if c.significant and c.diff is not None:
            c.regressed = c.diff < 0 if higher else c.diff > 0
        out.append(c)
    return out

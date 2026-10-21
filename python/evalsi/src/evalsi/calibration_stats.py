"""Case-weighted binary judge validity with group-bootstrap uncertainty.

Repeated calls estimate a case's probability of a pass; they are not new cases.
Intervals resample whole independent groups and preserve within-group correlation.
"""

from __future__ import annotations

import random
from collections import defaultdict
from collections.abc import Callable
from statistics import mean
from typing import Any

Observation = tuple[str, bool, float]  # independent group, human verdict, judge pass probability


def rates(rows: list[Observation]) -> dict[str, float | None]:
    n = len(rows)
    if not n:
        return dict.fromkeys(("accuracy", "precision", "recall", "fpr", "fnr", "kappa"))
    tp = sum(p for _, h, p in rows if h)
    fp = sum(p for _, h, p in rows if not h)
    positives = sum(h for _, h, _ in rows)
    negatives = n - positives
    tn = negatives - fp
    accuracy = (tp + tn) / n
    expected = (positives * (tp + fp) + negatives * (n - tp - fp)) / (n * n)
    return {
        "accuracy": accuracy,
        "precision": tp / (tp + fp) if tp + fp else None,
        "recall": tp / positives if positives else None,
        "fpr": fp / negatives if negatives else None,
        "fnr": (positives - tp) / positives if positives else None,
        "kappa": (accuracy - expected) / (1 - expected) if expected < 1 - 1e-12 else None,
    }


def _quantile(values: list[float], probability: float) -> float:
    ordered = sorted(values)
    index = (len(ordered) - 1) * probability
    low = int(index)
    high = min(low + 1, len(ordered) - 1)
    return ordered[low] + (ordered[high] - ordered[low]) * (index - low)


def bootstrap(
    rows: list[Any],
    *,
    group: Callable[[Any], str],
    statistic: Callable[[list[Any]], dict[str, Any]],
    confidence: float,
    resamples: int,
) -> dict[str, Any]:
    point = statistic(rows)
    groups: dict[str, list[Any]] = defaultdict(list)
    for row in rows:
        groups[group(row)].append(row)
    intervals: dict[str, list[float]] = {key: [] for key in point}
    if len(groups) > 1:
        buckets = list(groups.values())
        randomizer = random.Random(0)
        for _ in range(resamples):
            sample = [
                row for bucket in randomizer.choices(buckets, k=len(buckets)) for row in bucket
            ]
            for key, value in statistic(sample).items():
                if value is not None:
                    intervals[key].append(value)
    alpha = (1 - confidence) / 2
    estimates = {}
    for key, value in point.items():
        samples = intervals[key]
        if value is None:
            status = "undefined metric"
        elif len(groups) < 2:
            status = "fewer than two independent groups"
        elif len(samples) < resamples * 0.9:
            status = "too many resamples with undefined denominator"
        elif max(samples) - min(samples) < 1e-12:
            status = "no empirical variation; bootstrap cannot estimate uncertainty"
        else:
            status = "available"
        estimates[key] = {
            "value": value,
            "ci": [_quantile(samples, alpha), _quantile(samples, 1 - alpha)]
            if status == "available"
            else None,
            "ci_status": status,
            "defined_resamples": len(samples),
        }
    return estimates


def validity(rows: list[Observation], *, confidence: float, resamples: int) -> dict[str, Any]:
    return {
        "cases": len(rows),
        "groups": len({row[0] for row in rows}),
        "human_positive": sum(row[1] for row in rows),
        "human_negative": sum(not row[1] for row in rows),
        "expected_confusion": {
            "true_positive": sum(p for _, h, p in rows if h),
            "false_positive": sum(p for _, h, p in rows if not h),
            "true_negative": sum(1 - p for _, h, p in rows if not h),
            "false_negative": sum(1 - p for _, h, p in rows if h),
        },
        "metrics": bootstrap(
            rows,
            group=lambda row: row[0],
            statistic=rates,
            confidence=confidence,
            resamples=resamples,
        ),
    }


def paired_difference(
    before: dict[str, tuple[str, float]],
    after: dict[str, tuple[str, float]],
    *,
    confidence: float,
    resamples: int,
) -> dict[str, Any]:
    ids = sorted(before.keys() & after.keys())
    rows = [(before[key][0], after[key][1] - before[key][1]) for key in ids]
    estimate = bootstrap(
        rows,
        group=lambda row: row[0],
        statistic=lambda sample: {
            "accuracy_delta": mean(row[1] for row in sample) if sample else None
        },
        confidence=confidence,
        resamples=resamples,
    )
    return {
        "paired_cases": len(ids),
        "groups": len({row[0] for row in rows}),
        "unpaired_before": len(before.keys() - after.keys()),
        "unpaired_after": len(after.keys() - before.keys()),
        **estimate,
    }

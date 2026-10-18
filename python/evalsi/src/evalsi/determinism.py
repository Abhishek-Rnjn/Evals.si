"""Describe observed score stability across trials of the same records.

Identical observed scores do not prove deterministic generation. The trial
recommendation is Hoeffding's conservative bound for independent [0, 1]
observations of one fixed case, not a power calculation for model comparisons.
"""

from __future__ import annotations

import math
from collections import Counter, defaultdict
from collections.abc import Mapping
from statistics import fmean, variance
from typing import Any

from evalsi.results import result_metric


def recommended_trials(*, margin: float = 0.05, confidence: float = 0.95) -> int:
    if not math.isfinite(margin) or not 0 < margin < 1:
        raise ValueError("margin must be finite and between 0 and 1")
    if not math.isfinite(confidence) or not 0 < confidence < 1:
        raise ValueError("confidence must be finite and between 0 and 1")
    return math.ceil(math.log(2 / (1 - confidence)) / (2 * margin**2))


def determinism_report(
    data: Mapping[str, Any], *, margin: float = 0.05, confidence: float = 0.95
) -> dict[str, Any]:
    """Read an ``evalsi run --output`` results object, preserving trial identity."""
    count = recommended_trials(margin=margin, confidence=confidence)
    rows = data.get("results")
    if not isinstance(rows, list):
        raise ValueError("results must be an array from an evalsi results file")
    cells: dict[str, dict[str, dict[int, bool | float | str]]] = defaultdict(
        lambda: defaultdict(dict)
    )
    outcomes: Counter[str] = Counter()
    for row in rows:
        if not isinstance(row, Mapping):
            raise ValueError("each result must be an object")
        outcome = str(row.get("outcome", ""))
        outcomes[outcome] += 1
        if outcome != "scored":
            continue
        trial = row.get("trial", 0)
        if isinstance(trial, bool) or not isinstance(trial, int) or trial < 0:
            raise ValueError("trial must be a nonnegative integer")
        record_id = str(row.get("record_id", ""))
        evaluator = str(row.get("evaluator", ""))
        for score in row.get("scores", []):
            fields = [key for key in ("passed", "number", "label") if key in score]
            if len(fields) != 1:
                raise ValueError("each score must have one passed, number or label value")
            field = fields[0]
            value = score[field]
            if field == "number":
                if (
                    isinstance(value, bool)
                    or not isinstance(value, int | float)
                    or not math.isfinite(value)
                ):
                    raise ValueError("numeric scores must be finite numbers")
                value = float(value)
            elif (field == "passed" and not isinstance(value, bool)) or (
                field == "label" and not isinstance(value, str)
            ):
                raise ValueError("passed scores must be booleans and labels must be strings")
            metric = result_metric(
                evaluator, str(row.get("evaluator_ref", "")), str(score.get("name", ""))
            )
            trials = cells[metric][record_id]
            if trial in trials:
                raise ValueError(f"duplicate trial {trial} for {record_id!r}, {metric!r}")
            trials[trial] = value
    metrics = []
    for metric, records in sorted(cells.items()):
        repeated = [list(trials.values()) for trials in records.values() if len(trials) >= 2]
        agreement = []
        variances = []
        for values in repeated:
            n = len(values)
            agreement.append(sum(c * (c - 1) for c in Counter(values).values()) / (n * (n - 1)))
            if all(isinstance(v, bool | float) for v in values):
                variances.append(variance([float(v) for v in values]))
        metrics.append(
            {
                "metric": metric,
                "records": len(records),
                "repeated_records": len(repeated),
                "insufficient_records": len(records) - len(repeated),
                "observed_constant_records": sum(len(set(v)) == 1 for v in repeated),
                "mean_pair_agreement": fmean(agreement) if agreement else None,
                "mean_within_record_variance": fmean(variances) if variances else None,
            }
        )
    return {
        "outcomes": dict(sorted(outcomes.items())),
        "metrics": metrics,
        "trial_recommendation": {
            "method": "hoeffding",
            "trials": count,
            "margin": margin,
            "confidence": confidence,
            "assumptions": (
                "Independent observations in [0, 1] of one fixed case; "
                "not a comparison power analysis."
            ),
        },
        "note": (
            "Observed score stability does not prove deterministic outputs. "
            "Errors and skipped results are not scores."
        ),
    }

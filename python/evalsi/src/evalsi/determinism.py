"""Describe observed score stability across trials of the same records.

Identical observed scores do not prove deterministic generation. The trial
recommendation is Hoeffding's conservative bound for independent [0, 1]
observations of one fixed case, not a power calculation for model comparisons.
"""

from __future__ import annotations

import math
from collections import Counter, defaultdict
from collections.abc import Mapping
from statistics import NormalDist, fmean, variance
from typing import Any

from evalsi.results import result_metric


def recommended_trials(*, margin: float = 0.05, confidence: float = 0.95) -> int:
    if not math.isfinite(margin) or not 0 < margin < 1:
        raise ValueError("margin must be finite and between 0 and 1")
    if not math.isfinite(confidence) or not 0 < confidence < 1:
        raise ValueError("confidence must be finite and between 0 and 1")
    return math.ceil(math.log(2 / (1 - confidence)) / (2 * margin**2))


Cells = dict[str, dict[str, dict[int, bool | float | str]]]


def _collect(data: Mapping[str, Any]) -> tuple[Cells, Counter[str], int]:
    """Scores per metric, record and trial, the outcome counts and the number of
    structured scores skipped, from an ``evalsi run --output`` results object."""
    rows = data.get("results")
    if not isinstance(rows, list):
        raise ValueError("results must be an array from an evalsi results file")
    cells: Cells = defaultdict(lambda: defaultdict(dict))
    outcomes: Counter[str] = Counter()
    structured_skipped = 0
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
            fields = [key for key in ("passed", "number", "label", "structured") if key in score]
            if len(fields) != 1:
                raise ValueError(
                    "each score must have one passed, number, label or structured value"
                )
            field = fields[0]
            if field == "structured":
                structured_skipped += 1
                continue
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
    return cells, outcomes, structured_skipped


def determinism_report(
    data: Mapping[str, Any], *, margin: float = 0.05, confidence: float = 0.95
) -> dict[str, Any]:
    """Read an ``evalsi run --output`` results object, preserving trial identity."""
    count = recommended_trials(margin=margin, confidence=confidence)
    cells, outcomes, structured_skipped = _collect(data)
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
        "structured_scores_skipped": structured_skipped,
        "variance_aware_recommendation": trial_advice(data, confidence=confidence),
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


# --- trials from the variance observed per task -------------------------------------

# A pass/fail score is Bernoulli, whose variance cannot exceed 1/4.
BERNOULLI_MAX_VARIANCE = 0.25
DEFAULT_MARGINS = (0.05, 0.10, 0.20)


def _gamma_p(a: float, x: float) -> float:
    """The regularized lower incomplete gamma function P(a, x)."""
    if x <= 0:
        return 0.0
    log_front = a * math.log(x) - x - math.lgamma(a)
    if x < a + 1:  # the series converges quickly here
        term = total = 1.0 / a
        for n in range(1, 500):
            term *= x / (a + n)
            total += term
            if abs(term) < abs(total) * 1e-15:
                break
        return min(1.0, total * math.exp(log_front))
    # The continued fraction for Q(a, x) (modified Lentz).
    tiny = 1e-300
    b = x + 1.0 - a
    c = 1.0 / tiny
    d = 1.0 / b
    h = d
    for n in range(1, 500):
        an = -n * (n - a)
        b += 2.0
        d = an * d + b
        d = tiny if abs(d) < tiny else d
        c = b + an / c
        c = tiny if abs(c) < tiny else c
        d = 1.0 / d
        delta = d * c
        h *= delta
        if abs(delta - 1.0) < 1e-15:
            break
    return max(0.0, 1.0 - math.exp(log_front) * h)


def _chi2_lower(p: float, df: float) -> float:
    """The p-quantile of a chi-square with ``df`` degrees of freedom, found by bisection
    on its distribution function."""
    low, high = 0.0, max(10.0, 4.0 * df)
    while _gamma_p(df / 2.0, high / 2.0) < p:
        high *= 2.0
    for _ in range(200):
        mid = (low + high) / 2.0
        if _gamma_p(df / 2.0, mid / 2.0) < p:
            low = mid
        else:
            high = mid
        if high - low < 1e-12 * max(1.0, high):
            break
    return (low + high) / 2.0


def trials_for_margin(variance_bound: float, margin: float, confidence: float) -> int:
    """Trials per task so that the mean of a task's trials is within ``margin`` of its
    expected score at ``confidence``, for a score with at most ``variance_bound``."""
    z = NormalDist().inv_cdf(0.5 + confidence / 2)
    return max(1, math.ceil(z * z * variance_bound / (margin * margin)))


def trial_advice(
    data: Mapping[str, Any],
    *,
    confidence: float = 0.95,
    margins: tuple[float, ...] = DEFAULT_MARGINS,
) -> dict[str, Any]:
    """How many trials per task the variance seen across trials supports.

    The within-task variance is pooled over every task with at least two trials of a
    metric, and an upper confidence bound on it (chi-square, one-sided at
    ``confidence``) feeds ``n = z^2 * variance / margin^2``. Using the bound rather
    than the estimate keeps a few trials from promising precision they do not have.

    No variation across trials is not evidence of none. A pass/fail metric where
    every task came out the same in every trial is given the worst case for a
    pass/fail score (variance 1/4) and is marked as such; a numeric metric with no
    variation gets no recommendation. This only reports: it changes neither the
    default number of trials nor any gate. Read it as a per-task estimate; the
    interval across tasks has its own width.
    """
    for margin in margins:
        recommended_trials(margin=margin, confidence=confidence)  # validates both
    cells, _, _ = _collect(data)
    out = []
    for metric, records in sorted(cells.items()):
        repeated = [
            [float(v) for v in trials.values()]
            for trials in records.values()
            if len(trials) >= 2 and all(isinstance(v, bool | float) for v in trials.values())
        ]
        if not repeated:
            continue
        bernoulli = all(
            isinstance(v, bool)
            for trials in records.values()
            for v in trials.values()
            if len(trials) >= 2
        )
        df = sum(len(v) - 1 for v in repeated)
        pooled = sum((len(v) - 1) * variance(v) for v in repeated) / df
        entry: dict[str, Any] = {
            "metric": metric,
            "tasks_with_repeats": len(repeated),
            "degrees_of_freedom": df,
            "observed_variance": pooled,
            "trials_run_max": max(len(v) for v in repeated),
        }
        if pooled > 0:
            bound = pooled * df / _chi2_lower(1 - confidence, df)
            entry["variance_bound"] = min(bound, BERNOULLI_MAX_VARIANCE) if bernoulli else bound
            entry["basis"] = "upper confidence bound on the observed within-task variance"
        elif bernoulli:
            entry["variance_bound"] = BERNOULLI_MAX_VARIANCE
            entry["basis"] = (
                "worst case for a pass/fail score: no task varied across trials, which "
                "does not show the metric is deterministic"
            )
        else:
            entry["variance_bound"] = None
            entry["basis"] = "no variation observed in a numeric metric; no recommendation"
        bound = entry["variance_bound"]
        entry["trials_for_margin"] = (
            {f"{m:g}": trials_for_margin(bound, m, confidence) for m in margins}
            if bound is not None
            else {}
        )
        out.append(entry)
    return {
        "method": "within-task variance upper bound",
        "confidence": confidence,
        "metrics": out,
        "note": (
            "Trials per task so a task's mean score is within the margin, at the "
            "confidence. A report, not a setting: the default trials and gates are "
            "unchanged."
        ),
    }

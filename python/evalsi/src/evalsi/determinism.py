"""Describe observed score stability across trials of the same records.

Identical observed scores do not prove deterministic generation. The trial
recommendation is Hoeffding's conservative bound for independent [0, 1]
observations of one fixed case, not a power calculation for model comparisons.
"""

from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from collections.abc import Callable, Mapping, Sequence
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


def _binom_cdf(k: int, n: int, p: float) -> float:
    """P(X <= k) for X ~ Binomial(n, p)."""
    return sum(math.comb(n, i) * p**i * (1 - p) ** (n - i) for i in range(k + 1))


def _bisect(f: Callable[[float], float], target: float) -> float:
    """The p in [0, 1] where the decreasing f crosses target."""
    lo, hi = 0.0, 1.0
    for _ in range(60):
        mid = (lo + hi) / 2
        if f(mid) > target:
            lo = mid
        else:
            hi = mid
    return (lo + hi) / 2


def bernoulli_variance_bound(passes: int, trials: int, confidence: float) -> float:
    """The largest p(1-p) over the exact (Clopper-Pearson) interval for a task's pass
    probability. It is valid for pass/fail scores at any trial count; with few trials the
    interval takes in 1/2 and the bound is the worst case, 1/4."""
    tail = (1 - confidence) / 2
    lower = 0.0 if passes == 0 else _bisect(lambda p: _binom_cdf(passes - 1, trials, p), 1 - tail)
    upper = 1.0 if passes == trials else _bisect(lambda p: _binom_cdf(passes, trials, p), tail)
    nearest = min(max(0.5, lower), upper)  # the point of the interval closest to 1/2
    return nearest * (1 - nearest)


def trial_advice(
    data: Mapping[str, Any],
    *,
    confidence: float = 0.95,
    margins: tuple[float, ...] = DEFAULT_MARGINS,
) -> dict[str, Any]:
    """How many trials per task the variance seen across trials supports.

    Each task with at least two trials of a metric gets an upper bound on its own
    variance, and the metric's bound is the largest of them, so the advice covers the
    noisiest task and not an average one. The bound is exact for a pass/fail score (the
    largest p(1-p) over the Clopper-Pearson interval at ``confidence``; with few trials
    that is the worst case 1/4, as it should be). For a numeric score it is the
    chi-square upper bound, which assumes roughly normal scores and is not valid for
    heavy-tailed ones, capped at ``(max - min)^2 / 4`` when the evaluator declares a range
    (no score in a range can vary more). It feeds ``n = z^2 * variance / margin^2``.

    Margins are in the metric's own units. ``bounded`` says whether that unit is a known
    range (pass/fail, or a declared ``min`` and ``max``); for an unbounded metric such as
    a token count, a margin of 0.1 is rarely what anyone means.

    A numeric metric with no variation anywhere gets no recommendation. This only
    reports: it changes neither the default number of trials nor any gate.
    """
    for margin in margins:
        recommended_trials(margin=margin, confidence=confidence)  # validates both
    cells, _, _ = _collect(data)
    ranges = {
        str(s.get("metric")): (s.get("min"), s.get("max"))
        for s in data.get("summaries") or []
        if isinstance(s, Mapping)
    }
    out = []
    for metric, records in sorted(cells.items()):
        repeated = [
            list(trials.values())
            for trials in records.values()
            if len(trials) >= 2 and all(isinstance(v, bool | float) for v in trials.values())
        ]
        if not repeated:
            continue
        bernoulli = all(isinstance(v, bool) for trials in repeated for v in trials)
        floats = [[float(v) for v in trials] for trials in repeated]
        df = sum(len(v) - 1 for v in floats)
        pooled = sum((len(v) - 1) * variance(v) for v in floats) / df
        entry: dict[str, Any] = {
            "metric": metric,
            "tasks_with_repeats": len(floats),
            "degrees_of_freedom": df,
            "observed_variance": pooled,
            "trials_run_max": max(len(v) for v in floats),
        }
        low, high = ranges.get(metric, (None, None))
        span = (
            float(high) - float(low)
            if isinstance(low, int | float) and isinstance(high, int | float) and high > low
            else None
        )
        entry["bounded"] = bernoulli or span is not None
        bound: float | None
        if bernoulli:
            bound = max(bernoulli_variance_bound(int(sum(v)), len(v), confidence) for v in floats)
            entry["basis"] = (
                "largest per-task variance bound for a pass/fail score (exact binomial "
                "interval); few trials leave it at the worst case"
            )
        elif pooled > 0:
            bound = max(
                variance(v) * (len(v) - 1) / _chi2_lower(1 - confidence, len(v) - 1) for v in floats
            )
            entry["basis"] = (
                "largest per-task chi-square upper bound on the variance; assumes roughly "
                "normal scores"
            )
            if span is not None:
                cap = span**2 / 4
                if bound > cap:
                    bound = cap
                    entry["basis"] += "; capped at the most a score in its declared range can vary"
            else:
                entry["basis"] += "; no declared range, so the margins are in its own units"
        else:
            bound = None
            entry["basis"] = "no variation observed in a numeric metric; no recommendation"
        entry["variance_bound"] = bound
        entry["trials_for_margin"] = (
            {f"{m:g}": trials_for_margin(bound, m, confidence) for m in margins}
            if bound is not None
            else {}
        )
        out.append(entry)
    return {
        "method": "largest per-task within-task variance bound",
        "confidence": confidence,
        "metrics": out,
        "note": (
            "Trials per task so that even the noisiest task's mean score is within the "
            "margin, at the confidence. A report, not a setting: the default trials and "
            "gates are unchanged."
        ),
    }


# --- replay against a recorded trajectory ----------------------------------------------

# What of a step decides whether a replay reproduced it. Timing, span ids and token
# counts are not part of it; the attributes listed are (the model asked for and
# answering, and why it stopped; call ids are the provider's and new every time).
_STEP_ATTRIBUTES = (
    "gen_ai.request.model",
    "gen_ai.response.model",
    "gen_ai.response.finish_reasons",
)


def _text(value: Any) -> Any:
    """A step's input or output as comparable text (a message list as its JSON)."""
    if not isinstance(value, Mapping):
        return value
    if value.get("text") is not None:
        return value["text"]
    messages = value.get("messages")
    if isinstance(messages, list):
        # Tool call ids are the provider's, new on every call: what was asked of the
        # tool is compared, not what the call was named.
        value = {
            **value,
            "messages": [
                {
                    **m,
                    "tool_calls": [
                        {k: v for k, v in c.items() if k != "id"} for c in m.get("tool_calls") or []
                    ],
                }
                if isinstance(m, Mapping) and m.get("tool_calls")
                else m
                for m in messages
            ],
        }
    return json.dumps(value, sort_keys=True, ensure_ascii=False)


def _step_view(step: Mapping[str, Any]) -> dict[str, Any]:
    attrs = step.get("attributes") or {}
    return {
        "type": step.get("type", ""),
        "name": step.get("name", ""),
        "input": _text(step.get("input")),
        "output": _text(step.get("output")),
        "error": step.get("error", ""),
        **{k: attrs[k] for k in _STEP_ATTRIBUTES if k in attrs},
    }


def step_differences(
    recorded: Mapping[str, Any], replayed: Mapping[str, Any]
) -> list[dict[str, Any]]:
    """The fields in which two steps differ, each with both values."""
    a, b = _step_view(recorded), _step_view(replayed)
    return [
        {"field": key, "recorded": a.get(key), "replayed": b.get(key)}
        for key in a.keys() | b.keys()
        if a.get(key) != b.get(key)
    ]


def first_divergence(
    recorded: Sequence[Mapping[str, Any]], replayed: Sequence[Mapping[str, Any]]
) -> dict[str, Any] | None:
    """The first step at which a replay differs from the recorded trajectory and what
    differed, or None when they match step for step. A replay that stops early, or
    goes on past the recording, diverges at the first step one of them lacks."""
    for index, (a, b) in enumerate(zip(recorded, replayed, strict=False)):
        diffs = step_differences(a, b)
        if diffs:
            return {
                "step": index,
                "type": a.get("type", ""),
                "name": a.get("name", ""),
                "differences": sorted(diffs, key=lambda d: str(d["field"])),
            }
    if len(recorded) != len(replayed):
        recorded_longer = len(recorded) > len(replayed)
        index = min(len(recorded), len(replayed))
        step = (recorded if recorded_longer else replayed)[index]
        return {
            "step": index,
            "type": step.get("type", ""),
            "name": step.get("name", ""),
            "differences": [
                {
                    "field": "step",
                    "recorded": "present" if recorded_longer else "missing",
                    "replayed": "missing" if recorded_longer else "present",
                }
            ],
        }
    return None


def _flatten(value: Any, prefix: str = "") -> dict[str, Any]:
    if isinstance(value, Mapping):
        out: dict[str, Any] = {}
        for key, item in value.items():
            out.update(_flatten(item, f"{prefix}.{key}" if prefix else str(key)))
        return out
    return {prefix: value}


def record_changes(
    recorded: Mapping[str, Any], replayed: Mapping[str, Any]
) -> list[dict[str, Any]]:
    """What differs between two runs' determinism records (a model snapshot that moved,
    a tool version, an image digest), by dotted path."""
    a, b = _flatten(recorded), _flatten(replayed)
    return [
        {"path": path, "recorded": a.get(path), "replayed": b.get(path)}
        for path in sorted(a.keys() | b.keys())
        if a.get(path) != b.get(path)
    ]


def _by_run(data: Mapping[str, Any]) -> dict[tuple[str, int], Mapping[str, Any]]:
    records = data.get("records")
    if not isinstance(records, list):
        raise ValueError("results must hold a records array from `evalsi run --output`")
    out: dict[tuple[str, int], Mapping[str, Any]] = {}
    for record in records:
        if not isinstance(record, Mapping):
            raise ValueError("each record must be an object")
        trial = ((record.get("provenance") or {}).get("run") or {}).get("trial", 0) or 0
        key = (str(record.get("id", "")), int(trial))
        if key in out:
            raise ValueError(f"duplicate record {key[0]!r} trial {key[1]}")
        out[key] = record
    return out


def compare_runs(recorded: Mapping[str, Any], replayed: Mapping[str, Any]) -> dict[str, Any]:
    """Compares a replay's results with the recorded run's (two ``evalsi run --output``
    files), record by record and trial by trial. Each pair is ``identical`` or
    ``diverged`` at a named step with the fields that differ, and carries what changed in
    the runs' determinism records (``metadata.determinism``): the usual reason a
    replay that should match does not. Records only one side has are listed."""
    a, b = _by_run(recorded), _by_run(replayed)
    runs: list[dict[str, Any]] = []
    for key in sorted(a.keys() & b.keys()):
        ra, rb = a[key], b[key]
        steps_a = list((ra.get("trajectory") or {}).get("steps") or [])
        steps_b = list((rb.get("trajectory") or {}).get("steps") or [])
        divergence = first_divergence(steps_a, steps_b)
        changes = record_changes(
            (ra.get("metadata") or {}).get("determinism") or {},
            (rb.get("metadata") or {}).get("determinism") or {},
        )
        run: dict[str, Any] = {
            "record_id": key[0],
            "trial": key[1],
            "status": "identical" if divergence is None else "diverged",
            "recorded_steps": len(steps_a),
            "replayed_steps": len(steps_b),
        }
        if divergence is not None:
            run["first_divergence"] = divergence
        if changes:
            run["determinism_changes"] = changes
        runs.append(run)
    return {
        "runs": runs,
        "identical": sum(1 for r in runs if r["status"] == "identical"),
        "diverged": sum(1 for r in runs if r["status"] == "diverged"),
        "only_recorded": sorted(f"{i}#{t}" for i, t in a.keys() - b.keys()),
        "only_replayed": sorted(f"{i}#{t}" for i, t in b.keys() - a.keys()),
    }

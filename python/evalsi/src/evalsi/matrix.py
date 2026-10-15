"""A harness-by-model matrix over results files, with cost beside every score (H3).

Each results file (``evalsi run --output``) is one cell: one harness (a named
profile, or ``custom``) with one model. The matrix reports, for a metric, the
score with its interval, and next to it what the score cost: model calls, tokens
and dollars per task, and the score per thousand tokens. That makes a cheaper
harness that scores the same visible, and a costlier one that scores the same
too.

Every cell after the first is also compared with the first, paired by record
and trial (``evalsi.compare``), so the difference carries its own interval and
says whether it excludes zero. Give ``tuned_on`` the names or dataset hashes a
harness was tuned on, and cells are split into ``tuned`` and ``held-out``, so a
profile that only fits its tuning set does not look good.
"""

from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from dataclasses import asdict, dataclass
from statistics import fmean
from typing import Any

from evalsi.compare import compare_results, record_values


@dataclass
class Cost:
    """What a task cost on average; a field is None when the results lack it."""

    tasks: int = 0
    calls: float | None = None
    tokens: float | None = None
    usd: float | None = None


@dataclass
class Cell:
    name: str
    harness: str
    model: str
    dataset: str
    split: str  # "tuned", "held-out" or "" when no tuning set was named
    n: int
    mean: float | None
    low: float | None
    high: float | None
    cost: Cost
    # Mean score per 1,000 tokens of a task, when tokens are known.
    score_per_ktok: float | None = None
    # The paired difference from the first cell, when there is one.
    diff: float | None = None
    diff_low: float | None = None
    diff_high: float | None = None
    significant: bool = False

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _get(data: Mapping[str, Any] | None, *path: str) -> Any:
    cur: Any = data
    for key in path:
        if not isinstance(cur, Mapping):
            return None
        cur = cur.get(key)
    return cur


def harness_of(result: Mapping[str, Any]) -> str:
    """The profile the run used (``minimal@1``), else ``custom`` for a built-in
    agent without one, else the kind of agent it drove."""
    for record in result.get("records") or []:
        profile = _get(record, "metadata", "agent", "profile")
        if profile:
            return str(profile)
    builtin = _get(result, "manifest", "spec", "harness", "builtin")
    if isinstance(builtin, Mapping) and builtin.get("profile"):
        return str(builtin["profile"])
    agent = _get(result, "manifest", "spec", "target", "agent")
    if isinstance(agent, Mapping) and agent:
        return f"agent:{next(iter(agent))}"
    return "custom"


def _model_of(result: Mapping[str, Any]) -> str:
    target = _get(result, "manifest", "target")
    if isinstance(target, Mapping):
        return str(target.get("model") or target.get("connector") or "")
    return ""


def _cost(result: Mapping[str, Any]) -> Cost:
    calls: list[float] = []
    tokens: list[float] = []
    usd: list[float] = []
    records = result.get("records") or []
    for r in records:
        steps = _get(r, "metadata", "agent", "steps")
        if isinstance(steps, int | float) and not isinstance(steps, bool):
            calls.append(float(steps))
        usage = r.get("usage") or {}
        tin, tout = usage.get("input_tokens"), usage.get("output_tokens")
        if tin is not None or tout is not None:
            tokens.append(float((tin or 0) + (tout or 0)))
        if usage.get("cost_usd") is not None:
            usd.append(float(usage["cost_usd"]))
    return Cost(
        tasks=len(records),
        calls=fmean(calls) if calls else None,
        tokens=fmean(tokens) if tokens else None,
        usd=fmean(usd) if usd else None,
    )


def build(
    results: Sequence[tuple[str, Mapping[str, Any]]],
    metric: str,
    *,
    tuned_on: Sequence[str] = (),
    level: float = 0.95,
) -> list[Cell]:
    """One cell per ``(name, results)``, the first being the baseline."""
    cells: list[Cell] = []
    for name, data in results:
        summary = next((s for s in data.get("summaries") or [] if s.get("metric") == metric), None)
        if summary is None:
            have = [s.get("metric") for s in data.get("summaries") or []]
            raise ValueError(f"{name}: no metric {metric!r}; it has {have}")
        dataset = str(_get(data, "manifest", "dataset", "sha256") or "")
        run_name = str(_get(data, "manifest", "name") or "")
        if tuned_on:
            hit = {name, run_name, dataset} & set(tuned_on)
            split = "tuned" if hit else "held-out"
        else:
            split = ""
        ci = summary.get("ci") or {}
        cost = _cost(data)
        mean = summary.get("mean")
        per_ktok = (
            mean / (cost.tokens / 1000)
            if mean is not None and cost.tokens and cost.tokens > 0
            else None
        )
        cell = Cell(
            name=name,
            harness=harness_of(data),
            model=_model_of(data),
            dataset=dataset[:12],
            split=split,
            n=int(summary.get("n") or 0),
            mean=mean,
            low=ci.get("low"),
            high=ci.get("high"),
            cost=cost,
            score_per_ktok=per_ktok,
        )
        if cells:
            for c in compare_results(results[0][1], data, level=level):
                if c.metric == metric:
                    cell.diff, cell.diff_low, cell.diff_high = c.diff, c.diff_low, c.diff_high
                    cell.significant = c.significant
        cells.append(cell)
    return cells


def _f(value: float | None, fmt: str = ".3f") -> str:
    if value is None or (isinstance(value, float) and math.isnan(value)):
        return "-"
    return format(value, fmt)


def table(cells: Sequence[Cell]) -> str:
    """The matrix as text: score and interval, then cost, then the difference."""
    head = [
        "cell",
        "harness",
        "model",
        "split",
        "n",
        "score",
        "95% CI",
        "calls/task",
        "tokens/task",
        "$/task",
        "score/ktok",
        "diff vs first",
    ]
    rows = [head]
    for c in cells:
        diff = "-"
        if c.diff is not None:
            diff = f"{c.diff:+.3f} [{_f(c.diff_low)}, {_f(c.diff_high)}]"
            diff += " *" if c.significant else ""
        rows.append(
            [
                c.name,
                c.harness,
                c.model or "-",
                c.split or "-",
                str(c.n),
                _f(c.mean),
                f"[{_f(c.low)}, {_f(c.high)}]",
                _f(c.cost.calls, ".1f"),
                _f(c.cost.tokens, ".0f"),
                _f(c.cost.usd, ".4f"),
                _f(c.score_per_ktok, ".3f"),
                diff,
            ]
        )
    widths = [max(len(r[i]) for r in rows) for i in range(len(head))]
    lines = [
        "  ".join(cell.ljust(w) for cell, w in zip(r, widths, strict=True)).rstrip() for r in rows
    ]
    lines.append("* the interval of the paired difference excludes zero")
    return "\n".join(lines)


def paired_n(baseline: Mapping[str, Any], candidate: Mapping[str, Any], metric: str) -> int:
    """How many (record, trial) pairs a comparison rests on."""
    a = record_values(baseline).get(metric, {})
    b = record_values(candidate).get(metric, {})
    return len(set(a) & set(b))

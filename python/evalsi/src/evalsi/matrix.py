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
from evalsi.mechanisms import called_tools, context_acted


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
    level: float = 0.95
    # Mean score per 1,000 tokens of a task, when tokens are known.
    score_per_ktok: float | None = None
    # The paired difference from the first cell, when there is one.
    diff: float | None = None
    diff_low: float | None = None
    diff_high: float | None = None
    significant: bool = False
    # How many (record, trial) pairs the difference rests on.
    paired_n: int = 0
    # True when the difference is significant and in the bad direction.
    regressed: bool = False

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
    # An agent run records its target under manifest.agent.agent; a plain model
    # run under manifest.target.
    for path in (("manifest", "target"), ("manifest", "agent", "agent")):
        target = _get(result, *path)
        if isinstance(target, Mapping) and (target.get("model") or target.get("connector")):
            return str(target.get("model") or target.get("connector"))
    return ""


def _cost(result: Mapping[str, Any]) -> Cost:
    calls: list[float] = []
    tokens: list[float] = []
    usd: list[float] = []
    records = result.get("records") or []
    for r in records:
        steps = _get(r, "metadata", "agent", "steps")
        builtin = _get(r, "metadata", "agent", "tools_offered") is not None
        if builtin and isinstance(steps, int | float) and not isinstance(steps, bool):
            calls.append(float(steps))
        else:
            # A CLI agent has no step count of its own: count the model calls its
            # trajectory shows (a reader turned its event stream into them).
            llm = [
                s
                for s in _get(r, "trajectory", "steps") or []
                if isinstance(s, Mapping)
                and str(s.get("type", "")).lower().removeprefix("step_type_") == "llm"
            ]
            if llm:
                calls.append(float(len(llm)))
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
            level=float(ci.get("level") or level),
            score_per_ktok=per_ktok,
        )
        first = cells[0].dataset if cells else ""
        # Record ids repeat across unrelated datasets, so cells are paired only
        # when they ran the same one.
        comparable = not (first and dataset and not dataset.startswith(first))
        if cells and comparable:
            for c in compare_results(results[0][1], data, level=level):
                if c.metric == metric:
                    cell.diff, cell.diff_low, cell.diff_high = c.diff, c.diff_low, c.diff_high
                    cell.significant = c.significant
                    cell.paired_n = c.paired_n
                    cell.regressed = c.regressed
        cells.append(cell)
    return cells


def _f(value: float | None, fmt: str = ".3f") -> str:
    if value is None or (isinstance(value, float) and math.isnan(value)):
        return "-"
    return format(value, fmt)


def mechanism_lines(results: Sequence[tuple[str, Mapping[str, Any]]]) -> str:
    """One line per cell: what ran, in how many tasks, and a warning for what never did."""
    lines = []
    for name, data in results:
        found = mechanisms(data)
        if not found:
            lines.append(f"{name}: no optional mechanism recorded")
            continue
        parts = [
            f"{m.name} {m.ran}/{m.of}" + (" (never ran)" if m.never_ran else "") for m in found
        ]
        lines.append(f"{name}: " + ", ".join(parts))
    return "\n".join(lines)


def table(cells: Sequence[Cell]) -> str:
    """The matrix as text: score and interval, then cost, then the difference."""
    head = [
        "cell",
        "harness",
        "model",
        "split",
        "n",
        "score",
        "CI",
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
                f"[{_f(c.low)}, {_f(c.high)}] {c.level:.0%}",
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


# Tools a harness may add (the H2 toolset and friends): "offered but never
# called" is the finding that matters when a change is meant to add one.
OPTIONAL_TOOLS = (
    "edit_file",
    "glob",
    "grep",
    "apply_patch",
    "memory",
    "session_search",
    "delegate",
    "tool_search",
    "run_code",
    "evidence",
)


@dataclass
class Mechanism:
    """A part of the harness that was on, and in how many tasks it ran."""

    name: str
    ran: int
    of: int

    @property
    def never_ran(self) -> bool:
        return self.of > 0 and self.ran == 0


def mechanisms(result: Mapping[str, Any]) -> list[Mechanism]:
    """Which parts of the harness were on, and in how many tasks the traces show
    them running. A gain credited to a change whose mechanism ran in none of the
    tasks is not evidence the change helped: the window may never have filled,
    the new tool may never have been called."""
    records = [r for r in result.get("records") or [] if isinstance(r, Mapping)]
    n = len(records)
    out: list[Mechanism] = []
    offered: set[str] = set()
    for r in records:
        offered.update(_get(r, "metadata", "agent", "tools_offered") or [])
    for tool in OPTIONAL_TOOLS:
        if tool in offered:
            out.append(Mechanism(f"tool:{tool}", sum(tool in called_tools(r) for r in records), n))
    # The strategy that ran is what the run recorded (a profile may have chosen
    # it); the spec's own is the fallback for results from an older harness.
    strategy = next(
        (
            _get(r, "metadata", "agent", "context_strategy")
            for r in records
            if _get(r, "metadata", "agent", "context_strategy")
        ),
        _get(result, "manifest", "spec", "harness", "builtin", "context", "strategy"),
    )
    if strategy and strategy != "none":

        def acted(r: Mapping[str, Any]) -> bool:
            agent = _get(r, "metadata", "agent") or {}
            return isinstance(agent, Mapping) and context_acted(agent, str(strategy))

        out.append(Mechanism(f"context:{strategy}", sum(acted(r) for r in records), n))
    return out


# --- The regression gate (H7) ------------------------------------------------
#
# The gate reuses the paired difference and its interval from ``compare_results``
# (no interval code of its own). A cell fails when the interval lies wholly on
# the bad side of ``-max_regression``; it passes when the interval does not; and
# when there is nothing to pair (no shared records, fewer than two pairs, an
# interval that cannot be formed, a different dataset) the verdict is
# ``insufficient``. Insufficient data is reported, never counted as a pass in the
# text: it exits zero unless ``strict`` is set, which fails closed.

PASS, FAIL, INSUFFICIENT, BASELINE = "pass", "fail", "insufficient", "baseline"


@dataclass
class Check:
    name: str  # "vs-baseline", "held-out-vs-baseline" or "held-out-vs-tuned"
    status: str
    detail: str


@dataclass
class Verdict:
    cell: str
    status: str
    checks: list[Check]

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _higher_is_better(data: Mapping[str, Any], metric: str) -> bool:
    summary: Mapping[str, Any] = next(
        (s for s in data.get("summaries") or [] if s.get("metric") == metric), {}
    )
    return summary.get("higher_is_better", True) is not False


def _vs(
    name: str,
    reference: tuple[str, Mapping[str, Any]],
    candidate: Mapping[str, Any],
    metric: str,
    *,
    max_regression: float,
    level: float,
) -> Check:
    """Is the candidate's paired difference from the reference wholly worse than
    the tolerance?"""
    for c in compare_results(reference[1], candidate, level=level):
        if c.metric != metric:
            continue
        if c.diff is None or c.diff_low is None or c.diff_high is None or c.paired_n < 2:
            return Check(
                name, INSUFFICIENT, f"only {c.paired_n} paired record(s) with {reference[0]}"
            )
        # Orient so that a negative number is always worse.
        sign = 1.0 if _higher_is_better(candidate, metric) else -1.0
        lo, hi = sorted((sign * c.diff_low, sign * c.diff_high))
        text = f"{sign * c.diff:+.3f} [{lo:+.3f}, {hi:+.3f}] vs {reference[0]} (n={c.paired_n})"
        if hi < -max_regression:
            return Check(name, FAIL, f"worse by more than {max_regression:g}: {text}")
        return Check(name, PASS, text)
    return Check(name, INSUFFICIENT, f"no shared metric {metric!r} with {reference[0]}")


def gate(
    results: Sequence[tuple[str, Mapping[str, Any]]],
    cells: Sequence[Cell],
    metric: str,
    *,
    max_regression: float = 0.0,
    level: float = 0.95,
) -> list[Verdict]:
    """A verdict per cell. The first cell is the baseline. A candidate is checked
    against the baseline on its own dataset, and, when ``build`` split the cells
    by ``tuned_on``, a held-out candidate is also checked against the tuned cell
    of the same harness and model: it fails when its interval lies wholly below
    the tuned one by more than ``max_regression`` (the two sets of records
    differ, so this is a comparison of intervals, not a paired one)."""
    if max_regression < 0:
        raise ValueError("max_regression must be zero or positive")
    verdicts: list[Verdict] = []
    base_name = results[0][0]
    for i, (cell, (name, data)) in enumerate(zip(cells, results, strict=True)):
        if i == 0:
            verdicts.append(Verdict(name, BASELINE, []))
            continue
        checks: list[Check] = []
        # The baseline harness on this cell's own dataset: the first cell, or a
        # later cell of the same harness and model that ran the same data.
        refs = [
            (n, d)
            for j, (n, d) in enumerate(results)
            if j < i
            and cells[j].harness == cells[0].harness
            and cells[j].model == cells[0].model
            and cells[j].dataset == cell.dataset
        ]
        label = "held-out-vs-baseline" if cell.split == "held-out" else "vs-baseline"
        if cell.n == 0 or cell.mean is None:
            checks.append(Check(label, INSUFFICIENT, "the cell has no scored records"))
        elif refs:
            checks.append(
                _vs(label, refs[0], data, metric, max_regression=max_regression, level=level)
            )
        else:
            checks.append(
                Check(label, INSUFFICIENT, f"{base_name} has no run on this cell's dataset")
            )
        if cell.split == "held-out":
            tuned = [
                c
                for c in cells[:i]
                if c.split == "tuned" and c.harness == cell.harness and c.model == cell.model
            ]
            if not tuned:
                checks.append(
                    Check("held-out-vs-tuned", INSUFFICIENT, "no tuned cell of this harness")
                )
            elif cell.high is None or tuned[0].low is None or not cell.n or not tuned[0].n:
                checks.append(Check("held-out-vs-tuned", INSUFFICIENT, "an interval is missing"))
            else:
                t = tuned[0]
                text = (
                    f"held-out [{_f(cell.low)}, {_f(cell.high)}] "
                    f"vs tuned {t.name} [{_f(t.low)}, {_f(t.high)}]"
                )
                higher = _higher_is_better(data, metric)
                worse = (
                    cell.high < (t.low or 0) - max_regression
                    if higher
                    else (cell.low or 0) > (t.high or 0) + max_regression
                )
                checks.append(Check("held-out-vs-tuned", FAIL if worse else PASS, text))
        statuses = {c.status for c in checks}
        status = FAIL if FAIL in statuses else INSUFFICIENT if INSUFFICIENT in statuses else PASS
        verdicts.append(Verdict(name, status, checks))
    return verdicts


def gate_failed(verdicts: Sequence[Verdict], *, strict: bool = False) -> bool:
    """Whether the gate fails: any failing cell, or with ``strict`` any cell
    with insufficient data too."""
    bad = {FAIL, INSUFFICIENT} if strict else {FAIL}
    return any(v.status in bad for v in verdicts)


def gate_lines(verdicts: Sequence[Verdict], *, strict: bool = False) -> str:
    lines = []
    for v in verdicts:
        lines.append(f"{v.cell}: {v.status.upper()}")
        lines.extend(f"  {c.name}: {c.status} - {c.detail}" for c in v.checks)
    outcome = "FAIL" if gate_failed(verdicts, strict=strict) else "PASS"
    note = ""
    if any(v.status == INSUFFICIENT for v in verdicts):
        note = (
            " (strict: insufficient data fails)"
            if strict
            else " (some cells had insufficient data)"
        )
    lines.append(f"gate: {outcome}{note}")
    return "\n".join(lines)

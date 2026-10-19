"""Probe-signal calibration for Harness² runs (H7).

Harness² chooses between candidate harness executions by contrasting them, with
no ground truth. This module measures that signal: given a Harness² namespace
directory and scores from our own checker or judge for the same candidates, it
reports how often the choice (``Selection.chosen_label``) is the candidate we
score highest, with a Wilson interval (``evalsi.stats``) on the agreement rate.
Tasks that cannot be compared are listed with the reason and never counted as a
disagreement. It also computes judge-versus-human agreement (Cohen's kappa),
which the roadmap asks for before a judge is trusted.

It also holds the reader of a namespace directory that the run importer
(``evalsi_harness.benchmarks.harness2``) builds on. The layout was read from the
Harness² source (``core/schedule.py``, ``core/judge.py``, ``core/selection.py``,
``cli/judge.py``), not from a recorded run, so the reader is tolerant: a missing
key leaves a field ``None``, the legacy ``arm``/``arms`` spelling of ``mode``/
``modes`` is accepted, and an unreadable file is a note, not a crash. File
contents are untrusted data: nothing is executed, sizes are capped, a path that
resolves outside the namespace root (a symlink) is refused, the directory is
never written to, and recorded ``run_dir`` strings are kept as text and never
opened.
"""

from __future__ import annotations

import json
import math
import os
from collections import Counter
from collections.abc import Hashable, Mapping, Sequence
from dataclasses import asdict, dataclass, field
from pathlib import Path
from statistics import fmean
from typing import Any

from evalsi.compare import record_values
from evalsi.stats import wilson

MAX_JSON_BYTES = 64 * 1024 * 1024
MAX_TEXT = 500
EPS = 1e-9

# (task id, mode, k, label): one candidate execution of one task.
CandidateKey = tuple[str, str, int, str]


class UnsafePathError(ValueError):
    """A path resolves outside the namespace root."""


def safe_child(root: Path, path: Path) -> Path:
    """``path`` resolved (symlinks followed), if it stays inside ``root``.

    ``root`` must already be resolved. Raises UnsafePathError otherwise.
    """
    resolved = path.resolve()
    if not resolved.is_relative_to(root):
        raise UnsafePathError(f"{path.name} resolves outside the namespace root")
    return resolved


def _text(value: Any) -> str:
    return value[:MAX_TEXT] if isinstance(value, str) else ""


def _int(value: Any) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int | float):
        return None
    if isinstance(value, float) and (math.isnan(value) or math.isinf(value) or value != int(value)):
        return None
    return int(value)


def _float(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, int | float):
        return None
    return None if math.isnan(value) or math.isinf(value) else float(value)


def _mode(row: Mapping[str, Any]) -> str:
    # Older runs wrote `arm`; Harness²'s own reader accepts both.
    return _text(row["mode"] if "mode" in row else row.get("arm"))


def _read_json(root: Path, path: Path, notes: list[str]) -> Any:
    """The parsed JSON at ``path``, or None with a note saying why not."""
    rel = os.path.relpath(path, root)
    try:
        real = safe_child(root, path)
        if not real.is_file():
            return None
        if real.stat().st_size > MAX_JSON_BYTES:
            notes.append(f"{rel}: larger than {MAX_JSON_BYTES} bytes; not read")
            return None
        return json.loads(real.read_text(encoding="utf-8"))
    except UnsafePathError as exc:
        notes.append(f"{rel}: refused: {exc}")
    except (OSError, ValueError) as exc:
        notes.append(f"{rel}: unreadable: {type(exc).__name__}")
    return None


@dataclass
class SelectionRow:
    """One selection record of a task summary (``Selection.to_dict``)."""

    task_id: str
    mode: str
    k: int | None
    chosen_label: str
    chosen_run_id: str
    chosen_is_base: bool | None
    ok: bool | None
    usd: float | None
    harness_fingerprint: str
    n_candidates: int | None
    error: str


@dataclass
class JudgedRow:
    """One row of ``judged.json`` (``Scored``): the judge's verdict on a chosen run."""

    task_id: str
    mode: str
    k: int | None
    label: str
    run_id: str
    chosen_is_base: bool | None
    n_passed: int | None
    n_criteria: int | None
    error: str

    @property
    def frac(self) -> float | None:
        """Passed over criteria. Recomputed: the stored ``frac`` and ``graded``
        are derived and not trusted. None when the judge did not grade."""
        return _fraction(self.n_passed, self.n_criteria)

    @property
    def graded(self) -> bool:
        return self.frac is not None


@dataclass
class BaseRow:
    """A reference (base) execution; the score is None when it was not judged."""

    task_id: str
    label: str
    run_id: str
    run_dir: str
    n_passed: int | None
    n_criteria: int | None
    judged: bool

    @property
    def frac(self) -> float | None:
        return _fraction(self.n_passed, self.n_criteria)


def _fraction(n_passed: int | None, n_criteria: int | None) -> float | None:
    if n_passed is None or n_criteria is None or n_criteria <= 0:
        return None
    if not 0 <= n_passed <= n_criteria:
        return None
    return n_passed / n_criteria


@dataclass
class NamespaceData:
    root: Path
    config: dict[str, Any] = field(default_factory=dict)
    # task id -> its directory name under the root (the task id with "/" as "__").
    task_dirs: dict[str, str] = field(default_factory=dict)
    selections: list[SelectionRow] = field(default_factory=list)
    judged: list[JudgedRow] = field(default_factory=list)
    base: list[BaseRow] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)


def _score_fields(score: Any) -> tuple[int | None, int | None]:
    if not isinstance(score, Mapping):
        return None, None
    return _int(score.get("n_passed")), _int(score.get("n_criteria"))


def _selection(task_id: str, mode_key: str, raw: Any) -> SelectionRow | None:
    if not isinstance(raw, Mapping):
        return None
    ok = raw.get("ok")
    chosen_is_base = raw.get("chosen_is_base")
    return SelectionRow(
        task_id=_text(raw.get("task_id")) or task_id,
        mode=_mode(raw) or mode_key,
        k=_int(raw.get("k")),
        chosen_label=_text(raw.get("chosen_label")),
        chosen_run_id=_text(raw.get("chosen_run_id")),
        chosen_is_base=chosen_is_base if isinstance(chosen_is_base, bool) else None,
        ok=ok if isinstance(ok, bool) else None,
        usd=_float(raw.get("usd")),
        harness_fingerprint=_text(raw.get("harness_fingerprint")),
        n_candidates=_int(raw.get("n_candidates")),
        error=_text(raw.get("error")),
    )


def _read_judged(root: Path, notes: list[str]) -> list[JudgedRow]:
    raw_judged = _read_json(root, root / "judged.json", notes)
    if not isinstance(raw_judged, list):
        return []
    by_key: dict[tuple[str, str, int | None], JudgedRow] = {}
    for row in raw_judged:
        if not isinstance(row, Mapping):
            continue
        chosen_is_base = row.get("chosen_is_base")
        j = JudgedRow(
            task_id=_text(row.get("task_id")),
            mode=_mode(row),
            k=_int(row.get("k")),
            label=_text(row.get("label")),
            run_id=_text(row.get("run_id")),
            chosen_is_base=chosen_is_base if isinstance(chosen_is_base, bool) else None,
            n_passed=_int(row.get("n_passed")),
            n_criteria=_int(row.get("n_criteria")),
            error=_text(row.get("error")),
        )
        # One row per (task, mode, k), as judge.py's merge keeps; the last wins.
        by_key[(j.task_id, j.mode, j.k)] = j
    return list(by_key.values())


def _read_base_scores(root: Path, notes: list[str]) -> dict[tuple[str, str], Mapping[str, Any]]:
    """``base_judged.json`` rows by (task, label)."""
    raw = _read_json(root, root / "base_judged.json", notes)
    rows = raw if isinstance(raw, list) else []
    return {
        (_text(row.get("task")), _text(row.get("label"))): row
        for row in rows
        if isinstance(row, Mapping)
    }


def read_namespace(ns_dir: str | Path) -> NamespaceData:
    """Read a Harness² namespace directory without writing to it.

    Raises FileNotFoundError when ``ns_dir`` is not a directory. An empty
    directory gives empty data and a note.
    """
    given = Path(ns_dir)
    if not given.is_dir():
        raise FileNotFoundError(f"not a directory: {ns_dir}")
    root = given.resolve()
    data = NamespaceData(root=root)
    notes = data.notes

    config = _read_json(root, root / "config.json", notes)
    if isinstance(config, dict):
        data.config = config

    base_scores = _read_base_scores(root, notes)

    base_seen: set[tuple[str, str]] = set()
    with os.scandir(root) as it:
        entries = sorted(it, key=lambda e: e.name)
    for entry in entries:
        path = root / entry.name
        try:
            real = safe_child(root, path)
        except UnsafePathError as exc:
            notes.append(f"{entry.name}: refused: {exc}")
            continue
        if not real.is_dir() or not (path / "summary.json").exists():
            continue
        summary = _read_json(root, path / "summary.json", notes)
        if not isinstance(summary, Mapping):
            continue
        task_id = _text(summary.get("task_id")) or entry.name
        data.task_dirs[task_id] = entry.name
        selections = summary.get("selections")
        raws: list[tuple[str, Any]] = []
        if isinstance(selections, Mapping):
            raws = [(_text(m), r) for m, r in selections.items()]
        elif isinstance(selections, list):
            raws = [("", r) for r in selections]
        for mode_key, raw in raws:
            sel = _selection(task_id, mode_key, raw)
            if sel is not None:
                data.selections.append(sel)
        for item in summary.get("base") or []:
            if not isinstance(item, Mapping):
                continue
            label = _text(item.get("label"))
            scored = base_scores.get((task_id, label))
            n_passed, n_criteria = _score_fields(scored.get("score")) if scored else (None, None)
            base_seen.add((task_id, label))
            data.base.append(
                BaseRow(
                    task_id=task_id,
                    label=label,
                    run_id=_text(item.get("run_id")),
                    run_dir=_text(item.get("run_dir")),
                    n_passed=n_passed,
                    n_criteria=n_criteria,
                    judged=scored is not None,
                )
            )
    # Judged base rows whose task summary is missing are still scores; keep them.
    for (task, label), row in base_scores.items():
        if task and (task, label) not in base_seen:
            n_passed, n_criteria = _score_fields(row.get("score"))
            data.base.append(
                BaseRow(task, label, _text(row.get("run_id")), "", n_passed, n_criteria, True)
            )
    if not data.task_dirs:
        notes.append("no <task>/summary.json found")

    data.judged = _read_judged(root, notes)
    return data


# --- agreement of the selection with our own scores ---


@dataclass
class Ungraded:
    """A selection that could not be compared, and why. Never a disagreement."""

    task_id: str
    mode: str
    k: int | None
    reason: str


@dataclass
class Agreement:
    """How often Harness²'s choice matches the candidate we score highest."""

    cell: str  # "<mode><k>", or "all" for every cell pooled
    compared: int = 0
    agreed: int = 0
    rate: float | None = None
    low: float | None = None
    high: float | None = None
    level: float = 0.95
    interval_method: str = ""
    disagreements: list[str] = field(default_factory=list)
    ungraded: list[Ungraded] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _finish(cells: dict[str, Agreement], pooled: Agreement, level: float) -> list[Agreement]:
    out = [*cells.values(), pooled]
    for a in out:
        a.level = level
        if a.compared:
            a.rate = a.agreed / a.compared
            ci = wilson(a.agreed, a.compared, level)
            a.low, a.high, a.interval_method = ci.low, ci.high, ci.method
    return out


def _cell_name(mode: str, k: int | None) -> str:
    return f"{mode}{k if k is not None else ''}"


def metric_higher_is_better(results: Mapping[str, Any], metric: str) -> bool:
    """Whether a higher value of ``metric`` is better, from the results summary
    (``higher_is_better``); higher is better when the summary does not say."""
    for summary in results.get("summaries") or []:
        if isinstance(summary, Mapping) and summary.get("metric") == metric:
            return summary.get("higher_is_better", True) is not False
    return True


def load_candidate_scores(results: Mapping[str, Any], metric: str) -> dict[CandidateKey, float]:
    """Our score per candidate, from a results file (``evalsi run --output``).

    A record names its candidate in ``metadata.harness2`` (``task_id``, ``mode``,
    ``k``, ``label``), as the importer writes it. Records without it, or without
    a scored value for ``metric``, are left out: they are unscored, not zero.
    A candidate with several trials gets the mean.
    """
    values = record_values(results).get(metric, {})
    meta: dict[str, CandidateKey] = {}
    for rec in results.get("records") or []:
        if not isinstance(rec, Mapping) or not isinstance(rec.get("id"), str):
            continue
        h2 = (rec.get("metadata") or {}).get("harness2")
        if not isinstance(h2, Mapping):
            continue
        k = _int(h2.get("k"))
        if k is None or not h2.get("task_id") or not h2.get("label"):
            continue
        meta[rec["id"]] = (str(h2["task_id"]), _mode(h2), k, str(h2["label"]))
    grouped: dict[CandidateKey, list[float]] = {}
    for key, value in values.items():
        candidate = meta.get(key.rsplit("#", 1)[0])
        if candidate is not None:
            grouped.setdefault(candidate, []).append(value)
    return {c: fmean(v) for c, v in grouped.items()}


def agreement_with_scores(
    ns: NamespaceData,
    scores: Mapping[CandidateKey, float],
    *,
    level: float = 0.95,
    higher_is_better: bool = True,
) -> list[Agreement]:
    """Per (mode, k) cell, then pooled as ``all``: how often the chosen candidate
    is among those we score best. With ``higher_is_better=False`` (a metric such as
    ``contamination-fetch``) the lowest score is best.

    Left out of the rate and listed under ``ungraded`` with a reason: a selection
    that did not complete, a chosen candidate we have no score for, fewer than two
    scored candidates (the choice is forced), and tasks where every scored
    candidate ties (any choice is "best"). A candidate we could not score is
    absent from ``scores``, never zero. Tasks recur across cells, so the pooled
    interval is narrower than the evidence warrants; read the cells.
    """
    if not higher_is_better:
        # Rank on the negated score, so "best" is the maximum either way.
        scores = {key: -value for key, value in scores.items()}
    cells: dict[str, Agreement] = {}
    pooled = Agreement("all")
    for sel in sorted(ns.selections, key=lambda s: (s.mode, s.k or 0, s.task_id)):
        name = _cell_name(sel.mode, sel.k)
        cell = cells.setdefault(name, Agreement(name))
        reason = ""
        mine: dict[str, float] = {}
        if sel.ok is False or not sel.chosen_label or sel.k is None:
            reason = "selection did not complete or names no chosen candidate"
        else:
            mine = {
                key[3]: v for key, v in scores.items() if key[:3] == (sel.task_id, sel.mode, sel.k)
            }
            if sel.chosen_label not in mine:
                reason = "no score of ours for the chosen candidate"
            elif len(mine) < 2:
                reason = "fewer than two scored candidates; the choice is forced"
            elif all(abs(v - max(mine.values())) <= EPS for v in mine.values()):
                reason = "every scored candidate ties; the choice cannot be wrong"
        if reason:
            u = Ungraded(sel.task_id, sel.mode, sel.k, reason)
            cell.ungraded.append(u)
            pooled.ungraded.append(u)
            continue
        best_label = max(mine, key=lambda lbl: mine[lbl])
        agree = mine[sel.chosen_label] >= mine[best_label] - EPS
        for target in (cell, pooled):
            target.compared += 1
            target.agreed += agree
        if not agree:
            line = f"{sel.task_id} {name}: chose {sel.chosen_label}, best {best_label}"
            cell.disagreements.append(line)
            pooled.disagreements.append(line)
    return _finish(cells, pooled, level)


def agreement_with_base(ns: NamespaceData, *, level: float = 0.95) -> list[Agreement]:
    """The judged.json-only reading: how often the chosen run scores at least the
    mean of the task's judged base executions.

    ``judged.json`` scores only the chosen run, so this does not say whether the
    best candidate was chosen. It says whether adopting the refined harness was
    no loss against base by Harness²'s own judge. Use ``agreement_with_scores``
    for selection agreement. A row whose chosen run is itself a base run, was not
    graded, or has no graded base execution is listed as ungraded.
    """
    base: dict[str, list[float]] = {}
    for b in ns.base:
        if b.frac is not None:
            base.setdefault(b.task_id, []).append(b.frac)
    cells: dict[str, Agreement] = {}
    pooled = Agreement("all")
    for row in sorted(ns.judged, key=lambda r: (r.mode, r.k or 0, r.task_id)):
        name = _cell_name(row.mode, row.k)
        cell = cells.setdefault(name, Agreement(name))
        reason = ""
        if row.chosen_is_base:
            reason = "the chosen run is a base run; no refined score"
        elif row.frac is None:
            reason = row.error or "the chosen run was not graded"
        elif row.task_id not in base:
            reason = "no graded base execution for this task"
        if reason:
            u = Ungraded(row.task_id, row.mode, row.k, reason)
            cell.ungraded.append(u)
            pooled.ungraded.append(u)
            continue
        assert row.frac is not None
        base_mean = fmean(base[row.task_id])
        ok = row.frac >= base_mean - EPS
        for target in (cell, pooled):
            target.compared += 1
            target.agreed += ok
        if not ok:
            line = f"{row.task_id} {name}: chosen {row.frac:.3f} < base {base_mean:.3f}"
            cell.disagreements.append(line)
            pooled.disagreements.append(line)
    return _finish(cells, pooled, level)


# --- judge versus human labels ---


@dataclass
class LabelAgreement:
    """Agreement of two raters over the same items."""

    n: int
    agreed: int
    raw_agreement: float
    # None when chance agreement is 1 (the raters used one shared label throughout).
    kappa: float | None
    expected_agreement: float
    low: float
    high: float
    level: float = 0.95
    interval_method: str = "wilson"

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def label_agreement(
    judge: Sequence[Hashable], human: Sequence[Hashable], *, level: float = 0.95
) -> LabelAgreement:
    """Raw agreement (with a Wilson interval) and Cohen's kappa between the
    judge's labels and the human labels, item by item.

    Raises ValueError for lists of different lengths or no items. The caller
    pairs the items and drops any the judge failed on, rather than labelling them.
    """
    if len(judge) != len(human):
        raise ValueError(f"label lists differ in length: {len(judge)} and {len(human)}")
    n = len(judge)
    if n == 0:
        raise ValueError("no labels to compare")
    agreed = sum(a == b for a, b in zip(judge, human, strict=True))
    pj, ph = Counter(judge), Counter(human)
    expected = sum(pj[c] * ph[c] for c in pj.keys() & ph.keys()) / (n * n)
    observed = agreed / n
    kappa = None if expected >= 1.0 - EPS else (observed - expected) / (1.0 - expected)
    ci = wilson(agreed, n, level)
    return LabelAgreement(n, agreed, observed, kappa, expected, ci.low, ci.high, level)

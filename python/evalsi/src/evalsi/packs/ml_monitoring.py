"""The ``ml-monitoring`` pack: drift, data quality and fairness, without numpy.

Fields are read from records by path: ``input``, ``output``, ``reference``
(their text, or a number when it parses as one), or a metadata key,
optionally dotted (``metadata.age``, ``age`` and ``user.age`` all work).

- ``drift`` compares a feature's distribution in the evaluated records with
  a reference window: another JSONL file (``reference_data``), or the
  records whose ``window_field`` is ``reference_window``. Numeric features
  get the population stability index over the reference's deciles, the
  two-sample Kolmogorov-Smirnov statistic and its p-value, and the
  Jensen-Shannon divergence; categorical ones PSI and JS over categories.
- ``data-quality`` checks fields against a small schema (type, range,
  allowed values, required) and counts duplicate records.
- ``fairness`` compares rates across the groups of ``group_field``:
  selection rate (demographic parity), true positive rate (equal
  opportunity) and false positive rate (equalized odds).
"""

from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from evalsi.evaluator import MetricSpec, Requirements, Scope, ScoreType, SkipRecord, evaluator
from evalsi.packs.ml_classic import _label
from evalsi.registry import Pack
from evalsi.types import Content, Record, Score

NUMBER = ScoreType.NUMBER
PASSED = ScoreType.PASSED
NOTHING = Requirements(output=False)


# --- reading fields ------------------------------------------------------------------


def _scalar(value: Any) -> Any:
    if isinstance(value, Content):
        if value.is_json and not isinstance(value.json, dict | list):
            return value.json
        value = value.as_text()
    if isinstance(value, str):
        try:
            return float(value)
        except ValueError:
            return value
    return value


def _dig(data: Any, path: Sequence[str]) -> Any:
    for part in path:
        if not isinstance(data, dict) or part not in data:
            return None
        data = data[part]
    return data


def field_value(record: Record, path: str, *, coerce: bool = True) -> Any:
    """A record's field by path (see the module docstring); None when absent.

    Numeric-looking strings become numbers unless coerce is False, which
    keeps metadata as stored (for type checks)."""
    if path in ("input", "output", "reference"):
        return _scalar(getattr(record, path))
    parts = path.split(".")
    if parts[0] == "metadata":
        parts = parts[1:]
    found = _dig(record.metadata, parts)
    return _scalar(found) if coerce else found


def _row_value(row: dict[str, Any], path: str) -> Any:
    """The same path in a raw JSONL row (top level, or under metadata)."""
    parts = path.split(".")
    if parts[0] in ("input", "output", "reference"):
        value = row.get(parts[0])
        # Read it the way a dataset loader builds a record, so the reference
        # and the evaluated records agree ({"json": 3} is 3 in both).
        return None if value is None else _scalar(Content.from_value(value))
    if parts[0] == "metadata":
        parts = parts[1:]
    found = _dig(row.get("metadata") or {}, parts)
    if found is None:
        found = _dig(row, parts)
    return _scalar(found)


def _numeric(values: Sequence[Any]) -> bool:
    return bool(values) and all(
        isinstance(v, int | float) and not isinstance(v, bool) and math.isfinite(v) for v in values
    )


# --- drift --------------------------------------------------------------------------------


def quantile_edges(values: Sequence[float], bins: int) -> list[float]:
    """Inner bin edges at the reference's quantiles (deduplicated)."""
    s = sorted(values)
    edges: list[float] = []
    for i in range(1, bins):
        q = s[min(len(s) - 1, math.floor(i * len(s) / bins))]
        if not edges or q > edges[-1]:
            edges.append(q)
    return edges


def _bin(value: float, edges: Sequence[float]) -> int:
    lo, hi = 0, len(edges)
    while lo < hi:
        mid = (lo + hi) // 2
        if value < edges[mid]:
            hi = mid
        else:
            lo = mid + 1
    return lo


def _shares(counts: Counter[Any], keys: Sequence[Any], eps: float) -> list[float]:
    total = sum(counts.values())
    raw = [(counts.get(k, 0) / total if total else 0.0) for k in keys]
    smoothed = [max(p, eps) for p in raw]
    norm = sum(smoothed)
    return [p / norm for p in smoothed]


def psi(expected: Sequence[float], actual: Sequence[float]) -> float:
    return sum((a - e) * math.log(a / e) for e, a in zip(expected, actual, strict=True))


def js_divergence(p: Sequence[float], q: Sequence[float]) -> float:
    """Jensen-Shannon divergence in bits (0 to 1)."""
    m = [(a + b) / 2 for a, b in zip(p, q, strict=True)]

    def kl(x: Sequence[float], y: Sequence[float]) -> float:
        return sum(a * math.log2(a / b) for a, b in zip(x, y, strict=True) if a > 0)

    return (kl(p, m) + kl(q, m)) / 2


def ks_2samp(a: Sequence[float], b: Sequence[float]) -> tuple[float, float]:
    """Two-sample Kolmogorov-Smirnov statistic and asymptotic two-sided p-value."""
    xs, ys = sorted(a), sorted(b)
    n, m = len(xs), len(ys)
    i = j = 0
    d = 0.0
    while i < n and j < m:
        v = min(xs[i], ys[j])
        while i < n and xs[i] == v:
            i += 1
        while j < m and ys[j] == v:
            j += 1
        d = max(d, abs(i / n - j / m))
    if d == 0:
        return 0.0, 1.0  # identical distributions; the series below does not converge at 0
    # The Kolmogorov distribution with Stephens' small-sample correction.
    en = math.sqrt(n * m / (n + m))
    lam = (en + 0.12 + 0.11 / en) * d
    p = 0.0
    for k in range(1, 101):
        term = 2 * (-1) ** (k - 1) * math.exp(-2 * k * k * lam * lam)
        p += term
        if abs(term) < 1e-10:
            break
    return d, min(max(p, 0.0), 1.0)


def _reference_values(
    records: list[Record],
    feature: str,
    reference_data: str,
    window_field: str,
    reference_window: str,
) -> tuple[list[Any], list[Any]]:
    current: list[Any] = []
    reference: list[Any] = []
    if reference_data:
        path = Path(reference_data)
        if not path.is_file():
            raise ValueError(f"reference_data {reference_data} is not a file")
        with path.open(encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    v = _row_value(json.loads(line), feature)
                    if v is not None:
                        reference.append(v)
        current = [v for r in records if (v := field_value(r, feature)) is not None]
    else:
        for r in records:
            v = field_value(r, feature)
            if v is None:
                continue
            window = field_value(r, window_field)
            if str(window) == reference_window:
                reference.append(v)
            else:
                current.append(v)
    return reference, current


@evaluator(
    name="builtin/drift",
    version="1.0.0",
    description=(
        "Distribution drift of one feature against a reference window (reference_data, a "
        "JSONL file, or records whose window_field is reference_window): PSI, the KS statistic "
        "and p-value (numeric features), Jensen-Shannon divergence, and drifted when PSI "
        "exceeds psi_threshold or the KS p-value is below alpha."
    ),
    scope=Scope.DATASET,
    requires=NOTHING,
    outputs=[
        MetricSpec("psi", NUMBER, min=0, higher_is_better=False),
        MetricSpec("ks", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("ks-pvalue", NUMBER, min=0, max=1),
        MetricSpec("js-divergence", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("drifted", PASSED, higher_is_better=False),
    ],
)
def drift(
    records: list[Record],
    *,
    feature: str,
    reference_data: str = "",
    window_field: str = "window",
    reference_window: str = "reference",
    bins: int = 10,
    psi_threshold: float = 0.2,
    alpha: float = 0.01,
    eps: float = 1e-4,
) -> list[Score]:
    reference, current = _reference_values(
        records, feature, reference_data, window_field, reference_window
    )
    if not reference or not current:
        raise SkipRecord(f"{feature}: needs values in both the reference and current windows")
    meta: dict[str, Any] = {
        "feature": feature,
        "reference": len(reference),
        "current": len(current),
    }
    ks = pvalue = None
    if _numeric(reference) and _numeric(current):
        edges = quantile_edges(reference, bins)
        keys: list[Any] = list(range(len(edges) + 1))
        ref = _shares(Counter(_bin(v, edges) for v in reference), keys, eps)
        cur = _shares(Counter(_bin(v, edges) for v in current), keys, eps)
        ks, pvalue = ks_2samp(reference, current)
        meta["kind"] = "numeric"
    else:
        keys = sorted({str(v) for v in reference} | {str(v) for v in current})
        ref = _shares(Counter(str(v) for v in reference), keys, eps)
        cur = _shares(Counter(str(v) for v in current), keys, eps)
        meta["kind"] = "categorical"
    p = psi(ref, cur)
    drifted = p > psi_threshold or (pvalue is not None and pvalue < alpha)
    out = [Score(name="psi", number=p, metadata=meta)]
    if ks is not None and pvalue is not None:
        out += [Score(name="ks", number=ks), Score(name="ks-pvalue", number=pvalue)]
    out += [
        Score(name="js-divergence", number=js_divergence(ref, cur)),
        Score(name="drifted", passed=drifted),
    ]
    return out


# --- data quality ---------------------------------------------------------------------------

_TYPES = {
    "number": lambda v: isinstance(v, int | float) and not isinstance(v, bool),
    "integer": lambda v: (
        (isinstance(v, int) and not isinstance(v, bool))
        or (isinstance(v, float) and v.is_integer())
    ),
    "string": lambda v: isinstance(v, str),
    "boolean": lambda v: isinstance(v, bool),
}


def _invalid(value: Any, rule: dict[str, Any]) -> str:
    kind = rule.get("type")
    if kind and kind in _TYPES and not _TYPES[kind](value):
        return f"not a {kind}"
    if (
        "allowed" in rule
        and value not in rule["allowed"]
        and str(value) not in map(str, rule["allowed"])
    ):
        return "not an allowed value"
    if isinstance(value, int | float) and not isinstance(value, bool):
        if rule.get("min") is not None and value < rule["min"]:
            return "below min"
        if rule.get("max") is not None and value > rule["max"]:
            return "above max"
    if (
        isinstance(value, str)
        and rule.get("max_length") is not None
        and len(value) > rule["max_length"]
    ):
        return "too long"
    return ""


@evaluator(
    name="builtin/data-quality",
    version="1.0.0",
    description=(
        "Data quality against a schema {field: {type, min, max, allowed, max_length, "
        "required}}: the share of required values missing, of present values invalid, and "
        "of records duplicating an earlier one (on duplicate_fields, default the input)."
    ),
    scope=Scope.DATASET,
    requires=NOTHING,
    outputs=[
        MetricSpec("missing-rate", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("invalid-rate", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("duplicate-rate", NUMBER, min=0, max=1, higher_is_better=False),
    ],
)
def data_quality(
    records: list[Record],
    *,
    schema: dict[str, Any],
    duplicate_fields: list[str] | None = None,
) -> list[Score]:
    if not records:
        raise SkipRecord("no records")
    if not isinstance(schema, dict) or not schema:
        raise ValueError("schema must map fields to rules")
    missing = invalid = required_cells = present_cells = 0
    per_field: dict[str, Counter[str]] = defaultdict(Counter)
    for r in records:
        for field, given in schema.items():
            rule = given if isinstance(given, dict) else {"type": given}
            # Metadata as stored: "00123" is a string, and "30" is not a number.
            value = field_value(r, field, coerce=False)
            if value is None or value == "":
                if rule.get("required", True):
                    required_cells += 1
                    missing += 1
                    per_field[field]["missing"] += 1
                continue
            if rule.get("required", True):
                required_cells += 1
            present_cells += 1
            why = _invalid(value, rule)
            if why:
                invalid += 1
                per_field[field][why] += 1
    keys = duplicate_fields or ["input"]
    seen: set[str] = set()
    duplicates = 0
    for r in records:
        key = json.dumps([field_value(r, f) for f in keys], sort_keys=True, default=str)
        if key in seen:
            duplicates += 1
        seen.add(key)
    meta = {"records": len(records), "fields": {k: dict(v) for k, v in per_field.items()}}
    return [
        Score(
            name="missing-rate",
            number=missing / required_cells if required_cells else 0.0,
            metadata=meta,
        ),
        Score(name="invalid-rate", number=invalid / present_cells if present_cells else 0.0),
        Score(name="duplicate-rate", number=duplicates / len(records)),
    ]


# --- fairness ----------------------------------------------------------------------------------


@evaluator(
    name="builtin/fairness",
    version="1.0.0",
    description=(
        "Group fairness of a classifier across metadata[group_field]: the largest gap in "
        "selection rate (demographic parity difference, and the min/max ratio), in true "
        "positive rate (equal opportunity) and in TPR or FPR (equalized odds)."
    ),
    scope=Scope.DATASET,
    outputs=[
        MetricSpec("demographic-parity-difference", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("demographic-parity-ratio", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("equal-opportunity-difference", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("equalized-odds-difference", NUMBER, min=0, max=1, higher_is_better=False),
    ],
)
def fairness(
    records: list[Record],
    *,
    group_field: str,
    positive_label: str,
    label_field: str = "label",
    min_group_size: int = 1,
) -> list[Score]:
    positive = str(positive_label)
    stats: dict[str, Counter[str]] = defaultdict(Counter)
    for r in records:
        group = field_value(r, group_field)
        pred = _label(r.output, label_field)
        if group is None or pred is None:
            continue
        g = stats[str(group)]
        g["n"] += 1
        g["selected"] += pred == positive
        truth = _label(r.reference, label_field)
        if truth is not None:
            if truth == positive:
                g["pos"] += 1
                g["tp"] += pred == positive
            else:
                g["neg"] += 1
                g["fp"] += pred == positive
    groups = {k: v for k, v in stats.items() if v["n"] >= max(min_group_size, 1)}
    if len(groups) < 2:
        raise SkipRecord(f"needs at least two groups of {group_field} with predictions")
    rates: dict[str, dict[str, Any]] = {
        k: {
            "n": v["n"],
            "selection_rate": v["selected"] / v["n"],
            "tpr": v["tp"] / v["pos"] if v["pos"] else None,
            "fpr": v["fp"] / v["neg"] if v["neg"] else None,
        }
        for k, v in groups.items()
    }

    def gap(key: str) -> float | None:
        vals: list[float] = [r[key] for r in rates.values() if r[key] is not None]
        return max(vals) - min(vals) if len(vals) >= 2 else None

    sel: list[float] = [r["selection_rate"] for r in rates.values()]
    out = [
        Score(
            name="demographic-parity-difference",
            number=max(sel) - min(sel),
            metadata={"groups": rates},
        ),
        Score(name="demographic-parity-ratio", number=min(sel) / max(sel) if max(sel) else 1.0),
    ]
    tpr_gap, fpr_gap = gap("tpr"), gap("fpr")
    if tpr_gap is not None:
        out.append(Score(name="equal-opportunity-difference", number=tpr_gap))
    if tpr_gap is not None or fpr_gap is not None:
        out.append(
            Score(
                name="equalized-odds-difference",
                number=max(x for x in (tpr_gap, fpr_gap) if x is not None),
            )
        )
    return out


PACK = Pack(
    name="ml-monitoring",
    description=(
        "Monitoring classic models: feature drift (PSI, KS, Jensen-Shannon), data quality "
        "against a schema, and group fairness (demographic parity, equal opportunity, "
        "equalized odds)."
    ),
    evaluators=[drift, data_quality, fairness],
)

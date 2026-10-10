"""Evaluation results, metric summaries and the run manifest."""

from __future__ import annotations

import json
from collections.abc import Hashable, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from statistics import fmean, stdev
from typing import Any

from evalsi.evaluator import BoundEvaluator, MetricSpec, Scope, ScoreType
from evalsi.stats import Interval, interval
from evalsi.types import EvaluationResult, Outcome, Record, Score

KIND_BY_TYPE = {
    ScoreType.NUMBER: "number",
    ScoreType.PASSED: "proportion",
    ScoreType.LABEL: "label",
}


@dataclass
class MetricSummary:
    metric: str
    evaluator: str
    kind: str
    n: int = 0
    mean: float | None = None
    std: float | None = None
    ci: Interval | None = None
    skipped: int = 0
    errors: int = 0
    labels: dict[str, int] = field(default_factory=dict)
    higher_is_better: bool | None = None
    # Number of clusters behind a clustered interval. Few clusters mean a wide interval.
    clusters: int | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "metric": self.metric,
            "evaluator": self.evaluator,
            "kind": self.kind,
            "n": self.n,
            "mean": self.mean,
            "std": self.std,
            "skipped": self.skipped,
            "errors": self.errors,
        }
        if self.ci is not None:
            out["ci"] = {
                "low": self.ci.low,
                "high": self.ci.high,
                "level": self.ci.level,
                "method": self.ci.method,
            }
        if self.labels:
            out["labels"] = self.labels
        if self.higher_is_better is not None:
            out["higher_is_better"] = self.higher_is_better
        if self.clusters is not None:
            out["clusters"] = self.clusters
        return out


def metric_key(instance: BoundEvaluator, score_name: str) -> str:
    if not score_name or score_name == instance.spec.short_name:
        return instance.name
    return f"{instance.name}.{score_name}"


def result_metric(evaluator: str, evaluator_ref: str, score_name: str) -> str:
    """:func:`metric_key` for a stored result: ``evaluator`` is the instance
    name (an alias, perhaps) and ``evaluator_ref`` the manifest it ran
    (``builtin/exact-match@1.0.0``). Without a ref, a score named like the
    instance is its primary metric."""
    short = evaluator_ref.split("@", 1)[0].rsplit("/", 1)[-1] if evaluator_ref else evaluator
    if not score_name or score_name == short:
        return evaluator
    return f"{evaluator}.{score_name}"


def record_trial(record: Mapping[str, Any]) -> int | None:
    """The trial whose output a record (in its JSON form) is, from
    ``provenance.run``; ``None`` for a dataset record, the same in every
    trial."""
    run = (record.get("provenance") or {}).get("run")
    if not isinstance(run, Mapping):
        return None
    return int(run.get("trial", 0) or 0)


def summarize(
    instances: Sequence[BoundEvaluator],
    results: Sequence[EvaluationResult],
    records: Sequence[Record],
    *,
    level: float = 0.95,
    cluster_by: str | None = None,
    ci_method: str = "auto",
    trials: int = 1,
) -> list[MetricSummary]:
    """Summarize results per metric.

    With ``trials`` above 1 every record appears several times, so intervals
    cluster by record unless ``cluster_by`` says otherwise, and pass/fail
    metrics also get ``<metric>.pass@k`` (passed in at least one trial) and
    ``<metric>.pass^k`` (passed in every trial).
    """
    if trials > 1 and not cluster_by:
        cluster_by = RECORD_CLUSTER
    by_id = {r.id: r for r in records}
    summaries: list[MetricSummary] = []
    for instance in instances:
        mine = [r for r in results if r.evaluator == instance.name]
        skipped = sum(r.outcome is Outcome.SKIPPED for r in mine)
        errors = sum(r.outcome is Outcome.ERROR for r in mine)
        # Declared metrics first, so they appear even when every record was skipped.
        values: dict[str, list[tuple[Score, str]]] = {
            metric_key(instance, o.name): [] for o in instance.spec.outputs
        }
        specs: dict[str, MetricSpec | None] = {
            metric_key(instance, o.name): o for o in instance.spec.outputs
        }
        for result in mine:
            for score in result.scores:
                key = metric_key(instance, score.name)
                values.setdefault(key, []).append((score, result.record_id))
                specs.setdefault(key, instance.spec.output(score.name))
        for key, scored in values.items():
            summary = _summarize_metric(
                key,
                instance,
                specs[key],
                scored,
                by_id,
                level=level,
                cluster_by=cluster_by,
                ci_method=ci_method,
            )
            if summary is not None:
                summary.skipped, summary.errors = skipped, errors
                summaries.append(summary)
                if trials > 1 and summary.kind == "proportion":
                    summaries.extend(_pass_at_k(key, instance.name, scored, trials, level))
    return summaries


def _pass_at_k(
    key: str, evaluator: str, scored: list[tuple[Score, str]], k: int, level: float
) -> list[MetricSummary]:
    """pass@k: passed in at least one trial; pass^k: passed in every trial."""
    by_record: dict[str, list[bool]] = {}
    for score, rid in scored:
        if score.passed is not None:
            by_record.setdefault(rid, []).append(score.passed)
    out = []
    for name, reduce in ((f"pass@{k}", any), (f"pass^{k}", all)):
        values = [1.0 if reduce(v) else 0.0 for v in by_record.values()]
        summary = MetricSummary(
            metric=f"{key}.{name}", evaluator=evaluator, kind="proportion", higher_is_better=True
        )
        summary.n = len(values)
        if values:
            summary.mean = fmean(values)
            summary.std = stdev(values) if len(values) > 1 else None
            summary.ci = interval(values, proportion=True, level=level)
        out.append(summary)
    return out


def _summarize_metric(
    key: str,
    instance: BoundEvaluator,
    spec: MetricSpec | None,
    scored: list[tuple[Score, str]],
    records: dict[str, Record],
    *,
    level: float,
    cluster_by: str | None,
    ci_method: str,
) -> MetricSummary | None:
    score_type = spec.type if spec is not None else _infer_type(scored)
    if score_type not in KIND_BY_TYPE:
        return None  # structured scores are kept per record, not aggregated
    summary = MetricSummary(
        metric=key,
        evaluator=instance.name,
        kind=KIND_BY_TYPE[score_type],
        higher_is_better=spec.higher_is_better if spec is not None else None,
    )
    if score_type is ScoreType.LABEL:
        for score, _ in scored:
            label = str(score.label)
            summary.labels[label] = summary.labels.get(label, 0) + 1
        summary.n = len(scored)
        return summary
    pairs = [(v, rid) for s, rid in scored if (v := s.numeric()) is not None]
    numbers = [v for v, _ in pairs]
    summary.n = len(numbers)
    if not numbers:
        return summary
    summary.mean = fmean(numbers)
    summary.std = stdev(numbers) if len(numbers) > 1 else None
    if instance.spec.scope is Scope.DATASET:
        return summary  # one value computed over the whole dataset; no sampling interval
    clusters: list[Hashable] | None = None
    if cluster_by:
        clusters = [_cluster_of(records.get(rid), cluster_by, rid) for _, rid in pairs]
    ci = interval(
        numbers,
        proportion=score_type is ScoreType.PASSED,
        level=level,
        clusters=clusters,
        method=ci_method,
    )
    if ci is not None:
        if score_type is ScoreType.PASSED:
            ci = ci.clipped(0.0, 1.0)
        elif spec is not None:
            ci = ci.clipped(spec.min, spec.max)
    summary.ci = ci
    if clusters is not None:
        summary.clusters = len(set(clusters))
    return summary


def _infer_type(scored: list[tuple[Score, str]]) -> ScoreType:
    first = scored[0][0] if scored else None
    if first is None or first.number is not None:
        return ScoreType.NUMBER
    if first.passed is not None:
        return ScoreType.PASSED
    if first.label is not None:
        return ScoreType.LABEL
    return ScoreType.STRUCTURED


RECORD_CLUSTER = "@record"


def _cluster_of(record: Record | None, key: str, record_id: str) -> Hashable:
    if key == RECORD_CLUSTER:
        return ("record", record_id)
    key = key.removeprefix("metadata.")
    if record is None or key not in record.metadata:
        return ("record", record_id)  # unclustered records count as their own cluster
    value = record.metadata[key]
    return value if isinstance(value, Hashable) else json.dumps(value, sort_keys=True)


@dataclass
class EvalResult:
    records: list[Record]
    results: list[EvaluationResult]
    summaries: list[MetricSummary]
    manifest: dict[str, Any]
    # The trial that produced each of ``records``, when they are a run's
    # outputs (one per record and trial); empty for dataset records.
    record_trials: list[int] = field(default_factory=list)

    def metric(self, name: str) -> MetricSummary:
        for summary in self.summaries:
            if summary.metric == name:
                return summary
        raise KeyError(f"no metric {name!r}; have {[s.metric for s in self.summaries]}")

    def errors(self) -> list[EvaluationResult]:
        return [r for r in self.results if r.outcome is Outcome.ERROR]

    def to_dict(self) -> dict[str, Any]:
        return {
            "manifest": self.manifest,
            "summaries": [s.to_dict() for s in self.summaries],
            "results": [r.to_dict() for r in self.results],
            "records": [self._record_dict(i, r) for i, r in enumerate(self.records)],
        }

    def _record_dict(self, i: int, record: Record) -> dict[str, Any]:
        out = record.to_dict()
        if i < len(self.record_trials):
            # The server's shape (Record.provenance.run.trial), so reports and
            # analytics pair each result with the output it graded.
            out["provenance"] = {"run": {"trial": self.record_trials[i]}}
        return out

    def save(self, path: str | Path) -> None:
        """Write the manifest, summaries and every per-record result as JSON."""
        Path(path).write_text(
            json.dumps(self.to_dict(), indent=2, ensure_ascii=False, default=str) + "\n",
            encoding="utf-8",
        )

    def table(self) -> str:
        headers = ["metric", "n", "mean", f"{_level(self.summaries)} CI", "skipped", "errors"]
        rows = [_row(s) for s in self.summaries]
        widths = [max(len(str(c)) for c in col) for col in zip(headers, *rows, strict=False)]
        right = {1, 4, 5}
        lines = []
        for row in [headers, *rows]:
            cells = [
                str(c).rjust(w) if i in right else str(c).ljust(w)
                for i, (c, w) in enumerate(zip(row, widths, strict=True))
            ]
            lines.append("  ".join(cells).rstrip())
        return "\n".join(lines)


def _level(summaries: Sequence[MetricSummary]) -> str:
    level = next((s.ci.level for s in summaries if s.ci is not None), 0.95)
    return f"{level:.0%}"


def _fmt(value: float) -> str:
    return f"{value:,.0f}" if abs(value) >= 1000 else f"{value:.3f}"


def _row(s: MetricSummary) -> list[str]:
    if s.kind == "label":
        counts = ", ".join(f"{k}={v}" for k, v in sorted(s.labels.items()))
        return [s.metric, str(s.n), "-", counts or "-", str(s.skipped), str(s.errors)]
    mean = _fmt(s.mean) if s.mean is not None else "-"
    ci = f"[{_fmt(s.ci.low)}, {_fmt(s.ci.high)}]" if s.ci is not None else "-"
    return [s.metric, str(s.n), mean, ci, str(s.skipped), str(s.errors)]

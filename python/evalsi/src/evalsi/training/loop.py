"""Evaluating checkpoints: one run per checkpoint, keyed by (training run, step).

:class:`CheckpointEvaluator` serves a checkpoint (see
:mod:`evalsi.training.serving`), runs a run spec against it with the target
pointed at the served model, and records the result. Embedded, results go to
``<log_dir>/<training_run>/step-<step>.json``; with a server, each checkpoint
is a server run labelled ``training-run`` and ``training-step``, so the
history lives with the server's runs.

The learning curve compares every step with the base model (step ``base``,
from :meth:`CheckpointEvaluator.evaluate_base`), record by record: the
difference of means with a paired confidence interval. A regression gate
fails when a metric is significantly lower than the base model's (the
interval excludes zero) by more than ``max_drop``.
"""

from __future__ import annotations

import copy
import json
import math
import time
from collections.abc import Sequence
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from evalsi.judges import JudgeClient, JudgeConfig
from evalsi.runner import _run_sync
from evalsi.runspec import RunFile, load_spec, spec_to_dict
from evalsi.stats import t_interval
from evalsi.training.serving import Served, Serving
from evalsi.types import Outcome

TRAINING_RUN_LABEL = "training-run"
STEP_LABEL = "training-step"
BASE = "base"


@dataclass(frozen=True)
class RegressionGate:
    metric: str
    # Tolerated drop below the base model before a significant drop fails.
    max_drop: float = 0.0

    @classmethod
    def parse(cls, text: str) -> RegressionGate:
        """``metric`` or ``metric:max_drop``."""
        metric, _, drop = text.partition(":")
        if not metric:
            raise ValueError(f"regression gate {text!r} needs a metric")
        return cls(metric, float(drop) if drop else 0.0)


@dataclass
class Comparison:
    """A step against the base model on one metric."""

    metric: str
    base_mean: float | None
    mean: float | None
    # Mean per-record difference (step - base) over shared records and trials.
    diff: float | None = None
    diff_low: float | None = None
    diff_high: float | None = None
    paired_n: int = 0
    significant: bool = False
    regressed: bool = False


@dataclass
class StepResult:
    training_run: str
    step: str
    checkpoint: str
    # metric -> {"mean", "low", "high", "n"}
    summaries: dict[str, dict[str, Any]] = field(default_factory=dict)
    gates_passed: bool = True
    run_id: str = ""
    # Embedded only: metric -> {"record#trial": value}, for paired comparisons.
    values: dict[str, dict[str, float]] = field(default_factory=dict)
    comparisons: list[Comparison] = field(default_factory=list)
    error: str = ""
    created_at: float = field(default_factory=time.time)
    # The server run's status (RUN_STATUS_*); empty for embedded evaluations,
    # which finish before they return.
    status: str = ""

    @property
    def regressed(self) -> bool:
        return any(c.regressed for c in self.comparisons)

    @property
    def finished(self) -> bool:
        """Whether the evaluation has ended; a pending or running server run
        has not, and neither has one whose status is unknown."""
        return self.status in _FINISHED

    @property
    def passed(self) -> bool:
        return self.finished and not self.error and self.gates_passed and not self.regressed

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> StepResult:
        data = dict(data)
        data["comparisons"] = [Comparison(**c) for c in data.get("comparisons", [])]
        return cls(**data)


_FINISHED = frozenset(
    {"", "RUN_STATUS_SUCCEEDED", "RUN_STATUS_FAILED", "RUN_STATUS_ERROR", "RUN_STATUS_CANCELLED"}
)


def step_key(step: int | str) -> str:
    return str(step)


def _sort_key(step: str) -> tuple[int, int]:
    return (-1, 0) if step == BASE else (0, int(step)) if step.isdigit() else (1, 0)


def compare(
    base: StepResult, step: StepResult, gates: Sequence[RegressionGate], level: float = 0.95
) -> list[Comparison]:
    """Paired comparisons of ``step`` against ``base`` on every shared metric."""
    limits = {g.metric: g.max_drop for g in gates}
    out = []
    for metric in sorted(set(base.summaries) & set(step.summaries)):
        c = Comparison(
            metric=metric,
            base_mean=base.summaries[metric].get("mean"),
            mean=step.summaries[metric].get("mean"),
        )
        a, b = base.values.get(metric, {}), step.values.get(metric, {})
        shared = sorted(set(a) & set(b))
        if shared:
            diffs = [b[k] - a[k] for k in shared]
            c.diff = sum(diffs) / len(diffs)
            c.paired_n = len(diffs)
            interval = t_interval(diffs, level) if len(diffs) > 1 else None
            if interval is not None and not math.isnan(interval.low):
                c.diff_low, c.diff_high = interval.low, interval.high
                c.significant = interval.low > 0 or interval.high < 0
        if metric in limits and c.diff is not None:
            c.regressed = c.significant and c.diff < 0 and -c.diff > limits[metric]
        out.append(c)
    return out


class CheckpointEvaluator:
    """Runs a spec against checkpoints and keeps their learning curve."""

    def __init__(
        self,
        spec: str | Path | RunFile,
        *,
        training_run: str,
        serving: Serving,
        log_dir: str | Path = "evalsi-training",
        server: str | None = None,
        project: str = "",
        token: str | None = None,
        api_key: str | None = None,
        regression: Sequence[RegressionGate | str] = (),
        judge: JudgeConfig | JudgeClient | None = None,
        concurrency: int = 8,
    ) -> None:
        self.run_file = spec if isinstance(spec, RunFile) else load_spec(spec)
        if self.run_file.spec.target.HasField("agent"):
            raise ValueError(
                "checkpoint evaluation serves the checkpoint as the run's model target; "
                "this spec's target is a bring-your-own agent"
            )
        self.training_run = training_run
        self.serving = serving
        self.log_dir = Path(log_dir) / training_run
        self.server = server
        self.project = project or self.run_file.project
        self._token, self._api_key = token, api_key
        self.regression = [RegressionGate.parse(g) if isinstance(g, str) else g for g in regression]
        self.judge = judge
        self.concurrency = concurrency

    # --- evaluation ---

    def evaluate_base(self, model: str) -> StepResult:
        """Evaluate the base model (a path or a model name the serving takes)."""
        return self.evaluate(model, BASE)

    def evaluate(self, checkpoint: str, step: int | str) -> StepResult:
        """Serve ``checkpoint``, run the spec against it, and record the result."""
        key = step_key(step)
        name = f"{self.training_run}-{key}"
        try:
            with self.serving.serve(checkpoint, name) as served:
                result = (
                    self._on_server(served, key, checkpoint)
                    if self.server
                    else self._embedded(served, key, checkpoint)
                )
        except Exception as exc:
            result = StepResult(
                self.training_run, key, checkpoint, error=f"{type(exc).__name__}: {exc}"
            )
        if key != BASE and not result.error and result.finished:
            base = self.get(BASE)
            if base is not None and not base.error and base.finished:
                result.comparisons = self._compare(base, result)
        if not self.server:
            self._save(result)
        return result

    def _spec_for(self, served: Served) -> RunFile:
        run = copy.deepcopy(self.run_file)
        target = run.spec.target
        target.connector = "openai-compatible"
        target.base_url = served.base_url
        target.model = served.model
        return run

    def _embedded(self, served: Served, step: str, checkpoint: str) -> StepResult:
        from evalsi.run import execute

        run = self._spec_for(served)
        outcome = _run_sync(execute(run, judge=self.judge, concurrency=self.concurrency))
        summaries = {s.metric: _summary(s.to_dict()) for s in outcome.result.summaries}
        values: dict[str, dict[str, float]] = {}
        for r in outcome.result.results:
            if r.outcome is not Outcome.SCORED or not r.record_id:
                continue
            for score in r.scores:
                metric = r.evaluator
                if (
                    score.name
                    and score.name != r.evaluator
                    and f"{metric}.{score.name}" in summaries
                ):
                    metric = f"{metric}.{score.name}"
                value = score.numeric()
                if value is not None and metric in summaries:
                    values.setdefault(metric, {})[f"{r.record_id}#{r.trial}"] = value
        return StepResult(
            self.training_run,
            step,
            checkpoint,
            summaries=summaries,
            gates_passed=outcome.passed,
            values=values,
        )

    def _client(self) -> Any:
        from evalsi.client import Client

        assert self.server
        return Client(self.server, token=self._token, api_key=self._api_key)

    def _on_server(self, served: Served, step: str, checkpoint: str) -> StepResult:
        run_file = self._spec_for(served)
        labels = {**run_file.labels, TRAINING_RUN_LABEL: self.training_run, STEP_LABEL: step}
        with self._client() as client:
            run = client.create_run(
                spec_to_dict(run_file.spec),
                name=f"{self.training_run}-{step}",
                project=self.project,
                labels=labels,
            )
            for event in client.watch_run(run["id"]):
                if "run" in event:
                    run = event["run"]
            # A watch that ended early (a dropped stream) is not a result.
            if run.get("status", "") not in _FINISHED - {""}:
                run = client.get_run(run["id"])
        return _from_run(run, checkpoint)

    def _compare(self, base: StepResult, step: StepResult) -> list[Comparison]:
        if not self.server:
            return compare(base, step, self.regression)
        limits = {g.metric: g.max_drop for g in self.regression}
        with self._client() as client:
            rows = client.compare_runs(base.run_id, step.run_id)
        out = []
        for row in rows:
            ci = row.get("diffCi") or {}
            c = Comparison(
                metric=row["metric"],
                base_mean=row.get("baselineMean"),
                mean=row.get("candidateMean"),
                diff=row.get("diff"),
                diff_low=ci.get("low"),
                diff_high=ci.get("high"),
                paired_n=int(row.get("pairedN", 0)),
                significant=bool(row.get("significant", False)),
            )
            if c.metric in limits and c.diff is not None:
                c.regressed = c.significant and c.diff < 0 and -c.diff > limits[c.metric]
            out.append(c)
        return out

    # --- history ---

    def _save(self, result: StepResult) -> None:
        self.log_dir.mkdir(parents=True, exist_ok=True)
        path = self.log_dir / f"step-{result.step}.json"
        path.write_text(json.dumps(result.to_dict(), indent=1, default=str) + "\n")

    def get(self, step: int | str) -> StepResult | None:
        key = step_key(step)
        return next((r for r in self.history() if r.step == key), None)

    def history(self) -> list[StepResult]:
        """Every evaluated step, base first, then by step."""
        if self.server:
            results = self._server_history()
        else:
            results = [
                StepResult.from_dict(json.loads(p.read_text()))
                for p in self.log_dir.glob("step-*.json")
            ]
        return sorted(results, key=lambda r: _sort_key(r.step))

    def _server_history(self) -> list[StepResult]:
        latest: dict[str, StepResult] = {}
        with self._client() as client:
            token = ""
            while True:
                page = client.list_runs(project=self.project, page_token=token)
                for run in page.get("runs", []):
                    labels = run.get("labels", {})
                    if labels.get(TRAINING_RUN_LABEL) != self.training_run:
                        continue
                    step = labels.get(STEP_LABEL, "")
                    # Runs come newest first; a re-evaluated step keeps its latest run.
                    if step and step not in latest:
                        latest[step] = _from_run(run, "")
                token = page.get("nextPageToken", "")
                if not token:
                    break
        return list(latest.values())

    def curve(self) -> list[StepResult]:
        """The history with every step compared against the base model."""
        history = self.history()
        base = next((r for r in history if r.step == BASE and not r.error and r.finished), None)
        if base is not None:
            for r in history:
                if r.step != BASE and not r.error and r.finished and not r.comparisons:
                    r.comparisons = self._compare(base, r)
        return history


def _summary(data: dict[str, Any]) -> dict[str, Any]:
    ci = data.get("ci") or {}
    return {
        "mean": data.get("mean"),
        "low": ci.get("low"),
        "high": ci.get("high"),
        "n": data.get("n", 0),
    }


def _from_run(run: dict[str, Any], checkpoint: str) -> StepResult:
    labels = run.get("labels", {})
    status = run.get("status") or "RUN_STATUS_UNSPECIFIED"
    return StepResult(
        training_run=labels.get(TRAINING_RUN_LABEL, ""),
        step=labels.get(STEP_LABEL, ""),
        checkpoint=checkpoint,
        summaries={s["metric"]: _summary(s) for s in run.get("summaries", [])},
        gates_passed=status != "RUN_STATUS_FAILED",
        run_id=run.get("id", ""),
        error=(run.get("error") or status)
        if status in ("RUN_STATUS_ERROR", "RUN_STATUS_CANCELLED")
        else "",
        status=status,
    )


def format_curve(history: Sequence[StepResult], metrics: Sequence[str] = ()) -> str:
    """A plain-text learning curve: one row per step, mean and change from base per metric."""
    names = list(metrics) or sorted({m for r in history for m in r.summaries})
    headers = ["step", *names, "status"]
    rows = []
    for r in history:
        by_metric = {c.metric: c for c in r.comparisons}
        cells = [r.step]
        for m in names:
            mean = r.summaries.get(m, {}).get("mean")
            cell = "-" if mean is None else f"{mean:.3f}"
            c = by_metric.get(m)
            if c is not None and c.diff is not None:
                mark = "!" if c.regressed else ("*" if c.significant else "")
                cell += f" ({c.diff:+.3f}{mark})"
            cells.append(cell)
        if r.error:
            status = "error"
        elif not r.finished:
            status = r.status.removeprefix("RUN_STATUS_").lower() or "unfinished"
        else:
            status = "regressed" if r.regressed else ("ok" if r.gates_passed else "gates failed")
        cells.append(status)
        rows.append(cells)
    widths = [max(len(str(c)) for c in col) for col in zip(headers, *rows, strict=False)]
    lines = [
        "  ".join(str(c).ljust(w) for c, w in zip(row, widths, strict=True)).rstrip()
        for row in [headers, *rows]
    ]
    return "\n".join(lines)

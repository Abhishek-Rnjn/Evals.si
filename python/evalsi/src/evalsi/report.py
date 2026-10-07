"""Static reports of a run: one self-contained HTML page or a Markdown file.

``evalsi report results.json -o report.html`` reads what ``evalsi run``
and ``evalsi eval`` write with ``--output``; ``evalsi report --server URL
RUN_ID`` reads a server run. A report has the run's manifest, a table of
metrics with their confidence intervals, the gates, and for each metric
the records that scored worst, with their inputs, outputs and the
evaluator's explanation, so a failure can be read without opening JSON.
"""

from __future__ import annotations

import html
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content


@dataclass
class Example:
    record_id: str
    trial: int
    value: float | None
    label: str
    explanation: str
    input: str = ""
    output: str = ""
    reference: str = ""


@dataclass
class Report:
    title: str
    meta: dict[str, str] = field(default_factory=dict)
    summaries: list[dict[str, Any]] = field(default_factory=list)
    gates: list[dict[str, Any]] = field(default_factory=list)
    worst: dict[str, list[Example]] = field(default_factory=dict)
    errors: list[dict[str, str]] = field(default_factory=list)


def _text(value: Any, limit: int = 2000) -> str:
    if value is None:
        return ""
    try:
        text = Content.from_value(value).as_text()
    except ValueError:
        text = str(value)
    return text if len(text) <= limit else text[:limit] + " …"


def _num(value: Any) -> float | None:
    try:
        return None if value is None else float(value)
    except (TypeError, ValueError):
        return None


def _score_value(score: Mapping[str, Any]) -> tuple[float | None, str]:
    if "number" in score:
        return _num(score["number"]), ""
    if "passed" in score:
        return (1.0 if score["passed"] else 0.0), ("pass" if score["passed"] else "fail")
    if "label" in score:
        return None, str(score["label"])
    return None, ""


def build(
    *,
    title: str,
    meta: Mapping[str, Any],
    summaries: Iterable[Mapping[str, Any]],
    gates: Iterable[Mapping[str, Any]],
    results: Iterable[Mapping[str, Any]],
    records: Iterable[Mapping[str, Any]],
    examples: int = 5,
) -> Report:
    """A report from plain dicts (the JSON forms of results and records)."""
    report = Report(title=title, meta={k: str(v) for k, v in meta.items() if v not in (None, "")})
    for s in summaries:
        ci = s.get("ci") or {}
        report.summaries.append(
            {
                "metric": s.get("metric", ""),
                "n": int(s.get("n", 0) or 0),
                "mean": _num(s.get("mean")),
                "low": _num(ci.get("low")),
                "high": _num(ci.get("high")),
                "level": _num(ci.get("level")),
                "skipped": int(s.get("skipped", 0) or 0),
                "errors": int(s.get("errors", 0) or 0),
                "higher_is_better": s.get("higher_is_better", s.get("higherIsBetter")),
            }
        )
    report.gates = [dict(g) for g in gates]
    by_id = {str(r.get("id", "")): r for r in records}
    higher = {s["metric"]: s["higher_is_better"] for s in report.summaries}
    per_metric: dict[str, list[Example]] = {}
    for r in results:
        outcome = str(r.get("outcome", "")).lower().removeprefix("outcome_")
        rid = str(r.get("record_id", r.get("recordId", "")))
        if outcome == "error":
            report.errors.append(
                {
                    "record_id": rid,
                    "evaluator": str(r.get("evaluator", "")),
                    "reason": str(r.get("reason", "")),
                }
            )
            continue
        if outcome != "scored":
            continue
        rec = by_id.get(rid, {})
        evaluator = str(r.get("evaluator", ""))
        for score in r.get("scores") or []:
            name = str(score.get("name", ""))
            metric = evaluator if not name or name == evaluator else f"{evaluator}.{name}"
            if metric not in higher and evaluator in higher:
                metric = evaluator
            value, label = _score_value(score)
            per_metric.setdefault(metric, []).append(
                Example(
                    record_id=rid,
                    trial=int(r.get("trial", 0) or 0),
                    value=value,
                    label=label,
                    explanation=str(score.get("explanation", "")),
                    input=_text(rec.get("input")),
                    output=_text(rec.get("output")),
                    reference=_text(rec.get("reference")),
                )
            )
    for metric, items in per_metric.items():
        scored = [e for e in items if e.value is not None]
        reverse = higher.get(metric) is False
        scored.sort(key=lambda e: -(e.value or 0.0) if reverse else (e.value or 0.0))
        worst = [e for e in scored if not _is_best(e, metric, higher)][:examples]
        if worst:
            report.worst[metric] = worst
    return report


def _is_best(e: Example, metric: str, higher: Mapping[str, Any]) -> bool:
    """A pass, or a perfect score in [0, 1], is not worth showing as a failure."""
    if e.label == "pass":
        return True
    if higher.get(metric) is not False and e.value == 1.0:
        return True
    return higher.get(metric) is False and e.value == 0.0


def from_results_file(data: Mapping[str, Any], examples: int = 5) -> Report:
    """From what ``evalsi run --output`` or ``evalsi eval --output`` wrote."""
    manifest = data.get("manifest") or {}
    dataset = manifest.get("dataset") or {}
    meta = {
        "dataset": dataset.get("source"),
        "records": dataset.get("records"),
        "dataset sha256": dataset.get("sha256"),
        "evaluators": ", ".join(str(e.get("ref", "")) for e in manifest.get("evaluators") or []),
        "judge": (manifest.get("judge") or {}).get("model") if manifest.get("judge") else None,
        "trials": manifest.get("trials"),
        "started": manifest.get("started_at"),
        "finished": manifest.get("finished_at"),
        "evalsi": manifest.get("evalsi_version"),
    }
    return build(
        title=str(manifest.get("name") or "Evaluation report"),
        meta=meta,
        summaries=data.get("summaries") or [],
        gates=data.get("gates") or [],
        results=data.get("results") or [],
        records=data.get("records") or [],
        examples=examples,
    )


def from_server(client: Any, run_id: str, examples: int = 5) -> Report:
    """From a server run, its results and records."""
    run = client.get_run(run_id)
    results: list[dict[str, Any]] = []
    records: list[dict[str, Any]] = []
    token = ""
    while True:
        page = client.list_run_results(run_id, page_token=token)
        results += page.get("results", [])
        records += page.get("records", [])
        token = page.get("nextPageToken", "")
        if not token:
            break
    spec = run.get("spec") or {}
    target = spec.get("target") or {}
    meta = {
        "run": run.get("id"),
        "project": run.get("project"),
        "status": str(run.get("status", "")).removeprefix("RUN_STATUS_").lower(),
        "target": " ".join(str(target.get(k, "")) for k in ("connector", "model") if target.get(k)),
        "records": run.get("records"),
        "dataset sha256": run.get("datasetSha256"),
        "created by": run.get("createdBy"),
        "created": run.get("createdAt"),
        "finished": run.get("finishedAt"),
    }
    gates = [
        {
            **(g.get("gate") or {}),
            "passed": bool(g.get("passed")),
            "value": g.get("value"),
            "reason": g.get("reason", ""),
        }
        for g in run.get("gates", [])
    ]
    return build(
        title=str(run.get("name") or run.get("id")),
        meta=meta,
        summaries=run.get("summaries", []),
        gates=gates,
        results=results,
        records=records,
        examples=examples,
    )


def _fmt(v: float | None) -> str:
    return "-" if v is None else f"{v:.3f}"


def _ci(s: Mapping[str, Any]) -> str:
    return "-" if s["low"] is None else f"[{s['low']:.3f}, {s['high']:.3f}]"


def _gate_text(g: Mapping[str, Any]) -> str:
    bound = []
    if g.get("min") is not None:
        bound.append(f"≥ {g['min']}")
    if g.get("max") is not None:
        bound.append(f"≤ {g['max']}")
    stat = str(g.get("stat", "mean")).lower().removeprefix("gate_stat_")
    return f"{g.get('metric', '')} {stat} {' and '.join(bound)}".strip()


def to_markdown(r: Report) -> str:
    out = [f"# {r.title}", ""]
    for k, v in r.meta.items():
        out.append(f"- **{k}:** {v}")
    out += [
        "",
        "## Metrics",
        "",
        "| metric | n | mean | CI | skipped | errors |",
        "|---|---:|---:|---|---:|---:|",
    ]
    for s in r.summaries:
        out.append(
            f"| {s['metric']} | {s['n']} | {_fmt(s['mean'])} | {_ci(s)} "
            f"| {s['skipped']} | {s['errors']} |"
        )
    if r.gates:
        out += ["", "## Gates", ""]
        for g in r.gates:
            mark = "✅" if g.get("passed") else "❌"
            reason = f" ({g['reason']})" if g.get("reason") else ""
            out.append(f"- {mark} {_gate_text(g)}{reason}")
    for metric, items in r.worst.items():
        out += ["", f"## Lowest {metric}", ""]
        for e in items:
            shown = e.label or _fmt(e.value)
            out.append(f"### {e.record_id} (trial {e.trial}): {shown}")
            for name in ("input", "output", "reference"):
                if getattr(e, name):
                    body = getattr(e, name).replace("```", "'''")
                    out += [f"**{name}**", "", "```", body, "```"]
            if e.explanation:
                out += [f"> {e.explanation}", ""]
    if r.errors:
        out += ["", f"## Errors ({len(r.errors)})", ""]
        for err in r.errors[:50]:
            out.append(f"- {err['record_id']} · {err['evaluator']}: {err['reason']}")
    return "\n".join(out) + "\n"


def _css() -> str:
    from importlib.resources import files

    return files("evalsi").joinpath("report.css").read_text(encoding="utf-8")


def to_html(r: Report) -> str:
    e = html.escape
    parts = [
        "<!doctype html><html lang=en><head><meta charset=utf-8>",
        "<meta name=viewport content='width=device-width,initial-scale=1'>",
        f"<title>{e(r.title)}</title><style>{_css()}</style></head><body><main>",
        f"<h1>{e(r.title)}</h1><dl>",
    ]
    for k, v in r.meta.items():
        parts.append(f"<dt>{e(k)}</dt><dd>{e(v)}</dd>")
    parts.append(
        "</dl><h2>Metrics</h2><div class=scroll><table><tr><th>metric</th><th>n</th>"
        "<th>mean</th><th>CI</th><th>skipped</th><th>errors</th></tr>"
    )
    for s in r.summaries:
        parts.append(
            f"<tr><td>{e(s['metric'])}</td><td class=n>{s['n']}</td>"
            f"<td class=n>{_fmt(s['mean'])}</td><td class=n>{e(_ci(s))}</td>"
            f"<td class=n>{s['skipped']}</td><td class=n>{s['errors']}</td></tr>"
        )
    parts.append("</table></div>")
    if r.gates:
        parts.append("<h2>Gates</h2><ul>")
        for g in r.gates:
            cls, mark = ("ok", "passed") if g.get("passed") else ("bad", "failed")
            reason = f" — {e(str(g['reason']))}" if g.get("reason") else ""
            parts.append(f"<li><span class={cls}>{mark}</span> {e(_gate_text(g))}{reason}</li>")
        parts.append("</ul>")
    for metric, items in r.worst.items():
        parts.append(f"<h2>Lowest {e(metric)}</h2>")
        for x in items:
            shown = x.label or _fmt(x.value)
            parts.append(
                f"<details><summary>{e(x.record_id)} · trial {x.trial} · "
                f"<b>{e(shown)}</b></summary>"
            )
            for name in ("input", "output", "reference"):
                if getattr(x, name):
                    parts.append(f"<div class=label>{name}</div><pre>{e(getattr(x, name))}</pre>")
            if x.explanation:
                parts.append(f"<div class=label>explanation</div><pre>{e(x.explanation)}</pre>")
            parts.append("</details>")
    if r.errors:
        parts.append(f"<h2>Errors ({len(r.errors)})</h2><ul>")
        for err in r.errors[:50]:
            parts.append(
                f"<li>{e(err['record_id'])} · {e(err['evaluator'])}: {e(err['reason'])}</li>"
            )
        parts.append("</ul>")
    parts.append("</main></body></html>\n")
    return "".join(parts)

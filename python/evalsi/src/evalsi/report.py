"""Static reports of a run: one self-contained HTML page or a Markdown file.

``evalsi report results.json -o report.html`` reads what ``evalsi run``
and ``evalsi eval`` write with ``--output``; ``evalsi report --server URL
RUN_ID`` reads a server run. A report has the run's manifest, a table of
metrics with their confidence intervals, the gates, and for each metric
the records that scored worst, with their inputs, outputs and the
evaluator's explanation, so a failure can be read without opening JSON.
The opening readout separates explicit failed checks from execution errors,
skips and unavailable evidence; it never infers release approval from scores.
"""

from __future__ import annotations

import html
import json
import re
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from typing import Any

from evalsi.results import record_trial, result_metric
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
    output_available: bool = False
    reference_available: bool = False
    reproduction: str = ""


@dataclass
class Report:
    title: str
    meta: dict[str, str] = field(default_factory=dict)
    summaries: list[dict[str, Any]] = field(default_factory=list)
    gates: list[dict[str, Any]] = field(default_factory=list)
    worst: dict[str, list[Example]] = field(default_factory=dict)
    errors: list[dict[str, str]] = field(default_factory=list)
    counts: dict[str, int] = field(default_factory=dict)
    skipped: list[dict[str, str]] = field(default_factory=list)


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
    if score.get("number") is not None:
        return _num(score["number"]), ""
    if isinstance(score.get("passed"), bool):
        return (1.0 if score["passed"] else 0.0), ("pass" if score["passed"] else "fail")
    if "label" in score:
        return None, str(score["label"])
    return None, ""


_REPRODUCTION_LABEL = "Reproduction arguments (JSON array, not shell text)"
_REPRODUCTION_CONTEXT = (
    "Run these arguments in the original generated project directory with its Python environment "
    "and unchanged cases/callable. Review before running: metadata is not authenticated. "
    "The rerun replaces results.json."
)


def _reproduction(record: Mapping[str, Any], trial: int) -> str:
    """Recognize the generated runner's data contract, never arbitrary command text."""
    metadata = record.get("metadata")
    hint = metadata.get("evalsi_application_reproduction") if isinstance(metadata, dict) else None
    if (
        trial != 0
        or not isinstance(hint, dict)
        or set(hint) != {"version", "callable", "cases", "case"}
        or type(hint["version"]) is not int
        or hint["version"] != 1
    ):
        return ""
    for name, limit in (("callable", 256), ("cases", 512), ("case", 256)):
        value = hint[name]
        if (
            not isinstance(value, str)
            or not 0 < len(value) <= limit
            or not value.strip()
            or not value.isascii()
            or not value.isprintable()
        ):
            return ""
    target, cases, case = hint["callable"], hint["cases"], hint["case"]
    if (
        re.fullmatch(r"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*:[A-Za-z_]\w*", target, re.ASCII) is None
        or cases.startswith(("/", "-"))
        or "\\" in cases
        or ":" in cases
        or any(part in {"", ".", ".."} for part in cases.split("/"))
        or case.startswith("-")
        or case != record.get("id")
    ):
        return ""
    argv = ["python", "evaluate.py", "--callable", target, "--cases", cases, "--case", case]
    text = json.dumps(argv, ensure_ascii=True)
    # Keep JSON valid while preventing HTML tags or Markdown fences in argument values.
    for char in "<>&`":
        text = text.replace(char, f"\\u{ord(char):04x}")
    return text


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
    # A run's outputs are one record per trial (provenance.run.trial);
    # dataset records are the same in every trial.
    by_key: dict[tuple[str, int | None], Mapping[str, Any]] = {}
    for r in records:
        by_key[(str(r.get("id", "")), record_trial(r))] = r
    higher = {s["metric"]: s["higher_is_better"] for s in report.summaries}
    per_metric: dict[str, list[Example]] = {}
    for r in results:
        report.counts["results"] = report.counts.get("results", 0) + 1
        outcome = str(r.get("outcome", "")).lower().removeprefix("outcome_")
        category = outcome if outcome in {"scored", "error", "skipped", "cancelled"} else "unknown"
        report.counts[category] = report.counts.get(category, 0) + 1
        rid = str(r.get("record_id", r.get("recordId", "")))
        trial = int(r.get("trial", 0) or 0)
        # The output this result graded: its own trial's, else the dataset's.
        rec = by_key.get((rid, trial)) or by_key.get((rid, None)) or {}
        reproduction = _reproduction(rec, trial)
        if outcome == "skipped" and len(report.skipped) < 50:
            report.skipped.append(
                {
                    "record_id": _text(rid),
                    "evaluator": _text(r.get("evaluator", "")),
                    "reason": _text(r.get("reason")) or "No reason recorded.",
                }
            )
        if outcome == "error":
            error = {
                "record_id": rid,
                "evaluator": str(r.get("evaluator", "")),
                "reason": _text(r.get("reason", "")),
            }
            if reproduction:
                error["reproduction"] = reproduction
            report.errors.append(error)
            continue
        if outcome != "scored":
            continue
        if not rec:
            report.counts["missing_records"] = report.counts.get("missing_records", 0) + 1
        if not r.get("scores"):
            report.counts["empty_scores"] = report.counts.get("empty_scores", 0) + 1
        evaluator = str(r.get("evaluator", ""))
        ref = str(r.get("evaluator_ref", r.get("evaluatorRef", "")))
        for score in r.get("scores") or []:
            name = str(score.get("name", ""))
            metric = result_metric(evaluator, ref, name)
            if metric not in higher and evaluator in higher:
                metric = evaluator
            value, label = _score_value(score)
            kind = {"pass": "passed_checks", "fail": "failed_checks"}.get(label, "other_scores")
            # Only the boolean score variant declares a check passed or failed.
            if not isinstance(score.get("passed"), bool):
                kind = "other_scores"
            report.counts[kind] = report.counts.get(kind, 0) + 1
            per_metric.setdefault(metric, []).append(
                Example(
                    record_id=rid,
                    trial=trial,
                    value=value,
                    label=label,
                    explanation=_text(score.get("explanation", "")),
                    input=_text(rec.get("input")),
                    output=_text(rec.get("output")),
                    reference=_text(rec.get("reference")),
                    output_available="output" in rec and rec["output"] is not None,
                    reference_available="reference" in rec and rec["reference"] is not None,
                    reproduction=reproduction,
                )
            )
    for metric, items in per_metric.items():
        scored = [e for e in items if e.value is not None]
        reverse = higher.get(metric) is False
        scored.sort(key=lambda e: -(e.value or 0.0) if reverse else (e.value or 0.0))
        # Explicit failed checks precede numeric candidates whose scale is unknown.
        scored.sort(key=lambda e: e.label != "fail")
        worst = [e for e in scored if not _is_pass(e)][: max(0, examples)]
        if worst:
            report.worst[metric] = worst
    return report


def _is_pass(e: Example) -> bool:
    """A pass is not worth showing as a failure. Numeric scores are always
    candidates, worst first: a report does not know a metric's scale (1 is
    perfect in [0, 1] and the worst on 1 to 5)."""
    return e.label == "pass"


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
    # An archived run keeps its summaries and gates; its results are gone.
    while not run.get("archivedAt"):
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
        "archived": run.get("archivedAt"),
    }
    gates = [
        {
            **(g.get("gate") or {}),
            # A plain proto3 bool: the server's JSON leaves a false one out, so a missing
            # key is a failed gate, not an unavailable one (unlike a results file's).
            "passed": bool(g.get("passed", False)),
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


def _readout(r: Report) -> list[str]:
    """Decision guidance from recorded evidence, never from a score threshold."""
    c = r.counts
    lines = []
    if r.meta.get("finished"):
        lines.append("Evaluation finished; completion is not release approval.")
    else:
        lines.append(
            "Completion status is not recorded here; recorded results are not release approval."
        )
    if c.get("results"):
        lines.append(
            f"Observed checks: {c.get('failed_checks', 0)} failed, "
            f"{c.get('passed_checks', 0)} passed (explicit boolean scores). "
            f"{c.get('other_scores', 0)} other scores have no inferred pass/fail threshold. "
            "Check counts are scores, not unique records."
        )
        lines.append(
            f"Execution/evaluation errors: {c.get('error', 0)}; "
            f"skipped: {c.get('skipped', 0)}; cancelled: {c.get('cancelled', 0)}; "
            f"unrecognized outcomes: {c.get('unknown', 0)}. "
            "These are not failed checks or passes. Errors may originate in the application, "
            "evaluator, or infrastructure; the recorded reason does not always identify the cause."
        )
        if c.get("missing_records") or c.get("empty_scores"):
            lines.append(
                f"Unavailable evidence: {c.get('missing_records', 0)} scored results lack a "
                f"matching record; {c.get('empty_scores', 0)} scored results contain no scores."
            )
    else:
        lines.append(
            "Per-record results are unavailable; aggregate metrics alone cannot explain which "
            "checks failed, passed, skipped, or errored."
        )
    if r.meta.get("archived"):
        lines.append("This run is archived; per-record evidence is no longer available.")
    if r.gates:
        passed = sum(g.get("passed") is True for g in r.gates)
        failed = sum(g.get("passed") is False for g in r.gates)
        lines.append(
            f"Recorded gates: {passed} passed, {failed} failed, "
            f"{len(r.gates) - passed - failed} unavailable. "
            "Gate results are shown as recorded, not a new release decision."
        )
    else:
        lines.append(
            "Release approval is not established: no gate results were recorded. "
            "Passing checks or high scores alone do not approve a release."
        )
    if c.get("failed_checks"):
        lines.append(
            "Next: inspect failed checks below, compare expected/reference with actual output, "
            "and fix the application or check against the intended behavior."
        )
    if c.get("error"):
        lines.append(
            "Next: inspect Errors separately and resolve the recorded execution problem before "
            "drawing conclusions from missing scores."
        )
    if c.get("skipped") or c.get("cancelled") or c.get("unknown"):
        lines.append(
            "Next: review skipped reasons and incomplete outcomes; obtain the missing evidence "
            "before relying on this run."
        )
    if any(x.reproduction for items in r.worst.values() for x in items) or any(
        err.get("reproduction") for err in r.errors[:50]
    ):
        lines.append(
            "Reproduce: recorded application argument arrays appear with the cases below. "
            "They are data, not shell commands; "
            "use the original generated project and environment. "
            "For other results, an exact command is not recorded here; "
            "do not reconstruct credentials from a report."
        )
    else:
        lines.append(
            "Reproduce: use the original application/evaluation command, dataset, evaluator "
            "configuration, and record ID/trial shown below. "
            "An exact command is not recorded here; "
            "do not reconstruct credentials from a report."
        )
    lines.append(
        "Scope: this is evidence for the recorded sample only, not proof of evaluator validity "
        "or production readiness. No baseline comparison is included; use paired comparison "
        "of saved baseline and candidate results to investigate regressions."
    )
    return lines


def _evidence(e: Example) -> Iterable[tuple[str, str]]:
    if e.input:
        yield "input", e.input
    yield "actual output", (e.output or "(empty)") if e.output_available else "Not recorded."
    yield (
        "expected/reference",
        (e.reference or "(empty)") if e.reference_available else "Not recorded; do not infer it.",
    )


def _md(value: str) -> str:
    text = html.escape(value, quote=False).replace("\\", "\\\\").replace("\n", " ")
    for char in ("`", "*", "_", "[", "]", "|", "#"):
        text = text.replace(char, f"\\{char}")
    return text


def to_markdown(r: Report) -> str:
    out = [f"# {_md(r.title)}", "", "## Read this first", ""]
    out.extend(f"- {line}" for line in _readout(r))
    out.append("")
    for k, v in r.meta.items():
        out.append(f"- **{_md(k)}:** {_md(v)}")
    out += [
        "",
        "## Metrics",
        "",
        "| metric | n | mean | CI | skipped | errors |",
        "|---|---:|---:|---|---:|---:|",
    ]
    for s in r.summaries:
        out.append(
            f"| {_md(s['metric'])} | {s['n']} | {_fmt(s['mean'])} | {_ci(s)} "
            f"| {s['skipped']} | {s['errors']} |"
        )
    if r.gates:
        out += ["", "## Gates", ""]
        for g in r.gates:
            mark = "✅" if g.get("passed") is True else "❌" if g.get("passed") is False else "?"
            reason = f" ({_md(str(g['reason']))})" if g.get("reason") else ""
            out.append(f"- {mark} {_md(_gate_text(g))}{reason}")
    for metric, items in r.worst.items():
        out += ["", f"## Lowest {_md(metric)}", ""]
        out.append(
            "Explicit failed checks and lowest numeric scores; numeric scores are not gates."
        )
        for e in items:
            shown = e.label or _fmt(e.value)
            out.append(f"### {_md(e.record_id)} (trial {e.trial}): {_md(shown)}")
            for name, value in _evidence(e):
                body = value.replace("```", "'''")
                out += [f"**{name}**", "", "```", body, "```"]
            if e.reproduction:
                out += [
                    f"**{_REPRODUCTION_LABEL}**",
                    "",
                    _REPRODUCTION_CONTEXT,
                    "",
                    "```json",
                    e.reproduction,
                    "```",
                ]
            if e.explanation:
                out += [f"> {_md(e.explanation)}", ""]
            elif e.label == "fail":
                out += ["> Check returned false; no explanation was recorded.", ""]
    if r.errors:
        out += ["", f"## Errors ({len(r.errors)})", ""]
        for err in r.errors[:50]:
            out.append(f"- {_md(err['record_id'])} · {_md(err['evaluator'])}: {_md(err['reason'])}")
            if err.get("reproduction"):
                out += [
                    f"**{_REPRODUCTION_LABEL}**",
                    "",
                    _REPRODUCTION_CONTEXT,
                    "",
                    "```json",
                    err["reproduction"],
                    "```",
                ]
    if r.skipped:
        out += ["", f"## Skipped ({r.counts.get('skipped', 0)}; up to 50 shown)", ""]
        for item in r.skipped:
            out.append(
                f"- {_md(item['record_id'])} · {_md(item['evaluator'])}: {_md(item['reason'])}"
            )
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
        f"<h1>{e(r.title)}</h1><section class=readout><h2>Read this first</h2><ul>",
    ]
    parts.extend(f"<li>{e(line)}</li>" for line in _readout(r))
    parts.append("</ul></section><dl>")
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
            cls, mark = (
                ("ok", "passed")
                if g.get("passed") is True
                else ("bad", "failed")
                if g.get("passed") is False
                else ("", "unavailable")
            )
            reason = f" — {e(str(g['reason']))}" if g.get("reason") else ""
            parts.append(f"<li><span class={cls}>{mark}</span> {e(_gate_text(g))}{reason}</li>")
        parts.append("</ul>")
    for metric, items in r.worst.items():
        parts.append(f"<h2>Lowest {e(metric)}</h2>")
        parts.append(
            "<p>Explicit failed checks and lowest numeric scores; numeric scores are not gates.</p>"
        )
        for x in items:
            shown = x.label or _fmt(x.value)
            opened = " open" if x.label == "fail" else ""
            parts.append(
                f"<details{opened}><summary>{e(x.record_id)} · trial {x.trial} · "
                f"<b>{e(shown)}</b></summary>"
            )
            for name, value in _evidence(x):
                parts.append(f"<div class=label>{name}</div><pre>{e(value)}</pre>")
            if x.reproduction:
                parts.append(
                    f"<div class=label>{_REPRODUCTION_LABEL}</div>"
                    f"<p>{_REPRODUCTION_CONTEXT}</p><pre>{e(x.reproduction)}</pre>"
                )
            if x.explanation:
                parts.append(f"<div class=label>explanation</div><pre>{e(x.explanation)}</pre>")
            elif x.label == "fail":
                parts.append("<p>Check returned false; no explanation was recorded.</p>")
            parts.append("</details>")
    if r.errors:
        parts.append(f"<h2>Errors ({len(r.errors)})</h2><ul>")
        for err in r.errors[:50]:
            parts.append(
                f"<li>{e(err['record_id'])} · {e(err['evaluator'])}: {e(err['reason'])}</li>"
            )
            if err.get("reproduction"):
                parts.append(
                    f"<li><div class=label>{_REPRODUCTION_LABEL}</div>"
                    f"<p>{_REPRODUCTION_CONTEXT}</p><pre>{e(err['reproduction'])}</pre></li>"
                )
        parts.append("</ul>")
    if r.skipped:
        parts.append(f"<h2>Skipped ({r.counts.get('skipped', 0)}; up to 50 shown)</h2><ul>")
        for item in r.skipped:
            parts.append(
                f"<li>{e(item['record_id'])} · {e(item['evaluator'])}: {e(item['reason'])}</li>"
            )
        parts.append("</ul>")
    parts.append("</main></body></html>\n")
    return "".join(parts)

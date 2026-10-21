"""Offline, escaped calibration reports with no external resources or scripts."""

from __future__ import annotations

import html
import json
from typing import Any

from evalsi.calibration import REPORT_SCHEMA
from evalsi.calibration_sets import digest


def _escape(value: Any) -> str:
    return html.escape(str(value), quote=True)


def _json(value: Any) -> str:
    return _escape(json.dumps(value, indent=2, ensure_ascii=False, allow_nan=False))


def _metric(value: dict[str, Any]) -> str:
    point = value["value"]
    if point is None:
        return "unavailable"
    interval = value["ci"]
    return (
        f"{point:.3f} [{interval[0]:.3f}, {interval[1]:.3f}]"
        if interval
        else f"{point:.3f} (interval unavailable)"
    )


def render_report(report: dict[str, Any]) -> bytes:
    if report.get("schema") != REPORT_SCHEMA:
        raise ValueError("expected a calibration report")
    dataset = report["dataset"]
    parts = [
        "<!doctype html><html lang=en><meta charset=utf-8>",
        '<meta name=viewport content="width=device-width,initial-scale=1">',
        "<meta http-equiv=Content-Security-Policy content=\"default-src 'none'; "
        "style-src 'unsafe-inline'\">",
        "<title>Evaluator calibration</title><style>",
        "body{font:16px system-ui,sans-serif;max-width:1200px;margin:2rem auto;"
        "padding:0 1rem;color:#172033}",
        "table{border-collapse:collapse;width:100%;margin:1rem 0}"
        "th,td{padding:.6rem;border:1px solid #cbd5e1;text-align:left}",
        ".table-scroll{overflow:auto}code{overflow-wrap:anywhere}",
        "th{background:#edf2f7}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#f5f7fa;padding:1rem}",
        "summary{cursor:pointer;padding:.5rem}small{color:#44546a}h2{margin-top:2rem}</style><main>",
        "<h1>Evaluator validation and judge calibration</h1>",
        f"<p>{_escape(dataset['name'])} · label version {_escape(dataset['version'])}</p>",
        f"<p>Human question: {_escape(dataset['question'])}</p>",
        f"<p>Measurement coverage: {_escape(report['status'])}</p>",
        f"<p>Dataset pin <code>{_escape(report['dataset_sha256'])}</code><br>",
        f"Report pin <code>{digest(report)}</code></p>",
        "<p>Target outputs are fixed. These results measure evaluator agreement with "
        "human ratings; ",
        "changes here measure judge validity or drift. They do not measure target improvement.</p>",
        f"<p>Candidate selected on tuning data: "
        f"<strong>{_escape(report['selected_candidate'])}</strong>. ",
        "Threshold and candidate selection use tuning labels only. Held-out results "
        "follow that selection.</p>",
        "<p>Human labels use strict-majority pass/fail votes. Ties and missing votes "
        "remain unlabelled. ",
        "All repeats must score before a case contributes to validity. Skips and "
        "errors remain visible. ",
        "Repeated calls estimate expected single-call agreement and do not increase "
        "the case count.</p>",
        f"<p>Intervals: {_escape(report['statistics']['confidence'])} confidence, ",
        f"{_escape(report['statistics']['method'])}. Small or uniform samples can "
        f"leave intervals unavailable. ",
        "Undefined denominators remain unavailable. Slices and comparisons are "
        "exploratory, without multiplicity correction.</p>",
        "<p>Evals.si judge cache: disabled. Provider caching is outside this tool's control. ",
        "Model identities are reported declarations, not attestations of model weights.</p>",
        "<h2>Held-out validity</h2><div class=table-scroll role=region "
        'aria-label="Held-out validity" tabindex=0><table><thead><tr>'
        "<th>Candidate / threshold</th>",
        "<th>Labelled complete / total cases</th><th>Groups</th><th>Accuracy</th>",
        "<th>False positive rate</th><th>False negative rate</th><th>Repeated-call "
        "disagreement</th>",
        "</tr></thead><tbody>",
    ]
    for candidate in report["candidates"]:
        phase = candidate["held-out"]
        parts.append(
            f"<tr><th>{_escape(candidate['name'])} / "
            f"{_escape(candidate['selection']['threshold'])}</th>"
            f"<td>{_escape(phase['cases'])} / {_escape(phase['total_cases'])}</td>"
            f"<td>{_escape(phase['groups'])}</td>"
            f"<td>{_metric(phase['metrics']['accuracy'])}</td>"
            f"<td>{_metric(phase['metrics']['fpr'])}</td><td>{_metric(phase['metrics']['fnr'])}</td>"
            f"<td>{_metric(phase['repeatability']['decision_disagreement'])}</td></tr>"
        )
    parts.append(
        "</tbody></table></div><h2>Paired held-out comparisons</h2><pre>"
        + _json(report["held_out_comparisons"])
        + "</pre>"
    )
    cases = {case["record"]["id"]: case for case in dataset["cases"]}
    parts.append(
        "<h2>Human disagreements</h2><p>All individual ratings, comments and "
        "abstentions are retained.</p>"
    )
    for case in dataset["cases"]:
        votes = [r["passed"] for r in case["ratings"] if type(r.get("passed")) is bool]
        if case["label"] is None or len(set(votes)) > 1:
            parts.append(
                f"<details><summary>{_escape(case['record']['id'])}: "
                f"consensus "
                f"{_escape(case['label'])}</summary><pre>{_json(case['ratings'])}</pre></details>"
            )
    for candidate in report["candidates"]:
        parts.append(
            f"<h2>{_escape(candidate['name'])}</h2><details><summary>Pinned evaluator and "
            f"judge configuration</summary>"
            f"<pre>{_json(candidate['definition'])}</pre></details>"
            f"<details><summary>Threshold "
            f"selection</summary><pre>{_json(candidate['selection'])}</pre></details>"
        )
        for phase_name in ("tuning", "held-out"):
            phase = candidate[phase_name]
            parts.append(
                f"<h3>{_escape(phase_name)}</h3><pre>"
                + _json(
                    {
                        key: value
                        for key, value in phase.items()
                        if key not in ("cases_detail", "slices")
                    }
                )
                + "</pre><details><summary>Slice metrics and denominators</summary><pre>"
                + _json(phase["slices"])
                + "</pre></details>"
            )
            for row in phase["cases_detail"]:
                status = (
                    "incomplete"
                    if not row["complete"]
                    else "threshold unavailable"
                    if row["pass_probability"] is None
                    else (
                        "unlabelled"
                        if row["label"] is None
                        else "disagreement"
                        if row["accuracy"] < 1
                        else "agreement"
                    )
                )
                parts.append(
                    f"<details><summary>{_escape(row['id'])}: {status}</summary><pre>"
                    + _json(
                        {
                            "record": cases[row["id"]]["record"],
                            "human_ratings": cases[row["id"]]["ratings"],
                            "evaluation": row,
                        }
                    )
                    + "</pre></details>"
                )
        parts.append(
            "<details><summary>Fresh judge-call audit (prompt, system, schema and "
            "response hashes)</summary><pre>" + _json(candidate["judge_calls"]) + "</pre></details>"
        )
    parts.append(
        "<h2>Reproduction protocol</h2><pre>" + _json(report["protocol"]) + "</pre>"
        "<p>These files contain record content and human comments. Content hashes detect changes; ",
    )
    parts.append(
        "they do not prove label authorship or dataset independence beyond declared "
        "groups and exact input checks.</p></main></html>"
    )
    return "".join(parts).encode("utf-8")

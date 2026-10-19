"""Action adapter compatible with published evalsi 0.3.0; never echo raw run data."""

from __future__ import annotations

import hashlib
import html
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any


def cell(value: object) -> str:
    escaped = html.escape(str(value)).replace("\n", " ").replace("\r", " ")
    for char in "|`[]()!*_\\":
        escaped = escaped.replace(char, f"&#{ord(char)};")
    return escaped


def number(value: Any) -> str:
    return f"{value:.4g}" if isinstance(value, (int, float)) else "—"


def comparison(baseline: dict[str, Any], candidate: dict[str, Any]) -> list[str]:
    from evalsi.compare import compare_results, record_values

    a, b = baseline["manifest"], candidate["manifest"]
    for key in ("evalsi_version", "api_version", "evaluators", "judge", "trials"):
        if key not in a or key not in b or a[key] != b[key]:
            raise ValueError("baseline evaluation contract differs")
    if (a.get("summary"), a.get("spec", {}).get("summary")) != (
        b.get("summary"),
        b.get("spec", {}).get("summary"),
    ):
        raise ValueError("baseline summary configuration differs")
    digest = a["dataset"].get("sha256")
    if not digest or digest != b["dataset"].get("sha256"):
        raise ValueError("baseline dataset identity differs")
    av, bv = record_values(baseline), record_values(candidate)
    if not av or set(av) != set(bv) or any(not av[k] or av[k].keys() != bv[k].keys() for k in av):
        raise ValueError("baseline scored record/trial coverage differs")
    lines = [
        "",
        "### Baseline comparison (advisory)",
        "",
        "Same dataset hash and evaluation contract; fully matched scored record/trial pairs.",
        "Intervals are 95%; repeated trials are clustered by record. "
        "Small samples are limited evidence.",
        "Agent/target changes are allowed. This is a comparison, not signed release evidence.",
        "",
        "| Metric | Baseline mean | Candidate mean | Paired n | Mean delta | 95% interval |",
        "|---|---:|---:|---:|---:|---|",
    ]
    for c in compare_results(baseline, candidate):
        interval = (
            f"{number(c.diff_low)} … {number(c.diff_high)}"
            if c.diff_low is not None and c.diff_high is not None
            else "unavailable: insufficient independent records"
        )
        lines.append(
            f"| {cell(c.metric)} | {number(c.baseline_mean)} | {number(c.candidate_mean)} "
            f"| {c.paired_n} | {number(c.diff)} | {interval} |"
        )
    return lines


def summary(data: dict[str, Any], code: int) -> list[str]:
    manifest = data["manifest"]
    lines = [
        "# Evals.si gate",
        "",
        "**Passed**"
        if code == 0
        else "**Failed gates (exit 3)**"
        if code == 3
        else "**Execution error**",
        "",
        f"SDK: `{cell(manifest['evalsi_version'])}`",
        f"Dataset SHA-256: `{cell(manifest['dataset']['sha256'])}`",
        "",
        "| Metric | Scored n | Mean | Errors | Skipped | Score interval |",
        "|---|---:|---:|---:|---:|---|",
    ]
    if not data.get("summaries") or not data.get("gates"):
        raise ValueError("metric summaries and explicit gates are required")
    for metric in data["summaries"]:
        ci = metric.get("ci") or {}
        # Labels and hashes are escaped; raw records, reasons and explanations stay private.
        lines.append(
            f"| {cell(metric['metric'])} | {number(metric.get('n'))} "
            f"| {number(metric.get('mean'))} | {number(metric.get('errors', 0))} "
            f"| {number(metric.get('skipped', 0))} "
            f"| {number(ci.get('low'))} … {number(ci.get('high'))} "
            f"(level {number(ci.get('level'))}) |"
        )
    lines += ["", "| Gate | Statistic | Passed |", "|---|---|---|"]
    for result in data.get("gates", []):
        gate = result
        lines.append(
            f"| {cell(gate['metric'])} | {cell(gate.get('stat', 'mean'))} "
            f"| {'yes' if result['passed'] else 'no'} |"
        )
    return lines


def main() -> int:
    root = Path(tempfile.mkdtemp(prefix="evalsi-action-", dir=os.environ["RUNNER_TEMP"]))
    result = root / "results.json"
    code = 1
    lines = ["# Evals.si gate", "", "**Execution/configuration error (exit 1)**"]
    try:
        directory = Path(os.environ["GITHUB_WORKSPACE"]) / os.environ["EVALSI_ACTION_DIRECTORY"]
        spec = directory / os.environ["EVALSI_ACTION_SPEC"]
        spec_digest = hashlib.sha256(spec.read_bytes()).hexdigest()
        with (root / "process.log").open("w", encoding="utf-8") as log:
            proc = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "evalsi",
                    "run",
                    "-f",
                    str(spec.resolve()),
                    "--output",
                    str(result),
                    "--format",
                    "json",
                    "--quiet",
                ],
                cwd=directory,
                stdout=log,
                stderr=subprocess.STDOUT,
                check=False,
            )
        code = proc.returncode if proc.returncode in (0, 3) else 1
        if code == 1:
            raise ValueError("evaluation could not complete")
        data = json.loads(result.read_text(encoding="utf-8"))
        lines = summary(data, code)
        lines += [
            "",
            f"Candidate commit: `{cell(os.environ.get('GITHUB_SHA', 'unknown'))}`",
            f"Spec SHA-256: `{spec_digest}`",
        ]
        baseline = os.environ.get("EVALSI_ACTION_BASELINE", "")
        if baseline:
            baseline_bytes = (directory / baseline).read_bytes()
            lines += comparison(json.loads(baseline_bytes), data)
            lines += [
                "",
                f"Baseline results SHA-256: `{hashlib.sha256(baseline_bytes).hexdigest()}`",
            ]
    except (OSError, ValueError, KeyError, TypeError, AttributeError, ImportError):
        code = 1
        lines[2] = "**Action failed (exit 1)**"
        lines += [
            "",
            "**Action failed (exit 1).** Check the spec, installation, and any baseline's",
            "dataset hash, evaluation contract and complete scored record/trial coverage.",
            "A failed or incompatible comparison cannot pass the action.",
        ]
    lines += [
        "",
        "Only metrics, labels and identity hashes appear here. Raw results and process logs",
        "remain on the runner unless artifact upload is explicitly enabled.",
        "Reproduce with the same checkout, package version and spec: `evalsi run -f YOUR_SPEC`.",
        "Gate thresholds and error-rate checks come from your spec; comparison is advisory.",
    ]
    rendered = "\n".join(lines) + "\n"
    (root / "summary.md").write_text(rendered, encoding="utf-8")
    with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a", encoding="utf-8") as out:
        out.write(rendered)
    with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as out:
        out.write(f"exit-code={code}\nresults={result if result.exists() else ''}\n")
        out.write(f"artifact-directory={root}\n")
    # Final composite step propagates the recorded code after any artifact upload.
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

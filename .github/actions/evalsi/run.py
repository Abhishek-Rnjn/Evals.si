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

import httpx


def cell(value: object) -> str:
    escaped = html.escape(str(value)).replace("\n", " ").replace("\r", " ")
    for char in "|`[]()!*_\\":
        escaped = escaped.replace(char, f"&#{ord(char)};")
    return escaped


def number(value: Any) -> str:
    return f"{value:.4g}" if isinstance(value, (int, float)) else "—"


def complete_values(data: dict[str, Any]) -> dict[str, dict[str, float]]:
    """Require complete record-scoped evaluations before accepting paired evidence."""
    from evalsi.compare import record_values
    from evalsi.results import record_trial

    manifest = data["manifest"]
    count, trials = manifest["dataset"]["records"], manifest["trials"]
    if type(count) is not int or type(trials) is not int or count < 1 or trials < 1:
        raise ValueError("invalid expected record/trial counts")
    records = data["records"]
    if len(records) != count * trials:
        raise ValueError("incomplete output records")
    ids = {r["id"] for r in records}
    if len(ids) != count or any(not isinstance(rid, str) or not rid for rid in ids):
        raise ValueError("invalid dataset record identities")
    expected = {(rid, trial) for rid in ids for trial in range(trials)}
    if {(r["id"], record_trial(r)) for r in records} != expected:
        raise ValueError("incomplete output record/trial coverage")
    evaluators = {e["name"] for e in manifest["evaluators"]}
    if not evaluators or len(evaluators) != len(manifest["evaluators"]):
        raise ValueError("invalid evaluator identities")
    rows = data["results"]
    if len(rows) != len(expected) * len(evaluators):
        raise ValueError("incomplete record-scoped evaluation coverage")
    seen = set()
    for row in rows:
        key = (row["record_id"], row.get("trial", 0), row["evaluator"])
        if (
            key[:2] not in expected
            or key[2] not in evaluators
            or key in seen
            or row["outcome"] != "scored"
        ):
            raise ValueError("missing, duplicate, skipped or errored evaluation")
        seen.add(key)
    values = record_values(data)
    expected_keys = {f"{rid}#{trial}" for rid, trial in expected}
    for metric in data["summaries"]:
        name = metric["metric"]
        if metric.get("errors", 0) or metric.get("skipped", 0):
            raise ValueError("incomplete summary coverage")
        if metric["kind"] not in ("number", "proportion"):
            continue
        if trials > 1 and any(
            name.endswith(f".{suffix}{trials}") and name.rsplit(".", 1)[0] in values
            for suffix in ("pass@", "pass^")
        ):
            if metric["n"] != count:
                raise ValueError("incomplete aggregate metric coverage")
            continue
        # An optional metric absent from every record is inapplicable (e.g. task-score).
        if metric["n"] == 0 and name not in values:
            continue
        if metric["n"] != len(expected) or set(values.get(name, {})) != expected_keys:
            raise ValueError("incomplete numeric metric record/trial coverage")
    return values


def comparison(baseline: dict[str, Any], candidate: dict[str, Any]) -> list[str]:
    from evalsi.compare import compare_results

    a, b = baseline["manifest"], candidate["manifest"]
    for key in ("evalsi_version", "api_version", "evaluators", "judge", "trials"):
        if key not in a or key not in b or a[key] != b[key]:
            raise ValueError("baseline evaluation contract differs")
    if (a.get("summary"), a.get("spec", {}).get("summary")) != (
        b.get("summary"),
        b.get("spec", {}).get("summary"),
    ):
        raise ValueError("baseline summary configuration differs")
    if a.get("spec", {}).get("environment", {}).get("checker") != (
        b.get("spec", {}).get("environment", {}).get("checker")
    ):
        raise ValueError("baseline checker configuration differs")
    digest = a["dataset"].get("sha256")
    if not digest or digest != b["dataset"].get("sha256"):
        raise ValueError("baseline dataset identity differs")
    av, bv = complete_values(baseline), complete_values(candidate)
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


def embedded_gate(root: Path, result: Path, directory: Path, spec: Path) -> tuple[int, list[str]]:
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
    return code, lines


def server_gate(result: Path, directory: Path, spec: Path) -> tuple[int, list[str]]:
    from evalsi.client import Client
    from evalsi.release_decisions import exit_code, gate_candidate, load_policy, write_report

    if os.environ.get("EVALSI_ACTION_BASELINE"):
        raise ValueError("a baseline file cannot be combined with server release decisions")
    required = (
        "RELEASE_POLICY",
        "RELEASE_POLICY_SHA256",
        "BASELINE_RUN",
        "BASELINE_SHA256",
        "BASELINE_COMMIT",
    )
    if any(not os.environ.get("EVALSI_ACTION_" + key) for key in required):
        raise ValueError("server release mode requires reviewed policy and baseline pins")
    decision_id = (
        os.environ.get("EVALSI_ACTION_RELEASE_ID")
        or f"ci-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}"
    )
    with Client(os.environ["EVALSI_ACTION_SERVER"]) as client:
        decision = gate_candidate(
            client,
            policy=load_policy(str(directory / os.environ["EVALSI_ACTION_RELEASE_POLICY"])),
            policy_sha256=os.environ["EVALSI_ACTION_RELEASE_POLICY_SHA256"],
            baseline={
                "runId": os.environ["EVALSI_ACTION_BASELINE_RUN"],
                "sha256": os.environ["EVALSI_ACTION_BASELINE_SHA256"],
                "commit": os.environ["EVALSI_ACTION_BASELINE_COMMIT"],
            },
            spec_file=spec,
            candidate_commit=os.environ["GITHUB_SHA"],
            decision_id=decision_id,
        )
    write_report(result, decision)
    return exit_code(decision), release_summary(decision)


def release_summary(decision: dict[str, Any]) -> list[str]:
    lines = [
        "# Evals.si release decision",
        "",
        "**" + cell(decision["outcome"].removeprefix("RELEASE_OUTCOME_")) + "**",
        "",
        "One explicit baseline, one candidate attempt. "
        "Commit labels are declared, not execution attestations.",
        "",
        f"Decision: `{cell(decision['id'])}`",
        f"Decision SHA-256: `{cell(decision['sha256'])}`",
        f"Policy SHA-256: `{cell(decision['policySha256'])}`",
        f"Dataset SHA-256: `{cell(decision['policy']['dataset']['sha256'])}`",
    ]
    lines += [
        f"Minimum independent records: `{cell(decision['policy']['minRecords'])}`",
        f"Confidence level: `{number(decision['policy'].get('confidenceLevel', 0.95))}`",
    ]
    for reason in decision.get("reasons", []):
        lines.append("- " + cell(reason))
    for side in ("baseline", "candidate"):
        evidence = decision[side]
        lines += [
            "",
            f"{side.title()} run: `{cell(evidence['reference']['runId'])}`",
            f"Declared commit: `{cell(evidence['reference']['commit'])}`",
            f"Evidence SHA-256: `{cell(evidence['reference']['sha256'])}`",
        ]
    lines += [
        "",
        "| Metric | Independent pairs | Baseline | Candidate | Regression | Interval | Outcome |",
        "|---|---:|---:|---:|---:|---|---|",
    ]
    for metric in decision.get("metrics", []):
        columns = [
            cell(metric["metric"]),
            cell(metric.get("pairedRecords", 0)),
            number(metric.get("baselineMean")),
            number(metric.get("candidateMean")),
            number(metric.get("meanRegression")),
            number(metric.get("regressionLow")) + " … " + number(metric.get("regressionHigh")),
            cell(metric["outcome"].removeprefix("RELEASE_OUTCOME_")),
        ]
        lines.append("| " + " | ".join(columns) + " |")
    for metric in decision.get("metrics", []):
        for reason in metric.get("reasons", []):
            lines.append("- " + cell(metric["metric"]) + ": " + cell(reason))
    lines += [
        "",
        "Positive regression is worse. "
        "Case identities/content stay in the authorized server report.",
        "Open the server's Releases screen using the decision ID above, "
        "or retrieve with `evalsi release get`.",
        "Verify the portable report with `evalsi release verify FILE --sha256 DECISION_SHA256`.",
    ]
    return lines


def main() -> int:
    root = Path(tempfile.mkdtemp(prefix="evalsi-action-", dir=os.environ["RUNNER_TEMP"]))
    result = root / "results.json"
    code = 1
    lines = ["# Evals.si gate", "", "**Execution/configuration error (exit 1)**"]
    try:
        directory = Path(os.environ["GITHUB_WORKSPACE"]) / os.environ["EVALSI_ACTION_DIRECTORY"]
        spec = directory / os.environ["EVALSI_ACTION_SPEC"]
        if os.environ.get("EVALSI_ACTION_SERVER"):
            code, lines = server_gate(result, directory, spec)
        else:
            if os.environ.get("EVALSI_ACTION_RELEASE_POLICY"):
                raise ValueError("release policy requires a server")
            code, lines = embedded_gate(root, result, directory, spec)
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        AttributeError,
        ImportError,
        RuntimeError,
        httpx.HTTPError,
    ):
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
        "Release policy is enforced; insufficient evidence cannot pass."
        if os.environ.get("EVALSI_ACTION_SERVER")
        else "Gate thresholds and error-rate checks come from your spec; comparison is advisory.",
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

"""Validate evaluators against pinned human labels, with tuning isolated from test data."""

from __future__ import annotations

import copy
import hashlib
import inspect
import itertools
import math
import platform
import re
from contextlib import AsyncExitStack
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from pathlib import Path
from statistics import mean, pstdev
from typing import Any
from urllib.parse import urlsplit

from evalsi._version import __version__
from evalsi.calibration_sets import canonical, decode, digest, read_artifact, validate_set
from evalsi.calibration_stats import Observation, bootstrap, paired_difference, rates, validity
from evalsi.datasets import load_records
from evalsi.evaluator import BoundEvaluator, EvalContext, Scope, ScoreType
from evalsi.judges import JudgeClient, JudgeConfig, JudgeResponse, create_judge
from evalsi.registry import Registry
from evalsi.runner import bind_evaluators, run_evaluators
from evalsi.types import Outcome

REPORT_SCHEMA = "evalsi.calibration-report/v1"
PROTOCOL_SCHEMA = "evalsi.calibration-protocol/v1"


def _fields(value: Any, allowed: set[str], required: set[str], label: str) -> None:
    if not isinstance(value, dict) or set(value) - allowed or required - set(value):
        raise ValueError(f"unknown or missing {label} fields")


def _judge_config(raw: Any) -> JudgeConfig:
    _fields(raw, set(JudgeConfig.__dataclass_fields__), {"provider", "model"}, "judge")
    for field in ("provider", "model"):
        if not isinstance(raw[field], str) or not raw[field].strip():
            raise ValueError(f"judge {field} must be a non-empty string")
    for field in ("base_url", "api_key_env", "effort", "response_format"):
        if raw.get(field) is not None and not isinstance(raw[field], str):
            raise ValueError(f"judge {field} must be a string")
    if type(raw.get("max_tokens", 16000)) is not int or raw.get("max_tokens", 16000) <= 0:
        raise ValueError("judge max_tokens must be a positive integer")
    for field in ("temperature", "timeout_s", "requests_per_minute"):
        if raw.get(field) is not None and (
            type(raw[field]) not in (int, float)
            or not math.isfinite(raw[field])
            or (raw[field] <= 0 if field != "temperature" else raw[field] < 0)
        ):
            raise ValueError(f"invalid judge {field}")
    if raw.get("response_format", "json_schema") not in ("json_schema", "json_object", "none"):
        raise ValueError("invalid judge response_format")
    config = JudgeConfig(**raw)
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", config.key_env):
        raise ValueError("judge api_key_env must name an environment variable")
    if config.base_url:
        endpoint = urlsplit(config.base_url)
        if endpoint.username or endpoint.password or endpoint.query or endpoint.fragment:
            raise ValueError("judge URLs must not contain credentials, queries or fragments")
        if endpoint.scheme not in ("http", "https") or not endpoint.hostname:
            raise ValueError("judge base_url must be an absolute HTTP(S) URL")
    return config


@dataclass
class _Candidate:
    name: str
    bound: BoundEvaluator
    metric: str
    thresholds: list[float]
    direction: str
    judge_config: JudgeConfig | None
    definition: dict[str, Any]
    judge: _AuditedJudge | None = None


class _AuditedJudge(JudgeClient):
    def __init__(self, client: JudgeClient) -> None:
        super().__init__(client.config, client.backend, cache=None)
        self.calls: list[dict[str, Any]] = []
        self.phase = ""
        self.repeat = 0

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        call: dict[str, Any] = {
            "phase": self.phase,
            "repeat": self.repeat,
            "system_sha256": digest(system),
            "prompt_sha256": digest(prompt),
            "schema_sha256": digest(schema),
        }
        self.calls.append(call)
        try:
            response = await super().complete_json(system=system, prompt=prompt, schema=schema)
        except Exception as exc:
            call["error_type"] = type(exc).__name__
            raise
        call.update(
            model=response.model, cached=response.cached, response_sha256=digest(response.data)
        )
        if response.cached:
            raise ValueError("calibration requires fresh judge responses")
        return response


def _prepare(raw: Any, registry: Registry | None) -> _Candidate:
    _fields(
        raw,
        {"name", "evaluator", "metric", "thresholds", "pass_when", "judge"},
        {"name", "evaluator", "metric", "thresholds", "pass_when"},
        "candidate",
    )
    if not isinstance(raw["name"], str) or not raw["name"].strip():
        raise ValueError("candidate needs a non-empty name")
    _fields(raw["evaluator"], {"ref", "params"}, {"ref"}, "evaluator")
    if not isinstance(raw["evaluator"]["ref"], str):
        raise ValueError("evaluator ref must be a string")
    if not isinstance(raw["evaluator"].get("params", {}), dict):
        raise ValueError("evaluator params must be an object")
    bound = bind_evaluators([raw["evaluator"]], registry=registry)[0]
    if bound.spec.scope != Scope.RECORD:
        raise ValueError("calibration requires record-scope evaluators")
    metric = raw["metric"]
    spec = bound.spec.output(metric)
    if spec is None or spec.type not in (ScoreType.NUMBER, ScoreType.PASSED):
        raise ValueError("calibration metric must be a declared numeric or pass/fail output")
    thresholds = raw["thresholds"]
    if (
        not isinstance(thresholds, list)
        or not thresholds
        or not all(type(t) in (int, float) and math.isfinite(t) for t in thresholds)
    ):
        raise ValueError("thresholds must be a non-empty list of finite numbers")
    if len(set(thresholds)) != len(thresholds):
        raise ValueError("thresholds must be unique (declared order breaks tuning ties)")
    if raw["pass_when"] not in ("gte", "lte"):
        raise ValueError("pass_when must be gte or lte")
    judge_config = None
    if raw.get("judge") is not None:
        judge_config = _judge_config(raw["judge"])
    if bound.spec.requires.judge and judge_config is None:
        raise ValueError("judge-backed calibration candidates require an explicit judge config")
    if judge_config and not bound.spec.requires.judge:
        raise ValueError("judge configured for an evaluator that does not declare a judge")
    source = inspect.getsourcefile(bound.definition.fn)
    if source is None:
        raise ValueError("calibration requires inspectable evaluator source")
    definition = {
        "name": raw["name"],
        "evaluator": {"ref": bound.spec.ref, "params": bound.params},
        "implementation_sha256": hashlib.sha256(Path(source).read_bytes()).hexdigest(),
        "metric": metric,
        "thresholds": thresholds,
        "pass_when": raw["pass_when"],
        "judge": asdict(judge_config) if judge_config else None,
    }
    canonical(definition)
    return _Candidate(
        raw["name"], bound, metric, thresholds, raw["pass_when"], judge_config, definition
    )


def _pass(value: float, threshold: float, direction: str) -> bool:
    return value >= threshold if direction == "gte" else value <= threshold


def _observations(
    cases: list[dict[str, Any]], threshold: float, direction: str
) -> list[Observation]:
    return [
        (case["group"], case["label"], mean(_pass(v, threshold, direction) for v in case["values"]))
        for case in cases
        if case["complete"] and case["label"] is not None
    ]


async def _score(
    candidate: _Candidate,
    cases: list[dict[str, Any]],
    *,
    repeats: int,
    concurrency: int,
    phase: str,
) -> list[dict[str, Any]]:
    rows = [
        {
            "id": case["record"]["id"],
            "group": case["group"],
            "label": case["label"],
            "slices": case["slices"],
            "trials": [],
            "values": [],
        }
        for case in cases
    ]
    by_id = {row["id"]: row for row in rows}
    for repeat in range(repeats):
        if candidate.judge:
            candidate.judge.phase, candidate.judge.repeat = phase, repeat
        results = await run_evaluators(
            load_records(copy.deepcopy([case["record"] for case in cases])),
            [candidate.bound],
            EvalContext(judge=candidate.judge),
            concurrency=concurrency,
        )
        for result in results:
            row = by_id[result.record_id]
            trial = result.to_dict()
            trial["repeat"] = repeat
            selected = [score for score in result.scores if score.name == candidate.metric]
            if result.outcome == Outcome.SCORED:
                value = selected[0].numeric() if len(selected) == 1 else None
                if value is None or not math.isfinite(value):
                    trial.update(
                        outcome="error", reason="selected metric missing, duplicate or non-numeric"
                    )
                else:
                    row["values"].append(value)
            row["trials"].append(trial)
    for row in rows:
        row["complete"] = len(row["values"]) == repeats
    return rows


def _select(cases: list[dict[str, Any]], candidate: _Candidate) -> dict[str, Any]:
    if len(candidate.thresholds) == 1:
        return {"threshold": candidate.thresholds[0], "method": "predeclared", "sweep": []}
    labelled = [case for case in cases if case["label"] is not None]
    if not labelled or not all(case["complete"] for case in labelled):
        return {"threshold": None, "method": "incomplete tuning evidence", "sweep": []}
    if {case["label"] for case in labelled} != {True, False}:
        return {"threshold": None, "method": "tuning requires both human classes", "sweep": []}
    sweep: list[dict[str, Any]] = []
    for threshold in candidate.thresholds:
        accuracy = rates(_observations(cases, threshold, candidate.direction))["accuracy"]
        sweep.append({"threshold": threshold, "accuracy": accuracy})
    selected = max(sweep, key=lambda row: row["accuracy"])
    return {
        "threshold": selected["threshold"],
        "method": "highest tuning accuracy; declared-order ties",
        "sweep": sweep,
    }


def _phase_report(
    rows: list[dict[str, Any]],
    selection: dict[str, Any],
    candidate: _Candidate,
    *,
    confidence: float,
    resamples: int,
) -> dict[str, Any]:
    threshold = selection["threshold"]
    for row in rows:
        values = row.pop("values")
        if threshold is None or not row["complete"]:
            row["pass_probability"] = None
            row["accuracy"] = None
            row["repeatability"] = None
            continue
        decisions = [_pass(v, threshold, candidate.direction) for v in values]
        probability = mean(decisions)
        row["pass_probability"] = probability
        row["accuracy"] = (
            (probability if row["label"] else 1 - probability) if row["label"] is not None else None
        )
        pair_disagreement = (
            sum(a != b for a, b in itertools.combinations(decisions, 2))
            / (len(decisions) * (len(decisions) - 1) / 2)
            if len(values) > 1
            else None
        )
        row["repeatability"] = {
            "score_min": min(values),
            "score_max": max(values),
            "score_std": pstdev(values),
            "decision_disagreement": pair_disagreement,
        }
    observations = [
        (row["group"], row["label"], row["pass_probability"])
        for row in rows
        if row["accuracy"] is not None
    ]
    slices = []
    keys = sorted({(key, value) for row in rows for key, value in row["slices"].items()})
    for key, value in keys:
        sliced = [row for row in rows if row["slices"].get(key) == value]
        samples = [
            (row["group"], row["label"], row["pass_probability"])
            for row in sliced
            if row["accuracy"] is not None
        ]
        slices.append(
            {
                "key": key,
                "value": value,
                "total_cases": len(sliced),
                **validity(samples, confidence=confidence, resamples=resamples),
            }
        )
    repeated = [
        row
        for row in rows
        if row["repeatability"] is not None
        and row["repeatability"]["decision_disagreement"] is not None
    ]
    repeatability = bootstrap(
        repeated,
        group=lambda row: row["group"],
        statistic=lambda sample: {
            "decision_disagreement": mean(
                row["repeatability"]["decision_disagreement"] for row in sample
            )
            if sample
            else None
        },
        confidence=confidence,
        resamples=resamples,
    )
    return {
        "total_cases": len(rows),
        "unlabelled_cases": sum(row["label"] is None for row in rows),
        "incomplete_cases": sum(not row["complete"] for row in rows),
        "outcomes": {
            outcome: sum(trial["outcome"] == outcome for row in rows for trial in row["trials"])
            for outcome in ("scored", "skipped", "error", "cancelled")
        },
        **validity(observations, confidence=confidence, resamples=resamples),
        "repeatability": {"cases": len(repeated), **repeatability},
        "slices": slices,
        "cases_detail": rows,
    }


def _accuracy_map(phase: dict[str, Any]) -> dict[str, tuple[str, float]]:
    return {
        row["id"]: (row["group"], row["accuracy"])
        for row in phase["cases_detail"]
        if row["accuracy"] is not None
    }


async def acalibrate(
    dataset: str | Path, protocol: dict[str, Any], *, registry: Registry | None = None
) -> dict[str, Any]:
    """Run an explicit JSON protocol against a digest-pinned frozen calibration set.

    No target is executed. Threshold and candidate selection see tuning labels only.
    Every repeat uses a fresh local-cache-disabled judge request. A provider may
    still use its own caching; model names are declarations, not weight attestations.
    """
    protocol = decode(canonical(protocol))  # Freeze SDK caller-owned configuration before awaiting.
    _fields(
        protocol,
        {
            "schema",
            "dataset_sha256",
            "candidates",
            "repeats",
            "concurrency",
            "confidence",
            "resamples",
        },
        {"schema", "dataset_sha256", "candidates"},
        "protocol",
    )
    if protocol["schema"] != PROTOCOL_SCHEMA or not protocol["dataset_sha256"]:
        raise ValueError("calibration protocol needs schema and an approved dataset_sha256")
    document, sha256 = read_artifact(dataset, expected=protocol["dataset_sha256"])
    validate_set(document)
    repeats, concurrency = protocol.get("repeats", 3), protocol.get("concurrency", 8)
    confidence, resamples = protocol.get("confidence", 0.95), protocol.get("resamples", 2000)
    for label, value, low, high in (
        ("repeats", repeats, 1, 100),
        ("concurrency", concurrency, 1, 256),
        ("resamples", resamples, 100, 20000),
    ):
        if type(value) is not int or not low <= value <= high:
            raise ValueError(f"{label} must be an integer between {low} and {high}")
    if type(confidence) not in (float, int) or not 0 < confidence < 1:
        raise ValueError("confidence must be between 0 and 1")
    if not isinstance(protocol["candidates"], list) or not protocol["candidates"]:
        raise ValueError("calibration needs candidates")
    candidates = [_prepare(raw, registry) for raw in protocol["candidates"]]
    if len({candidate.name for candidate in candidates}) != len(candidates):
        raise ValueError("candidate names must be unique")
    # All configuration, split and source checks complete before any judge is created.
    phases = {
        phase: [case for case in document["cases"] if case["split"] == phase]
        for phase in ("tuning", "held-out")
    }
    results: list[dict[str, Any]] = []
    started = datetime.now(UTC).isoformat()
    async with AsyncExitStack() as stack:
        for candidate in candidates:
            if candidate.judge_config:
                candidate.judge = _AuditedJudge(create_judge(candidate.judge_config, cache=None))
                stack.push_async_callback(candidate.judge.aclose)
            tuning = await _score(
                candidate,
                phases["tuning"],
                repeats=repeats,
                concurrency=concurrency,
                phase="tuning",
            )
            selection = _select(tuning, candidate)
            results.append(
                {
                    "name": candidate.name,
                    "definition": candidate.definition,
                    "definition_sha256": digest(candidate.definition),
                    "selection": selection,
                    "tuning": _phase_report(
                        tuning, selection, candidate, confidence=confidence, resamples=resamples
                    ),
                }
            )
        eligible = [
            result
            for result in results
            if result["tuning"]["metrics"]["accuracy"]["value"] is not None
            and not result["tuning"]["incomplete_cases"]
        ]
        selected_candidate = (
            max(eligible, key=lambda result: result["tuning"]["metrics"]["accuracy"]["value"])[
                "name"
            ]
            if eligible
            else None
        )
        # The chosen threshold and candidate are fixed before touching held-out scores.
        for candidate, result in zip(candidates, results, strict=True):
            held_out = await _score(
                candidate,
                phases["held-out"],
                repeats=repeats,
                concurrency=concurrency,
                phase="held-out",
            )
            result["held-out"] = _phase_report(
                held_out, result["selection"], candidate, confidence=confidence, resamples=resamples
            )
            result["judge_calls"] = candidate.judge.calls if candidate.judge else []
    comparisons = [
        {
            "before": before["name"],
            "after": after["name"],
            **paired_difference(
                _accuracy_map(before["held-out"]),
                _accuracy_map(after["held-out"]),
                confidence=confidence,
                resamples=resamples,
            ),
        }
        for before, after in itertools.combinations(results, 2)
    ]
    return {
        "schema": REPORT_SCHEMA,
        "evalsi_source_sha256": digest(
            {
                str(path.relative_to(Path(__file__).parent)): hashlib.sha256(
                    path.read_bytes()
                ).hexdigest()
                for path in sorted(Path(__file__).parent.rglob("*.py"))
            }
        ),
        "status": "complete"
        if selected_candidate is not None
        and all(
            result["selection"]["threshold"] is not None
            and all(
                not result[phase]["incomplete_cases"] and result[phase]["cases"]
                for phase in ("tuning", "held-out")
            )
            for result in results
        )
        else "incomplete",
        "evalsi_version": __version__,
        "python_version": platform.python_version(),
        "started_at": started,
        "finished_at": datetime.now(UTC).isoformat(),
        "dataset_sha256": sha256,
        "dataset": document,
        "protocol": protocol,
        "protocol_sha256": digest(protocol),
        "cache": {"local": "disabled", "provider": "not controlled"},
        "statistics": {
            "confidence": confidence,
            "resamples": resamples,
            "seed": 0,
            "method": "percentile bootstrap of independent groups; equal weight per case",
            "estimand": "expected single-call agreement with strict-majority human label",
            "ci_min_defined_fraction": 0.9,
        },
        "selected_candidate": selected_candidate,
        "selection_source": "tuning only; declared-order ties",
        "candidates": results,
        "held_out_comparisons": comparisons,
    }


def calibrate(
    dataset: str | Path, protocol: dict[str, Any], *, registry: Registry | None = None
) -> dict[str, Any]:
    """Synchronous/notebook form of :func:`acalibrate`."""
    from evalsi.runner import _run_sync

    return _run_sync(acalibrate(dataset, protocol, registry=registry))


def compare_reports(before: dict[str, Any], after: dict[str, Any]) -> dict[str, Any]:
    """Compare evaluator drift on the same frozen targets and human labels."""
    for report in (before, after):
        if report.get("schema") != REPORT_SCHEMA:
            raise ValueError("expected a calibration report")
        if digest(report["dataset"]) != report["dataset_sha256"]:
            raise ValueError("calibration report dataset pin mismatch")
    if before["dataset_sha256"] != after["dataset_sha256"]:
        raise ValueError(
            "cannot attribute judge drift across different records or human-label revisions"
        )
    a = {candidate["name"]: candidate for candidate in before["candidates"]}
    b = {candidate["name"]: candidate for candidate in after["candidates"]}
    if a.keys() != b.keys():
        raise ValueError("report comparison requires the same candidate names")
    return {
        "schema": "evalsi.calibration-comparison/v1",
        "dataset_sha256": before["dataset_sha256"],
        "fixed_target_outputs": True,
        "candidates": [
            {
                "name": name,
                "evaluator_changed": a[name]["definition_sha256"] != b[name]["definition_sha256"],
                "threshold_changed": a[name]["selection"]["threshold"]
                != b[name]["selection"]["threshold"],
                **paired_difference(
                    _accuracy_map(a[name]["held-out"]),
                    _accuracy_map(b[name]["held-out"]),
                    confidence=after["statistics"]["confidence"],
                    resamples=after["statistics"]["resamples"],
                ),
            }
            for name in a
        ],
    }

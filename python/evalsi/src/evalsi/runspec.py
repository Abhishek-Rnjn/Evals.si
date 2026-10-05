"""Run specs (``run.yaml``) and gates.

A spec file looks like a Kubernetes object; ``spec`` is the
``evalsi.v1alpha1.RunSpec`` message, so the embedded runner, the server and
(later) the operator all validate it against the same schema::

    apiVersion: evals.si/v1alpha1
    kind: EvalRun
    metadata: {name: nightly, project: support}
    spec:
      target: {connector: anthropic, model: claude-opus-5-5}
      dataset: {path: qa.jsonl, mapping: {input: question, reference: answer}}
      evaluators: [{ref: exact-match}, {ref: llm-judge}]
      trials: 3
      gates: [{metric: exact-match, stat: GATE_STAT_CI_LOW, min: 0.8}]
"""

from __future__ import annotations

import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml
from google.protobuf import json_format
from google.protobuf.descriptor import Descriptor

from evalsi.results import MetricSummary
from evalsi.v1alpha1 import run_pb2

API_VERSION = "evals.si/v1alpha1"
KIND = "EvalRun"


class SpecError(ValueError):
    pass


@dataclass
class RunFile:
    name: str
    project: str
    spec: run_pb2.RunSpec
    # Directory the spec came from; dataset paths are relative to it.
    base_dir: Path = field(default_factory=Path.cwd)
    # Labels for access rules on a server (metadata.labels).
    labels: dict[str, str] = field(default_factory=dict)


_DURATION = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m|h)$")


def _duration(value: Any) -> Any:
    if isinstance(value, int | float) and not isinstance(value, bool):
        return f"{value}s"
    if isinstance(value, str) and (m := _DURATION.match(value.strip())):
        scale = {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[m.group(2)]
        return f"{float(m.group(1)) * scale:g}s"
    return value


def normalize_durations(value: Any, descriptor: Descriptor) -> Any:
    """Specs may write durations as 250ms, 30s, 10m or 1.5h (or a number of
    seconds); protobuf JSON only takes seconds, so they are converted here."""
    if not isinstance(value, Mapping):
        return value
    out: dict[str, Any] = {}
    fields = {f.name: f for f in descriptor.fields}
    fields.update({f.json_name: f for f in descriptor.fields})
    for key, item in value.items():
        field_desc = fields.get(key)
        if field_desc is None or field_desc.message_type is None:
            out[key] = item
            continue
        sub = field_desc.message_type
        if sub.full_name == "google.protobuf.Duration":
            out[key] = _duration(item)
        elif field_desc.is_repeated and isinstance(item, list):
            out[key] = [normalize_durations(i, sub) for i in item]
        elif sub.GetOptions().map_entry:
            out[key] = item
        else:
            out[key] = normalize_durations(item, sub)
    return out


def parse_spec(document: Mapping[str, Any], base_dir: Path | None = None) -> RunFile:
    if document.get("apiVersion") not in (None, API_VERSION):
        raise SpecError(f"apiVersion must be {API_VERSION}, not {document.get('apiVersion')!r}")
    if document.get("kind") not in (None, KIND):
        raise SpecError(f"kind must be {KIND}, not {document.get('kind')!r}")
    metadata = document.get("metadata") or {}
    body = document.get("spec")
    if not isinstance(body, Mapping):
        raise SpecError("the document needs a 'spec' mapping")
    try:
        spec = json_format.ParseDict(
            normalize_durations(body, run_pb2.RunSpec.DESCRIPTOR), run_pb2.RunSpec()
        )
    except json_format.ParseError as exc:
        raise SpecError(f"invalid spec: {exc}") from exc
    validate(spec)
    labels = metadata.get("labels") or {}
    if not isinstance(labels, Mapping):
        raise SpecError("metadata.labels must be a mapping")
    return RunFile(
        labels={str(k): str(v) for k, v in labels.items()},
        name=str(metadata.get("name", "")),
        project=str(metadata.get("project", "")),
        spec=spec,
        base_dir=base_dir or Path.cwd(),
    )


def load_spec(path: str | Path) -> RunFile:
    path = Path(path)
    try:
        document = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        raise SpecError(f"{path}: {exc}") from exc
    if not isinstance(document, Mapping):
        raise SpecError(f"{path}: expected a YAML mapping")
    return parse_spec(document, base_dir=path.resolve().parent)


def validate(spec: run_pb2.RunSpec) -> None:
    if not spec.evaluators:
        raise SpecError("spec.evaluators must list at least one evaluator")
    if spec.dataset.WhichOneof("source") is None:
        raise SpecError("spec.dataset needs one of inline, path, uri, traces or run")
    if spec.trials < 0:
        raise SpecError("spec.trials cannot be negative")
    agent_run = spec.HasField("harness") or spec.target.HasField("agent")
    if spec.trials > 1 and not spec.HasField("target") and not agent_run:
        raise SpecError("spec.trials above 1 needs a target; without one every trial is identical")
    if spec.HasField("target") and not agent_run and not spec.target.model:
        raise SpecError("spec.target needs a model (or an agent)")
    for gate in spec.gates:
        if not gate.metric:
            raise SpecError("every gate needs a metric")
        if not gate.HasField("min") and not gate.HasField("max"):
            raise SpecError(f"gate on {gate.metric!r} needs min or max")


def spec_to_dict(spec: run_pb2.RunSpec) -> dict[str, Any]:
    out: dict[str, Any] = json_format.MessageToDict(spec, preserving_proto_field_name=True)
    return out


# --- gates ---


@dataclass
class GateResult:
    metric: str
    stat: str
    min: float | None
    max: float | None
    passed: bool
    value: float | None = None
    reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {k: v for k, v in vars(self).items() if v is not None and v != ""}


_STATS = {
    run_pb2.GATE_STAT_UNSPECIFIED: "mean",
    run_pb2.GATE_STAT_MEAN: "mean",
    run_pb2.GATE_STAT_CI_LOW: "ci_low",
    run_pb2.GATE_STAT_CI_HIGH: "ci_high",
}


def check_gates(
    gates: Sequence[run_pb2.Gate], summaries: Sequence[MetricSummary]
) -> list[GateResult]:
    by_metric = {s.metric: s for s in summaries}
    out = []
    for gate in gates:
        stat = _STATS[gate.stat]
        lo = gate.min if gate.HasField("min") else None
        hi = gate.max if gate.HasField("max") else None
        result = GateResult(metric=gate.metric, stat=stat, min=lo, max=hi, passed=False)
        summary = by_metric.get(gate.metric)
        if summary is None:
            result.reason = "no such metric in this run"
        else:
            value = {
                "mean": summary.mean,
                "ci_low": summary.ci.low if summary.ci else None,
                "ci_high": summary.ci.high if summary.ci else None,
            }[stat]
            result.value = value
            if value is None:
                result.reason = f"metric has no {stat}"
            elif lo is not None and value < lo:
                result.reason = f"{stat} {value:.4g} is below {lo:.4g}"
            elif hi is not None and value > hi:
                result.reason = f"{stat} {value:.4g} is above {hi:.4g}"
            else:
                result.passed = True
        out.append(result)
    return out

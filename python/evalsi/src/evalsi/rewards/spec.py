"""``RewardSpec``: how a reward is put together from evaluators.

A reward is a weighted sum of components. Each component is an evaluator
(any registered one, typically from the ``rl`` pack) with its params, a
weight and optionally a gate::

    apiVersion: evals.si/v1alpha1
    kind: RewardSpec
    metadata: {name: code-grpo}
    spec:
      components:
        - {ref: format-check, weight: 0.1, gate: true}
        - ref: code-exec-tests
          weight: 0.9
          params: {timeout_s: 10}
          sandbox: {minIsolation: namespaced}
      cache: {enabled: true}

The rules, shared with the server's Reward Service (``testdata/reward_vectors.json``
checks both):

- A component's value is its score as a number (passed counts 1 or 0).
- A gate passes when its component scored at least ``threshold`` (default 1).
  A gate that was skipped or errored fails.
- If any gate fails, the total is ``floor`` (default 0).
- Otherwise the total is the sum of ``weight * value``; skipped components
  add nothing, and errored ones follow ``onError``: ``raise`` (default) stops
  the batch, ``zero`` counts them as 0, ``none`` makes the total ``None``.
- ``clip: [lo, hi]`` bounds the total last.
"""

from __future__ import annotations

import json
import math
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from evalsi.evaluator import EvaluatorConfigError

API_VERSION = "evals.si/v1alpha1"
KIND = "RewardSpec"
ON_ERROR = ("raise", "zero", "none")

# Columns that hold the reference answer, in order of preference, when the
# spec does not name one. These cover TRL, verl and common datasets.
REFERENCE_COLUMNS = ("reference", "ground_truth", "answer", "solution", "label")


class RewardSpecError(EvaluatorConfigError):
    """The reward spec is malformed."""


@dataclass(frozen=True)
class Component:
    ref: str
    name: str = ""
    weight: float = 1.0
    gate: bool = False
    threshold: float = 1.0
    # The metric to read when the evaluator reports several; default: its first.
    metric: str = ""
    params: Mapping[str, Any] = field(default_factory=dict)
    # Sandbox requirements. min_isolation and memory_mb become params of
    # evaluators that take them; warm_pool sizes the server's pool.
    min_isolation: str = ""
    network: str = ""
    warm_pool: int = 0

    @property
    def key(self) -> str:
        return self.name or self.ref.rsplit("/", 1)[-1].partition("@")[0]

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"ref": self.ref, "weight": self.weight}
        if self.name:
            out["name"] = self.name
        if self.gate:
            out["gate"] = True
            out["threshold"] = self.threshold
        if self.metric:
            out["metric"] = self.metric
        if self.params:
            out["params"] = dict(self.params)
        sandbox = {
            k: v
            for k, v in (
                ("minIsolation", self.min_isolation),
                ("network", self.network),
                ("warmPool", self.warm_pool),
            )
            if v
        }
        if sandbox:
            out["sandbox"] = sandbox
        return out


@dataclass(frozen=True)
class RewardSpec:
    name: str
    components: tuple[Component, ...]
    floor: float = 0.0
    on_error: str = "raise"
    clip: tuple[float, float] | None = None
    cache: bool = True
    cache_size: int = 100_000
    breakdown: bool = True
    concurrency: int = 64
    # Column holding the reference answer; empty means the first of REFERENCE_COLUMNS.
    reference_field: str = ""
    judge: Mapping[str, Any] | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> RewardSpec:
        if "spec" in data or "kind" in data:
            if data.get("kind", KIND) != KIND:
                raise RewardSpecError(f"kind is {data.get('kind')!r}, not {KIND}")
            version = data.get("apiVersion", API_VERSION)
            if version != API_VERSION:
                raise RewardSpecError(f"apiVersion {version!r} is not {API_VERSION}")
            name = str((data.get("metadata") or {}).get("name", ""))
            body = data.get("spec") or {}
        else:
            name, body = str(data.get("name", "")), data
        if not isinstance(body, Mapping):
            raise RewardSpecError("spec must be a mapping")
        known = {
            "name",
            "components",
            "floor",
            "onError",
            "clip",
            "cache",
            "breakdown",
            "concurrency",
            "referenceField",
            "judge",
        }
        unknown = set(body) - known
        if unknown:
            raise RewardSpecError(f"unknown RewardSpec fields: {sorted(unknown)}")
        raw = body.get("components")
        if not isinstance(raw, list) or not raw:
            raise RewardSpecError("a RewardSpec needs at least one component")
        components = tuple(_component(c, i) for i, c in enumerate(raw))
        keys = [c.key for c in components]
        duplicates = sorted({k for k in keys if keys.count(k) > 1})
        if duplicates:
            raise RewardSpecError(f"components {duplicates} appear twice; give each a 'name'")
        on_error = str(body.get("onError", "raise"))
        if on_error not in ON_ERROR:
            raise RewardSpecError(f"onError must be one of {ON_ERROR}")
        clip = body.get("clip")
        if clip is not None:
            if not (isinstance(clip, list) and len(clip) == 2 and float(clip[0]) <= float(clip[1])):
                raise RewardSpecError("clip must be [low, high] with low <= high")
            clip = (float(clip[0]), float(clip[1]))
        cache = body.get("cache", {})
        if isinstance(cache, bool):
            cache = {"enabled": cache}
        concurrency = int(body.get("concurrency", 64))
        if concurrency < 1:
            raise RewardSpecError("concurrency must be at least 1")
        judge = body.get("judge")
        if judge is not None and not isinstance(judge, Mapping):
            raise RewardSpecError("judge must be a mapping of JudgeConfig fields")
        return cls(
            name=name or str(body.get("name", "")) or "reward",
            components=components,
            floor=float(body.get("floor", 0.0)),
            on_error=on_error,
            clip=clip,
            cache=bool(cache.get("enabled", True)),
            cache_size=int(cache.get("size", 100_000)),
            breakdown=bool(body.get("breakdown", True)),
            concurrency=concurrency,
            reference_field=str(body.get("referenceField", "")),
            judge=dict(judge) if judge else None,
        )

    def to_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {
            "components": [c.to_dict() for c in self.components],
            "floor": self.floor,
            "onError": self.on_error,
            "cache": {"enabled": self.cache, "size": self.cache_size},
            "breakdown": self.breakdown,
            "concurrency": self.concurrency,
        }
        if self.clip is not None:
            body["clip"] = list(self.clip)
        if self.reference_field:
            body["referenceField"] = self.reference_field
        if self.judge:
            body["judge"] = dict(self.judge)
        return {
            "apiVersion": API_VERSION,
            "kind": KIND,
            "metadata": {"name": self.name},
            "spec": body,
        }

    def fingerprint(self) -> str:
        """Canonical JSON of everything that changes rewards."""
        return json.dumps(self.to_dict()["spec"], sort_keys=True, separators=(",", ":"))


def _component(data: Any, index: int) -> Component:
    if isinstance(data, str):
        data = {"ref": data}
    if not isinstance(data, Mapping) or not data.get("ref"):
        raise RewardSpecError(f"component {index} needs a 'ref'")
    known = {"ref", "name", "weight", "gate", "threshold", "metric", "params", "sandbox"}
    unknown = set(data) - known
    if unknown:
        raise RewardSpecError(f"component {data['ref']!r}: unknown fields {sorted(unknown)}")
    params = data.get("params") or {}
    if not isinstance(params, Mapping):
        raise RewardSpecError(f"component {data['ref']!r}: params must be a mapping")
    sandbox = data.get("sandbox") or {}
    if not isinstance(sandbox, Mapping):
        raise RewardSpecError(f"component {data['ref']!r}: sandbox must be a mapping")
    unknown = set(sandbox) - {"minIsolation", "network", "warmPool"}
    if unknown:
        raise RewardSpecError(
            f"component {data['ref']!r}: unknown sandbox fields {sorted(unknown)}"
        )
    weight = float(data.get("weight", 1.0))
    if math.isnan(weight):
        raise RewardSpecError(f"component {data['ref']!r}: weight is not a number")
    return Component(
        ref=str(data["ref"]),
        name=str(data.get("name", "")),
        weight=weight,
        gate=bool(data.get("gate", False)),
        threshold=float(data.get("threshold", 1.0)),
        metric=str(data.get("metric", "")),
        params=dict(params),
        min_isolation=str(sandbox.get("minIsolation", "")),
        network=str(sandbox.get("network", "")),
        warm_pool=int(sandbox.get("warmPool", 0)),
    )


def load_spec(source: str | Path | Mapping[str, Any] | RewardSpec) -> RewardSpec:
    """A spec from a YAML or JSON file, a mapping, or a spec."""
    if isinstance(source, RewardSpec):
        return source
    if isinstance(source, Mapping):
        return RewardSpec.from_dict(source)
    path = Path(source)
    try:
        data = yaml.safe_load(path.read_text())
    except OSError as exc:
        raise RewardSpecError(f"cannot read reward spec {path}: {exc}") from exc
    if not isinstance(data, Mapping):
        raise RewardSpecError(f"{path} does not hold a RewardSpec")
    return RewardSpec.from_dict(data)

"""The evaluator API: the ``@evaluator`` decorator and the objects it builds.

An evaluator is a function. Keyword-only parameters become its params (with
their defaults), an optional ``ctx`` parameter receives the run context
(the judge client, for example), and the function may be sync or async::

    @evaluator(name="acme/json-valid", version="1.0.0")
    def json_valid(record: Record) -> Score:
        ...

Record-scope evaluators take one ``Record``; dataset-scope evaluators take the
full ``list[Record]``. Both return a ``Score`` or a list of scores.
"""

from __future__ import annotations

import inspect
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from enum import StrEnum
from typing import TYPE_CHECKING, Any

from evalsi.types import Record, Score

if TYPE_CHECKING:
    from evalsi.judges import JudgeClient


class Scope(StrEnum):
    RECORD = "record"
    DATASET = "dataset"


class ScoreType(StrEnum):
    NUMBER = "number"
    PASSED = "passed"
    LABEL = "label"
    STRUCTURED = "structured"


@dataclass(frozen=True)
class Requirements:
    """What a record must contain. Records that lack a required field are
    reported as skipped, never scored as zero."""

    input: bool = False
    output: bool = True
    reference: bool = False
    context: bool = False
    judge: bool = False
    trajectory: bool = False
    # Executes code from the record in the sandbox. Servers expose this to
    # access rules as ``resource.runs_code``.
    sandbox: bool = False

    def missing(self, record: Record) -> str | None:
        """The reason ``record`` cannot be evaluated, or ``None`` if it can."""
        for name in ("input", "output", "reference", "trajectory"):
            if getattr(self, name) and getattr(record, name) is None:
                return f"record has no {name}"
        if self.context and not record.context:
            return "record has no context"
        return None


@dataclass(frozen=True)
class MetricSpec:
    name: str
    type: ScoreType
    min: float | None = None
    max: float | None = None
    higher_is_better: bool | None = None
    description: str = ""


@dataclass(frozen=True)
class EvaluatorSpec:
    """Static description of an evaluator; mirrors ``EvaluatorManifest`` in the proto."""

    name: str
    version: str
    description: str
    scope: Scope
    requires: Requirements
    outputs: tuple[MetricSpec, ...]
    params: Mapping[str, Any]
    pack: str = ""
    # "native", "wrapped" or "community"; see EvaluatorManifest.tier.
    tier: str = ""
    # "python", or "wasm" for plugins run by evalsid.
    runtime: str = "python"
    # Params whose values name worker environment variables (secrets the
    # evaluator reads), each mapped to the param holding the URL the value is
    # sent to ("" when none). A server checks the named variables against the
    # project's credential grants, and its worker refuses a call where any
    # other param names one of its variables.
    secrets: Mapping[str, str] = field(default_factory=dict)

    @property
    def short_name(self) -> str:
        return self.name.rsplit("/", 1)[-1]

    @property
    def ref(self) -> str:
        return f"{self.name}@{self.version}"

    def output(self, name: str) -> MetricSpec | None:
        return next((o for o in self.outputs if o.name == name), None)


class SkipRecord(Exception):
    """Raise from an evaluator when a record cannot be evaluated, for example a
    rubric that needs context the record does not have."""


class EvaluatorConfigError(ValueError):
    """An evaluator was referenced or configured incorrectly."""


@dataclass
class EvalContext:
    """Per-run services handed to evaluators that declare a ``ctx`` parameter."""

    judge: JudgeClient | None = None


EvaluatorFn = Callable[..., Score | list[Score] | Awaitable[Score | list[Score]]]


@dataclass
class EvaluatorDef:
    """An evaluator function plus its spec, before params are bound."""

    spec: EvaluatorSpec
    fn: EvaluatorFn
    wants_ctx: bool

    def bind(self, params: Mapping[str, Any] | None = None, alias: str = "") -> BoundEvaluator:
        params = dict(params or {})
        unknown = set(params) - set(self.spec.params)
        if unknown:
            known = ", ".join(sorted(self.spec.params)) or "none"
            raise EvaluatorConfigError(
                f"{self.spec.name} got unknown params {sorted(unknown)} (known: {known})"
            )
        merged = {**self.spec.params, **params}
        missing = sorted(k for k, v in merged.items() if v is inspect.Parameter.empty)
        if missing:
            raise EvaluatorConfigError(f"{self.spec.name} requires params {missing}")
        return BoundEvaluator(definition=self, params=merged, name=alias or self.spec.short_name)

    # Calling the decorated object directly runs it with default params, which
    # keeps evaluators easy to unit-test.
    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        return self.fn(*args, **kwargs)


@dataclass
class BoundEvaluator:
    """An evaluator with its params fixed, ready to run."""

    definition: EvaluatorDef
    params: dict[str, Any] = field(default_factory=dict)
    name: str = ""

    @property
    def spec(self) -> EvaluatorSpec:
        return self.definition.spec

    async def run(self, target: Record | list[Record], ctx: EvalContext) -> list[Score]:
        kwargs = dict(self.params)
        if self.definition.wants_ctx:
            kwargs["ctx"] = ctx
        result = self.definition.fn(target, **kwargs)
        if inspect.isawaitable(result):
            result = await result
        scores = result if isinstance(result, list) else [result]
        for score in scores:
            if not isinstance(score, Score):
                raise TypeError(f"{self.spec.name} returned {type(score).__name__}, not Score")
            if not score.name:
                score.name = self.spec.outputs[0].name if len(self.spec.outputs) == 1 else ""
        return scores


def evaluator(
    *,
    name: str,
    version: str,
    description: str = "",
    scope: Scope | str = Scope.RECORD,
    requires: Requirements | None = None,
    outputs: list[MetricSpec] | None = None,
    pack: str = "",
    secrets: Mapping[str, str] | None = None,
) -> Callable[[EvaluatorFn], EvaluatorDef]:
    """Turn a function into an evaluator.

    ``name`` is namespaced (``"acme/json-valid"``). If ``outputs`` is omitted
    the evaluator declares one numeric metric named after its short name.

    ``secrets`` declares the params whose values name environment variables
    the evaluator reads (``{"api_key_env": "url"}``: the param, and the param
    holding where the value goes, or ``""``). Params ending in ``_env`` are
    declared automatically, sent to ``url`` or ``base_url`` when the
    evaluator has one.
    """
    if "/" not in name:
        raise EvaluatorConfigError(
            f"evaluator name {name!r} must be namespaced, e.g. 'acme/{name}'"
        )

    def wrap(fn: EvaluatorFn) -> EvaluatorDef:
        signature = inspect.signature(fn)
        params: dict[str, Any] = {}
        wants_ctx = False
        for param in signature.parameters.values():
            if param.name == "ctx":
                wants_ctx = True
            elif param.kind is inspect.Parameter.KEYWORD_ONLY:
                params[param.name] = param.default
        declared = _secret_params(name, params, secrets or {})
        short = name.rsplit("/", 1)[-1]
        spec = EvaluatorSpec(
            name=name,
            version=version,
            description=description or inspect.getdoc(fn) or "",
            scope=Scope(scope),
            requires=requires or Requirements(),
            outputs=tuple(outputs or [MetricSpec(name=short, type=ScoreType.NUMBER)]),
            params=params,
            pack=pack,
            secrets=declared,
        )
        return EvaluatorDef(spec=spec, fn=fn, wants_ctx=wants_ctx)

    return wrap


def _secret_params(
    name: str, params: Mapping[str, Any], explicit: Mapping[str, str]
) -> dict[str, str]:
    dest = "url" if "url" in params else "base_url" if "base_url" in params else ""
    out = {p: dest for p in params if p.endswith("_env")}
    for param, sent_to in explicit.items():
        if param not in params:
            raise EvaluatorConfigError(f"{name}: secrets names {param!r}, which is not a param")
        if sent_to and sent_to not in params:
            raise EvaluatorConfigError(
                f"{name}: secrets sends {param!r} to {sent_to!r}, which is not a param"
            )
        out[param] = sent_to
    return out

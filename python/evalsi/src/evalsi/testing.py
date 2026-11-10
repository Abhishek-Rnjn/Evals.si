"""Helpers for testing evaluators and adapters without a real judge.

``scripted_judge`` builds an ``EvalContext`` whose judge answers each call
with ``reply(prompt, schema)`` and records the calls, so judge-based
evaluators, including adapters' framework metrics, run offline::

    ctx, judge = scripted_judge(lambda prompt, schema: {"score": 5, "reasoning": "ok"})
    scores = asyncio.run(my_evaluator.bind().run(record, ctx))
"""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from evalsi.evaluator import EvalContext, EvaluatorDef
from evalsi.judges import JudgeClient, JudgeConfig, JudgeFatalError, JudgeResponse
from evalsi.types import Record, Score

Reply = Callable[[str, dict[str, Any]], dict[str, Any]]
# prompt -> the first token's top logprobs (token -> log probability).
Logprobs = Callable[[str], dict[str, float]]

TEST_JUDGE = JudgeConfig(provider="openai-compatible", model="scripted", base_url="http://judge")


@dataclass
class ScriptedJudge:
    """A judge backend that answers from a function and records each call."""

    reply: Reply
    # Answers next_token_logprobs; without it the judge has no logprobs, like Claude.
    logprobs: Logprobs | None = None
    calls: list[tuple[str, dict[str, Any]]] = field(default_factory=list)

    async def complete_json(
        self, *, system: str, prompt: str, schema: dict[str, Any]
    ) -> JudgeResponse:
        self.calls.append((prompt, schema))
        return JudgeResponse(
            data=self.reply(prompt, schema), model="scripted", input_tokens=10, output_tokens=5
        )

    async def next_token_logprobs(
        self, *, system: str, prompt: str, top_logprobs: int
    ) -> JudgeResponse:
        if self.logprobs is None:
            raise JudgeFatalError("the scripted judge has no logprobs")
        self.calls.append((prompt, {"top_logprobs": top_logprobs}))
        return JudgeResponse(
            data={"top_logprobs": self.logprobs(prompt)},
            model="scripted",
            input_tokens=10,
            output_tokens=1,
        )

    async def aclose(self) -> None:
        return None


def scripted_judge(
    reply: Reply, logprobs: Logprobs | None = None
) -> tuple[EvalContext, ScriptedJudge]:
    backend = ScriptedJudge(reply, logprobs)
    return EvalContext(judge=JudgeClient(TEST_JUDGE, backend)), backend


def score(
    definition: EvaluatorDef,
    target: Record | list[Record],
    ctx: EvalContext | None = None,
    **params: Any,
) -> dict[str, Score]:
    """Run an evaluator on one record (or a dataset) and key its scores by name."""
    scores = asyncio.run(definition.bind(params).run(target, ctx or EvalContext()))
    return {s.name: s for s in scores}


# --- gates inside a test suite --------------------------------------------------

_GATE_STATS = {
    "mean": "GATE_STAT_MEAN",
    "ci_low": "GATE_STAT_CI_LOW",
    "ci_high": "GATE_STAT_CI_HIGH",
    "error_rate": "GATE_STAT_ERROR_RATE",
    "skipped_rate": "GATE_STAT_SKIPPED_RATE",
    "count": "GATE_STAT_COUNT",
}

# Where the pytest plugin (evalsi.pytest_plugin) wants results written, and the
# name of the test asking; unset outside pytest or without --evalsi-results.
_results_sink: Callable[[Any], None] | None = None


def _gate_messages(gates: Any, max_error_rate: float | None, max_skipped_rate: float | None) -> Any:
    from evalsi.v1alpha1 import run_pb2

    out = []
    for metric, bound in gates.items():
        bounds = bound if isinstance(bound, dict) else {"min": bound}
        unknown = set(bounds) - {"min", "max", "stat"}
        if unknown or not ({"min", "max"} & set(bounds)):
            raise ValueError(
                f"gate on {metric!r}: give a number (the minimum mean) or a dict with min "
                f"and/or max, and optionally stat ({', '.join(_GATE_STATS)})"
            )
        stat = str(bounds.get("stat", "mean"))
        if stat not in _GATE_STATS:
            raise ValueError(f"gate on {metric!r}: unknown stat {stat!r}")
        gate = run_pb2.Gate(metric=metric, stat=run_pb2.GateStat.Value(_GATE_STATS[stat]))
        for name in ("min", "max"):
            if bounds.get(name) is not None:
                setattr(gate, name, float(bounds[name]))
        out.append(gate)
        # A mean leaves errored and skipped records out: gate them unless told not to.
        for limit, rate in ((max_error_rate, "error_rate"), (max_skipped_rate, "skipped_rate")):
            if limit is not None and stat not in ("error_rate", "skipped_rate"):
                out.append(
                    run_pb2.Gate(
                        metric=metric, stat=run_pb2.GateStat.Value(_GATE_STATS[rate]), max=limit
                    )
                )
    return out


def _generate(records: list[Record], task: Callable[[str], Any]) -> None:
    """Fill in the output of each record that has none by calling ``task`` with
    its input text; an async ``task`` is awaited."""
    import inspect

    from evalsi.runner import _run_sync
    from evalsi.types import Content

    async def one(record: Record) -> None:
        value = task(record.input.as_text() if record.input is not None else "")
        if inspect.isawaitable(value):
            value = await value
        record.output = value if isinstance(value, Content) else Content.from_value(value)

    async def every() -> None:
        for record in records:
            if record.output is None:
                await one(record)

    _run_sync(every())


def assert_eval(
    data: Any,
    evaluators: Any,
    gates: dict[str, Any],
    *,
    task: Callable[[str], Any] | None = None,
    max_error_rate: float | None = 0.0,
    max_skipped_rate: float | None = 0.0,
    examples: int = 5,
    **options: Any,
) -> Any:
    """Evaluate ``data`` and fail the test when a gate fails.

    For pytest or unittest, with no server and no spec file::

        def test_refund_answers():
            assert_eval("cases.jsonl", ["exact-match"], {"exact-match": 0.9}, task=my_app.answer)

    ``gates`` maps a metric to its minimum mean, or to ``{"min", "max", "stat"}`` with
    ``stat`` one of mean, ci_low, ci_high, error_rate, skipped_rate or count. ``task``
    (sync or async, ``str -> str``) produces the output of records that have none.
    Unless set to ``None``, ``max_error_rate`` and ``max_skipped_rate`` also apply to
    every gated metric, so records that errored or lacked what an evaluator needs
    cannot pass by being left out of the mean. An empty dataset fails. Other options
    are :func:`evalsi.evaluate`'s. Returns the ``EvalResult``.
    """
    from evalsi.datasets import load_records
    from evalsi.runner import evaluate
    from evalsi.runspec import check_gates

    if not gates:
        raise ValueError("assert_eval needs at least one gate, for example {'exact-match': 0.9}")
    messages = _gate_messages(gates, max_error_rate, max_skipped_rate)
    records = load_records(
        data, mapping=options.pop("mapping", None), limit=options.pop("limit", None)
    )
    if not records:
        raise AssertionError("the dataset has no records: nothing was evaluated")
    if task is not None:
        _generate(records, task)
    result = evaluate(records, evaluators, **options)
    if _results_sink is not None:
        _results_sink(result)
    failed = [g for g in check_gates(messages, result.summaries) if not g.passed]
    if not failed:
        return result
    lines = ["evaluation gates failed:"]
    for g in failed:
        bound = " ".join(f"{k} {v:g}" for k, v in (("min", g.min), ("max", g.max)) if v is not None)
        lines.append(f"  {g.metric} {g.stat} {bound}: {g.reason}")
    lines += ["", result.table()]
    shown = 0
    for r in result.results:
        # A number has no pass mark of its own; failed checks and errors do.
        bad = r.outcome.value != "scored" or any(s.passed is False for s in r.scores)
        if not bad or shown >= max(0, examples):
            continue
        shown += 1
        detail = r.reason or "; ".join(
            f"{s.name or r.evaluator}="
            f"{s.passed if s.passed is not None else s.number}"
            f"{' (' + s.explanation + ')' if s.explanation else ''}"
            for s in r.scores
        )
        lines.append(f"  record {r.record_id!r} · {r.evaluator} · {r.outcome.value}: {detail}")
    raise AssertionError("\n".join(lines))

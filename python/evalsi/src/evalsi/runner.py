"""The embedded runner: evaluates records in-process with asyncio.

This is the Tier 0 "embedded library" from the design. The server (Phase 1)
runs the same evaluators through workers; specs and results are identical.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import time
from collections.abc import Awaitable, Callable, Iterable, Mapping, Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, TypeVar

from evalsi._version import __version__
from evalsi.datasets import load_records, records_hash
from evalsi.evaluator import (
    BoundEvaluator,
    EvalContext,
    EvaluatorConfigError,
    EvaluatorDef,
    Scope,
    SkipRecord,
)
from evalsi.judges import JudgeClient, JudgeConfig, JudgeFatalError, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.registry import Registry, default_registry
from evalsi.results import EvalResult, summarize
from evalsi.types import EvaluationResult, Outcome, Record, Score

EvaluatorLike = str | EvaluatorDef | BoundEvaluator | Mapping[str, Any]
ProgressFn = Callable[[int, int], None]
T = TypeVar("T")


def bind_evaluators(
    evaluators: Sequence[EvaluatorLike],
    *,
    params: Mapping[str, Mapping[str, Any]] | None = None,
    registry: Registry | None = None,
) -> list[BoundEvaluator]:
    """Resolve evaluator references and bind their params.

    Each item is a reference string (``"exact-match"``), a mapping
    ``{"ref": ..., "params": {...}, "name": "alias"}``, a decorated evaluator,
    or an already bound evaluator. ``params`` maps instance names to extra params.
    """
    registry = registry or default_registry()
    params = params or {}
    bound: list[BoundEvaluator] = []
    for item in evaluators:
        if isinstance(item, BoundEvaluator):
            instance = item
        elif isinstance(item, EvaluatorDef):
            instance = item.bind(params.get(item.spec.short_name))
        elif isinstance(item, str):
            definition = registry.resolve(item)
            instance = definition.bind(params.get(definition.spec.short_name))
        elif isinstance(item, Mapping):
            if "ref" not in item:
                raise EvaluatorConfigError(f"evaluator entry {dict(item)!r} has no 'ref'")
            definition = registry.resolve(str(item["ref"]))
            alias = str(item.get("name", ""))
            merged = {
                **dict(item.get("params") or {}),
                **params.get(alias or definition.spec.short_name, {}),
            }
            instance = definition.bind(merged, alias=alias)
        else:
            raise EvaluatorConfigError(f"cannot use {item!r} as an evaluator")
        bound.append(instance)
    names = [b.name for b in bound]
    duplicates = sorted({n for n in names if names.count(n) > 1})
    if duplicates:
        raise EvaluatorConfigError(
            f"evaluators {duplicates} are used more than once; give each use a distinct "
            "'name' alias"
        )
    unknown = set(params) - set(names)
    if unknown:
        raise EvaluatorConfigError(f"params given for evaluators not in the run: {sorted(unknown)}")
    return bound


async def run_evaluators(
    records: Sequence[Record],
    instances: Sequence[BoundEvaluator],
    ctx: EvalContext,
    *,
    concurrency: int = 16,
    on_progress: ProgressFn | None = None,
) -> list[EvaluationResult]:
    """Run every evaluator over every record. Results come back in a stable
    order (record, then evaluator) whatever order they finished in."""
    record_level = [i for i in instances if i.spec.scope is Scope.RECORD]
    dataset_level = [i for i in instances if i.spec.scope is Scope.DATASET]
    tasks = [
        (ri, ii, record, inst)
        for ri, record in enumerate(records)
        for ii, inst in enumerate(record_level)
    ]
    total = len(tasks) + len(dataset_level)
    done = 0
    slots: dict[tuple[int, int], EvaluationResult] = {}
    queue: asyncio.Queue[tuple[int, int, Record, BoundEvaluator]] = asyncio.Queue()
    for task in tasks:
        queue.put_nowait(task)

    def tick() -> None:
        nonlocal done
        done += 1
        if on_progress is not None:
            on_progress(done, total)

    async def worker() -> None:
        while True:
            try:
                ri, ii, record, inst = queue.get_nowait()
            except asyncio.QueueEmpty:
                return
            slots[(ri, ii)] = await _evaluate_one(inst, record, record.id, ctx)
            tick()

    try:
        async with asyncio.TaskGroup() as group:
            for _ in range(max(1, min(concurrency, len(tasks)))):
                group.create_task(worker())
    except* JudgeFatalError as eg:
        raise eg.exceptions[0] from None

    results = [slots[key] for key in sorted(slots)]
    for inst in dataset_level:
        eligible = [r for r in records if inst.spec.requires.missing(r) is None]
        result = await _evaluate_one(inst, eligible, "", ctx)
        if len(eligible) < len(records):
            result.reason = (
                result.reason or f"{len(records) - len(eligible)} records lacked required fields"
            )
        results.append(result)
        tick()
    return results


async def _evaluate_one(
    inst: BoundEvaluator, target: Record | list[Record], record_id: str, ctx: EvalContext
) -> EvaluationResult:
    start = time.perf_counter()

    def finish(
        outcome: Outcome, *, scores: list[Score] | None = None, reason: str = ""
    ) -> EvaluationResult:
        return EvaluationResult(
            record_id=record_id,
            evaluator=inst.name,
            evaluator_ref=inst.spec.ref,
            outcome=outcome,
            scores=scores or [],
            reason=reason,
            duration_ms=(time.perf_counter() - start) * 1000,
        )

    if isinstance(target, Record):
        missing = inst.spec.requires.missing(target)
        if missing is not None:
            return finish(Outcome.SKIPPED, reason=missing)
    try:
        scores = await inst.run(target, ctx)
    except SkipRecord as exc:
        return finish(Outcome.SKIPPED, reason=str(exc))
    except JudgeFatalError:
        raise
    except Exception as exc:
        return finish(Outcome.ERROR, reason=f"{type(exc).__name__}: {exc}")
    if not scores:
        return finish(Outcome.SKIPPED, reason="evaluator returned no scores")
    return finish(Outcome.SCORED, scores=scores)


async def aevaluate(
    data: str | Path | Iterable[Mapping[str, Any] | Record],
    evaluators: Sequence[EvaluatorLike],
    *,
    mapping: Mapping[str, str] | None = None,
    limit: int | None = None,
    params: Mapping[str, Mapping[str, Any]] | None = None,
    judge: JudgeConfig | JudgeClient | None = None,
    cache: bool | str | Path = True,
    concurrency: int = 16,
    confidence: float = 0.95,
    cluster_by: str | None = None,
    ci_method: str = "auto",
    registry: Registry | None = None,
    on_progress: ProgressFn | None = None,
) -> EvalResult:
    """Async form of :func:`evaluate`."""
    if not 0 < confidence < 1:
        raise ValueError("confidence must be between 0 and 1")
    instances = bind_evaluators(evaluators, params=params, registry=registry)
    records = load_records(data, mapping=mapping, limit=limit)

    owned_judge: JudgeClient | None = None
    judge_client: JudgeClient | None = judge if isinstance(judge, JudgeClient) else None
    if any(i.spec.requires.judge for i in instances) and judge_client is None:
        config = judge if isinstance(judge, JudgeConfig) else JudgeConfig.from_env()
        if config is None:
            needing = [i.name for i in instances if i.spec.requires.judge]
            raise EvaluatorConfigError(
                f"{needing} need a judge model. Pass judge=JudgeConfig(...) or set "
                "EVALSI_JUDGE_PROVIDER, EVALSI_JUDGE_MODEL and EVALSI_JUDGE_BASE_URL."
            )
        judge_cache = None
        if cache is not False:
            judge_cache = JudgeCache(None if cache is True else cache)
        owned_judge = judge_client = create_judge(config, cache=judge_cache)

    started = datetime.now(UTC)
    try:
        results = await run_evaluators(
            records,
            instances,
            EvalContext(judge=judge_client),
            concurrency=concurrency,
            on_progress=on_progress,
        )
    finally:
        if owned_judge is not None:
            await owned_judge.aclose()

    summaries = summarize(
        instances,
        results,
        records,
        level=confidence,
        cluster_by=cluster_by,
        ci_method=ci_method,
    )
    manifest = {
        "evalsi_version": __version__,
        "api_version": "evalsi.v1alpha1",
        "started_at": started.isoformat(),
        "finished_at": datetime.now(UTC).isoformat(),
        "dataset": {
            "source": str(data) if isinstance(data, str | Path) else "<in-memory>",
            "records": len(records),
            "sha256": records_hash(records),
        },
        "evaluators": [{"name": i.name, "ref": i.spec.ref, "params": i.params} for i in instances],
        "judge": judge_client.config.describe() if judge_client is not None else None,
        "summary": {
            "confidence_level": confidence,
            "cluster_by": cluster_by,
            "ci_method": ci_method,
        },
    }
    return EvalResult(records=records, results=results, summaries=summaries, manifest=manifest)


def evaluate(
    data: str | Path | Iterable[Mapping[str, Any] | Record],
    evaluators: Sequence[EvaluatorLike],
    **options: Any,
) -> EvalResult:
    """Evaluate records with the given evaluators, in-process.

    ``data`` is a JSONL/JSON path, an ``hf://repo?config=..&split=..`` reference,
    or an iterable of dicts or records. Options are those of :func:`aevaluate`.
    Works inside notebooks, where an event loop is already running.
    """
    return _run_sync(aevaluate(data, evaluators, **options))


def _run_sync(coro: Awaitable[T]) -> T:
    async def main() -> T:
        return await coro

    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(main())
    # A loop is already running (Jupyter): run ours on a separate thread.
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        return pool.submit(asyncio.run, main()).result()

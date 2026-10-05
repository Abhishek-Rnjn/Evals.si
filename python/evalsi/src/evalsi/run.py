"""Embedded execution of a run spec: the same ``run.yaml`` the server accepts.

For each trial the target (if any) answers every record, then the evaluators
grade the answers. Summaries account for trials (clustered intervals, pass@k
and pass^k), gates decide success, and budgets stop runaway spend.
"""

from __future__ import annotations

import asyncio
import dataclasses
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from evalsi._version import __version__
from evalsi.convert import from_struct, record_from_proto
from evalsi.datasets import DatasetError, load_records, records_hash, split_uri
from evalsi.evaluator import BoundEvaluator, EvalContext, EvaluatorConfigError
from evalsi.judges import JudgeClient, JudgeConfig, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.registry import Registry
from evalsi.results import EvalResult, summarize
from evalsi.runner import bind_evaluators, run_evaluators
from evalsi.runspec import GateResult, RunFile, check_gates, spec_to_dict
from evalsi.targets import TargetConfig, create_target, generate_all
from evalsi.types import EvaluationResult, Outcome, Record, Usage
from evalsi.v1alpha1 import evaluation_service_pb2, run_pb2

ProgressFn = Callable[[str, int, int], None]


class BudgetExceeded(RuntimeError):
    pass


@dataclass
class RunResult:
    result: EvalResult
    gates: list[GateResult] = field(default_factory=list)
    target_usage: Usage = field(default_factory=Usage)
    judge_usage: Usage = field(default_factory=Usage)

    @property
    def passed(self) -> bool:
        return all(g.passed for g in self.gates)


def is_agent_run(spec: run_pb2.RunSpec) -> bool:
    """A run that drives an agent through tasks (a harness or an agent target)."""
    return spec.HasField("harness") or (spec.HasField("target") and spec.target.HasField("agent"))


def target_config(target: run_pb2.Target) -> TargetConfig:
    return TargetConfig(
        connector=target.connector,
        model=target.model,
        base_url=target.base_url,
        api_key_env=target.api_key_env,
        system_prompt=target.system_prompt,
        max_tokens=target.max_tokens or 16000,
        temperature=target.temperature if target.HasField("temperature") else None,
        effort=target.effort,
    )


def load_dataset(source: run_pb2.DatasetSource, base_dir: Path) -> list[Record]:
    kind = source.WhichOneof("source")
    mapping = dict(source.mapping)
    limit = source.limit or None
    if kind == "inline":
        records = [record_from_proto(r) for r in source.inline.records]
        return load_records(records, mapping=mapping, limit=limit)
    if kind == "path":
        path = Path(source.path)
        return load_records(
            path if path.is_absolute() else base_dir / path, mapping=mapping, limit=limit
        )
    if kind == "uri":
        return load_records(_resolve_uri(source.uri, base_dir), mapping=mapping, limit=limit)
    if kind in ("traces", "run"):
        raise DatasetError(
            f"dataset.{kind} reads the server's stored {'traces' if kind == 'traces' else 'runs'}; "
            "run this spec with --server"
        )
    raise EvaluatorConfigError("dataset needs one of inline, path, uri, traces or run")


def _resolve_uri(uri: str, base_dir: Path) -> str:
    """Importer URIs name local files, which resolve against ``base_dir`` like paths do."""
    if "://" not in uri:
        raise DatasetError(
            f"dataset uri {uri!r} needs a scheme such as hf:// or inspect://; use path for files"
        )
    scheme, path, _ = split_uri(uri)
    if scheme == "hf" or Path(path).is_absolute():
        return uri
    return uri.replace(f"{scheme}://{path}", f"{scheme}://{base_dir / path}", 1)


def _evaluator_entries(spec: run_pb2.RunSpec) -> list[dict[str, Any]]:
    return [
        {"ref": e.ref, "name": e.name, "params": from_struct(e.params)} for e in spec.evaluators
    ]


def _add(total: Usage, part: Usage | None) -> None:
    if part is None:
        return
    for name in ("input_tokens", "output_tokens"):
        value = getattr(part, name)
        if value is not None:
            setattr(total, name, (getattr(total, name) or 0) + value)


def _tokens(usage: Usage) -> int:
    return (usage.input_tokens or 0) + (usage.output_tokens or 0)


_CI = {
    evaluation_service_pb2.CI_METHOD_UNSPECIFIED: "auto",
    evaluation_service_pb2.CI_METHOD_AUTO: "auto",
    evaluation_service_pb2.CI_METHOD_BOOTSTRAP: "bootstrap",
}


async def execute(
    run: RunFile,
    *,
    judge: JudgeConfig | JudgeClient | None = None,
    registry: Registry | None = None,
    cache: bool = True,
    concurrency: int = 8,
    on_progress: ProgressFn | None = None,
) -> RunResult:
    spec = run.spec
    instances = bind_evaluators(_evaluator_entries(spec), registry=registry)
    records = load_dataset(spec.dataset, run.base_dir)
    trials = max(spec.trials, 1)

    judge_client = judge if isinstance(judge, JudgeClient) else None
    owned: JudgeClient | None = None
    agent_run = is_agent_run(spec)
    needs_judge = any(i.spec.requires.judge for i in instances) or (
        agent_run and spec.harness.builtin.HasField("user_simulator")
    )
    if judge_client is None and needs_judge:
        config = judge if isinstance(judge, JudgeConfig) else JudgeConfig.from_env()
        if config is None:
            raise EvaluatorConfigError(
                "this run needs a judge model; pass --judge-* options or set EVALSI_JUDGE_*"
            )
        owned = judge_client = create_judge(config, cache=JudgeCache() if cache else None)

    target = None
    agent: _AgentRunner | None = None
    if agent_run:
        agent = _AgentRunner(spec, run, judge_client, concurrency)
    elif spec.HasField("target"):
        target = create_target(target_config(spec.target))
    target_usage, judge_usage = Usage(), Usage()
    results: list[EvaluationResult] = []
    all_outputs: list[Record] = []
    started = datetime.now(UTC)
    try:
        for trial in range(trials):
            if agent is not None:
                outputs, failed = await agent.run_trial(records, trial, target_usage, on_progress)
            else:
                outputs, failed = await _produce(
                    target,
                    records,
                    trial,
                    target_usage,
                    concurrency=concurrency,
                    on_progress=on_progress,
                )
            if (
                spec.budget.max_target_tokens
                and _tokens(target_usage) > spec.budget.max_target_tokens
            ):
                raise BudgetExceeded(
                    f"target used {_tokens(target_usage)} tokens, "
                    f"over the budget of {spec.budget.max_target_tokens}"
                )
            trial_results = await run_evaluators(
                outputs,
                instances,
                EvalContext(judge=judge_client),
                concurrency=concurrency,
                on_progress=(lambda d, t: on_progress("evaluate", d, t)) if on_progress else None,
            )
            trial_results += _failed_results(failed, instances)
            for r in trial_results:
                r.trial = trial
                for score in r.scores:
                    _add(judge_usage, score.cost)
            if spec.budget.max_judge_tokens and _tokens(judge_usage) > spec.budget.max_judge_tokens:
                raise BudgetExceeded(
                    f"judges used {_tokens(judge_usage)} tokens, "
                    f"over the budget of {spec.budget.max_judge_tokens}"
                )
            results += trial_results
            all_outputs += outputs
    finally:
        if target is not None:
            await target.aclose()
        if agent is not None:
            await agent.aclose()
        if owned is not None:
            await owned.aclose()

    summaries = summarize(
        instances,
        results,
        records,
        level=spec.summary.confidence_level or 0.95,
        cluster_by=spec.summary.cluster_by or None,
        ci_method=_CI[spec.summary.ci_method],
        trials=trials,
    )
    manifest = {
        "evalsi_version": __version__,
        "api_version": "evalsi.v1alpha1",
        "name": run.name,
        "project": run.project,
        "started_at": started.isoformat(),
        "finished_at": datetime.now(UTC).isoformat(),
        "spec": spec_to_dict(spec),
        "dataset": {"records": len(records), "sha256": records_hash(records)},
        "target": target.config.describe() if target is not None else None,
        "agent": agent.describe() if agent is not None else None,
        "evaluators": [{"name": i.name, "ref": i.spec.ref, "params": i.params} for i in instances],
        "judge": judge_client.config.describe() if judge_client is not None else None,
        "trials": trials,
    }
    result = EvalResult(
        records=all_outputs or records, results=results, summaries=summaries, manifest=manifest
    )
    return RunResult(
        result=result,
        gates=check_gates(spec.gates, summaries),
        target_usage=target_usage,
        judge_usage=judge_usage,
    )


async def _produce(
    target: Any,
    records: list[Record],
    trial: int,
    usage: Usage,
    *,
    concurrency: int,
    on_progress: ProgressFn | None,
) -> tuple[list[Record], list[tuple[Record, str]]]:
    """Records with outputs for this trial, plus those whose generation failed."""
    if target is None:
        return list(records), []
    if on_progress:
        on_progress(f"generate (trial {trial + 1})", 0, len(records))
    generations = await generate_all(target, records, concurrency=concurrency)
    outputs: list[Record] = []
    failed: list[tuple[Record, str]] = []
    for record, gen in zip(records, generations, strict=True):
        _add(usage, gen.usage)
        if gen.output is None:
            failed.append((record, gen.error))
            continue
        metadata = dict(record.metadata)
        if gen.error:
            metadata["target_error"] = gen.error
        outputs.append(
            dataclasses.replace(record, output=gen.output, usage=gen.usage, metadata=metadata)
        )
    if on_progress:
        on_progress(f"generate (trial {trial + 1})", len(records), len(records))
    return outputs, failed


def _failed_results(
    failed: list[tuple[Record, str]], instances: list[BoundEvaluator]
) -> list[EvaluationResult]:
    return [
        EvaluationResult(
            record_id=record.id,
            evaluator=inst.name,
            evaluator_ref=inst.spec.ref,
            outcome=Outcome.ERROR,
            reason=error if error.startswith("task failed:") else f"target failed: {error}",
        )
        for record, error in failed
        for inst in instances
    ]


class _AgentRunner:
    """Agent runs: each record is a task, driven through the spec's harness."""

    def __init__(
        self,
        spec: run_pb2.RunSpec,
        run: RunFile,
        judge: JudgeClient | None,
        concurrency: int,
    ) -> None:
        try:
            from evalsi_harness import HarnessContext, load_harness
        except ImportError as exc:
            raise EvaluatorConfigError(
                "this spec is an agent run, which needs the harness: pip install evalsi-harness"
            ) from exc
        from evalsi.sandbox.client import SandboxClient, connect

        self.spec = spec
        self.run = run
        self.concurrency = concurrency
        self._sandboxes: SandboxClient | None = None
        self._lock = asyncio.Lock()

        async def sandboxes() -> SandboxClient:
            async with self._lock:
                if self._sandboxes is None:
                    self._sandboxes = await connect()
                return self._sandboxes

        def judge_for(name: str) -> JudgeClient:
            if judge is None:
                raise ValueError(
                    "the user simulator needs a judge: pass --judge-* or EVALSI_JUDGE_*"
                )
            return judge

        self.harness = load_harness(
            spec, HarnessContext(sandboxes=sandboxes, judge=judge_for, base_dir=run.base_dir)
        )

    def describe(self) -> dict[str, Any]:
        info = self.harness.describe()
        return {
            "harness": {"name": info.name, "version": info.version},
            "agent": spec_to_dict(self.spec).get("target"),
        }

    async def run_trial(
        self,
        records: list[Record],
        trial: int,
        usage: Usage,
        on_progress: ProgressFn | None,
    ) -> tuple[list[Record], list[tuple[Record, str]]]:
        from evalsi_harness import Task, run_task

        semaphore = asyncio.Semaphore(self.concurrency)
        done = 0
        stage = f"agent tasks (trial {trial + 1})"
        if on_progress:
            on_progress(stage, 0, len(records))

        async def one(record: Record) -> tuple[Record, Record | None, str]:
            nonlocal done
            async with semaphore:
                task = Task.build(self.spec, record, trial=trial, run_id=self.run.name)
                outcome = await run_task(task, self.harness)
            done += 1
            if on_progress:
                on_progress(stage, done, len(records))
            return record, outcome.record, outcome.error

        outputs: list[Record] = []
        failed: list[tuple[Record, str]] = []
        for record, result, error in await asyncio.gather(*(one(r) for r in records)):
            if result is None:
                failed.append((record, f"task failed: {error}"))
                continue
            _add(usage, result.usage)
            outputs.append(result)
        return outputs, failed

    async def aclose(self) -> None:
        await self.harness.aclose()
        if self._sandboxes is not None:
            await self._sandboxes.aclose()

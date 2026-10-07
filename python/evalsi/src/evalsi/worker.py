"""The evaluator worker: serves installed evaluators to ``evalsid`` over gRPC.

It implements ``evalsi.plugin.v1alpha1.EvaluatorPluginService`` plus the
standard gRPC health service. ``evalsid`` starts one (or more) of these as
subprocesses listening on a Unix socket::

    evalsi worker --listen unix:///run/evalsi/worker.sock --judges judges.json

``judges.json`` maps judge names to ``JudgeConfig`` fields. Judge clients are
created on first use and shared, with one response cache, for the worker's
lifetime.
"""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncIterator, Mapping
from pathlib import Path
from typing import Any

import grpc
from grpc import aio
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from evalsi.convert import (
    coerce_params,
    content_to_proto,
    from_struct,
    isolation_to_proto,
    manifest_to_proto,
    record_from_proto,
    record_to_proto,
    result_to_proto,
    score_to_proto,
    undeclared_secret,
    usage_to_proto,
)
from evalsi.datasets import DatasetError, split_uri
from evalsi.evaluator import BoundEvaluator, EvalContext, EvaluatorConfigError, Scope
from evalsi.judges import JudgeClient, JudgeConfig, JudgeFatalError, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2 as pb
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2_grpc as pb_grpc
from evalsi.registry import Registry, default_registry
from evalsi.run import load_dataset, target_config
from evalsi.runner import run_evaluators
from evalsi.targets import Target, TargetConfig, create_target, generate_all

logger = logging.getLogger(__name__)

Context = aio.ServicerContext[Any, Any]

SERVICE_NAME = "evalsi.plugin.v1alpha1.EvaluatorPluginService"


class EvaluatorPlugin(pb_grpc.EvaluatorPluginServiceServicer):
    def __init__(
        self,
        registry: Registry,
        judges: Mapping[str, JudgeConfig] | None = None,
        *,
        cache: JudgeCache | None = None,
        concurrency: int = 16,
    ) -> None:
        self.registry = registry
        self.judge_configs = dict(judges or {})
        self.cache = cache
        self.concurrency = concurrency
        self._judges: dict[str, JudgeClient] = {}
        self._targets: dict[TargetConfig, Target] = {}
        # Agent-run harnesses by run, so a run's tasks share environment setups.
        self._harnesses: dict[str, Any] = {}
        self._sandboxes: Any = None
        self._sandbox_lock = asyncio.Lock()
        self._closing: set[asyncio.Future[None]] = set()

    async def Describe(self, request: pb.DescribeRequest, context: Context) -> pb.DescribeResponse:
        return pb.DescribeResponse(
            evaluators=[manifest_to_proto(d.spec) for d in self.registry.evaluators()]
        )

    async def Evaluate(
        self,
        request_iterator: AsyncIterator[pb.EvaluateRequest],
        context: Context,
    ) -> AsyncIterator[pb.EvaluateResponse]:
        async for request in request_iterator:
            instance = await self._bind(request.evaluator, request.params, context)
            if instance.spec.scope is not Scope.RECORD:
                await context.abort(
                    grpc.StatusCode.INVALID_ARGUMENT,
                    f"{instance.spec.name} is dataset-scope; call Reduce instead",
                )
            ctx = await self._context(instance, request.judge, context)
            records = [record_from_proto(r) for r in request.records]
            try:
                results = await run_evaluators(
                    records, [instance], ctx, concurrency=self.concurrency
                )
            except JudgeFatalError as exc:
                await context.abort(grpc.StatusCode.FAILED_PRECONDITION, str(exc))
            yield pb.EvaluateResponse(
                batch_id=request.batch_id, results=[result_to_proto(r) for r in results]
            )

    async def Reduce(self, request: pb.ReduceRequest, context: Context) -> pb.ReduceResponse:
        instance = await self._bind(request.evaluator, request.params, context)
        if instance.spec.scope is not Scope.DATASET:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"{instance.spec.name} is record-scope; call Evaluate instead",
            )
        ctx = await self._context(instance, request.judge, context)
        records = [record_from_proto(r) for r in request.records]
        try:
            scores = await instance.run(records, ctx)
        except JudgeFatalError as exc:
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, str(exc))
        return pb.ReduceResponse(scores=[score_to_proto(s) for s in scores])

    async def Generate(self, request: pb.GenerateRequest, context: Context) -> pb.GenerateResponse:
        try:
            config = target_config(request.target)
        except ValueError as exc:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
        target = self._targets.get(config)
        if target is None:
            target = self._targets[config] = create_target(config)
        records = [record_from_proto(r) for r in request.records]
        generations = await generate_all(target, records, concurrency=self.concurrency)
        return pb.GenerateResponse(
            results=[
                pb.GenerateResult(
                    record_id=record.id,
                    output=content_to_proto(gen.output) if gen.output is not None else None,
                    usage=usage_to_proto(gen.usage),
                    error=gen.error,
                )
                for record, gen in zip(records, generations, strict=True)
            ]
        )

    async def LoadDataset(
        self, request: pb.LoadDatasetRequest, context: Context
    ) -> AsyncIterator[pb.LoadDatasetResponse]:
        source = request.source
        if source.WhichOneof("source") == "path" and not Path(source.path).is_absolute():
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "dataset path must be absolute")
        if source.WhichOneof("source") == "uri":
            # The server resolves importer paths inside its datasets_dir first;
            # anything else would let a spec read arbitrary files.
            scheme, path, _ = split_uri(source.uri)
            if "://" not in source.uri or (scheme != "hf" and not Path(path).is_absolute()):
                await context.abort(
                    grpc.StatusCode.INVALID_ARGUMENT,
                    "dataset uri must be hf:// or an importer uri with an absolute path",
                )
        try:
            records = await asyncio.to_thread(load_dataset, source, Path("/"))
        except (DatasetError, EvaluatorConfigError) as exc:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
        for start in range(0, len(records), 500):
            yield pb.LoadDatasetResponse(
                records=[record_to_proto(r) for r in records[start : start + 500]]
            )

    async def RunTask(
        self, request: pb.RunTaskRequest, context: Context
    ) -> AsyncIterator[pb.RunTaskResponse]:
        """One agent task through the spec's harness (evalsi-harness)."""
        try:
            from evalsi_harness import Task, TaskError, run_task
            from evalsi_harness.service import event_to_proto
        except ImportError:
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                "agent runs need the harness on the worker: pip install evalsi-harness",
            )
            raise  # unreachable: abort raises
        try:
            harness = self._harness(request.run_id, request.spec)
            task = Task.build(
                request.spec,
                record_from_proto(request.record),
                trial=request.trial,
                run_id=request.run_id,
            )
        except (TaskError, ValueError) as exc:
            yield pb.RunTaskResponse(result=pb.TaskResult(error=str(exc)))
            return
        queue: asyncio.Queue[Any] = asyncio.Queue()
        runner = asyncio.create_task(run_task(task, harness, on_event=queue.put_nowait))
        runner.add_done_callback(lambda _: queue.put_nowait(None))
        while (event := await queue.get()) is not None:
            yield pb.RunTaskResponse(trajectory=event_to_proto(event))
        outcome = await runner
        if outcome.record is None:
            yield pb.RunTaskResponse(result=pb.TaskResult(error=outcome.error))
            return
        record = record_to_proto(outcome.record)
        isolation = outcome.record.metadata.get("isolation")
        if isinstance(isolation, dict):
            record.provenance.isolation.CopyFrom(isolation_to_proto(isolation))
        yield pb.RunTaskResponse(result=pb.TaskResult(record=record))

    def _harness(self, run_id: str, spec: Any) -> Any:
        from evalsi_harness import HarnessContext, load_harness
        from evalsi_harness.trust import Trust

        harness = self._harnesses.get(run_id)
        if harness is not None:
            return harness
        while len(self._harnesses) >= 8:
            # Oldest first; its snapshots go with it.
            oldest = next(iter(self._harnesses))
            closing = asyncio.ensure_future(self._harnesses.pop(oldest).aclose())
            self._closing.add(closing)
            closing.add_done_callback(self._closing.discard)

        def judge(name: str) -> JudgeClient:
            name = name or spec.judge
            if name not in self.judge_configs:
                known = ", ".join(sorted(self.judge_configs)) or "none"
                raise ValueError(
                    f"unknown judge {name!r} for the user simulator; configured: {known}"
                )
            if name not in self._judges:
                self._judges[name] = create_judge(self.judge_configs[name], cache=self.cache)
            return self._judges[name]

        ctx = HarnessContext(
            sandboxes=self._sandbox_client, judge=judge, base_dir=Path("/"), trust=Trust.from_env()
        )
        harness = self._harnesses[run_id or "_"] = load_harness(spec, ctx)
        return harness

    async def _sandbox_client(self) -> Any:
        async with self._sandbox_lock:
            if self._sandboxes is None:
                from evalsi.sandbox.client import connect

                self._sandboxes = await connect()
            return self._sandboxes

    async def _bind(self, name: str, params: Any, context: Context) -> BoundEvaluator:
        try:
            definition = self.registry.resolve(name)
            values = coerce_params(definition.spec, from_struct(params))
            if problem := undeclared_secret(definition.spec, values):
                raise EvaluatorConfigError(problem)
            return definition.bind(values)
        except EvaluatorConfigError as exc:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
            raise  # unreachable: abort raises

    async def _context(self, instance: BoundEvaluator, judge: str, context: Context) -> EvalContext:
        if not instance.spec.requires.judge:
            return EvalContext()
        if not judge:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT, f"{instance.spec.name} needs a judge"
            )
        if judge not in self.judge_configs:
            known = ", ".join(sorted(self.judge_configs)) or "none"
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"unknown judge {judge!r}; configured: {known}",
            )
        if judge not in self._judges:
            self._judges[judge] = create_judge(self.judge_configs[judge], cache=self.cache)
        return EvalContext(judge=self._judges[judge])

    async def aclose(self) -> None:
        for harness in self._harnesses.values():
            await harness.aclose()
        if self._sandboxes is not None:
            await self._sandboxes.aclose()
        for target in self._targets.values():
            await target.aclose()
        for client in self._judges.values():
            await client.backend.aclose()
        if self.cache is not None:
            self.cache.close()


def load_judges(path: str | Path | None) -> dict[str, JudgeConfig]:
    if path is None:
        return {}
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path}: expected an object mapping judge names to configs")
    return {name: JudgeConfig(**fields) for name, fields in data.items()}


async def serve(
    listen: str,
    *,
    judges: Mapping[str, JudgeConfig] | None = None,
    registry: Registry | None = None,
    cache: bool = True,
    ready: asyncio.Event | None = None,
) -> None:
    """Serve until cancelled. ``listen`` is ``unix:///path`` or ``host:port``."""
    plugin = EvaluatorPlugin(
        registry or default_registry(), judges, cache=JudgeCache() if cache else None
    )
    server = aio.server()
    pb_grpc.add_EvaluatorPluginServiceServicer_to_server(plugin, server)
    health_servicer = health.aio.HealthServicer()  # type: ignore[attr-defined]  # missing from types-grpcio-health-checking
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    if server.add_insecure_port(listen) == 0 and not listen.startswith("unix:"):
        raise OSError(f"could not listen on {listen}")
    await server.start()
    for name in ("", SERVICE_NAME):
        await health_servicer.set(name, health_pb2.HealthCheckResponse.SERVING)
    logger.info("evalsi worker listening on %s", listen)
    # One line per model request would drown the worker's own log.
    logging.getLogger("httpx").setLevel(logging.WARNING)
    if ready is not None:
        ready.set()
    try:
        await server.wait_for_termination()
    finally:
        await server.stop(grace=2)
        await plugin.aclose()

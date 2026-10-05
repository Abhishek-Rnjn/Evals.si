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
    from_struct,
    manifest_to_proto,
    record_from_proto,
    result_to_proto,
    score_to_proto,
)
from evalsi.evaluator import BoundEvaluator, EvalContext, EvaluatorConfigError, Scope
from evalsi.judges import JudgeClient, JudgeConfig, JudgeFatalError, create_judge
from evalsi.judges.cache import JudgeCache
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2 as pb
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2_grpc as pb_grpc
from evalsi.registry import Registry, default_registry
from evalsi.runner import run_evaluators

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

    async def _bind(self, name: str, params: Any, context: Context) -> BoundEvaluator:
        try:
            definition = self.registry.resolve(name)
            return definition.bind(coerce_params(definition.spec, from_struct(params)))
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
    if ready is not None:
        ready.set()
    try:
        await server.wait_for_termination()
    finally:
        await server.stop(grace=2)
        await plugin.aclose()

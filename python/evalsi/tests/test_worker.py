from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Awaitable, Callable
from pathlib import Path
from typing import Any, cast

import grpc
import pytest
from google.protobuf import struct_pb2
from grpc_health.v1 import health_pb2, health_pb2_grpc
from test_judge import OPENAI, FakeBackend

from conftest import make_record
from evalsi import Content, Message, Record, Score, ToolCall, Usage, evaluator
from evalsi.convert import (
    coerce_params,
    manifest_to_proto,
    record_from_proto,
    record_to_proto,
    result_from_proto,
    result_to_proto,
    score_from_proto,
    score_to_proto,
)
from evalsi.judges import JudgeClient
from evalsi.packs import core
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2 as pb
from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2_grpc as pb_grpc
from evalsi.registry import default_registry
from evalsi.types import EvaluationResult, Outcome
from evalsi.v1alpha1 import evaluator_pb2
from evalsi.worker import EvaluatorPlugin, load_judges, serve

# --- conversions ---


def test_record_round_trip() -> None:
    record = Record(
        id="r1",
        input=Content(
            messages=[
                Message(role="user", content="hi"),
                Message(
                    role="assistant",
                    content="",
                    tool_calls=[ToolCall(name="f", arguments='{"x": 1', id="c1")],
                ),
            ]
        ),
        output=Content(text="out"),
        reference=Content(json=["a", "b"], is_json=True),
        context=[Content(text="doc")],
        usage=Usage(input_tokens=3, output_tokens=4, cost_usd=0.5, latency_ms=12.5),
        metadata={"topic": "t", "n": 2},
    )
    back = record_from_proto(record_to_proto(record))
    assert back.input == record.input
    assert back.output == record.output
    assert back.reference == record.reference
    assert back.context == record.context
    assert back.usage == record.usage
    assert back.metadata == {"topic": "t", "n": 2.0}  # protobuf Value widens ints


@pytest.mark.parametrize(
    "score",
    [
        Score(number=0.5, name="m", explanation="e", confidence=0.9),
        Score(passed=False, name="p", cost=Usage(input_tokens=1)),
        Score(label="cat", name="l", metadata={"k": "v"}),
        Score(structured={"a": [1.0, 2.0]}, name="s"),
    ],
)
def test_score_round_trip(score: Score) -> None:
    assert score_from_proto(score_to_proto(score)) == score


def test_result_round_trip() -> None:
    result = EvaluationResult(
        record_id="r",
        evaluator="em",
        evaluator_ref="builtin/exact-match@1.0.0",
        outcome=Outcome.SKIPPED,
        reason="why",
        duration_ms=1.25,
    )
    assert result_from_proto(result_to_proto(result)) == result


def test_manifest_and_param_coercion() -> None:
    manifest = manifest_to_proto(core.regex_match.spec)
    assert manifest.name == "builtin/regex-match"
    assert manifest.scope == evaluator_pb2.SCOPE_RECORD
    assert manifest.outputs[0].type == evaluator_pb2.SCORE_TYPE_PASSED
    assert manifest.params_schema["required"] == ["pattern"]
    spec = core.numeric_match.spec
    assert coerce_params(spec, {"rel_tol": 1.0}) == {"rel_tol": 1.0}

    @evaluator(name="test/ints", version="1.0.0")
    def ints(record: Record, *, n: int = 3, flag: bool = False) -> Score:
        return Score(number=n)

    assert coerce_params(ints.spec, {"n": 4.0, "flag": 1.0}) == {"n": 4, "flag": 1.0}


# --- the gRPC worker ---

# AsyncStub exists only in the .pyi stubs, hence the string.
Call = Callable[["pb_grpc.EvaluatorPluginServiceAsyncStub"], Awaitable[Any]]


def with_worker(plugin: EvaluatorPlugin, call: Call, tmp_path: Path) -> Any:
    async def main() -> Any:
        listen = f"unix://{tmp_path}/w.sock"
        server = grpc.aio.server()
        pb_grpc.add_EvaluatorPluginServiceServicer_to_server(plugin, server)
        server.add_insecure_port(listen)
        await server.start()
        try:
            async with grpc.aio.insecure_channel(listen) as channel:
                return await call(pb_grpc.EvaluatorPluginServiceStub(channel))
        finally:
            await server.stop(None)

    return asyncio.run(main())


def plugin_with_fake_judge() -> EvaluatorPlugin:
    plugin = EvaluatorPlugin(default_registry(), {"fake": OPENAI})
    plugin._judges["fake"] = JudgeClient(OPENAI, FakeBackend({"reasoning": "ok", "score": 5}))
    return plugin


def params(**values: Any) -> struct_pb2.Struct:
    s = struct_pb2.Struct()
    s.update(values)
    return s


async def stream(*requests: pb.EvaluateRequest) -> AsyncIterator[pb.EvaluateRequest]:
    for request in requests:
        yield request


def test_describe_lists_installed_evaluators(tmp_path: Path) -> None:
    async def call(stub: pb_grpc.EvaluatorPluginServiceAsyncStub) -> pb.DescribeResponse:
        return await stub.Describe(pb.DescribeRequest())

    names = {m.name for m in with_worker(plugin_with_fake_judge(), call, tmp_path).evaluators}
    assert {"builtin/exact-match", "builtin/llm-judge"} <= names


def test_evaluate_streams_one_response_per_batch(tmp_path: Path) -> None:
    records = [
        record_to_proto(make_record("Paris", "Paris", id="a")),
        record_to_proto(make_record("Lyon", "Paris", id="b")),
        record_to_proto(make_record("x", None, id="c")),
    ]

    async def call(stub: pb_grpc.EvaluatorPluginServiceAsyncStub) -> list[pb.EvaluateResponse]:
        requests = stream(
            pb.EvaluateRequest(batch_id="1", evaluator="exact-match", records=records[:2]),
            pb.EvaluateRequest(
                batch_id="2",
                evaluator="builtin/length",
                params=params(unit="chars"),
                records=records[2:],
            ),
            pb.EvaluateRequest(batch_id="3", evaluator="llm-judge", judge="fake", records=records),
        )
        return [r async for r in stub.Evaluate(requests)]

    first, second, third = with_worker(plugin_with_fake_judge(), call, tmp_path)
    assert first.batch_id == "1"
    assert [result_from_proto(r).scores[0].passed for r in first.results] == [True, False]
    assert result_from_proto(second.results[0]).scores[0].number == 1.0
    assert [result_from_proto(r).scores[0].number for r in third.results] == [1.0, 1.0, 1.0]


@pytest.mark.parametrize(
    ("request_", "code", "message"),
    [
        (pb.EvaluateRequest(evaluator="nope"), grpc.StatusCode.INVALID_ARGUMENT, "unknown"),
        (
            pb.EvaluateRequest(evaluator="llm-judge"),
            grpc.StatusCode.INVALID_ARGUMENT,
            "needs a judge",
        ),
        (
            pb.EvaluateRequest(evaluator="llm-judge", judge="other"),
            grpc.StatusCode.INVALID_ARGUMENT,
            "unknown judge",
        ),
        (
            pb.EvaluateRequest(evaluator="regex-match"),
            grpc.StatusCode.INVALID_ARGUMENT,
            "requires params",
        ),
    ],
)
def test_evaluate_rejects_bad_requests(
    request_: pb.EvaluateRequest, code: grpc.StatusCode, message: str, tmp_path: Path
) -> None:
    async def call(stub: pb_grpc.EvaluatorPluginServiceAsyncStub) -> None:
        async for _ in stub.Evaluate(stream(request_)):
            pass

    with pytest.raises(grpc.aio.AioRpcError) as info:
        with_worker(plugin_with_fake_judge(), call, tmp_path)
    assert info.value.code() == code
    assert message in (info.value.details() or "")


def test_reduce_runs_dataset_scope_evaluators(tmp_path: Path) -> None:
    @evaluator(name="test/count", version="1.0.0", scope="dataset")
    def count(records: list[Record]) -> Score:
        return Score(number=len(records))

    registry = default_registry().__class__([*default_registry().packs.values()])
    registry.add(count)
    plugin = EvaluatorPlugin(registry)

    async def call(stub: pb_grpc.EvaluatorPluginServiceAsyncStub) -> pb.ReduceResponse:
        recs = [record_to_proto(make_record(id=str(i))) for i in range(4)]
        return await stub.Reduce(pb.ReduceRequest(evaluator="test/count", records=recs))

    response = with_worker(plugin, call, tmp_path)
    assert score_from_proto(response.scores[0]).number == 4


def test_serve_reports_healthy_and_loads_judges(tmp_path: Path) -> None:
    judges_file = tmp_path / "judges.json"
    judges_file.write_text(
        '{"j": {"provider": "openai-compatible", "model": "m", "base_url": "http://x/v1"}}'
    )
    assert load_judges(judges_file)["j"].model == "m"

    async def main() -> int:
        listen = f"unix://{tmp_path}/s.sock"
        ready = asyncio.Event()
        task = asyncio.create_task(serve(listen, judges=load_judges(judges_file), ready=ready))
        await asyncio.wait_for(ready.wait(), 10)
        try:
            async with grpc.aio.insecure_channel(listen) as channel:
                check = cast(Any, health_pb2_grpc.HealthStub(channel)).Check
                response = await check(health_pb2.HealthCheckRequest(service=""))
                return int(response.status)
        finally:
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task

    assert asyncio.run(main()) == health_pb2.HealthCheckResponse.SERVING

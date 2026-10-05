"""The harness protocol over gRPC (``evalsi.harness.v1alpha1.HarnessService``).

``serve`` exposes any Python harness as the service, which is how a harness
ships as a container image. ``GrpcHarness`` is the client side: it lets the
worker drive a harness at an address, or one it starts as a command (the
command serves on the Unix socket named by ``EVALSI_HARNESS_LISTEN``).
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import shutil
import tempfile
from collections.abc import AsyncIterator, Sequence
from pathlib import Path
from typing import Any

import grpc
from grpc import aio

from evalsi.convert import (
    check_from_proto,
    check_to_proto,
    content_from_proto,
    content_to_proto,
    record_from_proto,
    record_to_proto,
    step_from_proto,
    step_to_proto,
    to_struct,
    usage_from_proto,
    usage_to_proto,
)
from evalsi.harness.v1alpha1 import harness_pb2 as pb
from evalsi.harness.v1alpha1 import harness_pb2_grpc as pb_grpc
from evalsi.types import TaskCheck, Usage
from evalsi_harness.events import Event, FinalEvent, PolicyEvent, StepEvent
from evalsi_harness.harness import Harness, HarnessManifest
from evalsi_harness.task import Task, TaskError

logger = logging.getLogger(__name__)
Context = aio.ServicerContext[Any, Any]


def event_to_proto(event: Event) -> pb.TrajectoryEvent:
    if isinstance(event, StepEvent):
        return pb.TrajectoryEvent(step=step_to_proto(event.step))
    if isinstance(event, PolicyEvent):
        return pb.TrajectoryEvent(
            policy=pb.PolicyEvent(
                kind=event.kind, detail=event.detail, granted=event.granted, span_id=event.span_id
            )
        )
    final = pb.Final(
        stop_reason=event.stop_reason, error=event.error, usage=usage_to_proto(event.usage)
    )
    if event.output is not None:
        final.output.CopyFrom(content_to_proto(event.output))
    return pb.TrajectoryEvent(final=final)


def event_from_proto(msg: pb.TrajectoryEvent) -> Event:
    kind = msg.WhichOneof("event")
    if kind == "step":
        return StepEvent(step_from_proto(msg.step))
    if kind == "policy":
        p = msg.policy
        return PolicyEvent(kind=p.kind, detail=p.detail, granted=p.granted, span_id=p.span_id)
    f = msg.final
    return FinalEvent(
        output=content_from_proto(f.output) if f.HasField("output") else None,
        usage=usage_from_proto(f.usage) if f.HasField("usage") else Usage(),
        stop_reason=f.stop_reason or "completed",
        error=f.error,
    )


def task_to_proto(task: Task) -> pb.Task:
    msg = pb.Task(
        id=task.id, record=record_to_proto(task.record), run_id=task.run_id, trial=task.trial
    )
    msg.spec.CopyFrom(task.spec)
    if task.config:
        msg.config.CopyFrom(to_struct(task.config))
    return msg


def task_from_proto(msg: pb.Task) -> Task:
    from google.protobuf import json_format

    task = Task.build(msg.spec, record_from_proto(msg.record), trial=msg.trial, run_id=msg.run_id)
    if msg.HasField("config"):
        task.config = json_format.MessageToDict(msg.config)
    return task


class HarnessServicer(pb_grpc.HarnessServiceServicer):
    def __init__(self, harness: Harness) -> None:
        self.harness = harness

    async def Describe(self, request: pb.DescribeRequest, context: Context) -> pb.DescribeResponse:
        m = self.harness.describe()
        return pb.DescribeResponse(
            manifest=pb.HarnessManifest(
                name=m.name,
                version=m.version,
                description=m.description,
                task_formats=m.task_formats,
                uses_sandbox=m.uses_sandbox,
                capabilities=m.capabilities,
            )
        )

    async def Setup(self, request: pb.SetupRequest, context: Context) -> pb.SetupResponse:
        try:
            handle = await self.harness.setup(task_from_proto(request.task))
        except TaskError as exc:
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, str(exc))
        return pb.SetupResponse(handle_id=handle)

    async def Run(self, request: pb.RunRequest, context: Context) -> AsyncIterator[pb.RunResponse]:
        async for event in self.harness.run(request.handle_id):
            yield pb.RunResponse(event=event_to_proto(event))

    async def Check(self, request: pb.CheckRequest, context: Context) -> pb.CheckResponse:
        check = await self.harness.check(request.handle_id)
        response = pb.CheckResponse()
        if check is not None:
            response.check.CopyFrom(check_to_proto(check))
        return response

    async def Teardown(self, request: pb.TeardownRequest, context: Context) -> pb.TeardownResponse:
        await self.harness.teardown(request.handle_id)
        return pb.TeardownResponse()


async def serve(harness: Harness, listen: str | None = None) -> None:
    """Serve ``harness`` until cancelled, on ``listen`` or ``EVALSI_HARNESS_LISTEN``."""
    address = listen or os.environ.get("EVALSI_HARNESS_LISTEN") or "127.0.0.1:50051"
    server = aio.server()
    pb_grpc.add_HarnessServiceServicer_to_server(HarnessServicer(harness), server)
    if server.add_insecure_port(address) == 0 and not address.startswith("unix:"):
        raise OSError(f"could not listen on {address}")
    await server.start()
    logger.info("harness listening on %s", address)
    try:
        await server.wait_for_termination()
    finally:
        await server.stop(grace=2)
        await harness.aclose()


class GrpcHarness:
    """A harness reached over HarnessService."""

    def __init__(
        self,
        *,
        address: str = "",
        command: Sequence[str] = (),
        config: dict[str, Any] | None = None,
    ) -> None:
        if bool(address) == bool(command):
            raise TaskError("an external harness needs exactly one of address or command")
        self.address = address
        self.command = list(command)
        self.config = config or {}
        self._process: asyncio.subprocess.Process | None = None
        self._dir: Path | None = None
        self._channel: aio.Channel | None = None
        self._stub: pb_grpc.HarnessServiceAsyncStub | None = None
        self._lock = asyncio.Lock()
        self._manifest = HarnessManifest(name="external", version="")

    async def _connect(self) -> pb_grpc.HarnessServiceAsyncStub:
        async with self._lock:
            if self._stub is not None:
                return self._stub
            address = self.address
            if self.command:
                self._dir = Path(tempfile.mkdtemp(prefix="evalsi-harness-"))
                address = f"unix://{self._dir / 'harness.sock'}"
                self._process = await asyncio.create_subprocess_exec(
                    *self.command, env={**os.environ, "EVALSI_HARNESS_LISTEN": address}
                )
            self._channel = aio.insecure_channel(address)
            stub = pb_grpc.HarnessServiceStub(self._channel)
            for _ in range(100):
                try:
                    resp = await stub.Describe(pb.DescribeRequest(), timeout=5)
                    break
                except grpc.aio.AioRpcError as exc:
                    if self._process is not None and self._process.returncode is not None:
                        raise TaskError(
                            f"harness command exited with {self._process.returncode}"
                        ) from exc
                    if not self.command:
                        raise TaskError(f"harness at {address}: {exc.details()}") from exc
                    await asyncio.sleep(0.1)
            else:
                raise TaskError(f"harness command did not start serving on {address}")
            m = resp.manifest
            self._manifest = HarnessManifest(
                name=m.name,
                version=m.version,
                description=m.description,
                task_formats=list(m.task_formats),
                uses_sandbox=m.uses_sandbox,
                capabilities=list(m.capabilities),
            )
            self._stub = stub
            return stub

    def describe(self) -> HarnessManifest:
        return self._manifest

    async def setup(self, task: Task) -> str:
        stub = await self._connect()
        task.config = {**self.config, **task.config}
        try:
            resp = await stub.Setup(pb.SetupRequest(task=task_to_proto(task)))
        except grpc.aio.AioRpcError as exc:
            raise TaskError(f"harness setup: {exc.details()}") from exc
        return resp.handle_id

    async def run(self, handle: str) -> AsyncIterator[Event]:
        stub = await self._connect()
        try:
            async for resp in stub.Run(pb.RunRequest(handle_id=handle)):
                yield event_from_proto(resp.event)
        except grpc.aio.AioRpcError as exc:
            yield FinalEvent(
                output=None, stop_reason="error", error=f"harness run: {exc.details()}"
            )

    async def check(self, handle: str) -> TaskCheck | None:
        stub = await self._connect()
        try:
            resp = await stub.Check(pb.CheckRequest(handle_id=handle))
        except grpc.aio.AioRpcError as exc:
            raise TaskError(f"harness check: {exc.details()}") from exc
        return check_from_proto(resp.check) if resp.HasField("check") else None

    async def teardown(self, handle: str) -> None:
        if self._stub is None:
            return
        with contextlib.suppress(grpc.aio.AioRpcError):
            await self._stub.Teardown(pb.TeardownRequest(handle_id=handle))

    async def aclose(self) -> None:
        if self._channel is not None:
            await self._channel.close()
        if self._process is not None and self._process.returncode is None:
            self._process.terminate()
            try:
                await asyncio.wait_for(self._process.wait(), 10)
            except TimeoutError:
                self._process.kill()
                await self._process.wait()
        if self._dir is not None:
            shutil.rmtree(self._dir, ignore_errors=True)

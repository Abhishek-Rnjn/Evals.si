"""Persistent sandboxes through ``evalsi.sandbox.v1alpha1.SandboxService``.

Agent harnesses keep one sandbox per task: the agent's commands run in it one
after another, its files persist, and it can be snapshotted after setup and
restored for every trial. Workers started by ``evalsid serve`` find the
service at ``EVALSI_SANDBOX_ADDR``; elsewhere :func:`connect` starts
``evalsid sandbox serve`` for the lifetime of the client.

Needs the ``server`` extra (grpcio).
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import shutil
import tempfile
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import grpc
from google.protobuf import duration_pb2
from grpc import aio

from evalsi.sandbox import SandboxError, sandbox_binary
from evalsi.sandbox.v1alpha1 import sandbox_pb2 as pb
from evalsi.sandbox.v1alpha1 import sandbox_pb2_grpc as pb_grpc
from evalsi.v1alpha1 import record_pb2

_NETWORK = {
    "deny": pb.NETWORK_MODE_DENY,
    "allowlist": pb.NETWORK_MODE_ALLOWLIST,
    "allow": pb.NETWORK_MODE_ALLOW,
}
_OUTCOME = {
    pb.OUTCOME_EXIT: "exit",
    pb.OUTCOME_DENIED: "denied",
    pb.OUTCOME_TIMEOUT: "timeout",
    pb.OUTCOME_RUNNER_FAILURE: "runner_failure",
}
_LEVEL = {
    record_pb2.ISOLATION_LEVEL_NONE: "none",
    record_pb2.ISOLATION_LEVEL_CONFINED: "confined",
    record_pb2.ISOLATION_LEVEL_NAMESPACED: "namespaced",
    record_pb2.ISOLATION_LEVEL_KERNEL: "kernel",
    record_pb2.ISOLATION_LEVEL_VM: "vm",
}


class SandboxUnavailable(SandboxError):
    """No rung on this host meets the requested isolation (fail closed)."""


@dataclass
class SandboxSpec:
    """What a sandbox looks like; mirrors ``SandboxSpec`` in the proto."""

    image: str = ""
    read_only_root: bool = False
    min_isolation: str = "confined"
    mode: str = "workspace-write"
    network: str = "deny"
    allow_hosts: list[str] = field(default_factory=list)
    memory_mb: int = 0
    max_procs: int = 0
    cpu_seconds: int = 0
    env: dict[str, str] = field(default_factory=dict)
    files: dict[str, bytes | str] = field(default_factory=dict)
    workdir: str = ""
    idle_timeout_s: float = 0.0

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> SandboxSpec:
        known = set(cls.__dataclass_fields__)
        unknown = set(data) - known
        if unknown:
            raise ValueError(f"unknown sandbox field(s): {', '.join(sorted(unknown))}")
        return cls(**dict(data))

    def to_proto(self) -> pb.SandboxSpec:
        if self.network not in _NETWORK:
            raise ValueError(f"network must be one of {sorted(_NETWORK)}, not {self.network!r}")
        spec = pb.SandboxSpec(
            image=self.image,
            read_only_root=self.read_only_root,
            min_isolation=self.min_isolation,
            mode=self.mode,
            network=pb.NetworkPolicy(mode=_NETWORK[self.network], allow=self.allow_hosts),
            resources=pb.Resources(
                memory_mb=self.memory_mb, max_procs=self.max_procs, cpu_seconds=self.cpu_seconds
            ),
            env=self.env,
            files={k: v.encode() if isinstance(v, str) else v for k, v in self.files.items()},
            workdir=self.workdir,
        )
        if self.idle_timeout_s:
            spec.idle_timeout.CopyFrom(_duration(self.idle_timeout_s))
        return spec


@dataclass
class Isolation:
    driver: str
    level: str
    enforcement: str
    notes: list[str] = field(default_factory=list)

    @classmethod
    def from_proto(cls, report: record_pb2.IsolationReport) -> Isolation:
        return cls(
            driver=report.driver,
            level=_LEVEL.get(report.level, "none"),
            enforcement="partial"
            if report.enforcement == record_pb2.ENFORCEMENT_PARTIAL
            else "full",
            notes=list(report.notes),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "driver": self.driver,
            "level": self.level,
            "enforcement": self.enforcement,
            "notes": self.notes,
        }


@dataclass
class ExecResult:
    # exit, denied or timeout; a runner failure raises SandboxError.
    outcome: str
    exit_code: int
    stdout: str = ""
    stderr: str = ""
    duration_ms: float = 0.0
    truncated: bool = False
    denials: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.outcome == "exit" and self.exit_code == 0


@dataclass
class EgressEvent:
    host: str
    port: int
    allowed: bool
    bytes_sent: int = 0
    bytes_received: int = 0


def _duration(seconds: float) -> duration_pb2.Duration:
    d = duration_pb2.Duration()
    d.FromNanoseconds(int(seconds * 1e9))
    return d


def _raise(exc: grpc.aio.AioRpcError) -> None:
    detail = exc.details() or ""
    if detail.startswith("SANDBOX_UNAVAILABLE"):
        raise SandboxUnavailable(detail) from exc
    raise SandboxError(f"sandbox service: {exc.code().name}: {detail}") from exc


class Sandbox:
    """One live sandbox. Use as an async context manager to destroy it."""

    def __init__(self, client: SandboxClient, sandbox_id: str, isolation: Isolation) -> None:
        self.client = client
        self.id = sandbox_id
        self.isolation = isolation
        self.image_digest = ""

    async def exec(
        self,
        command: Sequence[str],
        *,
        stdin: bytes | str = b"",
        env: Mapping[str, str] | None = None,
        timeout_s: float = 60.0,
        cwd: str = "",
        output_limit: int = 0,
        on_output: Callable[[str, str], None] | None = None,
    ) -> ExecResult:
        """Run a command; ``on_output(stream, text)`` sees output as it arrives."""
        request = pb.ExecRequest(
            sandbox_id=self.id,
            command=list(command),
            stdin=stdin.encode() if isinstance(stdin, str) else stdin,
            env=dict(env or {}),
            timeout=_duration(timeout_s),
            output_limit_bytes=output_limit,
            cwd=cwd,
        )
        out, err = bytearray(), bytearray()
        result: pb.ExecResult | None = None
        try:
            async for event in self.client.stub.Exec(request):
                kind = event.WhichOneof("event")
                if kind == "stdout":
                    out += event.stdout
                    if on_output:
                        on_output("stdout", event.stdout.decode(errors="replace"))
                elif kind == "stderr":
                    err += event.stderr
                    if on_output:
                        on_output("stderr", event.stderr.decode(errors="replace"))
                else:
                    result = event.result
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        if result is None:
            raise SandboxError("sandbox service ended the exec without a result")
        outcome = _OUTCOME.get(result.outcome, "runner_failure")
        if outcome == "runner_failure":
            raise SandboxError(f"sandbox runner failure: {result.error}")
        return ExecResult(
            outcome=outcome,
            exit_code=result.exit_code,
            stdout=out.decode(errors="replace"),
            stderr=err.decode(errors="replace"),
            duration_ms=result.duration.ToNanoseconds() / 1e6,
            truncated=result.truncated,
            denials=list(result.denials),
        )

    async def write_files(self, files: Mapping[str, bytes | str], mode: int = 0) -> None:
        request = pb.WriteFilesRequest(
            sandbox_id=self.id,
            files=[
                pb.File(path=p, content=c.encode() if isinstance(c, str) else c, mode=mode)
                for p, c in files.items()
            ],
        )
        try:
            await self.client.stub.WriteFiles(request)
        except grpc.aio.AioRpcError as exc:
            _raise(exc)

    async def read_files(self, paths: Sequence[str], max_bytes: int = 0) -> dict[str, bytes]:
        """Files by path (directories are read recursively); missing paths are absent."""
        try:
            resp = await self.client.stub.ReadFiles(
                pb.ReadFilesRequest(sandbox_id=self.id, paths=list(paths), max_bytes=max_bytes)
            )
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return {f.path: f.content for f in resp.files}

    async def snapshot(self) -> str:
        try:
            resp = await self.client.stub.Snapshot(pb.SnapshotRequest(sandbox_id=self.id))
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return resp.snapshot_id

    async def egress(self) -> list[EgressEvent]:
        try:
            resp = await self.client.stub.Stats(pb.StatsRequest(sandbox_id=self.id))
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return [
            EgressEvent(e.host, e.port, e.allowed, e.bytes_sent, e.bytes_received)
            for e in resp.egress
        ]

    async def destroy(self) -> None:
        with contextlib.suppress(grpc.aio.AioRpcError):
            await self.client.stub.Destroy(pb.DestroyRequest(sandbox_id=self.id))

    async def __aenter__(self) -> Sandbox:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.destroy()


class SandboxClient:
    def __init__(self, address: str, process: asyncio.subprocess.Process | None = None) -> None:
        self.address = address
        self._process = process
        self._cleanup: Path | None = None
        self._channel = aio.insecure_channel(address)
        self.stub = pb_grpc.SandboxServiceStub(self._channel)

    async def create(self, spec: SandboxSpec | None = None) -> Sandbox:
        try:
            resp = await self.stub.Create(pb.CreateRequest(spec=(spec or SandboxSpec()).to_proto()))
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        sandbox = Sandbox(self, resp.sandbox_id, Isolation.from_proto(resp.isolation))
        sandbox.image_digest = resp.image_digest
        return sandbox

    async def restore(
        self, snapshot_id: str, *, network: str = "", allow_hosts: Sequence[str] = ()
    ) -> Sandbox:
        """A new sandbox from a snapshot; ``network`` replaces the snapshot's policy."""
        request = pb.RestoreRequest(snapshot_id=snapshot_id)
        if network:
            if network not in _NETWORK:
                raise ValueError(f"network must be one of {sorted(_NETWORK)}, not {network!r}")
            request.network.CopyFrom(
                pb.NetworkPolicy(mode=_NETWORK[network], allow=list(allow_hosts))
            )
        try:
            resp = await self.stub.Restore(request)
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return Sandbox(self, resp.sandbox_id, Isolation.from_proto(resp.isolation))

    async def delete_snapshot(self, snapshot_id: str) -> None:
        with contextlib.suppress(grpc.aio.AioRpcError):
            await self.stub.DeleteSnapshot(pb.DeleteSnapshotRequest(snapshot_id=snapshot_id))

    async def probe(self) -> list[dict[str, Any]]:
        resp = await self.stub.Probe(pb.ProbeRequest())
        return [
            {
                "driver": r.driver,
                "level": _LEVEL.get(r.level, "none"),
                "available": r.available,
                "reason": r.reason,
            }
            for r in resp.rungs
        ]

    async def aclose(self) -> None:
        await self._channel.close()
        if self._process is not None and self._process.returncode is None:
            if self._process.stdin is not None:
                self._process.stdin.close()
            try:
                await asyncio.wait_for(self._process.wait(), 10)
            except TimeoutError:
                self._process.kill()
                await self._process.wait()
        if self._cleanup is not None:
            shutil.rmtree(self._cleanup, ignore_errors=True)

    async def __aenter__(self) -> SandboxClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()


async def connect(address: str | None = None) -> SandboxClient:
    """A client for ``address``, ``EVALSI_SANDBOX_ADDR``, or a private ``evalsid sandbox serve``."""
    address = address or os.environ.get("EVALSI_SANDBOX_ADDR")
    if address:
        return SandboxClient(address)
    socket_dir = Path(tempfile.mkdtemp(prefix="evalsi-sandbox-"))
    socket = socket_dir / "sandbox.sock"
    args = [sandbox_binary(), "sandbox", "serve", "--listen", f"unix://{socket}"]
    args.append("--until-stdin-eof")
    if config := os.environ.get("EVALSI_SANDBOX_CONFIG"):
        args += ["--config", config]
    log = socket_dir / "serve.log"
    with log.open("wb") as err:
        process = await asyncio.create_subprocess_exec(
            *args, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=err
        )
    assert process.stdout is not None
    try:
        line = await asyncio.wait_for(process.stdout.readline(), 30)
    except TimeoutError:
        line = b""
    if not line.startswith(b"listening on"):
        process.kill()
        await process.wait()
        detail = log.read_text(errors="replace").strip()
        shutil.rmtree(socket_dir, ignore_errors=True)
        raise SandboxError(f"could not start `evalsid sandbox serve`: {detail}")
    client = SandboxClient(f"unix://{socket}", process)
    client._cleanup = socket_dir
    return client

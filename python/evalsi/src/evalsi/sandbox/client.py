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
from evalsi.v1alpha1 import agent_pb2, record_pb2

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


class SnapshotsUnsupported(SandboxError):
    """The sandbox's rung cannot snapshot (the pod rung)."""


@dataclass
class ServiceSpec:
    """Private per-trial pod sidecar. Limits and a TCP readiness port are required."""

    name: str
    image: str
    command: list[str]
    port: int
    memory_mb: int
    cpu_millis: int
    env: dict[str, str] = field(default_factory=dict)
    depends_on: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ServiceSpec:
        return cls(**dict(data))

    def to_proto(self) -> agent_pb2.EnvironmentService:
        return agent_pb2.EnvironmentService(
            name=self.name,
            image=self.image,
            command=self.command,
            port=self.port,
            memory_mb=self.memory_mb,
            cpu_millis=self.cpu_millis,
            env=self.env,
            depends_on=self.depends_on,
        )


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
    services: list[ServiceSpec] = field(default_factory=list)
    service_lifetime_s: float = 0.0

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> SandboxSpec:
        known = set(cls.__dataclass_fields__)
        unknown = set(data) - known
        if unknown:
            raise ValueError(f"unknown sandbox field(s): {', '.join(sorted(unknown))}")
        values = dict(data)
        values["services"] = [ServiceSpec.from_dict(s) for s in values.get("services", [])]
        return cls(**values)

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
            services=[s.to_proto() for s in self.services],
            service_lifetime=_duration(self.service_lifetime_s),
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
class ProcessInfo:
    """A background process in a sandbox (see :meth:`Sandbox.start_process`)."""

    id: str
    command: list[str]
    running: bool
    # Ended because it was killed (by ``kill_process`` or the sandbox's end).
    killed: bool = False
    # How it ended; ``None`` while it runs.
    exit_code: int | None = None
    outcome: str = ""
    stdout_bytes: int = 0
    stderr_bytes: int = 0

    @classmethod
    def from_proto(cls, info: pb.ProcessInfo) -> ProcessInfo:
        ended = info.HasField("result")
        return cls(
            id=info.process_id,
            command=list(info.command),
            running=info.running,
            killed=info.killed,
            exit_code=info.result.exit_code if ended else None,
            outcome=_OUTCOME.get(info.result.outcome, "runner_failure") if ended else "",
            stdout_bytes=info.stdout_bytes,
            stderr_bytes=info.stderr_bytes,
        )


@dataclass
class ProcessOutput:
    """New output of a background process since the offsets it was read with.
    Pass ``stdout_offset`` and ``stderr_offset`` to the next read. ``*_dropped``
    counts bytes the process's bounded buffer overwrote before they were read."""

    stdout: str
    stderr: str
    stdout_offset: int
    stderr_offset: int
    process: ProcessInfo
    stdout_dropped: int = 0
    stderr_dropped: int = 0
    # The bytes behind ``stdout``, for a reader that must not split a UTF-8
    # character across reads (``stdout`` decodes each read on its own).
    stdout_raw: bytes = b""
    stderr_raw: bytes = b""


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
    if exc.code() == grpc.StatusCode.UNIMPLEMENTED:
        raise SnapshotsUnsupported(f"sandbox service: {detail}") from exc
    raise SandboxError(f"sandbox service: {exc.code().name}: {detail}") from exc


class ReadFilesResult(dict[str, bytes]):
    """Files by path, as :meth:`Sandbox.read_files` returns them, plus what
    was not delivered: ``missing`` paths (absent, or a symlink or special
    file) and ``truncated`` when ``max_bytes`` ran out."""

    missing: list[str]
    truncated: bool


class Sandbox:
    """One live sandbox. Use as an async context manager to destroy it."""

    def __init__(self, client: SandboxClient, sandbox_id: str, isolation: Isolation) -> None:
        self.client = client
        self.id = sandbox_id
        self.isolation = isolation
        self.image_digest = ""

    async def services(self, *, include_logs: bool = False) -> list[dict[str, Any]]:
        """Return service readiness and restart counts (no environment values)."""
        try:
            response = await self.client.stub.Stats(
                pb.StatsRequest(sandbox_id=self.id, include_service_logs=include_logs)
            )
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return [
            {
                "name": s.name,
                "ready": s.ready,
                "restarts": s.restarts,
                "reason": s.reason,
                "logs": s.logs,
            }
            for s in response.services
        ]

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

    async def start_process(
        self,
        command: Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
        buffer_bytes: int = 0,
        stdin: bool = False,
    ) -> ProcessInfo:
        """Start a command that outlives this call, in the same sandbox, under the
        same confinement and limits as :meth:`exec`. Its output is kept in bounded
        buffers (``buffer_bytes`` per stream, default 1 MiB) for ``read_process``;
        ``stdin=True`` keeps its stdin open for ``write_process``. ``timeout_s`` is
        its wall-clock limit (default one hour). It dies with the sandbox."""
        request = pb.StartProcessRequest(
            sandbox_id=self.id,
            command=list(command),
            env=dict(env or {}),
            cwd=cwd,
            timeout=_duration(timeout_s),
            buffer_bytes=buffer_bytes,
            stdin=stdin,
        )
        try:
            resp = await self.client.stub.StartProcess(request)
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return ProcessInfo.from_proto(resp.process)

    async def list_processes(self) -> list[ProcessInfo]:
        try:
            resp = await self.client.stub.ListProcesses(pb.ListProcessesRequest(sandbox_id=self.id))
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return [ProcessInfo.from_proto(p) for p in resp.processes]

    async def read_process(
        self,
        process_id: str,
        *,
        stdout_offset: int = 0,
        stderr_offset: int = 0,
        max_bytes: int = 0,
        wait_s: float = 0.0,
    ) -> ProcessOutput:
        """Output from the given offsets. With ``wait_s`` (at most 30), blocks until
        there is more output or the process ends."""
        request = pb.ReadProcessRequest(
            sandbox_id=self.id,
            process_id=process_id,
            stdout_offset=stdout_offset,
            stderr_offset=stderr_offset,
            max_bytes=max_bytes,
            wait=_duration(wait_s),
        )
        try:
            resp = await self.client.stub.ReadProcess(request)
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return ProcessOutput(
            stdout=resp.stdout.decode(errors="replace"),
            stderr=resp.stderr.decode(errors="replace"),
            stdout_offset=resp.stdout_offset,
            stderr_offset=resp.stderr_offset,
            process=ProcessInfo.from_proto(resp.process),
            stdout_dropped=resp.stdout_dropped,
            stderr_dropped=resp.stderr_dropped,
            stdout_raw=bytes(resp.stdout),
            stderr_raw=bytes(resp.stderr),
        )

    async def write_process(
        self, process_id: str, data: bytes | str = b"", *, close: bool = False
    ) -> None:
        """Write to a process's stdin (it must have been started with ``stdin=True``);
        ``close=True`` then closes it, so the process reads EOF."""
        request = pb.WriteProcessRequest(
            sandbox_id=self.id,
            process_id=process_id,
            data=data.encode() if isinstance(data, str) else data,
            close=close,
        )
        try:
            await self.client.stub.WriteProcess(request)
        except grpc.aio.AioRpcError as exc:
            _raise(exc)

    async def kill_process(self, process_id: str) -> ProcessInfo:
        try:
            resp = await self.client.stub.KillProcess(
                pb.KillProcessRequest(sandbox_id=self.id, process_id=process_id)
            )
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        return ProcessInfo.from_proto(resp.process)

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

    async def read_files(self, paths: Sequence[str], max_bytes: int = 0) -> ReadFilesResult:
        """Files by path (directories are read recursively). Paths that were
        not read are listed in ``.missing``; ``.truncated`` is set when
        ``max_bytes`` cut the read short."""
        try:
            resp = await self.client.stub.ReadFiles(
                pb.ReadFilesRequest(sandbox_id=self.id, paths=list(paths), max_bytes=max_bytes)
            )
        except grpc.aio.AioRpcError as exc:
            _raise(exc)
        result = ReadFilesResult((f.path, f.content) for f in resp.files)
        result.missing = list(resp.missing)
        result.truncated = resp.truncated
        return result

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


def _channel(address: str) -> aio.Channel:
    """unix:///path (a local evalsid), or tls://host:port: sandboxd on another
    pod, with mutual TLS from EVALSI_SANDBOX_TLS_CA, _CERT and _KEY."""
    target = address.removeprefix("tls://")
    if target == address:
        return aio.insecure_channel(address)

    def read(var: str) -> bytes:
        path = os.environ.get(var)
        if not path:
            raise SandboxError(f"{address}: set {var} (mutual TLS to the sandbox service)")
        return Path(path).read_bytes()

    creds = grpc.ssl_channel_credentials(
        root_certificates=read("EVALSI_SANDBOX_TLS_CA"),
        private_key=read("EVALSI_SANDBOX_TLS_KEY"),
        certificate_chain=read("EVALSI_SANDBOX_TLS_CERT"),
    )
    options = []
    if name := os.environ.get("EVALSI_SANDBOX_TLS_SERVER_NAME"):
        options.append(("grpc.ssl_target_name_override", name))
    return aio.secure_channel(target, creds, options=options)


class SandboxClient:
    def __init__(self, address: str, process: asyncio.subprocess.Process | None = None) -> None:
        self.address = address
        self._process = process
        self._cleanup: Path | None = None
        self._channel = _channel(address)
        self.stub = pb_grpc.SandboxServiceStub(self._channel)

    async def create(self, spec: SandboxSpec | None = None) -> Sandbox:
        try:
            if spec and spec.services:
                capabilities = await self.stub.Probe(pb.ProbeRequest())
                if not capabilities.services_supported:
                    raise SandboxUnavailable("sandbox server does not support services; upgrade it")
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
                "warning": r.warning,
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

"""Task environments: sandboxes prepared once per environment and restored
for every trial.

Setup (installing dependencies, checking out a repository) runs once per
distinct environment, possibly with network access, and the result is
snapshotted. Each trial then starts from a restore of that snapshot under
the agent's own network policy, so trials are independent and identical, and
setup is never paid twice.
"""

from __future__ import annotations

import asyncio
import contextlib
import posixpath
import shlex
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import (
    EgressEvent,
    ExecResult,
    Isolation,
    ProcessInfo,
    ProcessOutput,
    SandboxSpec,
    SnapshotsUnsupported,
)
from evalsi_harness.events import PolicyEvent
from evalsi_harness.task import EnvironmentConfig, StageCopy, TaskError

# What a multi-stage build may carry out of its earlier stages, in all.
MAX_STAGE_BYTES = 256 << 20

# (path in the stage, content, executable) for each file a copy takes.
StageFiles = list[tuple[str, bytes, bool]]


class SandboxLike(Protocol):
    id: str
    isolation: Isolation

    async def exec(
        self,
        command: Sequence[str],
        *,
        stdin: bytes | str = ...,
        env: dict[str, str] | None = ...,
        timeout_s: float = ...,
        cwd: str = ...,
        output_limit: int = ...,
    ) -> ExecResult: ...

    async def write_files(self, files: dict[str, bytes | str], mode: int = ...) -> None: ...

    async def read_files(self, paths: Sequence[str], max_bytes: int = ...) -> dict[str, bytes]: ...

    async def services(self, *, include_logs: bool = ...) -> list[dict[str, Any]]: ...

    async def start_process(
        self,
        command: Sequence[str],
        *,
        env: Mapping[str, str] | None = ...,
        cwd: str = ...,
        timeout_s: float = ...,
        buffer_bytes: int = ...,
        stdin: bool = ...,
    ) -> ProcessInfo: ...

    async def list_processes(self) -> list[ProcessInfo]: ...

    async def read_process(
        self,
        process_id: str,
        *,
        stdout_offset: int = ...,
        stderr_offset: int = ...,
        max_bytes: int = ...,
        wait_s: float = ...,
    ) -> ProcessOutput: ...

    async def write_process(
        self, process_id: str, data: bytes | str = ..., *, close: bool = ...
    ) -> None: ...

    async def kill_process(self, process_id: str) -> ProcessInfo: ...

    async def snapshot(self) -> str: ...

    async def egress(self) -> list[EgressEvent]: ...

    async def destroy(self) -> None: ...


class SandboxClientLike(Protocol):
    async def create(self, spec: SandboxSpec | None = ...) -> Any: ...

    async def restore(
        self, snapshot_id: str, *, network: str = ..., allow_hosts: Sequence[str] = ...
    ) -> Any: ...

    async def delete_snapshot(self, snapshot_id: str) -> None: ...


@dataclass
class TaskEnvironment:
    """A sandbox ready for one task trial."""

    sandbox: SandboxLike
    config: EnvironmentConfig
    policy_events: list[PolicyEvent] = field(default_factory=list)
    _egress_seen: int = 0
    # Large tool outputs written to .evalsi-out/ so far (file names count up).
    spilled: int = 0

    @property
    def workdir(self) -> str:
        return self.config.sandbox.workdir

    @property
    def isolation(self) -> Isolation:
        return self.sandbox.isolation

    async def run(
        self, command: str | Sequence[str], *, timeout_s: float | None = None, span: str = ""
    ) -> ExecResult:
        """Run a shell command (a string) or an argv in the workdir, recording
        sandbox denials and refused egress as policy events."""
        argv = ["sh", "-c", command] if isinstance(command, str) else list(command)
        result = await self.sandbox.exec(
            argv, timeout_s=timeout_s or self.config.command_timeout_s, env=self.config.env
        )
        for denial in result.denials:
            kind = "egress_denied" if denial.startswith("egress denied") else "denial"
            self.policy_events.append(PolicyEvent(kind=kind, detail=denial, span_id=span))
        return result

    async def egress_log(self) -> list[EgressEvent]:
        return await self.sandbox.egress()


def _spec(config: EnvironmentConfig, network: str, allow_hosts: Sequence[str]) -> SandboxSpec:
    sb = config.sandbox
    return SandboxSpec(
        image=config.image,
        read_only_root=sb.read_only_root,
        min_isolation=sb.min_isolation,
        network=network,
        allow_hosts=list(allow_hosts),
        memory_mb=sb.memory_mb,
        max_procs=sb.max_procs,
        env=dict(config.env),
        files=dict(config.files),
        workdir=sb.workdir,
        services=list(config.services),
        service_lifetime_s=config.service_lifetime_s,
        volumes=list(config.volumes),
    )


class EnvironmentPool:
    """Builds task environments on a sandbox client, one setup per environment."""

    def __init__(self, client: SandboxClientLike) -> None:
        self.client = client
        self._snapshots: dict[str, str] = {}
        # Environments whose rung cannot snapshot (the pod rung): their setup
        # runs again in every trial's own sandbox.
        self._per_trial: set[str] = set()
        self._locks: dict[str, asyncio.Lock] = {}
        self._failed: dict[str, str] = {}
        # Files taken out of build stages, by environment: built once, placed per setup.
        self._stage_cache: dict[str, dict[tuple[str, str], StageFiles]] = {}

    async def acquire(
        self, config: EnvironmentConfig, *, extra_allow_hosts: Sequence[str] = ()
    ) -> TaskEnvironment:
        """A fresh sandbox for one trial. ``extra_allow_hosts`` widens the
        agent's network to an allowlist (a CLI agent's model API)."""
        network, allow = config.sandbox.network, list(config.sandbox.allow_hosts)
        if extra_allow_hosts and network != "allow":
            network, allow = "allowlist", [*allow, *extra_allow_hosts]
        try:
            if config.services:
                # Sidecar state is deliberately never snapshotted or reused.
                return await self._set_up_in_place(config, network, allow)
            if not config.setup:
                sandbox = await self.client.create(_spec(config, network, allow))
                return TaskEnvironment(sandbox, config)
            snapshot = await self._setup(config)
            if snapshot is None:
                return await self._set_up_in_place(config, network, allow)
            sandbox = await self.client.restore(snapshot, network=network, allow_hosts=allow)
        except SandboxError as exc:
            raise TaskError(f"environment: {exc}") from exc
        return TaskEnvironment(sandbox, config)

    async def restore(self, config: EnvironmentConfig, snapshot_id: str) -> TaskEnvironment:
        """A sandbox restored from a snapshot of a running episode (not the
        environment's setup snapshot), under the environment's own network."""
        sb = config.sandbox
        try:
            sandbox = await self.client.restore(
                snapshot_id, network=sb.network, allow_hosts=list(sb.allow_hosts)
            )
        except SandboxError as exc:
            raise TaskError(f"environment: {exc}") from exc
        return TaskEnvironment(sandbox, config)

    async def _set_up_in_place(
        self, config: EnvironmentConfig, network: str, allow: list[str]
    ) -> TaskEnvironment:
        """Without snapshots, setup runs in the trial's own sandbox, so it gets
        the agent's network: a setup that needs more fails, rather than the
        agent getting the setup's network."""
        setup_network = config.setup_network or config.sandbox.network
        setup_allow = set(config.setup_allow_hosts or config.sandbox.allow_hosts)
        wider = (setup_network == "allow" and network != "allow") or (
            setup_network == "allowlist" and network != "allow" and not setup_allow <= set(allow)
        )
        if wider:
            raise TaskError(
                "environment: this sandbox rung cannot snapshot, so setup would run with the "
                "agent's network, which is narrower than setup_network; use a prebuilt image "
                "(environment.image) or a rung with snapshots (bubblewrap, Firecracker)"
            )
        sandbox = await self.client.create(_spec(config, network, allow))
        try:
            await self._apply_stages(sandbox, config)
            await self._run_setup(sandbox, config)
        except BaseException:
            await sandbox.destroy()
            raise
        return TaskEnvironment(sandbox, config)

    async def _stage_files(self, config: EnvironmentConfig) -> dict[tuple[str, str], StageFiles]:
        """Build the earlier stages of a multi-stage build, each in a sandbox of its own, and
        take out the files later stages (and the environment) copy from them."""
        key = config.key()
        if key in self._stage_cache:
            return self._stage_cache[key]
        wanted: dict[str, set[str]] = {}
        for c in [*config.copy_from, *(c for st in config.stages for c in st.copy_from)]:
            wanted.setdefault(c.stage, set()).add(c.source)
        network = config.setup_network or config.sandbox.network
        allow = config.setup_allow_hosts or config.sandbox.allow_hosts
        if network != "allowlist":
            allow = []
        out: dict[tuple[str, str], StageFiles] = {}
        total = 0
        for stage in config.stages:
            if stage.name not in wanted:
                continue  # nothing is taken from it
            sb = config.sandbox
            spec = SandboxSpec(
                image=stage.image,
                min_isolation=sb.min_isolation,
                network=network,
                allow_hosts=list(allow),
                memory_mb=sb.memory_mb,
                max_procs=sb.max_procs,
                env=dict(stage.env),
                files=dict(stage.files),
                workdir=sb.workdir,
            )
            sandbox = await self.client.create(spec)
            try:
                await self._place(sandbox, stage.copy_from, out, sb.workdir, stage.env)
                for command in stage.setup:
                    result = await sandbox.exec(
                        ["sh", "-c", command], timeout_s=config.setup_timeout_s, env=stage.env
                    )
                    if not result.ok:
                        tail = (result.stderr or result.stdout)[-1500:]
                        raise TaskError(
                            f"build stage {stage.name} failed ({result.outcome}, exit "
                            f"{result.exit_code}) running {shlex.quote(command)}: {tail}"
                        )
                for source in sorted(wanted[stage.name]):
                    got = await sandbox.read_files([source], max_bytes=MAX_STAGE_BYTES)
                    if not got:
                        raise TaskError(
                            f"build stage {stage.name}: {source} does not exist (or is empty)"
                        )
                    total += sum(len(v) for v in got.values())
                    if total >= MAX_STAGE_BYTES:
                        raise TaskError("the files copied out of build stages exceed 256 MiB")
                    listing = await sandbox.exec(
                        [
                            "sh",
                            "-c",
                            'find "$1" -type f -perm /111 2>/dev/null; true',
                            "sh",
                            source,
                        ],
                        timeout_s=120,
                    )
                    executable = set(listing.stdout.splitlines())
                    out[(stage.name, source)] = [
                        (path, data, path in executable) for path, data in sorted(got.items())
                    ]
            finally:
                await sandbox.destroy()
        self._stage_cache[key] = out
        return out

    async def _apply_stages(self, sandbox: Any, config: EnvironmentConfig) -> None:
        if not config.copy_from:
            return
        files = await self._stage_files(config)
        await self._place(sandbox, config.copy_from, files, config.sandbox.workdir, config.env)

    @staticmethod
    async def _place(
        sandbox: Any,
        copies: Sequence[StageCopy],
        files: dict[tuple[str, str], StageFiles],
        workdir: str,
        env: dict[str, str],
    ) -> None:
        """Put the files ``copies`` name where they go: a file at the destination (inside it
        when it ends with a slash), a directory's files under it."""
        executable: list[str] = []
        for c in copies:
            items = files[(c.stage, c.source)]
            dest = c.destination
            if not dest.startswith("/"):
                dest = posixpath.join(workdir or "/", dest) + ("/" if dest.endswith("/") else "")
            is_file = len(items) == 1 and items[0][0] == c.source
            batch: dict[str, bytes | str] = {}
            for path, data, is_exec in items:
                if is_file:
                    target = dest + posixpath.basename(path) if dest.endswith("/") else dest
                else:
                    target = posixpath.join(dest, posixpath.relpath(path, c.source))
                batch[target] = data
                if is_exec:
                    executable.append(target)
            await sandbox.write_files(batch)
        if executable:
            command = "chmod +x " + " ".join(shlex.quote(p) for p in executable)
            result = await sandbox.exec(["sh", "-c", command], timeout_s=120, env=env)
            if not result.ok:
                raise TaskError(f"could not restore execute bits: {result.stderr[-300:]}")

    async def _run_setup(self, sandbox: Any, config: EnvironmentConfig) -> None:
        for command in config.setup:
            result = await sandbox.exec(
                ["sh", "-c", command], timeout_s=config.setup_timeout_s, env=config.env
            )
            if not result.ok:
                tail = (result.stderr or result.stdout)[-1500:]
                raise TaskError(
                    f"environment setup failed ({result.outcome}, exit "
                    f"{result.exit_code}) running {shlex.quote(command)}: {tail}"
                )

    async def _setup(self, config: EnvironmentConfig) -> str | None:
        """The environment's setup snapshot, made once; None when the rung
        cannot snapshot (setup then runs per trial)."""
        key = config.key()
        lock = self._locks.setdefault(key, asyncio.Lock())
        async with lock:
            if key in self._failed:
                raise TaskError(self._failed[key])
            if key in self._snapshots:
                return self._snapshots[key]
            if key in self._per_trial:
                return None
            network = config.setup_network or config.sandbox.network
            allow = config.setup_allow_hosts or config.sandbox.allow_hosts
            if network != "allowlist":
                allow = []
            sandbox = await self.client.create(_spec(config, network, allow))
            try:
                try:
                    await self._apply_stages(sandbox, config)
                    await self._run_setup(sandbox, config)
                except TaskError as exc:
                    self._failed[key] = str(exc)
                    raise
                try:
                    snapshot: str = await sandbox.snapshot()
                except SnapshotsUnsupported:
                    self._per_trial.add(key)
                    return None
            finally:
                await sandbox.destroy()
            self._snapshots[key] = snapshot
            return snapshot

    async def aclose(self) -> None:
        for snapshot in self._snapshots.values():
            with contextlib.suppress(Exception):
                await self.client.delete_snapshot(snapshot)
        self._snapshots.clear()

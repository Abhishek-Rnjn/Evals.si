"""Tasks and their environments.

A task is one record of an agent run, for one trial. Its environment comes
from the run spec's ``environment``, overridden field by field by the
record's ``metadata["environment"]``, so a benchmark importer can give each
task its own image, setup and checker while a spec sets shared defaults::

    spec:
      environment:
        image: python:3.12-slim
        setup: ["pip install -e ."]
        sandbox: {network: deny}
        checker: {command: ["pytest", "-q"]}
"""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Mapping
from dataclasses import asdict, dataclass, field
from typing import Any

from google.protobuf import json_format

from evalsi.sandbox.client import ServiceSpec
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2


class TaskError(RuntimeError):
    """The task could not run (environment, agent or harness failure).

    Infrastructure errors are reported as errors, never scored as the agent failing."""


_DURATION = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m|h)?$")


def seconds(value: Any, default: float) -> float:
    """``"120s"``, ``"10m"``, ``"1.5h"``, ``"250ms"`` or a number of seconds."""
    if value is None or value == "":
        return default
    if isinstance(value, int | float):
        return float(value)
    m = _DURATION.match(str(value).strip())
    if not m:
        raise ValueError(f"bad duration {value!r}; use for example 30s, 10m or 1h")
    scale = {"ms": 0.001, "s": 1, "m": 60, "h": 3600, None: 1}[m.group(2)]
    return float(m.group(1)) * scale


@dataclass
class SandboxPolicy:
    min_isolation: str = "namespaced"
    network: str = "deny"
    allow_hosts: list[str] = field(default_factory=list)
    memory_mb: int = 0
    max_procs: int = 0
    workdir: str = "/workspace"
    read_only_root: bool = False


@dataclass
class CheckerConfig:
    command: list[str]
    files: dict[str, str] = field(default_factory=dict)
    timeout_s: float = 600.0
    parser: str = "exit-code"
    # Globs (relative to the workdir) of files the agent must not change, and
    # what to do when it does: "fail" or "restore" (see protect.py).
    protect: list[str] = field(default_factory=list)
    on_change: str = "fail"
    # "shared" runs the checker in the agent's sandbox. "clean" runs it in a fresh
    # one that receives only the files under ``outputs`` (see isolate.py); "diff"
    # in a fresh one that receives the agent's diff (see diffcheck.py).
    isolation: str = "shared"
    outputs: list[str] = field(default_factory=list)
    # With "diff": globs whose changes are never applied (see diffcheck.py).
    exclude: list[str] = field(default_factory=list)


@dataclass
class DatabaseToolConfig:
    name: str
    sql: str
    description: str = ""
    parameters: dict[str, Any] = field(default_factory=dict)
    mode: str = "read"
    min_rows: int = 0
    max_rows: int = 0


@dataclass
class DatabaseAssertion:
    name: str
    sql: str
    expect: Any = None


@dataclass
class DatabaseConfig:
    """A SQLite tool environment (see dbenv.py)."""

    schema: str = ""
    seed: str = ""
    tools: list[DatabaseToolConfig] = field(default_factory=list)
    assertions: list[DatabaseAssertion] = field(default_factory=list)


@dataclass
class BuildStageConfig:
    """An earlier stage of a multi-stage build (see stages.py)."""

    name: str
    image: str
    files: dict[str, str] = field(default_factory=dict)
    setup: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)


@dataclass
class StageCopy:
    stage: str
    source: str
    destination: str


@dataclass
class EnvironmentConfig:
    image: str = ""
    files: dict[str, str] = field(default_factory=dict)
    setup: list[str] = field(default_factory=list)
    setup_timeout_s: float = 1800.0
    # Network during setup; empty means the sandbox's own.
    setup_network: str = ""
    setup_allow_hosts: list[str] = field(default_factory=list)
    sandbox: SandboxPolicy = field(default_factory=SandboxPolicy)
    checker: CheckerConfig | None = None
    env: dict[str, str] = field(default_factory=dict)
    command_timeout_s: float = 120.0
    services: list[ServiceSpec] = field(default_factory=list)
    service_lifetime_s: float = 0.0
    stages: list[BuildStageConfig] = field(default_factory=list)
    copy_from: list[StageCopy] = field(default_factory=list)
    database: DatabaseConfig | None = None

    @property
    def needs_sandbox(self) -> bool:
        """A task with only a database has no use for a sandbox."""
        return self.database is None or bool(
            self.image
            or self.files
            or self.setup
            or self.services
            or self.stages
            or self.checker is not None
        )

    def key(self) -> str:
        """Identifies environments that can share one setup snapshot."""
        built = {
            "image": self.image,
            "files": self.files,
            "setup": self.setup,
            "setup_network": self.setup_network,
            "setup_allow_hosts": self.setup_allow_hosts,
            "sandbox": asdict(self.sandbox),
            "env": self.env,
            "services": [asdict(s) for s in self.services],
            "service_lifetime_s": self.service_lifetime_s,
            "stages": [asdict(s) for s in self.stages],
            "copy_from": [asdict(c) for c in self.copy_from],
        }
        return hashlib.sha256(json.dumps(built, sort_keys=True).encode()).hexdigest()[:16]


_ENV_FIELDS = {
    "image",
    "files",
    "setup",
    "setup_timeout",
    "setup_network",
    "setup_allow_hosts",
    "sandbox",
    "checker",
    "env",
    "command_timeout",
    "services",
    "service_lifetime",
    "stages",
    "copy_from",
    "database",
}


def _merge(base: dict[str, Any], override: Mapping[str, Any]) -> dict[str, Any]:
    """Field-wise override; nested sandbox and checker merge one level down."""
    out = dict(base)
    for key, value in override.items():
        if key in ("sandbox", "checker") and isinstance(value, Mapping):
            out[key] = {**(out.get(key) or {}), **value}
        else:
            out[key] = value
    return out


def _camel_to_snake(data: Mapping[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in data.items():
        snake = re.sub(r"(?<!^)(?=[A-Z])", "_", key).lower()
        out[snake] = (
            _camel_to_snake(value)
            if isinstance(value, Mapping)
            and key
            not in (
                "files",
                "env",
            )
            else value
        )
    return out


def environment_for(spec: run_pb2.RunSpec, record: Record) -> EnvironmentConfig | None:
    """The record's environment: the spec's, overridden by ``metadata["environment"]``."""
    data: dict[str, Any] = {}
    present = spec.HasField("environment")
    if present:
        data = json_format.MessageToDict(spec.environment, preserving_proto_field_name=True)
    override = record.metadata.get("environment")
    if override is not None:
        if not isinstance(override, Mapping):
            raise TaskError(f"record {record.id}: metadata.environment must be a mapping")
        override = _camel_to_snake(override)
        unknown = set(override) - _ENV_FIELDS
        if unknown:
            raise TaskError(
                f"record {record.id}: unknown environment field(s) {', '.join(sorted(unknown))}"
            )
        data = _merge(data, override)
        present = True
    if not present:
        return None
    sandbox = SandboxPolicy(**{k: v for k, v in (data.get("sandbox") or {}).items() if v})
    checker = None
    raw_checker = data.get("checker") or {}
    if raw_checker.get("command"):
        checker = CheckerConfig(
            command=[str(c) for c in raw_checker["command"]],
            files={str(k): str(v) for k, v in (raw_checker.get("files") or {}).items()},
            timeout_s=seconds(raw_checker.get("timeout"), 600.0),
            parser=str(raw_checker.get("parser") or "exit-code"),
            protect=[str(p) for p in raw_checker.get("protect") or []],
            on_change=str(raw_checker.get("on_change") or "fail"),
            isolation=str(raw_checker.get("isolation") or "shared"),
            outputs=[str(p) for p in raw_checker.get("outputs") or []],
            exclude=[str(p) for p in raw_checker.get("exclude") or []],
        )
        from evalsi_harness.protect import MODES, validate_patterns

        if checker.on_change not in MODES:
            raise TaskError(f"checker.on_change must be one of {', '.join(MODES)}")
        try:
            validate_patterns(checker.protect)
        except ValueError as exc:
            raise TaskError(f"checker.protect: {exc}") from exc
    try:
        services = [ServiceSpec.from_dict(_camel_to_snake(s)) for s in data.get("services", [])]
        service_lifetime = seconds(data.get("service_lifetime"), 0)
    except (TypeError, ValueError, AttributeError) as exc:
        raise TaskError(f"invalid environment services: {exc}") from exc
    database = _database(data.get("database"), record.id)
    stages, copies = _stages(data, record.id)
    if checker is not None:
        from evalsi_harness import isolate

        try:
            isolate.validate(checker, has_services=bool(services))
        except ValueError as exc:
            raise TaskError(f"checker: {exc}") from exc
    return EnvironmentConfig(
        stages=stages,
        copy_from=copies,
        database=database,
        services=services,
        service_lifetime_s=service_lifetime,
        image=str(data.get("image", "")),
        files={str(k): str(v) for k, v in (data.get("files") or {}).items()},
        setup=[str(c) for c in data.get("setup") or []],
        setup_timeout_s=seconds(data.get("setup_timeout"), 1800.0),
        setup_network=str(data.get("setup_network", "")),
        setup_allow_hosts=[str(h) for h in data.get("setup_allow_hosts") or []],
        sandbox=sandbox,
        checker=checker,
        env={str(k): str(v) for k, v in (data.get("env") or {}).items()},
        command_timeout_s=seconds(data.get("command_timeout"), 120.0),
    )


def _database(raw: Any, record_id: str) -> DatabaseConfig | None:
    if not raw:
        return None
    if not isinstance(raw, Mapping):
        raise TaskError(f"record {record_id}: environment.database must be a mapping")
    try:
        config = DatabaseConfig(
            schema=str(raw.get("schema") or ""),
            seed=str(raw.get("seed") or ""),
            tools=[
                DatabaseToolConfig(
                    name=str(t.get("name", "")),
                    sql=str(t.get("sql", "")),
                    description=str(t.get("description", "")),
                    parameters=dict(t.get("parameters") or {}),
                    mode=str(t.get("mode") or "read"),
                    min_rows=int(t.get("min_rows") or 0),
                    max_rows=int(t.get("max_rows") or 0),
                )
                for t in raw.get("tools") or []
            ],
            assertions=[
                DatabaseAssertion(str(a.get("name", "")), str(a.get("sql", "")), a.get("expect"))
                for a in raw.get("assertions") or []
            ],
        )
        from evalsi_harness import dbenv

        dbenv.validate(config)
    except (ValueError, TypeError, AttributeError) as exc:
        raise TaskError(f"environment.database: {exc}") from exc
    return config


def _stages(
    data: Mapping[str, Any], record_id: str
) -> tuple[list[BuildStageConfig], list[StageCopy]]:
    try:
        stages = [
            BuildStageConfig(
                name=str(s.get("name", "")),
                image=str(s.get("image", "")),
                files={str(k): str(v) for k, v in (s.get("files") or {}).items()},
                setup=[str(c) for c in s.get("setup") or []],
                env={str(k): str(v) for k, v in (s.get("env") or {}).items()},
            )
            for s in data.get("stages") or []
        ]
        copies = [
            StageCopy(
                str(c.get("stage", "")), str(c.get("source", "")), str(c.get("destination", ""))
            )
            for c in data.get("copy_from") or []
        ]
    except (TypeError, AttributeError) as exc:
        raise TaskError(f"record {record_id}: invalid stages or copy_from: {exc}") from exc
    names = [s.name for s in stages]
    for s in stages:
        if not s.name or not s.image:
            raise TaskError("every build stage needs a name and an image")
    if len(set(names)) != len(names):
        raise TaskError("build stage names must be unique")
    for c in copies:
        if c.stage not in names:
            raise TaskError(f"copy_from names an unknown stage {c.stage!r}")
        if not c.source.startswith("/") or not c.destination:
            raise TaskError("copy_from needs an absolute source and a destination")
    return stages, copies


@dataclass
class Task:
    id: str
    record: Record
    spec: run_pb2.RunSpec
    trial: int = 0
    run_id: str = ""
    environment: EnvironmentConfig | None = None
    # Harness-specific configuration (ExternalHarness.config).
    config: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def build(
        cls, spec: run_pb2.RunSpec, record: Record, *, trial: int = 0, run_id: str = ""
    ) -> Task:
        return cls(
            id=record.id,
            record=record,
            spec=spec,
            trial=trial,
            run_id=run_id,
            environment=environment_for(spec, record),
        )

    @property
    def instruction(self) -> str:
        """The record's input as text: the task the agent is given."""
        if self.record.input is None:
            raise TaskError(f"record {self.record.id} has no input to give the agent")
        return self.record.input.as_text()

    @property
    def conversation(self) -> list[dict[str, Any]]:
        """The input as chat messages (a message list, or one user message)."""
        content: Content | None = self.record.input
        if content is None:
            raise TaskError(f"record {self.record.id} has no input to give the agent")
        if content.messages is not None:
            return [
                {"role": m.role, "content": m.content}
                for m in content.messages
                if m.role in ("user", "assistant")
            ]
        return [{"role": "user", "content": content.as_text()}]

    @property
    def slug(self) -> str:
        """A filesystem-safe name for this task and trial."""
        safe = re.sub(r"[^A-Za-z0-9._-]+", "_", self.id)[:80] or "task"
        return f"{safe}.{self.trial}"

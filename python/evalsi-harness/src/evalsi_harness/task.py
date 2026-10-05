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
        )
    return EnvironmentConfig(
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

"""Docker Compose files of Terminal-Bench 1 tasks as an agent environment (decision 0033).

A Compose file names the agent's container (``client``, or the service whose image is
``${T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME}``) and the services around it. This module
turns the file into what an Evals.si environment can say: the client's build, files,
environment and shared volumes, and a list of services. The aim is the whole of the
Compose that Terminal-Bench's tasks use, not a subset; what cannot be kept is refused
with the reason, and what is dropped without changing what a task measures is listed
in ``Compose.ignored`` and recorded on the task.

Kept: ``image``, ``build`` (context, dockerfile, args, target), ``command`` and
``entrypoint`` (Docker's rules for how they combine with the image's), ``environment``
and ``env_file``, ``depends_on`` with its conditions, ``healthcheck`` (as the readiness
check), ``ports`` and ``expose`` (the listening ports), ``volumes`` (bind-mounted files
and directories of the task, named volumes shared between services and with the
client's workdir), ``working_dir``, ``hostname``, ``container_name``, ``links``,
``networks`` and ``extra_hosts`` (host names on the one loopback the services share),
``deploy`` and ``mem_limit``/``cpus`` (limits), variable interpolation, and the
``x-evalsi`` block of the earlier restricted importer.

A command that installs packages before it starts the server (``sh -c "pip install x
&& python app.py"``) is split: the installs become the service's setup, run once
with the network, and the rest is the command, since a running service has no network.

Refused: ``privileged``, ``cap_add``, ``devices``, ``network_mode``, ``pid``,
``ipc``, ``sysctls`` and the like (the sandbox gives a service none of them), bind
mounts of host paths outside the task, ``extends``, ``include``, ``profiles``, ``configs``
and ``secrets``, services on networks that share none with the others, two services that
listen on one port (they share a loopback), a service build that is itself multi-stage,
and files that cannot be embedded as text.
"""

from __future__ import annotations

import posixpath
import re
import shlex
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from evalsi_harness.benchmarks.dockerfile import (
    Build,
    TaskFormatError,
    prepared_setup,
    put_file,
    translate_dockerfile,
)

# What Terminal-Bench's runner sets for a task's Compose, and the paths it mounts the
# logs at (Evals.si manages those itself).
TB_VARS = {
    "T_BENCH_TEST_DIR": "/tests",
    "T_BENCH_CONTAINER_LOGS_PATH": "/logs",
    "T_BENCH_CONTAINER_AGENT_LOGS_PATH": "/agent-logs",
    "T_BENCH_TASK_LOGS_PATH": "/dev/null",
    "T_BENCH_TASK_AGENT_LOGS_PATH": "/dev/null",
    "T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME": "client",
    "T_BENCH_TASK_DOCKER_CLIENT_CONTAINER_NAME": "client",
}
MANAGED_VARS = (
    "T_BENCH_TASK_LOGS_PATH",
    "T_BENCH_TASK_AGENT_LOGS_PATH",
    "T_BENCH_CONTAINER_LOGS_PATH",
    "T_BENCH_CONTAINER_AGENT_LOGS_PATH",
)
# The task's own framework files: a bind mount of the task directory does not carry them.
TASK_FILES = {
    "task.yaml",
    "solution.sh",
    "solution.yaml",
    "run-tests.sh",
    "docker-compose.yaml",
    "docker-compose.yml",
    "tests",
    "Dockerfile",
}
DEFAULT_MEMORY_MB = 2048
DEFAULT_CPU_MILLIS = 1000
MAX_BIND_BYTES = 8 * 1024 * 1024

SERVICE_KEYS = {
    "image",
    "build",
    "command",
    "entrypoint",
    "environment",
    "env_file",
    "depends_on",
    "ports",
    "expose",
    "volumes",
    "working_dir",
    "hostname",
    "container_name",
    "links",
    "networks",
    "extra_hosts",
    "healthcheck",
    "x-evalsi",
    "deploy",
    "mem_limit",
    "cpus",
    # Dropped: they change nothing a task measures.
    "restart",
    "labels",
    "user",
    "tty",
    "stdin_open",
    "init",
    "platform",
    "logging",
    "stop_signal",
    "stop_grace_period",
    "pull_policy",
    "read_only",
    "tmpfs",
    "shm_size",
    "ulimits",
    "dns",
    "dns_search",
    "mem_reservation",
    "memswap_limit",
    "cpu_shares",
    "scale",
    "annotations",
}
DROPPED = {
    "restart": "services are not restarted; one that exits is an infrastructure error",
    "user": "the service runs as the sandbox's unprivileged user",
    "tmpfs": "every service has a private /tmp",
    "ulimits": "limits are memory, files and processes",
    "read_only": "the service's root is a private copy",
}
REFUSED = {
    "privileged": "a service is never privileged",
    "cap_add": "a service gets no capabilities",
    "cap_drop": "a service gets no capabilities, so there is nothing to drop",
    "devices": "a service gets no host devices",
    "network_mode": "all services share one private network",
    "pid": "a service has its own process namespace",
    "ipc": "a service has its own IPC namespace",
    "uts": "a service has its own UTS namespace",
    "sysctls": "a service cannot set kernel parameters",
    "security_opt": "a service runs under the sandbox's own security profile",
    "cgroup_parent": "limits are the sandbox's own",
    "userns_mode": "a service runs in a user namespace of the sandbox's own",
    "extends": "extends is not resolved; write the service out",
    "profiles": "profiles select services at run time; write the services that run",
    "configs": "configs are not supported",
    "secrets": "secrets are not supported",
    "volumes_from": "volumes_from is not supported; name the volumes",
    "runtime": "a service runs under the sandbox's own runtime",
    "gpus": "no sandbox rung gives a service a GPU",
}


@dataclass
class ClientBuild:
    context: Path
    dockerfile: Path
    args: dict[str, str] = field(default_factory=dict)
    target: str = ""


@dataclass
class Compose:
    """What a task's Compose file says, in the terms of an Evals.si environment."""

    services: list[dict[str, Any]] = field(default_factory=list)
    client: ClientBuild | None = None
    client_image: str = ""
    env: dict[str, str] = field(default_factory=dict)
    files: dict[str, str] = field(default_factory=dict)
    executable: list[str] = field(default_factory=list)
    # Named volumes of the client: (volume, path, read-only), until finalize.
    client_volumes: list[tuple[str, str, bool]] = field(default_factory=list)
    # The client's volumes shared with services, "name:/path[:ro]", after finalize.
    volumes: list[str] = field(default_factory=list)
    aliases: list[str] = field(default_factory=list)
    # Commands for the client's setup: its extra_hosts that point beyond loopback.
    setup: list[str] = field(default_factory=list)
    workdir: str = ""
    ignored: list[str] = field(default_factory=list)

    def finalize(self, workdir: str) -> None:
        """Settle the named volumes once the client's workdir is known: a volume mounted at
        the workdir is the workdir itself (a service sees the agent's files at its own path),
        any other volume of the client's is shared by name."""
        at_workdir = {v for v, dst, _ in self.client_volumes if posixpath.normpath(dst) == workdir}
        for v, dst, ro in self.client_volumes:
            if v not in at_workdir:
                self.volumes.append(f"{v}:{dst}" + (":ro" if ro else ""))
        self.client_volumes = []
        for s in self.services:
            vols = []
            for v, dst, ro in s.pop("_vols", []):
                if v in at_workdir:
                    if "workdir_mount" in s:
                        raise TaskFormatError(
                            f"service {s['name']} mounts the client's workdir twice"
                        )
                    s["workdir_mount"] = dst
                else:
                    vols.append(f"{v}:{dst}" + (":ro" if ro else ""))
            if vols:
                s["volumes"] = vols


# --- interpolation ---

_VAR = re.compile(
    r"\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)"
)


def interpolate(value: str, variables: dict[str, str]) -> str:
    """Compose's ``$VAR``, ``${VAR}``, ``${VAR:-default}``, ``${VAR-default}``, ``${VAR:?error}``
    and ``${VAR:+alternative}``, and ``$$`` for a dollar sign. A ``$`` that starts no name is
    literal. A variable that is not set is an error rather than the empty string Compose
    would use, since a silent empty value changes what a task is."""

    def sub(m: re.Match[str]) -> str:
        if m.group(0) == "$$":
            return "$"
        name = m.group(1) or m.group(4)
        op, arg = m.group(2) or "", m.group(3) or ""
        got = variables.get(name)
        empty = got is None or (got == "" and op.startswith(":"))
        if op.endswith("-"):
            return arg if empty else str(got)
        if op.endswith("+"):
            return "" if empty else arg
        if op.endswith("?"):
            if empty:
                raise TaskFormatError(f"Compose variable {name}: {arg or 'is required'}")
            return str(got)
        if got is None:
            raise TaskFormatError(
                f"Compose interpolation of ${name}: the variable is not defined "
                f"(use ${{{name}:-default}} or define it)"
            )
        return got

    return _VAR.sub(sub, value)


def _expand(obj: Any, variables: dict[str, str]) -> Any:
    if isinstance(obj, str):
        return interpolate(obj, variables)
    if isinstance(obj, list):
        return [_expand(x, variables) for x in obj]
    if isinstance(obj, dict):
        return {k: _expand(v, variables) for k, v in obj.items()}
    return obj


# --- small parsers ---


def _argv(value: Any, what: str, name: str) -> list[str] | None:
    if value is None:
        return None
    if isinstance(value, str):
        return shlex.split(value)
    if isinstance(value, list):
        return [str(x) for x in value]
    raise TaskFormatError(f"service {name}: {what} must be a string or a list")


def _env_map(value: Any, name: str) -> dict[str, str]:
    if value is None:
        return {}
    if isinstance(value, dict):
        for k, v in value.items():
            if isinstance(v, dict | list):
                raise TaskFormatError(f"service {name}: environment {k} must be a plain value")
        return {
            str(k): "" if v is None else str(v).lower() if isinstance(v, bool) else str(v)
            for k, v in value.items()
        }
    if isinstance(value, list):
        out: dict[str, str] = {}
        for item in value:
            key, eq, val = str(item).partition("=")
            if not eq:
                raise TaskFormatError(
                    f"service {name}: environment entry {item!r} needs a value (KEY=value)"
                )
            out[key] = val
        return out
    raise TaskFormatError(f"service {name}: environment must be a mapping or a list")


def _env_file(value: Any, task: Path, name: str) -> dict[str, str]:
    out: dict[str, str] = {}
    paths = [value] if isinstance(value, str) else value
    for entry in paths or []:
        rel = entry["path"] if isinstance(entry, dict) else entry
        path = _inside(task, str(rel), f"service {name}: env_file")
        if not path.is_file():
            raise TaskFormatError(f"service {name}: env_file {rel} does not exist")
        for text in path.read_text().splitlines():
            line = text.strip()
            if line and not line.startswith("#") and "=" in line:
                key, _, val = line.partition("=")
                out[key.strip()] = val.strip().strip("\"'")
    return out


def _inside(task: Path, rel: str, what: str) -> Path:
    root = task.resolve()
    path = (root / rel).resolve()
    if root not in (path, *path.parents):
        raise TaskFormatError(f"{what} {rel} is outside the task directory")
    return path


def _ports(service: dict[str, Any], name: str) -> list[int]:
    """The container ports a service listens on, from ``ports`` and ``expose``."""
    out: list[int] = []
    for entry in [*(service.get("ports") or []), *(service.get("expose") or [])]:
        if isinstance(entry, dict):
            targets = [entry.get("target")]
        else:
            text = str(entry).split("/")[0]
            container = text.split(":")[-1]
            lo, _, hi = container.partition("-")
            if not lo.isdigit() or (hi and not hi.isdigit()):
                raise TaskFormatError(f"service {name}: cannot read the port {entry!r}")
            targets = list(range(int(lo), int(hi or lo) + 1))
        for t in targets:
            if not isinstance(t, int | str) or not str(t).isdigit():
                raise TaskFormatError(f"service {name}: cannot read the port {entry!r}")
            if int(t) not in out:
                out.append(int(t))
    return out


def _size_mb(value: Any, what: str, name: str) -> int:
    text = str(value).strip().lower()
    m = re.fullmatch(r"(\d+(?:\.\d+)?)\s*([kmgb]?)b?", text)
    if not m:
        raise TaskFormatError(f"service {name}: cannot read {what} {value!r}")
    scale = {"": 1 / (1 << 20), "b": 1 / (1 << 20), "k": 1 / 1024, "m": 1, "g": 1024}[m.group(2)]
    return max(1, int(float(m.group(1)) * scale))


def _limits(service: dict[str, Any], name: str) -> tuple[int, int]:
    limits = ((service.get("deploy") or {}).get("resources") or {}).get("limits") or {}
    memory = limits.get("memory") or service.get("mem_limit")
    cpus = limits.get("cpus") or service.get("cpus")
    x = service.get("x-evalsi") or {}
    mb = int(x["memory_mb"]) if "memory_mb" in x else None
    millis = int(x["cpu_millis"]) if "cpu_millis" in x else None
    if mb is None:
        mb = _size_mb(memory, "the memory limit", name) if memory else DEFAULT_MEMORY_MB
    if millis is None:
        millis = int(float(cpus) * 1000) if cpus else DEFAULT_CPU_MILLIS
    return max(16, mb), max(1, millis)


def _healthcheck(service: dict[str, Any], name: str) -> list[str]:
    hc = service.get("healthcheck")
    if not hc or hc.get("disable") or hc.get("test") in (None, ["NONE"], "NONE"):
        return []
    test = hc["test"]
    if isinstance(test, str):
        return ["sh", "-c", test]
    if not isinstance(test, list) or not test:
        raise TaskFormatError(f"service {name}: cannot read the healthcheck")
    if test[0] == "CMD":
        return [str(x) for x in test[1:]]
    if test[0] == "CMD-SHELL":
        return ["sh", "-c", str(test[1])]
    return [str(x) for x in test]


def _depends(service: dict[str, Any], names: set[str], name: str) -> dict[str, str]:
    raw = service.get("depends_on") or []
    deps: dict[str, str] = {}
    if isinstance(raw, list):
        deps = {str(d): "service_started" for d in raw}
    elif isinstance(raw, dict):
        for dep, spec in raw.items():
            deps[str(dep)] = str((spec or {}).get("condition", "service_started"))
    else:
        raise TaskFormatError(f"service {name}: depends_on must be a list or a mapping")
    for dep, cond in deps.items():
        if dep not in names:
            raise TaskFormatError(f"service {name}: depends_on names the unknown service {dep}")
        if cond not in ("service_started", "service_healthy", "service_completed_successfully"):
            raise TaskFormatError(f"service {name}: depends_on condition {cond!r} is not supported")
    return deps


def _networks(service: dict[str, Any]) -> tuple[set[str], list[str]]:
    """The networks a service is on (the default one when it names none) and its aliases."""
    raw = service.get("networks")
    aliases: list[str] = []
    if isinstance(raw, dict):
        for spec in raw.values():
            aliases += [str(a) for a in (spec or {}).get("aliases") or []]
        return set(raw), aliases
    return (set(raw) if raw else {"default"}), aliases


def _hosts(service: dict[str, Any], name: str) -> list[str]:
    """Host names a service answers to, from hostname, container_name, links, network
    aliases and extra_hosts that point at loopback."""
    out = [str(service[k]) for k in ("hostname", "container_name") if service.get(k)]
    for link in service.get("links") or []:
        target, _, alias = str(link).partition(":")
        out.append(alias or target)
    out += _networks(service)[1]
    extra = service.get("extra_hosts") or []
    pairs = (
        list(extra.items()) if isinstance(extra, dict) else [str(e).split(":", 1) for e in extra]
    )
    for pair in pairs:
        if len(pair) != 2:
            raise TaskFormatError(f"service {name}: cannot read extra_hosts entry {pair!r}")
        host, ip = str(pair[0]), str(pair[1])
        if ip not in ("127.0.0.1", "::1", "localhost"):
            raise TaskFormatError(
                f"service {name}: extra_hosts maps {host} to {ip}; the services share one "
                "loopback and have no other address to give"
            )
        out.append(host)
    return out


_INSTALL = re.compile(
    r"^\s*(?:pip3?|apt|apt-get|apk|npm|yarn|gem|uv|python3?\s+-m\s+pip)\b", re.IGNORECASE
)


def _top_level_and(script: str) -> list[int]:
    """Offsets of ``&&`` outside quotes, parentheses and braces."""
    out, quote, depth, i = [], "", 0, 0
    while i < len(script):
        c = script[i]
        if quote:
            if c == "\\" and quote == '"':
                i += 1
            elif c == quote:
                quote = ""
        elif c in "\"'":
            quote = c
        elif c in "({":
            depth += 1
        elif c in ")}":
            depth -= 1
        elif c == "&" and script[i : i + 2] == "&&" and depth == 0:
            out.append(i)
            i += 1
        i += 1
    return out


def split_installs(argv: list[str]) -> tuple[list[str], list[str]]:
    """``sh -c "pip install x && python app.py"`` as (``["pip install x"]``, the command
    ``sh -c "python app.py"``). Only a run of package installs at the front of the
    script is moved; the rest of the script is kept byte for byte."""
    if len(argv) != 3 or argv[0] not in ("sh", "bash", "/bin/sh", "/bin/bash") or argv[1] != "-c":
        return [], argv
    script = argv[2]
    cuts = _top_level_and(script)
    starts = [0, *[c + 2 for c in cuts]]
    ends = [*cuts, len(script)]
    installs: list[str] = []
    taken = 0
    for k, (a, b) in enumerate(zip(starts, ends, strict=True)):
        segment = script[a:b]
        if k < len(cuts) and _INSTALL.match(segment) and "&" not in segment.replace("&&", ""):
            installs.append(" ".join(segment.split()))
            taken = k + 1
        else:
            break
    if not installs:
        return [], argv
    return installs, [argv[0], "-c", script[starts[taken] :].strip()]


# --- the file ---

_IDLE = re.compile(
    r"^\s*(?:sleep\s+(?:infinity|\d+)|tail\s+-f\s+/dev/null|while\s+true.*|exec\s+sleep\s+infinity)\s*$"
)
_HOSTS_ECHO = re.compile(r"""^\s*echo\s+['"]127\.0\.0\.1\s+([^'"\s]+)['"]\s*>>\s*/etc/hosts\s*$""")
_DNS_NAME = re.compile(r"^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$")


def load_compose(task: Path, task_name: str = "") -> Compose | None:
    """The task's ``docker-compose.yaml`` as a :class:`Compose`, or None when it has none."""
    import yaml

    path = task / "docker-compose.yaml"
    if not path.is_file():
        path = task / "docker-compose.yml"
        if not path.is_file():
            return None
    doc = yaml.safe_load(path.read_text()) or {}
    if not isinstance(doc, dict):
        raise TaskFormatError("Compose document must be a mapping")
    for key in ("configs", "secrets", "include"):
        if key in doc:
            raise TaskFormatError(f"Compose top-level {key} is not supported")
    raw = doc.get("services") or {}
    if not isinstance(raw, dict) or not raw:
        raise TaskFormatError("Compose services must be a mapping")
    variables = {**TB_VARS, "T_BENCH_TASK_DOCKER_NAME_PREFIX": task_name or task.name}
    services: dict[str, dict[str, Any]] = {}
    for n, s in raw.items():
        if s is not None and not isinstance(s, dict):
            raise TaskFormatError(f"service {n}: configuration must be a mapping")
        services[str(n)] = _expand(s or {}, variables)
    client_name = _client(raw)
    _check_networks(services)
    compose = Compose()
    _read_client(task, client_name, services[client_name], compose)
    sanitized = _names([n for n in services if n != client_name])
    for n, svc_name in sanitized.items():
        compose.services.append(
            _service(task, n, svc_name, services, sanitized, client_name, compose)
        )
    _link(compose, services, sanitized, client_name)
    _unique_ports(compose)
    if compose.setup and compose.services:
        raise TaskFormatError(
            "the client's extra_hosts point beyond loopback, and the services' generated "
            "hosts file would replace them"
        )
    return compose


def _client(services: dict[str, Any]) -> str:
    for name, svc in services.items():
        if "T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME" in str((svc or {}).get("image", "")):
            return str(name)
    if "client" in services:
        return "client"
    if len(services) == 1:
        return str(next(iter(services)))
    raise TaskFormatError(
        "Compose has no agent service: name one client, or give it "
        "image: ${T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME}"
    )


def _check_networks(services: dict[str, Any]) -> None:
    """Services that share no network cannot reach each other in Compose; here they would."""
    nets = {n: _networks(s)[0] for n, s in services.items()}
    names = list(nets)
    for i, a in enumerate(names):
        for b in names[i + 1 :]:
            if not nets[a] & nets[b]:
                raise TaskFormatError(
                    f"services {a} and {b} are on networks that share none; the services share "
                    "one private network, so the isolation would be lost"
                )


def _names(names: list[str]) -> dict[str, str]:
    """Compose service name -> sandbox service name (a DNS label). The original name stays
    reachable as an alias, so ``master_service`` still resolves."""
    out: dict[str, str] = {}
    for n in names:
        label = n.lower().replace("_", "-").replace(".", "-")
        if not re.fullmatch(r"[a-z][a-z0-9-]{0,39}", label) or label.endswith("-"):
            raise TaskFormatError(f"service name {n!r} cannot be a host name here")
        if label in out.values() or label in ("localhost", "sandbox", "guest", "workdir"):
            raise TaskFormatError(f"service name {n!r} is reserved or clashes with another")
        out[n] = label
    return out


def _check_keys(name: str, service: dict[str, Any], allowed: set[str]) -> None:
    for key in service:
        if key in REFUSED:
            raise TaskFormatError(f"service {name}: {key} is not supported: {REFUSED[key]}")
    unknown = sorted(set(service) - allowed - set(REFUSED))
    if unknown:
        raise TaskFormatError(f"service {name}: unsupported Compose fields {unknown}")


def _build_of(task: Path, name: str, spec: Any) -> ClientBuild:
    if isinstance(spec, str):
        spec = {"context": spec}
    if not isinstance(spec, dict):
        raise TaskFormatError(f"service {name}: build must be a path or a mapping")
    unknown = sorted(set(spec) - {"context", "dockerfile", "args", "target"})
    if unknown:
        raise TaskFormatError(f"service {name}: unsupported build fields {unknown}")
    context = _inside(task, str(spec.get("context", ".")), f"service {name}: build context")
    dockerfile = context / str(spec.get("dockerfile", "Dockerfile"))
    args = spec.get("args") or {}
    if isinstance(args, list):
        args = dict(str(a).split("=", 1) for a in args)
    return ClientBuild(
        context, dockerfile, {str(k): str(v) for k, v in args.items()}, str(spec.get("target", ""))
    )


_CLIENT_KEYS = SERVICE_KEYS | {"stdin_open"}


def _read_client(task: Path, name: str, client: dict[str, Any], compose: Compose) -> None:
    _check_keys(f"{name} (the client)", client, _CLIENT_KEYS)
    if "build" in client:
        compose.client = _build_of(task, name, client["build"])
    elif (
        client.get("image") and client["image"] != TB_VARS["T_BENCH_TASK_DOCKER_CLIENT_IMAGE_NAME"]
    ):
        compose.client_image = str(client["image"])
    command = _argv(client.get("command"), "command", name)
    for part in _idle_parts(command, name):
        compose.aliases.append(part)
    env = {
        **_env_file(client.get("env_file"), task, name),
        **_env_map(client.get("environment"), name),
    }
    for k, v in env.items():
        if not (k == "TEST_DIR" and v == TB_VARS["T_BENCH_TEST_DIR"]):
            compose.env[k] = v
    if client.get("working_dir"):
        compose.workdir = str(client["working_dir"])
    mounts = _mounts(task, name, client.get("volumes") or [], compose)
    for dst, text in mounts.files.items():
        compose.files[dst] = text
    compose.executable += mounts.executable
    compose.files.update(mounts.binary)
    compose.client_volumes += mounts.named
    own = dict(client)
    extra = own.pop("extra_hosts", None) or []
    pairs = (
        list(extra.items()) if isinstance(extra, dict) else [str(e).split(":", 1) for e in extra]
    )
    for pair in pairs:
        if len(pair) != 2:
            raise TaskFormatError(f"service {name}: cannot read extra_hosts entry {pair!r}")
        host, ip = str(pair[0]), str(pair[1])
        if ip in ("127.0.0.1", "::1", "localhost"):
            compose.aliases.append(host)
        else:
            # Written into the client's /etc/hosts by its setup; only a root of its own can hold it.
            compose.setup.append(f"echo {shlex.quote(f'{ip} {host}')} >> /etc/hosts")
    compose.aliases += [a for a in _hosts(own, name) if a != "client"]
    for key in ("deploy", "mem_limit", "cpus", "ports", "expose", "healthcheck"):
        if key in client:
            compose.ignored.append(f"client {key}")
    for key in client.keys() & set(DROPPED):
        compose.ignored.append(f"client {key}")


def _idle_parts(command: list[str] | None, name: str) -> list[str]:
    """Host names a client command adds to /etc/hosts. The command must otherwise only
    idle: the client runs the agent, and anything else it starts would be lost."""
    if not command:
        return []
    script = (
        command[2]
        if len(command) == 3 and command[0] in ("sh", "bash") and command[1] == "-c"
        else shlex.join(command)
    )
    aliases: list[str] = []
    for part in re.split(r"&&|;|\n", script):
        if not part.strip():
            continue
        if m := _HOSTS_ECHO.match(part):
            aliases.append(m.group(1))
        elif not _IDLE.match(part):
            raise TaskFormatError(
                f"the client {name} runs {part.strip()[:60]!r} when it starts, and the agent's "
                "container only idles here; move it into the Dockerfile or a service"
            )
    return aliases


@dataclass
class _Mounts:
    files: dict[str, str] = field(default_factory=dict)
    executable: list[str] = field(default_factory=list)
    binary: dict[str, str] = field(default_factory=dict)
    # (volume, path, read-only)
    named: list[tuple[str, str, bool]] = field(default_factory=list)


def _mounts(task: Path, name: str, entries: list[Any], compose: Compose) -> _Mounts:
    out = _Mounts()
    for entry in entries:
        if isinstance(entry, dict):
            kind = entry.get(
                "type",
                "volume"
                if entry.get("source") and not str(entry["source"]).startswith((".", "/"))
                else "bind",
            )
            src, dst, ro = (
                str(entry.get("source") or ""),
                str(entry.get("target") or ""),
                bool(entry.get("read_only")),
            )
            if kind == "tmpfs":
                compose.ignored.append(f"{name} tmpfs {dst}")
                continue
        else:
            parts = str(entry).split(":")
            if len(parts) == 1:
                src, dst, ro = "", parts[0], False
            elif len(parts) in (2, 3):
                src, dst = parts[0], parts[1]
                ro = len(parts) == 3 and "ro" in parts[2].split(",")
            else:
                raise TaskFormatError(f"service {name}: cannot read the volume {entry!r}")
        if not dst.startswith("/"):
            raise TaskFormatError(f"service {name}: volume target {dst!r} must be absolute")
        if not src:
            compose.ignored.append(f"{name} anonymous volume {dst}")
        elif src.startswith(("/", ".", "~")):
            _bind(task, name, src, dst, out, compose)
        else:
            out.named.append((src, dst, ro))
    return out


def _bind(  # noqa: PLR0917
    task: Path, name: str, src: str, dst: str, out: _Mounts, compose: Compose
) -> None:
    if src == TB_VARS["T_BENCH_TASK_LOGS_PATH"]:
        compose.ignored.append(f"{name} log mount {dst}")
        return
    if src.startswith(("/", "~")):
        raise TaskFormatError(
            f"service {name}: bind mount of the host path {src}; only paths in the task "
            "directory can be mounted"
        )
    path = _inside(task, src, f"service {name}: bind mount")
    if not path.exists():
        raise TaskFormatError(f"service {name}: bind mount source {src} does not exist")
    files = (
        [path]
        if path.is_file()
        else sorted(
            p
            for p in path.rglob("*")
            if p.is_file()
            and "__pycache__" not in p.parts
            and ".git" not in p.parts
            and not (
                path.resolve() == task.resolve() and p.relative_to(path).parts[0] in TASK_FILES
            )
        )
    )
    total = 0
    for f in files:
        total += f.stat().st_size
        if total > MAX_BIND_BYTES:
            raise TaskFormatError(f"service {name}: bind mount {src} is larger than 8 MiB")
        target = dst if path.is_file() else f"{dst.rstrip('/')}/{f.relative_to(path).as_posix()}"
        put_file(out.files, f, target)
        if f.stat().st_mode & 0o111:
            out.executable.append(target)
    # Binary files travel base64-encoded; they are split off so a service can refuse them.
    for key in [k for k in out.files if k.endswith(".evalsi-b64")]:
        out.binary[key] = out.files.pop(key)


def _service(  # noqa: PLR0915, PLR0917
    task: Path,
    orig: str,
    name: str,
    services: dict[str, dict[str, Any]],
    names: dict[str, str],
    client_name: str,
    compose: Compose,
) -> dict[str, Any]:
    svc = services[orig]
    _check_keys(orig, svc, SERVICE_KEYS)
    deps = _depends(svc, set(services), orig)
    if client_name in deps:
        raise TaskFormatError(f"service {orig} depends on the agent's container {client_name}")
    build: Build | None = None
    image = str(svc.get("image") or "")
    if "build" in svc:
        spec = _build_of(task, orig, svc["build"])
        if not spec.dockerfile.is_file():
            raise TaskFormatError(f"service {orig}: {spec.dockerfile.name} does not exist")
        build = translate_dockerfile(
            spec.dockerfile.read_text(), spec.context, build_args=spec.args, target=spec.target
        )
        if build.stages:
            raise TaskFormatError(
                f"service {orig}: its Dockerfile is multi-stage; build the image "
                "and name it in image:"
            )
        image = build.image
    if not image:
        raise TaskFormatError(f"service {orig} has neither image nor build")
    env = {
        **(build.env if build else {}),
        **_env_file(svc.get("env_file"), task, orig),
        **_env_map(svc.get("environment"), orig),
    }
    argv, entrypoint = _argv_of(orig, svc, build)
    installs, argv = split_installs(argv) if argv else ([], argv)
    mounts = _mounts(task, orig, svc.get("volumes") or [], compose)
    if mounts.binary:
        raise TaskFormatError(
            f"service {orig}: {sorted(k.removesuffix('.evalsi-b64') for k in mounts.binary)} "
            "are binary files, which a service cannot be given"
        )
    out: dict[str, Any] = {"name": name, "image": image, "command": argv}
    mb, millis = _limits(svc, orig)
    out["memory_mb"], out["cpu_millis"] = mb, millis
    if entrypoint:
        out["image_entrypoint"] = True
    if env:
        out["env"] = env
    x = svc.get("x-evalsi") or {}
    ports = _ports(svc, orig)
    if "port" in x:
        ports = [int(x["port"]), *[p for p in ports if p != int(x["port"])]]
    if ports:
        out["port"] = ports[0]
        if ports[1:]:
            out["ports"] = ports[1:]
    if ready := _healthcheck(svc, orig):
        out["ready_command"] = ready
    setup: list[str] = []
    if build:
        setup += prepared_setup(build)
        if build.workdir:
            out["workdir"] = build.workdir
        files = dict(build.files)
        if files:
            out["files"] = files
    setup_net = bool(build and build.setup) or bool(installs)
    setup += installs
    if setup:
        out["setup"] = setup
        if setup_net:
            out["setup_network"] = "allow"
    if svc.get("working_dir"):
        out["workdir"] = str(svc["working_dir"])
    if mounts.files:
        out["files"] = {**out.get("files", {}), **mounts.files}
    if mounts.executable and "setup" not in out:
        out["setup"] = []
    if mounts.executable:
        # Best effort: a file bound over a mounted volume keeps the mode it has on the host.
        quoted = " ".join(shlex.quote(p) for p in mounts.executable)
        out["setup"].append(f"chmod +x {quoted} 2>/dev/null || true")
        out["setup_network"] = out.get("setup_network", "deny")
    out["_vols"] = mounts.named
    out["depends_on"] = [names[d] for d in deps if d in names]
    out["_completed"] = [
        names[d] for d, c in deps.items() if c == "service_completed_successfully" and d in names
    ]
    out["aliases"] = sorted({a for a in [orig, *_hosts(svc, orig)] if a != name})
    return out


def _argv_of(name: str, svc: dict[str, Any], build: Build | None) -> tuple[list[str], bool]:
    """(the command, whether the image's ENTRYPOINT runs first), by Docker's rules: an
    entrypoint in Compose replaces the image's and drops its CMD; a command replaces CMD."""
    entry = _argv(svc.get("entrypoint"), "entrypoint", name)
    cmd = _argv(svc.get("command"), "command", name)
    if "entrypoint" in svc and not entry:
        return (cmd if cmd is not None else (build.cmd if build and build.cmd else []), False)
    if entry:
        return [*entry, *(cmd or [])], False
    if build and build.entrypoint:
        return [*build.entrypoint, *(cmd if cmd is not None else (build.cmd or []))], False
    return (cmd if cmd is not None else (build.cmd if build and build.cmd else [])), True


def _link(
    compose: Compose,
    services: dict[str, dict[str, Any]],
    names: dict[str, str],
    client_name: str,
) -> None:
    """Resolve what needs the whole file: one-shot services, aliases, and volumes shared with
    the client's workdir (finished by :meth:`Compose.finalize`, which knows the workdir)."""
    by_name = {s["name"]: s for s in compose.services}
    for s in compose.services:
        for dep in s.pop("_completed"):
            by_name[dep]["oneshot"] = True
    # The client's names (extra_hosts, links, echo >> /etc/hosts) hang on the first service:
    # every name maps to the one loopback.
    if compose.aliases and compose.services:
        first = compose.services[0]
        first["aliases"] = sorted({*first["aliases"], *compose.aliases})
        compose.aliases = []
    taken: dict[str, str] = {}
    for s in compose.services:
        clean = []
        for raw_alias in s["aliases"]:
            alias = raw_alias.lower()
            if not _DNS_NAME.match(alias):
                raise TaskFormatError(f"service {s['name']}: {alias!r} is not a usable host name")
            if alias in taken and taken[alias] != s["name"]:
                raise TaskFormatError(
                    f"the host name {alias} is used by {taken[alias]} and {s['name']}"
                )
            taken[alias] = s["name"]
            if alias not in names.values():
                clean.append(alias)
        s["aliases"] = clean
        if not clean:
            del s["aliases"]
        if not s["depends_on"]:
            del s["depends_on"]
    for s in compose.services:
        if s.get("oneshot"):
            for key in ("port", "ports", "ready_command"):
                s.pop(key, None)


def _unique_ports(compose: Compose) -> None:
    seen: dict[int, str] = {}
    for s in compose.services:
        for port in [s.get("port", 0), *s.get("ports", [])]:
            if port and port in seen:
                raise TaskFormatError(
                    f"services {seen[port]} and {s['name']} both listen on port {port}; "
                    "services share one loopback, so each needs its own port"
                )
            if port:
                seen[port] = s["name"]

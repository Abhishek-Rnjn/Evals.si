"""Local installation and execution-capability diagnostics; no credentials or API calls."""

from __future__ import annotations

import argparse
import importlib.metadata
import importlib.util
import json
import os
import platform
import re
import subprocess
from dataclasses import asdict, dataclass
from typing import Any, Literal

from evalsi._binaries import evalsid_path
from evalsi._version import __version__

_LEVELS = ("confined", "namespaced", "kernel", "vm")


@dataclass
class Check:
    name: str
    status: Literal["ok", "warning", "error"]
    detail: str
    remedy: str = ""


def add_doctor_command(sub: Any) -> None:
    p = sub.add_parser("doctor", help="check this installation and its execution capabilities")
    p.add_argument("--format", choices=["table", "json"], default="table")
    p.add_argument(
        "--require",
        choices=["sdk", "server", "agent"],
        default="sdk",
        help="check only the capabilities needed for sdk, server or agent (default: sdk)",
    )
    p.add_argument(
        "--all", action="store_true", help="also inspect optional execution capabilities"
    )
    p.set_defaults(func=_cmd_doctor)


def installed_version(name: str) -> str | None:
    try:
        return importlib.metadata.version(name)
    except importlib.metadata.PackageNotFoundError:
        return None


def diagnose(required: str, *, include_optional: bool = False) -> list[Check]:
    checks = [
        Check(
            "python",
            "ok",
            f"{platform.python_version()} on {platform.system()} {platform.machine()}",
        ),
        Check("evalsi", "ok", __version__),
    ]
    needs_server, needs_agent = required in ("server", "agent"), required == "agent"
    if not needs_server and not include_optional:
        return checks

    def missing(name: str, detail: str, remedy: str, needed: bool) -> None:
        checks.append(Check(name, "error" if needed else "warning", detail, remedy))

    if needs_agent or include_optional:
        harness = installed_version("evalsi-harness")
        if harness == __version__:
            checks.append(Check("harness", "ok", harness))
        else:
            missing(
                "harness",
                "not installed" if harness is None else f"{harness}; SDK is {__version__}",
                "For a development install, sync the matching Python workspace."
                if ".dev" in __version__
                else f"python -m pip install 'evalsi-harness=={__version__}'",
                needs_agent,
            )
    if all(importlib.util.find_spec(name) is not None for name in ("grpc", "grpc_health")):
        checks.append(Check("worker", "ok", "gRPC worker dependencies installed"))
    else:
        missing(
            "worker",
            "gRPC worker dependencies missing",
            "For a development install, sync the matching Python workspace."
            if ".dev" in __version__
            else f"python -m pip install 'evalsi[server]=={__version__}'",
            needs_server,
        )
    binary = evalsid_path()
    if not binary:
        missing(
            "server",
            "evalsid not found",
            "Build a matching evalsid from this checkout and set EVALSID to its absolute path."
            if ".dev" in __version__
            else "Install the matching platform wheel, or set EVALSID to a matching executable.",
            needs_server,
        )
        if needs_agent or include_optional:
            missing(
                "sandbox",
                "not probed: no evalsid",
                "Use a supported Linux host/container for sandboxed execution.",
                needs_agent,
            )
        return checks
    try:
        version = subprocess.run(
            [binary, "version"], capture_output=True, text=True, timeout=10, check=False
        )
    except (OSError, subprocess.TimeoutExpired):
        missing(
            "server",
            "evalsid execution failed/timed out",
            "Check EVALSID and reinstall the matching platform wheel.",
            needs_server,
        )
        return checks
    match = re.fullmatch(r"evalsid (\S+) \(api evalsi\.v1alpha1\)", version.stdout.strip())
    if version.returncode != 0 or match is None:
        missing(
            "server",
            "evalsid did not return a recognized version",
            "Check EVALSID and reinstall the matching platform wheel.",
            needs_server,
        )
        return checks
    if match[1] != __version__:
        missing(
            "server",
            f"evalsid {match[1]}; SDK is {__version__}",
            "Use matching SDK, harness and server versions.",
            needs_server,
        )
    else:
        checks.append(Check("server", "ok", f"evalsid {match[1]}"))
    if not needs_agent and not include_optional:
        return checks
    try:
        probe_command = [binary, "sandbox", "probe"]
        if config := os.environ.get("EVALSI_SANDBOX_CONFIG"):
            probe_command += ["--config", config]
        probe = subprocess.run(
            probe_command, capture_output=True, text=True, timeout=15, check=False
        )
        rungs = json.loads(probe.stdout)
        if probe.returncode != 0 or not isinstance(rungs, list):
            raise ValueError("invalid probe")
        available = [
            r
            for r in rungs
            if isinstance(r, dict)
            and r.get("available") is True
            and r.get("driver")
            and (not needs_agent or r.get("level") in ("namespaced", "kernel", "vm"))
        ]
        if available:
            # Use known backend names only; probe text may contain private addresses.
            # With its isolation level, so a plain check that lists landlock and an
            # agent check that does not (confined is too weak for agents) read consistently.
            drivers = [
                (
                    r["driver"]
                    if r["driver"] in ("bwrap", "landlock", "pod", "firecracker")
                    else "configured backend"
                )
                + (f" ({r['level']})" if r.get("level") in _LEVELS else "")
                for r in available
            ]
            checks.append(Check("sandbox", "ok", ", ".join(drivers)))
        else:
            missing(
                "sandbox",
                "no namespaced-or-stronger sandbox backend is available"
                if needs_agent
                else "no sandbox backend is available",
                "Use a Linux host/container with a supported sandbox. "
                "No unconfined fallback is used.",
                needs_agent,
            )
        if any(isinstance(r, dict) and r.get("warning") for r in rungs):
            checks.append(
                Check(
                    "sandbox-enforcement",
                    "warning",
                    "A sandbox backend reported an enforcement warning; availability does not "
                    "guarantee full isolation.",
                    "Review evalsid sandbox probe with the same configuration "
                    "in a private terminal; "
                    "correct host/cluster enforcement before relying on its isolation guarantees.",
                )
            )
    except (OSError, subprocess.TimeoutExpired, ValueError):
        missing(
            "sandbox",
            "sandbox probe failed/timed out",
            "Check host capabilities; run evalsid sandbox probe directly for details.",
            needs_agent,
        )
    return checks


def _cmd_doctor(args: argparse.Namespace) -> int:
    checks = diagnose(args.require, include_optional=args.all)
    ready = not any(c.status == "error" for c in checks)
    if args.format == "json":
        print(
            json.dumps(
                {"required": args.require, "ready": ready, "checks": [asdict(c) for c in checks]},
                indent=2,
            )
        )
    else:
        for c in checks:
            print(f"{c.status.upper():7} {c.name}: {c.detail}")
            if c.remedy:
                print(f"        {c.remedy}")
        print(f"\n{args.require} capabilities: {'ready' if ready else 'not ready'}")
        if args.require == "sdk" and not args.all:
            print(
                "Optional execution checks were not run. Use 'evalsi doctor --all' to inspect them."
            )
    return 0 if ready else 1

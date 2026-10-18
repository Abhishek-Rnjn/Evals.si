"""Inspect verification prerequisites without treating missing hardware as a pass."""

from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import stat
import subprocess
import time
from pathlib import Path
from typing import Any


def kvm_probe(device: str = "/dev/kvm") -> dict[str, Any]:
    if platform.system() != "Linux":
        return {"ready": False, "reason": "KVM requires Linux"}
    import fcntl

    try:
        fd = os.open(device, os.O_RDWR | os.O_CLOEXEC)
        try:
            if not stat.S_ISCHR(os.fstat(fd).st_mode):
                return {"ready": False, "reason": "KVM path is not a character device"}
            version = fcntl.ioctl(fd, 0xAE00, 0)  # KVM_GET_API_VERSION
            if version != 12:
                return {"ready": False, "reason": f"unsupported KVM API {version}"}
            vm = fcntl.ioctl(
                fd, 0xAE01, 0
            )  # KVM_CREATE_VM: prove access, not just presence
            os.close(vm)
            return {"ready": True, "api_version": version, "vm_created": True}
        finally:
            os.close(fd)
    except OSError as exc:
        return {"ready": False, "reason": f"{type(exc).__name__}: {exc.strerror}"}


def command_probe(
    argv: list[str], *, timeout: int = 15, env: Any = None
) -> dict[str, Any]:
    if shutil.which(argv[0]) is None:
        return {"ready": False, "reason": f"{argv[0]} is not installed"}
    try:
        result = subprocess.run(
            argv, capture_output=True, text=True, timeout=timeout, env=env, check=False
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"ready": False, "reason": type(exc).__name__}
    # Commands here only print versions, device models, or fixed probe tokens.
    detail = (result.stdout or result.stderr).strip().splitlines()
    return {
        "ready": result.returncode == 0,
        "exit_code": result.returncode,
        "detail": detail[0][:300] if detail else "",
    }


def inspect() -> dict[str, Any]:
    docker_env = dict(os.environ)
    for name in (
        "DOCKER_HOST",
        "DOCKER_CONTEXT",
        "DOCKER_TLS",
        "DOCKER_TLS_VERIFY",
        "DOCKER_CERT_PATH",
    ):
        docker_env.pop(name, None)
    checks = {
        "kvm": kvm_probe(),
        "firecracker_binary": command_probe(["firecracker", "--version"]),
        "bubblewrap": command_probe(
            ["bwrap", "--unshare-all", "--ro-bind", "/", "/", "/bin/true"]
        ),
        "docker": command_probe(
            [
                "docker",
                "--host=unix:///var/run/docker.sock",
                "info",
                "--format",
                "{{.ServerVersion}} {{.OSType}} {{.Architecture}}",
            ],
            env=docker_env,
        ),
        "gpu": command_probe(
            ["nvidia-smi", "--query-gpu=name", "--format=csv,noheader"]
        ),
        "go": command_probe(["go", "version"]),
        "helm": command_probe(["helm", "version", "--short"]),
        "kubectl": {"ready": shutil.which("kubectl") is not None},
        "cgroup_delegation": {
            "ready": os.access("/sys/fs/cgroup", os.W_OK),
            "scope": "writable mount only; enforcement needs a real child cgroup",
        },
    }
    # Do not inspect kubeconfigs, tokens, secret files, or credential values.
    credentials = {
        name: bool(os.environ.get(name))
        for name in (
            "LANGSMITH_API_KEY",
            "BRAINTRUST_API_KEY",
            "EVALSI_TEST_LANGSMITH_CREDENTIAL",
            "EVALSI_JUDGE_MODEL",
        )
    }
    return {
        "schema_version": 1,
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "platform": {
            "system": platform.system(),
            "architecture": platform.machine(),
            "python": platform.python_version(),
            "logical_cpus": os.cpu_count(),
        },
        "checks": checks,
        "configured_variables": credentials,
        "scope": "Prerequisites only. A ready prerequisite is not a passing product verification.",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path)
    parser.add_argument(
        "--require",
        nargs="*",
        default=[],
        help="Fail if any named prerequisite is unavailable",
    )
    args = parser.parse_args()
    result = inspect()
    unknown = set(args.require) - result["checks"].keys()
    if unknown:
        parser.error(f"unknown prerequisites: {sorted(unknown)}")
    result["unmet_requirements"] = [
        name for name in args.require if not result["checks"][name]["ready"]
    ]
    rendered = json.dumps(result, indent=2) + "\n"
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(rendered)
    print(rendered, end="")
    return 2 if result["unmet_requirements"] else 0


if __name__ == "__main__":
    raise SystemExit(main())

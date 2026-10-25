"""Actual OS write failures against an owned, unprivileged strict-audit daemon.

Run as root inside a disposable Linux container. No mounts, shared filesystems,
host settings or existing services are changed. Runtime EFBIG is not power-loss
or a read-only-remount certification.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import resource
import signal
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

from aks_followup_storage import live_children

BASE = "http://127.0.0.1:18085"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--binary", type=Path, default=Path("/work/build/evalsid"))
    args = parser.parse_args()
    if os.geteuid() != 0 or not hasattr(resource, "prlimit"):
        parser.error("requires root in a disposable Linux container, not the host")
    args.output.mkdir(parents=True)
    state = args.output / "state"
    state.mkdir()
    os.chown(state, 65534, 65534)
    token = "evk_" + os.urandom(32).hex()
    config = args.output / "config.json"
    config.write_text(
        json.dumps(
            {
                "listen": "127.0.0.1:18085",
                "data_dir": str(state),
                "worker": {"command": ["/work/venv/bin/python", "-m", "evalsi"]},
                "sinks": [],
                "auth": {
                    "api_keys": {
                        "keys": [
                            {
                                "name": "filesystem-owner",
                                "key": "sha256:" + hashlib.sha256(token.encode()).hexdigest(),
                                "roles": {"*": ["owner"]},
                            }
                        ]
                    }
                },
                "audit": {"mode": "strict"},
            }
        )
    )
    command = [str(args.binary), "serve", "--config", str(config)]
    env = dict(os.environ, HOME=str(state), TMPDIR=str(state))
    os.chmod(state, 0o555)
    refused = subprocess.run(
        command,
        user=65534,
        group=65534,
        extra_groups=[],
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    (args.output / "startup.log").write_text(refused.stdout + refused.stderr)
    if refused.returncode == 0 or "permission denied" not in refused.stderr.lower():
        raise AssertionError("unwritable data directory did not explicitly refuse startup")
    os.chmod(state, 0o755)

    def request(method: str, body: dict) -> tuple[int, dict]:
        req = urllib.request.Request(
            BASE + "/evalsi.v1alpha1.AuthService/" + method,
            data=json.dumps(body).encode(),
            headers={
                "content-type": "application/json",
                "authorization": "Bearer " + token,
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=30) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as exc:
            return exc.code, json.load(exc)

    with (args.output / "runtime.log").open("w") as log:
        previous = signal.signal(signal.SIGXFSZ, signal.SIG_IGN)
        try:
            process = subprocess.Popen(
                command,
                user=65534,
                group=65534,
                extra_groups=[],
                env=env,
                restore_signals=False,
                stdout=log,
                stderr=subprocess.STDOUT,
            )
        finally:
            signal.signal(signal.SIGXFSZ, previous)
        children = []
        try:
            for _ in range(120):
                if process.poll() is not None:
                    raise RuntimeError("audit daemon exited before readiness; inspect runtime.log")
                try:
                    status, _ = request("ListProjects", {})
                except urllib.error.URLError as exc:
                    if not isinstance(exc.reason, ConnectionRefusedError):
                        raise
                else:
                    if status != 200:
                        raise AssertionError(f"authenticated readiness returned HTTP {status}")
                    break
                time.sleep(0.5)
            else:
                raise TimeoutError("audit daemon did not become ready")
            if request("CreateProject", {"name": "before-write-fault"})[0] != 200:
                raise AssertionError("writable-filesystem positive control failed")
            resource.prlimit(process.pid, resource.RLIMIT_FSIZE, (0, 0))
            status, body = request("CreateProject", {"name": "after-write-fault"})
            if status != 503 or body.get("code") != "unavailable":
                raise AssertionError(f"strict write-fault request returned HTTP {status}: {body}")
            status, projects = request("ListProjects", {})
            if status != 200 or any(
                p["name"] == "after-write-fault" for p in projects.get("projects", [])
            ):
                raise AssertionError("refused mutation exists, or post-fault reads failed")
            result = {
                "daemon_uid": 65534,
                "unwritable_directory_startup_refused": True,
                "writable_positive_control": True,
                "runtime_fault": "RLIMIT_FSIZE=0 (OS EFBIG), SIGXFSZ ignored",
                "strict_mutation_http": 503,
                "mutation_absent": True,
                "daemon_alive": process.poll() is None,
                "read_only_remount_tested": False,
                "power_loss_tested": False,
            }
            (args.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
            print(json.dumps(result), flush=True)
        finally:
            children = live_children(process.pid)
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=30)
            for pid in children:
                try:
                    os.kill(pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass


if __name__ == "__main__":
    main()

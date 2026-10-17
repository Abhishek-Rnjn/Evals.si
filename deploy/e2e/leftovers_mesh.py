"""Observe ambient policy leakage and verify an explicit mesh opt-out.

Requires the dedicated evalsi-ambient namespace/pool and a local kind cluster.
Recreates only that test pool after updating its trial and pool labels.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import yaml

from evalsi.sandbox import SandboxError
from evalsi.sandbox.client import SandboxClient, SandboxSpec

NS = "evalsi-ambient"


def kube(*args: str, data: bytes | None = None) -> bytes:
    return subprocess.check_output(["kubectl", "-n", NS, *args], input=data)


def configure_opt_out(pod: dict[str, Any]) -> None:
    cm = json.loads(kube("get", "configmap", "pool", "-o", "json"))
    cfg = yaml.safe_load(cm["data"]["evalsi.yaml"])
    cfg["sandbox"]["pod"]["labels"] = {"istio.io/dataplane-mode": "none"}
    cm["data"]["evalsi.yaml"] = yaml.safe_dump(cfg)
    kube("replace", "-f", "-", data=json.dumps(cm).encode())
    # The pool and its trial pods must both avoid ambient interception.
    pod["metadata"] = {
        "name": "pool",
        "namespace": NS,
        "labels": {"app": "pool", "istio.io/dataplane-mode": "none"},
    }
    pod.pop("status", None)
    kube("delete", "pod", "pool", "--wait=true")
    kube("create", "-f", "-", data=json.dumps(pod).encode())
    kube("wait", "--for=condition=Ready", "pod/pool", "--timeout=90s")


async def check() -> dict[str, Any]:
    from google.protobuf.json_format import ParseDict
    from services import ROOT
    from services_lifecycle import db_spec

    from evalsi.types import Content, Record
    from evalsi.v1alpha1 import run_pb2
    from evalsi_harness.task import environment_for

    spec = ParseDict(
        yaml.safe_load((ROOT / "examples/multi-service/run.yaml").read_text())["spec"],
        run_pb2.RunSpec(),
    )
    config = environment_for(spec, Record(id="mesh", input=Content(text="check")))
    assert config
    pod = json.loads(kube("get", "pod", "pool", "-o", "json"))
    assert pod["metadata"]["annotations"].get("ambient.istio.io/redirection") == "enabled"
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:
        probe = await client.probe()
        print("ambient probe: " + json.dumps(probe), flush=True)
        sandbox = await client.create(SandboxSpec(image=config.image, min_isolation="namespaced"))
        try:
            assert sandbox.isolation.enforcement == "partial", sandbox.isolation
            assert any("canary connection got through" in n for n in sandbox.isolation.notes), (
                sandbox.isolation
            )
        finally:
            await sandbox.destroy()
        try:
            rejected = await client.create(db_spec(config))
        except SandboxError as exc:
            if "enforced pod NetworkPolicy" not in str(exc):
                raise
        else:
            await rejected.destroy()
            raise AssertionError("unsafe ambient multi-service request was accepted")

    configure_opt_out(pod)
    forward = subprocess.Popen(
        ["kubectl", "-n", NS, "port-forward", "pod/pool", "17446:7443"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        env = {
            **os.environ,
            "EVALSI_SERVICES_NAMESPACE": NS,
            "EVALSI_SANDBOX_ADDR": "tls://localhost:17446",
        }
        for _ in range(60):
            try:
                async with SandboxClient(env["EVALSI_SANDBOX_ADDR"]) as client:
                    await asyncio.wait_for(client._channel.channel_ready(), 0.5)
                break
            except TimeoutError:
                assert forward.poll() is None
        else:
            raise AssertionError("pool forward never became ready")
        await asyncio.to_thread(
            subprocess.run,
            [sys.executable, str(ROOT / "deploy/e2e/services.py"), "--cases", "basic"],
            env=env,
            check=True,
            timeout=180,
        )
        current = json.loads(kube("get", "pod", "pool", "-o", "json"))
        assert current["metadata"]["labels"]["istio.io/dataplane-mode"] == "none"
        assert (
            current["metadata"].get("annotations", {}).get("ambient.istio.io/redirection")
            != "enabled"
        )
        return {
            "istio": "1.30.6",
            "cni": "Calico 3.30.3",
            "ambient_ipv6": False,
            "ambient_canary": "leak detected, partial isolation",
            "unsafe_service_request": "refused",
            "explicit_opt_out": "pool and sandbox pods",
            "opt_out_checks": [
                "readiness",
                "positive/negative grades",
                "fresh state",
                "denied cross-trial TCP",
                "crash and cleanup",
            ],
        }
    finally:
        forward.terminate()
        forward.wait(10)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    result = asyncio.run(check())
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))

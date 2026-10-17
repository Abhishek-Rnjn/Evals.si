"""Admission transport controls for the chart's dedicated leftovers operator.

Install the hostNetwork chart option for host-network mode. For strict mode,
install Istio, inject the operator and apply mesh-wide STRICT and the chart's
operator.istio.peerAuthentication policy first. Only fixture resources change.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import time
from pathlib import Path
from typing import Any

NS = "evalsi-leftovers"


def kube(*args: str, data: dict[str, Any] | None = None) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        ["kubectl", "-n", NS, *args],
        input=json.dumps(data).encode() if data else None,
        capture_output=True,
        check=False,
    )


def dry_run(invalid: bool = False) -> subprocess.CompletedProcess[bytes]:
    spec = {
        "dataset": {"inline": {"records": [{"id": "check", "output": {"text": "a"}}]}},
        "evaluators": [{"ref": "exact-match"}],
    }
    if invalid:
        spec["gates"] = [{"metric": "exact-match"}]
    resource = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "metadata": {
            "name": "verification-dry-run",
            "namespace": NS,
            "labels": {"evals.si/project": "default"},
        },
        "spec": spec,
    }
    return kube("create", "--dry-run=server", "-f", "-", data=resource)


def check(mode: str) -> dict[str, Any]:
    pods = json.loads(
        kube("get", "pod", "-l", "app.kubernetes.io/name=leftovers-operator", "-o", "json").stdout
    )["items"]
    assert len(pods) == 1
    pod = pods[0]
    assert dry_run().returncode == 0, "valid control was not admitted"
    invalid = dry_run(True)
    assert invalid.returncode != 0
    assert b"needs min or max" in invalid.stderr, invalid.stderr.decode()
    if mode == "host-network":
        assert pod["spec"]["hostNetwork"]
        assert pod["spec"]["dnsPolicy"] == "ClusterFirstWithHostNet"
        assert pod["status"]["podIP"] == pod["status"]["hostIP"]
        return {
            "mode": mode,
            "passed": ["node network", "valid admission", "invalid gate rejection"],
        }
    containers = pod["spec"]["containers"] + pod["spec"].get("initContainers", [])
    assert any(c["name"] == "istio-proxy" for c in containers)
    policy = json.loads(
        kube("get", "peerauthentication", "leftovers-operator-webhook", "-o", "json").stdout
    )
    removed = kube("delete", "peerauthentication", "leftovers-operator-webhook")
    assert removed.returncode == 0
    try:
        assert kube("rollout", "restart", "deployment/leftovers-operator").returncode == 0
        assert (
            kube("rollout", "status", "deployment/leftovers-operator", "--timeout=90s").returncode
            == 0
        )
        denied = dry_run()
        assert denied.returncode != 0, "STRICT accepted a request without the port exception"
        assert b"failed calling webhook" in denied.stderr, denied.stderr.decode()
    finally:
        assert kube("apply", "-f", "-", data=policy).returncode == 0
    for _ in range(20):
        if dry_run().returncode == 0:
            return {
                "mode": mode,
                "passed": [
                    "meshed operator",
                    "valid admission",
                    "invalid gate rejection",
                    "STRICT rejection without port exception",
                    "admission restored with chart exception",
                ],
            }
        time.sleep(0.5)
    raise AssertionError("restored port policy did not admit the valid request")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["host-network", "strict"], required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    result = check(args.mode)
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))

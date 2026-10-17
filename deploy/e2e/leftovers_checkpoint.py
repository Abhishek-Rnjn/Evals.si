"""Real CheckpointServer reconciliation; HTTP fixture replaces vLLM/GPU inference."""

from __future__ import annotations

import argparse
import json
import subprocess
import time
from pathlib import Path
from typing import Any

NS = "evalsi-leftovers"


def kube(*args: str, data: dict[str, Any] | None = None) -> dict[str, Any]:
    result = subprocess.check_output(
        ["kubectl", "-n", NS, *args, "-o", "json"],
        input=json.dumps(data).encode() if data else None,
    )
    return json.loads(result)


def check() -> dict[str, Any]:
    script = """from http.server import HTTPServer, BaseHTTPRequestHandler
import sys
assert "--served-model-name" in sys.argv
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"fixture")
HTTPServer(("0.0.0.0",8000),Handler).serve_forever()
"""
    resource = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "CheckpointServer",
        "metadata": {"name": "verify-checkpoint", "namespace": NS},
        "spec": {
            "image": "python:3.13-slim",
            "model": "synthetic-checkpoint",
            "servedModelName": "verification-model",
            "command": ["python", "-u", "-c", script],
            "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}},
        },
    }
    kube("apply", "-f", "-", data=resource)
    try:
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            current = kube("get", "checkpointserver", "verify-checkpoint")
            if any(
                c["type"] == "Ready" and c["status"] == "True"
                for c in current.get("status", {}).get("conditions", [])
            ):
                break
            time.sleep(1)
        else:
            raise AssertionError(current)
        endpoint = current["status"]["endpoint"]
        assert endpoint == "http://leftovers-ckpt-verify-checkpoint.evalsi-leftovers.svc:8000/v1", (
            endpoint
        )
        dep = kube("get", "deployment", "leftovers-ckpt-verify-checkpoint")
        assert dep["metadata"]["ownerReferences"][0]["uid"] == current["metadata"]["uid"]
        assert dep["spec"]["strategy"]["type"] == "Recreate"
        assert dep["spec"]["template"]["spec"]["automountServiceAccountToken"] is False
        kube(
            "patch",
            "checkpointserver",
            "verify-checkpoint",
            "--type=merge",
            "-p",
            '{"spec":{"replicas":0}}',
        )
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            current = kube("get", "checkpointserver", "verify-checkpoint")
            if any(
                c["type"] == "Ready"
                and c["status"] == "False"
                and c["observedGeneration"] == current["metadata"]["generation"]
                for c in current.get("status", {}).get("conditions", [])
            ):
                break
            time.sleep(0.5)
        else:
            raise AssertionError("scale-to-zero kept Ready status")
        return {
            "controller": "real Kubernetes/operator",
            "serving": "HTTP fixture",
            "gpu_vllm": "not verified",
            "passed": [
                "deployment/service creation",
                "owner references",
                "ready endpoint",
                "Recreate strategy",
                "no service-account token",
                "scale-to-zero readiness",
            ],
        }
    finally:
        subprocess.check_call(
            ["kubectl", "-n", NS, "delete", "checkpointserver", "verify-checkpoint", "--wait=true"]
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    result = check()
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print("PASS " + json.dumps(result))

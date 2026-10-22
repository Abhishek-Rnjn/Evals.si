"""Namespace-only capability probes; never print node names or cluster addresses."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import time
from pathlib import Path
from typing import TypedDict

NS = "evalsi-verify-20261021-followup"
IMAGE = "docker.io/library/python@sha256:cf50a4f6ac4f026458b08d5222023d700ac806410dff5e9ca8728fa44f2b850f"
KUBECONFIG = os.environ.get("KUBECONFIG") or str(
    Path(__file__).resolve().parents[2] / "kube-aks.yaml"
)
K = [shutil.which("kubectl") or "kubectl", "--kubeconfig", KUBECONFIG]


class ProbeResults(TypedDict):
    node_count: int
    network: dict[str, bool]
    kata: dict[str, int | str | None]
    kvm: dict[str, object]


def command(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run([*K, *args], capture_output=True, text=True, check=False)


def get(*args: str) -> dict:
    result = command(*args, "-o", "json")
    if result.returncode:
        verb = args[2] if args[0] == "-n" else args[0]
        raise RuntimeError(f"kubectl {verb} failed with exit {result.returncode}")
    return json.loads(result.stdout)


def apply(doc: dict) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [*K, "apply", "-f", "-"],
        input=json.dumps(doc),
        text=True,
        capture_output=True,
        check=False,
    )


def pod(name: str, script: str) -> dict:
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": name, "namespace": NS, "labels": {"probe": name}},
        "spec": {
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists", "effect": "NoSchedule"}],
            "containers": [
                {
                    "name": "probe",
                    "image": IMAGE,
                    "command": ["python", "-c", script],
                    "resources": {
                        "requests": {"cpu": "25m", "memory": "32Mi"},
                        "limits": {"cpu": "100m", "memory": "64Mi"},
                    },
                }
            ],
        },
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    nodes = get("get", "nodes")["items"]
    result: ProbeResults = {
        "node_count": len(nodes),
        "network": {},
        "kata": {},
        "kvm": {},
    }
    try:
        kata = get("get", "runtimeclass", "kata-vm-isolation")
        selector = kata.get("scheduling", {}).get("nodeSelector", {})
        result["kata"]["matching_nodes"] = sum(
            all(n["metadata"].get("labels", {}).get(k) == v for k, v in selector.items())
            for n in nodes
        )
        probe = pod("kata-probe", "print('KATA_EXECUTED')")
        probe["spec"]["runtimeClassName"] = "kata-vm-isolation"
        admitted = apply(probe)
        result["kata"]["admitted"] = admitted.returncode == 0
        if admitted.returncode == 0:
            time.sleep(20)
            state = get("-n", NS, "get", "pod", "kata-probe")["status"]
            result["kata"]["phase"] = state.get("phase")
            result["kata"]["scheduled"] = any(
                c["type"] == "PodScheduled" and c["status"] == "True"
                for c in state.get("conditions", [])
            )
            command("-n", NS, "delete", "pod", "kata-probe", "--wait=true")

        for doc in [
            pod(
                "network-canary",
                "from http.server import HTTPServer,SimpleHTTPRequestHandler; HTTPServer(('0.0.0.0',8000),SimpleHTTPRequestHandler).serve_forever()",
            ),
            pod("network-client", "import time; time.sleep(3600)"),
        ]:
            if apply(doc).returncode:
                raise RuntimeError("network probe pod was refused")
        ready = command(
            "-n",
            NS,
            "wait",
            "--for=condition=Ready",
            "pod/network-canary",
            "pod/network-client",
            "--timeout=120s",
        )
        if ready.returncode:
            raise RuntimeError("network probe pods did not become ready")
        ip = get("-n", NS, "get", "pod", "network-canary")["status"]["podIP"]
        script = f"import urllib.request; print(urllib.request.urlopen('http://{ip}:8000', timeout=5).status)"
        result["network"]["baseline_connected"] = (
            command("-n", NS, "exec", "network-client", "--", "python", "-c", script).returncode
            == 0
        )
        policy = {
            "apiVersion": "networking.k8s.io/v1",
            "kind": "NetworkPolicy",
            "metadata": {"name": "probe-deny", "namespace": NS},
            "spec": {
                "podSelector": {"matchLabels": {"probe": "network-client"}},
                "policyTypes": ["Egress"],
                "egress": [],
            },
        }
        if apply(policy).returncode:
            raise RuntimeError("network policy was refused")
        time.sleep(10)
        result["network"]["connected_after_deny"] = (
            command("-n", NS, "exec", "network-client", "--", "python", "-c", script).returncode
            == 0
        )
        command("-n", NS, "delete", "networkpolicy", "probe-deny")
        command("-n", NS, "delete", "pod", "network-client", "network-canary", "--wait=true")

        device = pod(
            "kvm-probe",
            "import os,fcntl,json,time; f=os.open('/dev/kvm',os.O_RDWR); v=fcntl.ioctl(f,0xAE00,0); m=fcntl.ioctl(f,0xAE01,0); os.close(m); os.close(f); print(json.dumps({'api':v,'vm_created':True}),flush=True); time.sleep(600)",
        )
        container = device["spec"]["containers"][0]
        container["securityContext"] = {"privileged": True}
        container["volumeMounts"] = [{"name": "kvm", "mountPath": "/dev/kvm"}]
        device["spec"]["volumes"] = [
            {"name": "kvm", "hostPath": {"path": "/dev/kvm", "type": "CharDevice"}}
        ]
        spec = device["spec"]
        daemon = {
            "apiVersion": "apps/v1",
            "kind": "DaemonSet",
            "metadata": {"name": "kvm-probe", "namespace": NS},
            "spec": {
                "selector": {"matchLabels": {"probe": "kvm-probe"}},
                "template": {"metadata": {"labels": {"probe": "kvm-probe"}}, "spec": spec},
            },
        }
        spec["restartPolicy"] = "Always"
        admitted = apply(daemon)
        result["kvm"]["admitted"] = admitted.returncode == 0
        if admitted.returncode == 0:
            time.sleep(45)
            pods = get("-n", NS, "get", "pods", "-l", "probe=kvm-probe")["items"]
            result["kvm"]["pods"] = len(pods)
            observations = []
            by_name = {n["metadata"]["name"]: n for n in nodes}
            for p in pods:
                log = command("-n", NS, "logs", p["metadata"]["name"])
                observations.append(
                    {
                        "sku": by_name.get(p["spec"].get("nodeName"), {})
                        .get("metadata", {})
                        .get("labels", {})
                        .get("node.kubernetes.io/instance-type", "unscheduled"),
                        "phase": p["status"].get("phase"),
                        "vm_created": log.returncode == 0 and '"vm_created": true' in log.stdout,
                    }
                )
            result["kvm"]["observations"] = observations
            command("-n", NS, "delete", "daemonset", "kvm-probe", "--wait=true")
    finally:
        command("-n", NS, "delete", "daemonset", "kvm-probe", "--ignore-not-found", "--wait=false")
        command("-n", NS, "delete", "networkpolicy", "probe-deny", "--ignore-not-found")
        command(
            "-n",
            NS,
            "delete",
            "pod",
            "kata-probe",
            "kvm-probe",
            "network-client",
            "network-canary",
            "--ignore-not-found",
            "--wait=false",
        )
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()

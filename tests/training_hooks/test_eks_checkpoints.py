"""Opt-in GPU/PVC checks against a current-source EKS checkpoint operator.

Run inside the cluster with kubectl, CheckpointServer permissions, a PVC
mounted at /checkpoints containing checkpoint-1/checkpoint-2 and adapter-3,
and one free GPU. The adapter is a synthetic zero-LoRA protocol control,
not a training-quality test.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import httpx
import pytest

from evalsi.cli import main
from evalsi.training.serving import LoRA, Operator, ServingError

pytestmark = pytest.mark.skipif(
    os.environ.get("EVALSI_EKS_CHECKPOINTS") != "1", reason="opt-in live EKS GPU/PVC check"
)


def _check_lora(url: str) -> None:
    with httpx.Client(timeout=90) as client:
        models = client.get(url + "/models")
        models.raise_for_status()
        assert models.json()["data"][0]["root"] == "/checkpoints/checkpoint-1"
        with LoRA(url).serve("/checkpoints/adapter-3", "eks-adapter") as served:
            models = client.get(url + "/models")
            models.raise_for_status()
            assert served.model in [m["id"] for m in models.json()["data"]]
            reply = client.post(
                url + "/chat/completions",
                json={
                    "model": served.model,
                    "messages": [{"role": "user", "content": "Say hello."}],
                    "max_tokens": 8,
                },
            )
            reply.raise_for_status()
            assert reply.json()["usage"]["completion_tokens"] > 0
        models = client.get(url + "/models")
        models.raise_for_status()
        assert "eks-adapter" not in [m["id"] for m in models.json()["data"]]
        reply = client.post(
            url + "/chat/completions",
            json={
                "model": "eks-adapter",
                "messages": [{"role": "user", "content": "hello"}],
                "max_tokens": 8,
            },
        )
        assert reply.status_code == 404
        with (
            pytest.raises(ServingError, match="did not load adapter"),
            LoRA(url).serve("/checkpoints/absent-adapter", "eks-missing"),
        ):
            pytest.fail("a missing adapter was accepted")


def test_checkpoint_operator_lifecycle(tmp_path: Path) -> None:
    namespace = os.environ["EVALSI_EKS_CHECKPOINT_NAMESPACE"]
    claim = os.environ["EVALSI_EKS_CHECKPOINT_CLAIM"]
    args = [
        "--max-model-len",
        "2048",
        "--gpu-memory-utilization",
        "0.2",
        "--max-num-seqs",
        "2",
        "--enforce-eager",
    ]
    op = Operator(
        namespace,
        "vllm/vllm-openai:v0.11.0",
        claim=claim,
        args=args,
        resources={"nvidia.com/gpu": "1", "cpu": "1", "memory": "12Gi"},
        timeout_s=420,
    )

    def drain() -> None:
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            children = json.loads(op._kubectl("get", "deployments,services", "-o", "json"))["items"]
            if not any(
                item["metadata"]
                .get("labels", {})
                .get("app.kubernetes.io/instance", "")
                .startswith("eks-")
                for item in children
            ):
                return
            time.sleep(1)
        pytest.fail("owned checkpoint children were not garbage-collected")

    spec = tmp_path / "run.json"
    spec.write_text(
        json.dumps(
            {
                "apiVersion": "evals.si/v1alpha1",
                "kind": "EvalRun",
                "metadata": {"name": "checkpoint-control"},
                "spec": {
                    "target": {
                        "connector": "openai-compatible",
                        "model": "replaced-by-watcher",
                        "base_url": "http://unused",
                        "max_tokens": 16,
                    },
                    "dataset": {
                        "inline": {"records": [{"id": "hello", "input": {"text": "Say hello."}}]}
                    },
                    "evaluators": [{"ref": "regex-match", "params": {"pattern": "\\S"}}],
                    "gates": [{"metric": "regex-match", "min": 1}],
                },
            }
        )
    )
    common = ["-f", str(spec), "--log-dir", str(tmp_path / "results")]
    manifest = op.manifest("checkpoint-1", "eks-lora")
    assert isinstance(manifest["spec"], dict)
    manifest["spec"]["lora"] = True
    op._kubectl("apply", "-f", "-", stdin=json.dumps(manifest))
    try:
        op._kubectl("wait", "--for=condition=Ready", "checkpointserver/eks-lora", "--timeout=420s")
        resource = json.loads(op._kubectl("get", "checkpointserver/eks-lora", "-o", "json"))
        url = resource["status"]["endpoint"]
        _check_lora(url)
        assert (
            main(
                [
                    "checkpoints",
                    "watch",
                    "/checkpoints",
                    "--once",
                    "--settle",
                    "0",
                    "--serve",
                    "endpoint",
                    "--base-url",
                    url,
                    "--model",
                    "definitely-not-loaded",
                    "--training-run",
                    "eks-error",
                    *common,
                ]
            )
            == 1
        )
        failures = list((tmp_path / "results/eks-error").glob("step-*.json"))
        assert len(failures) == 2
        assert all(json.loads(path.read_text())["error"] for path in failures)
    finally:
        op._delete("checkpointserver/eks-lora")
        drain()

    watcher = [
        "checkpoints",
        "watch",
        "/checkpoints",
        "--once",
        "--settle",
        "0",
        "--serve",
        "operator",
        "--namespace",
        namespace,
        "--image",
        op.image,
        "--checkpoint-claim",
        claim,
        "--gpus",
        "1",
        "--training-run",
        "eks-watcher",
        *[f"--vllm-arg={arg}" for arg in args],
        *common,
    ]
    assert main(watcher) == 0
    results = sorted((tmp_path / "results/eks-watcher").glob("step-*.json"))
    assert len(results) == 2
    for path in results:
        data = json.loads(path.read_text())
        assert not data["error"]
        assert data["gates_passed"]
        assert data["summaries"]["regex-match"]["mean"] == 1.0
    drain()
    before = {path.name: path.read_bytes() for path in results}
    assert main(watcher) == 0
    assert {path.name: path.read_bytes() for path in results} == before
    assert not json.loads(op._kubectl("get", "checkpointservers", "-o", "json"))["items"]

    with (
        pytest.raises(RuntimeError, match="deliberate evaluation failure"),
        op.serve("checkpoint-1", "eks-exception"),
    ):
        raise RuntimeError("deliberate evaluation failure")
    assert not json.loads(op._kubectl("get", "checkpointservers", "-o", "json"))["items"]
    drain()

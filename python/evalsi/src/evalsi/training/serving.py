"""Serving a checkpoint for evaluation.

A serving strategy turns a checkpoint into an OpenAI-compatible endpoint
for the length of one evaluation:

- :class:`VLLM` starts ``vllm serve <checkpoint>`` on a free port, waits
  until it answers, and stops it afterwards (full checkpoints).
- :class:`LoRA` hot-loads the checkpoint as an adapter into a running vLLM
  (started with ``--enable-lora`` and ``VLLM_ALLOW_RUNTIME_LORA_UPDATING=True``)
  and unloads it afterwards, so one base-model server serves every step.
- :class:`Endpoint` uses a model that is already served.
- :class:`Operator` asks the Evals.si Kubernetes operator for a vLLM
  (a ``CheckpointServer``) per checkpoint, through ``kubectl``, and deletes it
  afterwards.
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import logging
import os
import re
import socket
import subprocess
import time
from collections.abc import Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import httpx

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class Served:
    base_url: str
    model: str


class Serving(Protocol):
    def serve(self, checkpoint: str, name: str) -> contextlib.AbstractContextManager[Served]: ...


class ServingError(RuntimeError):
    """The checkpoint could not be served; the evaluation is not run."""


def _headers(api_key_env: str) -> dict[str, str]:
    key = os.environ.get(api_key_env, "") if api_key_env else ""
    return {"Authorization": f"Bearer {key}"} if key else {}


@dataclass
class Endpoint:
    """A model that is already served; the checkpoint is ignored."""

    base_url: str
    model: str

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        yield Served(self.base_url, self.model)


@dataclass
class LoRA:
    """Load each checkpoint as a LoRA adapter into a running vLLM."""

    base_url: str
    api_key_env: str = ""
    timeout_s: float = 300.0

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        headers = _headers(self.api_key_env)
        root = self.base_url.rstrip("/")
        with httpx.Client(timeout=self.timeout_s, headers=headers) as client:
            response = client.post(
                f"{root}/load_lora_adapter", json={"lora_name": name, "lora_path": checkpoint}
            )
            if response.status_code >= 400:
                raise ServingError(
                    f"vLLM did not load adapter {checkpoint}: {response.status_code} "
                    f"{response.text[:300]} (is it running with --enable-lora and "
                    "VLLM_ALLOW_RUNTIME_LORA_UPDATING=True?)"
                )
            try:
                yield Served(self.base_url, name)
            finally:
                with contextlib.suppress(httpx.HTTPError):
                    client.post(f"{root}/unload_lora_adapter", json={"lora_name": name})


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


@dataclass
class VLLM:
    """Start ``vllm serve`` for each checkpoint and stop it afterwards."""

    command: Sequence[str] = ("vllm", "serve")
    args: Sequence[str] = ()
    host: str = "127.0.0.1"
    port: int = 0
    startup_timeout_s: float = 900.0
    env: dict[str, str] = field(default_factory=dict)
    log_dir: str = ""

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        port = self.port or free_port()
        argv = [
            *self.command,
            checkpoint,
            "--host",
            self.host,
            "--port",
            str(port),
            "--served-model-name",
            name,
            *self.args,
        ]
        log_path = Path(self.log_dir or ".") / f"vllm-{name}.log"
        log_path.parent.mkdir(parents=True, exist_ok=True)
        with log_path.open("wb") as log:
            try:
                process = subprocess.Popen(
                    argv, stdout=log, stderr=subprocess.STDOUT, env={**os.environ, **self.env}
                )
            except OSError as exc:
                raise ServingError(f"could not start {argv[0]}: {exc}") from exc
            base_url = f"http://{self.host}:{port}/v1"
            try:
                self._wait(process, base_url, log_path)
                yield Served(base_url, name)
            finally:
                process.terminate()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()

    def _wait(self, process: subprocess.Popen[bytes], base_url: str, log: Path) -> None:
        deadline = time.monotonic() + self.startup_timeout_s
        while time.monotonic() < deadline:
            if process.poll() is not None:
                tail = log.read_text(errors="replace")[-1500:]
                raise ServingError(f"vllm exited with {process.returncode}:\n{tail}")
            with contextlib.suppress(httpx.HTTPError):
                if httpx.get(f"{base_url}/models", timeout=5).status_code == 200:
                    return
            time.sleep(1)
        raise ServingError(f"vllm did not answer within {self.startup_timeout_s:g}s (log: {log})")


def resource_name(name: str) -> str:
    """A DNS-1123 label for a checkpoint's name (``checkpoint-200``, ``step_200``...),
    stable for the same name and distinct for names that differ only in characters
    a label cannot hold."""
    label = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")[:40].strip("-")
    if label == name:
        return label
    return f"{label or 'ckpt'}-{hashlib.sha256(name.encode()).hexdigest()[:8]}"


@dataclass
class Operator:
    """Serve each checkpoint as a ``CheckpointServer`` the operator reconciles.

    ``kubectl`` (its current context and ``namespace``) applies the resource,
    waits for it to be Ready and reads its endpoint from the status; the
    resource is deleted when the evaluation is done. The endpoint is a
    cluster-internal Service address, so the process that evaluates (the
    checkpoint watcher as a Job, or beside the trainer) must run in the cluster.
    A checkpoint is a path under ``checkpoint_root`` (where ``claim`` is mounted
    by the operator) or an absolute path.
    """

    namespace: str
    image: str
    claim: str = ""
    checkpoint_root: str = "/checkpoints"
    args: Sequence[str] = ()
    resources: dict[str, str] = field(default_factory=dict)
    api_key_secret: str = ""
    kubectl: str = "kubectl"
    timeout_s: float = 1800.0
    delete_attempts: int = 3
    delete_backoff_s: float = 2.0

    def _kubectl(self, *argv: str, stdin: str = "") -> str:
        command = [self.kubectl, "--namespace", self.namespace, *argv]
        try:
            done = subprocess.run(
                command,
                input=stdin,
                capture_output=True,
                text=True,
                check=False,
                timeout=self.timeout_s + 60,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise ServingError(f"{self.kubectl}: {exc}") from exc
        if done.returncode != 0:
            raise ServingError(f"{' '.join(command[:4])}...: {done.stderr.strip()[-500:]}")
        return done.stdout

    def manifest(self, checkpoint: str, name: str) -> dict[str, object]:
        model = checkpoint if checkpoint.startswith("/") else f"{self.checkpoint_root}/{checkpoint}"
        spec: dict[str, object] = {"image": self.image, "model": model, "servedModelName": name}
        if self.claim:
            spec["checkpointClaim"] = self.claim
        if self.args:
            spec["args"] = list(self.args)
        if self.resources:
            spec["resources"] = {"limits": dict(self.resources)}
        if self.api_key_secret:
            spec["apiKeySecret"] = self.api_key_secret
        return {
            "apiVersion": "evals.si/v1alpha1",
            "kind": "CheckpointServer",
            "metadata": {
                "name": resource_name(name),
                "labels": {"app.kubernetes.io/managed-by": "evalsi"},
            },
            "spec": spec,
        }

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        manifest = self.manifest(checkpoint, name)
        resource = f"checkpointservers.evals.si/{resource_name(name)}"
        self._kubectl("apply", "-f", "-", stdin=json.dumps(manifest))
        try:
            self._kubectl(
                "wait", "--for=condition=Ready", resource, f"--timeout={int(self.timeout_s)}s"
            )
            status = json.loads(self._kubectl("get", resource, "-o", "json")).get("status", {})
            endpoint = status.get("endpoint")
            if not endpoint:
                raise ServingError(f"{resource} is Ready but reports no endpoint")
            yield Served(endpoint, status.get("servedModelName") or name)
        except BaseException as exc:
            # The evaluation's own error stays the error; a server left behind
            # is said so on it and in the log.
            try:
                self._delete(resource)
            except ServingError as leak:
                exc.add_note(f"and the CheckpointServer was left running: {leak}")
                logger.error("checkpoint server not deleted: %s", leak)
            raise
        self._delete(resource)

    def _delete(self, resource: str) -> None:
        """Delete the resource, retrying: nothing else collects a server that
        is left behind, and it holds its GPUs until someone does."""
        for attempt in range(self.delete_attempts):
            try:
                self._kubectl("delete", resource, "--ignore-not-found", "--wait=false")
                return
            except ServingError as exc:
                if attempt == self.delete_attempts - 1:
                    raise ServingError(
                        f"could not delete {resource} after {self.delete_attempts} attempts "
                        f"(delete it by hand, it holds its GPUs): {exc}"
                    ) from exc
                time.sleep(self.delete_backoff_s * 2**attempt)

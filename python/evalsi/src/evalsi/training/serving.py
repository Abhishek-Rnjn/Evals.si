"""Serving a checkpoint for evaluation.

A serving strategy turns a checkpoint into an OpenAI-compatible endpoint
for the length of one evaluation:

- :class:`VLLM` starts ``vllm serve <checkpoint>`` on a free port, waits
  until it answers, and stops it afterwards (full checkpoints).
- :class:`LoRA` hot-loads the checkpoint as an adapter into a running vLLM
  (started with ``--enable-lora`` and ``VLLM_ALLOW_RUNTIME_LORA_UPDATING=True``)
  and unloads it afterwards, so one base-model server serves every step.
- :class:`Endpoint` uses a model that is already served.
"""

from __future__ import annotations

import contextlib
import os
import socket
import subprocess
import time
from collections.abc import Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import httpx


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

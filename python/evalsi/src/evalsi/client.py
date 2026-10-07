"""A small client for evalsid over the Connect protocol (HTTP/JSON).

It needs only ``httpx``: unary calls are JSON POSTs, and server-streaming
calls (``WatchRun``) use Connect's enveloped JSON framing, which works over
plain HTTP/1.1. gRPC clients can use the generated stubs instead.

Credentials: pass ``api_key=`` or ``token=``, or let the client find one
(``EVALSI_API_KEY``, ``EVALSI_TOKEN``, GitHub Actions OIDC, or the login
cached by ``evalsi login``); see :mod:`evalsi.auth`.
"""

from __future__ import annotations

import json
import struct
from collections.abc import Iterator
from typing import Any

import httpx

END_STREAM = 0x02


class ServerError(RuntimeError):
    def __init__(self, code: str, message: str) -> None:
        hint = ""
        if code == "unauthenticated":
            hint = " (sign in with `evalsi login`, or set EVALSI_API_KEY or EVALSI_TOKEN)"
        super().__init__(f"{code}: {message}{hint}")
        self.code = code
        self.message = message


class Client:
    def __init__(
        self,
        base_url: str,
        *,
        timeout: float = 60.0,
        transport: httpx.BaseTransport | None = None,
        token: str | None = None,
        api_key: str | None = None,
    ) -> None:
        from evalsi.auth import ServerAuth

        self.base_url = base_url.rstrip("/")
        self.auth = ServerAuth(self.base_url, token=token, api_key=api_key)
        self._http = httpx.Client(timeout=timeout, transport=transport, auth=self.auth)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def call(self, service: str, method: str, body: dict[str, Any]) -> dict[str, Any]:
        response = self._http.post(
            f"{self.base_url}/evalsi.v1alpha1.{service}/{method}",
            json=body,
            headers={"Connect-Protocol-Version": "1"},
        )
        if response.status_code != 200:
            raise _error(response.json() if response.content else {}, response.status_code)
        out: dict[str, Any] = response.json()
        return out

    def _run(self, method: str, body: dict[str, Any]) -> dict[str, Any]:
        run: dict[str, Any] = self.call("RunService", method, body)["run"]
        return run

    def stream(self, service: str, method: str, body: dict[str, Any]) -> Iterator[dict[str, Any]]:
        payload = json.dumps(body).encode()
        request = struct.pack(">BI", 0, len(payload)) + payload
        with self._http.stream(
            "POST",
            f"{self.base_url}/evalsi.v1alpha1.{service}/{method}",
            content=request,
            headers={"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
            timeout=None,
        ) as response:
            if response.status_code != 200:
                response.read()
                raise _error(response.json() if response.content else {}, response.status_code)
            buffer = b""
            for chunk in response.iter_bytes():
                buffer += chunk
                while len(buffer) >= 5:
                    flags, size = struct.unpack(">BI", buffer[:5])
                    if len(buffer) < 5 + size:
                        break
                    message = json.loads(buffer[5 : 5 + size] or b"{}")
                    buffer = buffer[5 + size :]
                    if flags & END_STREAM:
                        if "error" in message:
                            raise _error(message["error"], 200)
                        return
                    yield message

    # --- runs ---

    def create_run(
        self,
        spec: dict[str, Any],
        *,
        name: str = "",
        project: str = "",
        labels: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        body = {"name": name, "project": project, "spec": spec, "labels": labels or {}}
        return self._run("CreateRun", body)

    def get_run(self, run_id: str) -> dict[str, Any]:
        return self._run("GetRun", {"id": run_id})

    def list_runs(
        self, *, project: str = "", page_size: int = 100, page_token: str = ""
    ) -> dict[str, Any]:
        """One page of runs, newest first: ``{"runs": [...], "nextPageToken": ...}``."""
        body = {"project": project, "pageSize": page_size, "pageToken": page_token}
        return self.call("RunService", "ListRuns", body)

    def list_run_results(
        self, run_id: str, *, page_size: int = 1000, page_token: str = ""
    ) -> dict[str, Any]:
        """One page of a run's results with their records."""
        body = {"runId": run_id, "pageSize": page_size, "pageToken": page_token}
        return self.call("RunService", "ListRunResults", body)

    def watch_run(self, run_id: str, *, include_results: bool = False) -> Iterator[dict[str, Any]]:
        return self.stream(
            "RunService", "WatchRun", {"id": run_id, "includeResults": include_results}
        )

    def cancel_run(self, run_id: str) -> dict[str, Any]:
        return self._run("CancelRun", {"id": run_id})

    def resume_run(self, run_id: str) -> dict[str, Any]:
        return self._run("ResumeRun", {"id": run_id})

    def compare_runs(self, baseline: str, candidate: str) -> list[dict[str, Any]]:
        out = self.call(
            "RunService", "CompareRuns", {"baselineRunId": baseline, "candidateRunId": candidate}
        )
        comparisons: list[dict[str, Any]] = out.get("comparisons", [])
        return comparisons

    def promote_results(
        self, run_id: str, dataset: str, when: str, *, include_outputs: bool = False
    ) -> dict[str, Any]:
        body = {
            "runId": run_id,
            "dataset": dataset,
            "when": when,
            "includeOutputs": include_outputs,
        }
        return self.call("RunService", "PromoteResults", body)

    def create_shadow_replay(
        self,
        spec: dict[str, Any],
        *,
        name: str = "",
        project: str = "",
        labels: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        body = {"name": name, "project": project, "candidate": spec, "labels": labels or {}}
        return self.call("RunService", "CreateShadowReplay", body)

    # --- identity and access ---

    def auth_call(self, method: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
        return self.call("AuthService", method, body or {})

    def whoami(self) -> dict[str, Any]:
        return self.auth_call("WhoAmI")


def _error(body: dict[str, Any], status: int) -> ServerError:
    return ServerError(str(body.get("code", f"http_{status}")), str(body.get("message", "")))

"""A small client for evalsid over the Connect protocol (HTTP/JSON).

It needs only ``httpx``: unary calls are JSON POSTs, and server-streaming
calls (``WatchRun``) use Connect's enveloped JSON framing, which works over
plain HTTP/1.1. gRPC clients can use the generated stubs instead.

Credentials: pass ``api_key=`` or ``token=``, or let the client find one
(``EVALSI_API_KEY``, ``EVALSI_TOKEN``, GitHub Actions OIDC, or the login
cached by ``evalsi login``); see :mod:`evalsi.auth`.
"""

from __future__ import annotations

import asyncio
import json
import struct
from collections.abc import Iterable, Iterator, Mapping, Sequence
from pathlib import Path
from typing import TYPE_CHECKING, Any

import httpx

if TYPE_CHECKING:
    from evalsi.results import EvalResult, MetricSummary
    from evalsi.types import EvaluationResult, Record

END_STREAM = 0x02


class ServerError(RuntimeError):
    def __init__(self, code: str, message: str) -> None:
        hint = ""
        if code == "unauthenticated":
            hint = " (sign in with `evalsi login`, or set EVALSI_API_KEY or EVALSI_TOKEN)"
        super().__init__(f"{code}: {message}{hint}")
        self.code = code
        self.message = message


def import_run_body(
    result: EvalResult,
    *,
    name: str,
    project: str = "",
    labels: Mapping[str, str] | None = None,
    gates: Sequence[Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    """The ``ImportRun`` request for a result evaluated in this process."""
    from google.protobuf import json_format

    from evalsi.convert import record_to_proto, result_to_proto, summary_to_proto
    from evalsi.v1alpha1 import run_pb2

    manifest = result.manifest
    embedded = manifest.get("spec") or {}
    evaluators = [
        {"ref": e.get("ref", ""), "name": e.get("name", ""), "params": e.get("params") or {}}
        for e in manifest.get("evaluators") or []
    ]
    spec: dict[str, Any] = {
        # Params are recorded as text where JSON cannot hold them (a callable).
        "evaluators": json.loads(json.dumps(evaluators, default=str)),
        "gates": list(gates) if gates is not None else embedded.get("gates", []),
    }
    trials = int(manifest.get("trials") or embedded.get("trials") or 1)
    if trials > 1:
        spec["trials"] = trials
    summary = manifest.get("summary") or {}
    options = {
        "confidenceLevel": summary.get("confidence_level"),
        "clusterBy": summary.get("cluster_by"),
    }
    if summary.get("ci_method") == "bootstrap":
        options["ciMethod"] = "CI_METHOD_BOOTSTRAP"
    options = {k: v for k, v in options.items() if v not in (None, "")}
    if options:
        spec["summary"] = options
    try:
        checked = json_format.ParseDict(spec, run_pb2.RunSpec())
    except json_format.ParseError as exc:
        raise ValueError(f"the run cannot be stored: {exc}") from exc
    # A run's outputs repeat a record id once per trial; the server keeps one
    # record per id, and every trial's results.
    records: dict[str, Any] = {}
    for record in result.records:
        records.setdefault(record.id, record)
    body: dict[str, Any] = {
        "name": name,
        "project": project,
        "labels": dict(labels or {}),
        "spec": json_format.MessageToDict(checked),
        "records": [json_format.MessageToDict(record_to_proto(r)) for r in records.values()],
        "results": [json_format.MessageToDict(result_to_proto(r)) for r in result.results],
        "summaries": [json_format.MessageToDict(summary_to_proto(s)) for s in result.summaries],
    }
    for key, field in (("started_at", "startedAt"), ("finished_at", "finishedAt")):
        if manifest.get(key):
            body[field] = manifest[key]
    return body


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

    def call(
        self,
        service: str,
        method: str,
        body: dict[str, Any],
        *,
        timeout: float | None = None,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        headers = {"Connect-Protocol-Version": "1"}
        if idempotency_key is not None:
            headers["Idempotency-Key"] = idempotency_key
        response = self._http.post(
            f"{self.base_url}/evalsi.v1alpha1.{service}/{method}",
            json=body,
            headers=headers,
            timeout=httpx.USE_CLIENT_DEFAULT if timeout is None else timeout,
        )
        if response.status_code != 200:
            raise _error(_json_body(response), response.status_code)
        out: dict[str, Any] = _json_body(response, ok=True)
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
                raise _error(_json_body(response), response.status_code)
            buffer = b""
            for chunk in response.iter_bytes():
                buffer += chunk
                while len(buffer) >= 5:
                    flags, size = struct.unpack(">BI", buffer[:5])
                    if len(buffer) < 5 + size:
                        break
                    try:
                        message = json.loads(buffer[5 : 5 + size] or b"{}")
                    except ValueError as exc:
                        raise ServerError("malformed_stream", "stream frame is not JSON") from exc
                    buffer = buffer[5 + size :]
                    if flags & END_STREAM:
                        if "error" in message:
                            raise _error(message["error"], 200)
                        return
                    yield message
            if buffer:
                raise ServerError("truncated_stream", "stream ended inside a frame")
            raise ServerError("truncated_stream", "stream ended without an end-of-stream frame")

    # --- evaluation ---

    def evaluate(
        self,
        data: str | Path | Iterable[Mapping[str, Any] | Record],
        evaluators: Sequence[str | Mapping[str, Any]],
        *,
        mapping: Mapping[str, str] | None = None,
        limit: int | None = None,
        params: Mapping[str, Mapping[str, Any]] | None = None,
        judge: str = "",
        project: str = "",
        confidence: float = 0.95,
        cluster_by: str | None = None,
        ci_method: str = "auto",
        timeout: float | None = 600.0,
    ) -> EvalResult:
        """Score records on the cluster; the same result as in-process ``evalsi.evaluate()``.

        ``data`` and ``evaluators`` are as for :func:`evalsi.evaluate`, except that
        evaluators run on the server, so they are references (``"exact-match"``,
        ``{"ref": ..., "params": ..., "name": ...}``) to ones it has. ``judge`` names
        a judge configured on the server; there is no default. Scores, outcomes and
        intervals are computed by the server's own code, with the same evaluators.
        """
        from google.protobuf import json_format

        from evalsi.convert import record_to_proto
        from evalsi.v1alpha1 import evaluation_service_pb2 as pb

        records, refs = _prepare_evaluation(data, evaluators, mapping, limit, params)
        request = pb.EvaluateRequest(
            project=project,
            records=[record_to_proto(r) for r in records],
            evaluators=refs,
            judge=judge,
            summary=_summary_options(confidence, cluster_by, ci_method),
        )
        body = json_format.MessageToDict(request)
        reply = self.call("EvaluationService", "Evaluate", body, timeout=timeout)
        response = json_format.ParseDict(reply, pb.EvaluateResponse(), ignore_unknown_fields=True)
        return _assemble(
            self.base_url,
            records,
            refs,
            list(response.results),
            list(response.summaries),
            confidence=confidence,
            cluster_by=cluster_by,
            ci_method=ci_method,
        )

    async def evaluate_async(
        self,
        data: str | Path | Iterable[Mapping[str, Any] | Record],
        evaluators: Sequence[str | Mapping[str, Any]],
        **options: Any,
    ) -> EvalResult:
        """:meth:`evaluate` without blocking the event loop."""
        return await asyncio.to_thread(self.evaluate, data, evaluators, **options)

    def evaluate_stream(
        self,
        data: str | Path | Iterable[Mapping[str, Any] | Record],
        evaluators: Sequence[str | Mapping[str, Any]],
        *,
        mapping: Mapping[str, str] | None = None,
        limit: int | None = None,
        params: Mapping[str, Mapping[str, Any]] | None = None,
        judge: str = "",
        project: str = "",
        confidence: float = 0.95,
        cluster_by: str | None = None,
        ci_method: str = "auto",
        timeout: float | None = None,
    ) -> EvaluationStream:
        """Score records as they arrive: iterate for results as they finish.

        Streaming is bidirectional, which the server speaks over gRPC, so this
        needs ``grpcio`` (``pip install 'evalsi[grpc]'``). After the iteration
        ends, :attr:`EvaluationStream.summaries` holds the summaries, and
        :meth:`EvaluationStream.collect` the same :class:`~evalsi.EvalResult` that
        :meth:`evaluate` returns.
        """
        from evalsi.convert import record_to_proto
        from evalsi.v1alpha1 import evaluation_service_pb2 as pb

        records, refs = _prepare_evaluation(data, evaluators, mapping, limit, params)
        config = pb.EvaluateStreamConfig(
            project=project,
            evaluators=refs,
            judge=judge,
            summary=_summary_options(confidence, cluster_by, ci_method),
        )

        def requests() -> Iterator[Any]:
            yield pb.EvaluateStreamRequest(config=config)
            for record in records:
                yield pb.EvaluateStreamRequest(record=record_to_proto(record))

        return EvaluationStream(
            self,
            requests(),
            records,
            refs,
            timeout=timeout,
            confidence=confidence,
            cluster_by=cluster_by,
            ci_method=ci_method,
        )

    # --- runs ---

    def create_run(
        self,
        spec: dict[str, Any],
        *,
        name: str = "",
        project: str = "",
        labels: dict[str, str] | None = None,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        body = {"name": name, "project": project, "spec": spec, "labels": labels or {}}
        run: dict[str, Any] = self.call(
            "RunService", "CreateRun", body, idempotency_key=idempotency_key
        )["run"]
        return run

    def log_run(
        self,
        result: EvalResult,
        *,
        name: str,
        project: str = "",
        labels: dict[str, str] | None = None,
        gates: Sequence[Mapping[str, Any]] | None = None,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        """Store a result evaluated in this process (``evalsi.evaluate()``, an
        embedded run, a training loop's checkpoint evaluation) as a finished
        run on the server, beside its own runs: it lists, reports, compares
        and fires webhooks like them. Nothing is evaluated again. ``gates``
        (``{"metric": ..., "min": ...}``) are checked by the server against the
        summaries; an embedded run's own gates are used when not given.
        Returns the stored run, whose status is failed when a gate failed."""
        body = import_run_body(result, name=name, project=project, labels=labels, gates=gates)
        run: dict[str, Any] = self.call(
            "RunService", "ImportRun", body, idempotency_key=idempotency_key
        )["run"]
        return run

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

    def archive_run(self, run_id: str) -> dict[str, Any]:
        """Drop a finished run's records and results, keeping its summary."""
        out: dict[str, Any] = self.call("RunService", "ArchiveRun", {"id": run_id})
        run: dict[str, Any] = out.get("run", {})
        return run

    def delete_run(self, run_id: str, *, idempotency_key: str | None = None) -> None:
        self.call("RunService", "DeleteRun", {"id": run_id}, idempotency_key=idempotency_key)

    def upload_dataset(
        self, file: str, *, name: str, project: str, format: str | None = None
    ) -> dict[str, Any]:
        from pathlib import Path
        from urllib.parse import quote

        source = Path(file)
        encoding = format or source.suffix.removeprefix(".")
        with source.open("rb") as reader:
            response = self._http.post(
                f"{self.base_url}/v1alpha1/datasets/{quote(name, safe='')}/upload",
                params={"project": project, "format": encoding},
                content=iter(lambda: reader.read(256 * 1024), b""),
                headers={"Content-Type": "application/octet-stream"},
            )
        if response.status_code != 201:
            raise ServerError(str(response.status_code), response.text.strip())
        result: dict[str, Any] = response.json()
        return result

    def import_dataset(
        self,
        uri: str,
        *,
        name: str,
        project: str,
        format: str,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        return self.call(
            "DatasetService",
            "ImportDataset",
            {"config": {"name": name, "project": project, "format": format}, "uri": uri},
            idempotency_key=idempotency_key,
        )

    def compare_runs(self, baseline: str, candidate: str) -> list[dict[str, Any]]:
        out = self.call(
            "RunService", "CompareRuns", {"baselineRunId": baseline, "candidateRunId": candidate}
        )
        comparisons: list[dict[str, Any]] = out.get("comparisons", [])
        return comparisons

    def slice_runs(
        self,
        metric: str,
        by: str,
        *,
        project: str = "",
        labels: dict[str, str] | None = None,
        run_ids: list[str] | None = None,
        limit: int = 0,
        level: float = 0.95,
    ) -> dict[str, Any]:
        """A metric per run and per value of a record metadata field, computed
        on the server (``RunService.SliceRuns``): ``{"rows": [...], "runs": n}``."""
        body: dict[str, Any] = {
            "metric": metric,
            "by": by,
            "project": project,
            "labels": labels or {},
            "runIds": run_ids or [],
            "limit": limit,
            "confidenceLevel": level,
        }
        return self.call("RunService", "SliceRuns", body)

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

    # --- release evidence and immutable decisions ---

    def inspect_release_policy(self, policy: dict[str, Any]) -> str:
        result = self.call("ReleaseService", "InspectReleasePolicy", {"policy": policy})
        digest: str = result["sha256"]
        return digest

    def release_run_evidence(self, run_id: str) -> dict[str, Any]:
        result = self.call("ReleaseService", "GetReleaseRunEvidence", {"runId": run_id})
        evidence: dict[str, Any] = result["evidence"]
        return evidence

    def create_release_decision(
        self,
        decision_id: str,
        *,
        project: str,
        policy: dict[str, Any],
        policy_sha256: str,
        baseline: dict[str, str],
        candidate: dict[str, str],
    ) -> dict[str, Any]:
        result = self.call(
            "ReleaseService",
            "CreateReleaseDecision",
            {
                "id": decision_id,
                "project": project,
                "policy": policy,
                "policySha256": policy_sha256,
                "baseline": baseline,
                "candidate": candidate,
            },
        )
        decision: dict[str, Any] = result["decision"]
        return decision

    def get_release_decision(self, decision_id: str, *, project: str = "") -> dict[str, Any]:
        result = self.call(
            "ReleaseService", "GetReleaseDecision", {"id": decision_id, "project": project}
        )
        decision: dict[str, Any] = result["decision"]
        return decision

    def list_release_decisions(
        self, *, project: str = "", page_size: int = 20, page_token: str = ""
    ) -> dict[str, Any]:
        return self.call(
            "ReleaseService",
            "ListReleaseDecisions",
            {
                "project": project,
                "pageSize": page_size,
                "pageToken": page_token,
            },
        )

    # --- shared dataset catalog ---

    def publish_dataset_version(
        self,
        content: dict[str, Any],
        *,
        failures: dict[str, int] | None = None,
    ) -> dict[str, Any]:
        """Publish an immutable draft from wire records or approved failure revisions."""
        return self.call(
            "DatasetCatalogService",
            "PublishDatasetVersion",
            {
                "content": content,
                "failures": [
                    {"id": key, "revision": str(revision)}
                    for key, revision in (failures or {}).items()
                ],
            },
        )

    def list_dataset_versions(
        self,
        *,
        project: str = "",
        name: str = "",
        state: str = "",
        owner: str = "",
        page_size: int = 20,
        page_token: str = "",
    ) -> dict[str, Any]:
        body: dict[str, Any] = {
            "project": project,
            "name": name,
            "owner": owner,
            "pageSize": page_size,
            "pageToken": page_token,
        }
        if state:
            body["state"] = state
        return self.call("DatasetCatalogService", "ListDatasetVersions", body)

    def get_dataset_version(self, name: str, version: str, *, project: str = "") -> dict[str, Any]:
        return self.call(
            "DatasetCatalogService",
            "GetDatasetVersion",
            {"project": project, "name": name, "version": version},
        )

    def review_dataset_version(
        self,
        name: str,
        version: str,
        *,
        sha256: str,
        expected_revision: int,
        decision: str,
        note: str,
        project: str = "",
    ) -> dict[str, Any]:
        return self.call(
            "DatasetCatalogService",
            "ReviewDatasetVersion",
            {
                "project": project,
                "name": name,
                "version": version,
                "sha256": sha256,
                "expectedRevision": str(expected_revision),
                "decision": decision,
                "note": note,
            },
        )

    def dataset_version_events(
        self,
        name: str,
        version: str,
        *,
        project: str = "",
        page_size: int = 20,
        page_token: str = "",
    ) -> dict[str, Any]:
        return self.call(
            "DatasetCatalogService",
            "ListDatasetVersionEvents",
            {
                "project": project,
                "name": name,
                "version": version,
                "pageSize": page_size,
                "pageToken": page_token,
            },
        )

    def diff_dataset_versions(
        self,
        name: str,
        before: str,
        after: str,
        *,
        before_sha256: str,
        after_sha256: str,
        project: str = "",
    ) -> dict[str, Any]:
        return self.call(
            "DatasetCatalogService",
            "DiffDatasetVersions",
            {
                "project": project,
                "name": name,
                "beforeVersion": before,
                "afterVersion": after,
                "beforeSha256": before_sha256,
                "afterSha256": after_sha256,
            },
        )

    # --- production failure inbox ---

    def capture_failure(
        self, trace_id: str, *, project: str = "", title: str, reason: str
    ) -> dict[str, Any]:
        """Capture once per source revision; retries preserve human edits."""
        return self.call(
            "FailureService",
            "CaptureFailure",
            {"project": project, "traceId": trace_id, "title": title, "reason": reason},
        )

    def list_failures(
        self,
        *,
        project: str = "",
        status: str = "",
        assignee: str = "",
        page_size: int = 50,
        page_token: str = "",
    ) -> dict[str, Any]:
        """One payload-free page; follow nextPageToken even when the page is empty."""
        body = {
            "project": project,
            "assignee": assignee,
            "pageSize": page_size,
            "pageToken": page_token,
        }
        if status:
            body["status"] = status
        return self.call("FailureService", "ListFailures", body)

    def get_failure(self, case_id: str, *, project: str = "") -> dict[str, Any]:
        out: dict[str, Any] = self.call(
            "FailureService", "GetFailure", {"project": project, "id": case_id}
        )["failure"]
        return out

    def update_failure(
        self,
        case_id: str,
        *,
        project: str = "",
        expected_revision: int,
        record: dict[str, Any],
        title: str,
        reason: str,
        assignee: str = "",
        status: str = "FAILURE_STATUS_OPEN",
    ) -> dict[str, Any]:
        out: dict[str, Any] = self.call(
            "FailureService",
            "UpdateFailure",
            {
                "project": project,
                "id": case_id,
                "expectedRevision": str(expected_revision),
                "record": record,
                "title": title,
                "reason": reason,
                "assignee": assignee,
                "status": status,
            },
        )["failure"]
        return out

    def review_failure(
        self,
        case_id: str,
        *,
        project: str = "",
        expected_revision: int,
        decision: str,
        note: str,
    ) -> dict[str, Any]:
        out: dict[str, Any] = self.call(
            "FailureService",
            "ReviewFailure",
            {
                "project": project,
                "id": case_id,
                "expectedRevision": str(expected_revision),
                "decision": decision,
                "note": note,
            },
        )["failure"]
        return out

    def failure_events(
        self,
        case_id: str,
        *,
        project: str = "",
        page_size: int = 50,
        page_token: str = "",
    ) -> dict[str, Any]:
        return self.call(
            "FailureService",
            "ListFailureEvents",
            {
                "project": project,
                "id": case_id,
                "pageSize": page_size,
                "pageToken": page_token,
            },
        )

    def export_failures(
        self, selections: dict[str, int], *, project: str = ""
    ) -> list[dict[str, Any]]:
        """Export explicit approved revisions; stale/unapproved selections fail together."""
        out: list[dict[str, Any]] = self.call(
            "FailureService",
            "ExportFailures",
            {
                "project": project,
                "selections": [
                    {"id": key, "revision": str(revision)} for key, revision in selections.items()
                ],
            },
        ).get("records", [])
        return out

    def delete_failure(self, case_id: str, *, project: str = "", expected_revision: int) -> None:
        self.call(
            "FailureService",
            "DeleteFailure",
            {
                "project": project,
                "id": case_id,
                "expectedRevision": str(expected_revision),
            },
        )

    # --- human annotation ---

    def annotation_call(self, method: str, body: dict[str, Any]) -> dict[str, Any]:
        return self.call("AnnotationService", method, body)

    def create_queue(self, queue: dict[str, Any]) -> dict[str, Any]:
        out: dict[str, Any] = self.annotation_call("CreateQueue", {"queue": queue})["queue"]
        return out

    def list_queues(self, project: str = "") -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = self.annotation_call("ListQueues", {"project": project}).get(
            "queues", []
        )
        return out

    def add_items(
        self,
        project: str,
        queue: str,
        *,
        run_id: str = "",
        when: str = "",
        all_trials: bool = False,
        records: list[dict[str, Any]] | None = None,
        limit: int = 0,
    ) -> int:
        """Add a run's records (those matching ``when``) or inline records; returns the count."""
        body: dict[str, Any] = {"project": project, "queue": queue, "limit": limit}
        if run_id:
            body["run"] = {"runId": run_id, "when": when, "allTrials": all_trials}
        else:
            body["records"] = {"records": records or []}
        return int(self.annotation_call("AddItems", body).get("added", 0))

    def next_item(self, project: str, queue: str, *, lease_seconds: int = 0) -> dict[str, Any]:
        """``{"item": ..., "remaining": ...}``; no item when nothing is left for the caller."""
        body = {"project": project, "queue": queue, "leaseSeconds": lease_seconds}
        return self.annotation_call("NextItem", body)

    def submit_annotation(
        self,
        project: str,
        queue: str,
        item_id: str,
        answers: list[dict[str, Any]],
        *,
        comment: str = "",
        skip: bool = False,
    ) -> dict[str, Any]:
        body = {
            "project": project,
            "queue": queue,
            "itemId": item_id,
            "answers": answers,
            "comment": comment,
            "skip": skip,
        }
        out: dict[str, Any] = self.annotation_call("SubmitAnnotation", body)["annotation"]
        return out

    def summarize_queue(self, project: str, queue: str) -> dict[str, Any]:
        return self.annotation_call("SummarizeQueue", {"project": project, "queue": queue})

    def list_annotations(
        self, project: str, queue: str, *, page_size: int = 500, page_token: str = ""
    ) -> dict[str, Any]:
        body = {"project": project, "queue": queue, "pageSize": page_size, "pageToken": page_token}
        return self.annotation_call("ListAnnotations", body)

    # --- webhooks ---

    def apply_webhook(self, webhook: dict[str, Any]) -> dict[str, Any]:
        """Create or replace a webhook. A new one comes back with its generated ``secret`` once."""
        out: dict[str, Any] = self.call("WebhookService", "ApplyWebhook", {"webhook": webhook})[
            "webhook"
        ]
        return out

    def list_webhooks(self, project: str = "") -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = self.call(
            "WebhookService", "ListWebhooks", {"project": project}
        ).get("webhooks", [])
        return out

    def delete_webhook(self, name: str, project: str = "") -> None:
        self.call("WebhookService", "DeleteWebhook", {"project": project, "name": name})

    def test_webhook(self, name: str, project: str = "") -> dict[str, Any]:
        """Send a ping now: ``{"delivered": ..., "statusCode": ..., "error": ...}``."""
        return self.call("WebhookService", "TestWebhook", {"project": project, "name": name})

    def list_webhook_deliveries(
        self, name: str, *, project: str = "", limit: int = 50
    ) -> list[dict[str, Any]]:
        body = {"project": project, "name": name, "limit": limit}
        out: list[dict[str, Any]] = self.call("WebhookService", "ListWebhookDeliveries", body).get(
            "deliveries", []
        )
        return out

    # --- guardrails ---

    def apply_guardrail(self, guardrail: dict[str, Any]) -> dict[str, Any]:
        out: dict[str, Any] = self.call(
            "GuardrailService", "ApplyGuardrail", {"guardrail": guardrail}
        )["guardrail"]
        return out

    def list_guardrails(self, project: str = "") -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = self.call(
            "GuardrailService", "ListGuardrails", {"project": project}
        ).get("guardrails", [])
        return out

    def delete_guardrail(self, project: str, name: str) -> None:
        self.call("GuardrailService", "DeleteGuardrail", {"project": project, "name": name})

    def check_guardrail(
        self,
        content: str,
        *,
        project: str = "",
        guardrail: str = "",
        inline: dict[str, Any] | None = None,
        phase: str = "request",
        labels: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        """Check content against a stored guardrail, or an inline one (a dry run)."""
        body: dict[str, Any] = {
            "project": project,
            "content": content,
            "phase": "GUARDRAIL_PHASE_" + phase.upper(),
            "labels": labels or {},
        }
        if inline is not None:
            body["inline"] = inline
        else:
            body["guardrail"] = guardrail
        return self.call("GuardrailService", "Check", body)

    # --- credentials ---

    def list_credentials(self, project: str = "") -> dict[str, Any]:
        """The worker variables (names and hosts) and judges a project's
        requests may use, and whether grants are enforced."""
        return self.call("CatalogService", "ListCredentials", {"project": project})

    # --- identity and access ---

    def auth_call(self, method: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
        return self.call("AuthService", method, body or {})

    def whoami(self) -> dict[str, Any]:
        return self.auth_call("WhoAmI")


def _json_body(response: httpx.Response, *, ok: bool = False) -> dict[str, Any]:
    """The response's JSON object; a proxy's HTML or an empty error body is a ServerError."""
    if not response.content and not ok:
        return {}
    snippet = response.text.strip()[:300]
    try:
        body = response.json()
    except ValueError:
        body = None
    if isinstance(body, dict):
        return body
    code = "invalid_response" if ok else f"http_{response.status_code}"
    raise ServerError(code, f"HTTP {response.status_code}: non-JSON body: {snippet}")


def _error(body: dict[str, Any], status: int) -> ServerError:
    return ServerError(str(body.get("code", f"http_{status}")), str(body.get("message", "")))


class EvaluationStream:
    """The results of :meth:`Client.evaluate_stream`, in the order they finish."""

    def __init__(
        self,
        client: Client,
        requests: Iterator[Any],
        records: list[Record],
        refs: list[Any],
        *,
        timeout: float | None,
        confidence: float,
        cluster_by: str | None,
        ci_method: str,
    ) -> None:
        self._client = client
        self._requests = requests
        self._records = records
        self._refs = refs
        self._timeout = timeout
        self.confidence, self.cluster_by, self.ci_method = confidence, cluster_by, ci_method
        self.results: list[EvaluationResult] = []
        self.summaries: list[MetricSummary] = []
        self._started = False

    def __iter__(self) -> Iterator[EvaluationResult]:
        if self._started:
            yield from self.results
            return
        self._started = True
        import grpc

        from evalsi.convert import result_from_proto, summary_from_proto
        from evalsi.v1alpha1 import evaluation_service_pb2_grpc as stubs

        channel = _grpc_channel(self._client.base_url)
        try:
            call = stubs.EvaluationServiceStub(channel).EvaluateStream(
                self._requests,
                metadata=tuple((k.lower(), v) for k, v in self._client.auth.headers().items()),
                timeout=self._timeout,
            )
            for message in call:
                kind = message.WhichOneof("message")
                if kind == "result":
                    result = result_from_proto(message.result)
                    self.results.append(result)
                    yield result
                elif kind == "summaries":
                    self.summaries = [summary_from_proto(m) for m in message.summaries.summaries]
        except grpc.RpcError as exc:
            code = exc.code().name.lower() if hasattr(exc, "code") else "unknown"
            detail = exc.details() if hasattr(exc, "details") else str(exc)
            raise ServerError(code, str(detail)) from exc
        finally:
            channel.close()

    def collect(self) -> EvalResult:
        """Drain the stream and return the whole :class:`~evalsi.EvalResult`."""
        for _ in self:
            pass
        return _assemble_streamed(self._client.base_url, self._records, self._refs, self)


def _grpc_channel(base_url: str) -> Any:
    import grpc

    parsed = httpx.URL(base_url)
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    target = f"{parsed.host}:{port}"
    if parsed.scheme == "https":
        return grpc.secure_channel(target, grpc.ssl_channel_credentials())
    return grpc.insecure_channel(target)


def _summary_options(confidence: float, cluster_by: str | None, ci_method: str) -> Any:
    from evalsi.v1alpha1 import evaluation_service_pb2 as pb

    if not 0 < confidence < 1:
        raise ValueError("confidence must be between 0 and 1")
    methods = {"auto": pb.CI_METHOD_AUTO, "bootstrap": pb.CI_METHOD_BOOTSTRAP}
    if ci_method not in methods:
        raise ValueError(f"ci_method must be one of {sorted(methods)}, not {ci_method!r}")
    return pb.SummaryOptions(
        confidence_level=confidence, cluster_by=cluster_by or "", ci_method=methods[ci_method]
    )


def _prepare_evaluation(
    data: str | Path | Iterable[Mapping[str, Any] | Record],
    evaluators: Sequence[str | Mapping[str, Any]],
    mapping: Mapping[str, str] | None,
    limit: int | None,
    params: Mapping[str, Mapping[str, Any]] | None,
) -> tuple[list[Record], list[Any]]:
    from evalsi.convert import to_struct
    from evalsi.datasets import load_records
    from evalsi.evaluator import EvaluatorConfigError
    from evalsi.v1alpha1 import evaluator_pb2

    params = params or {}
    refs = []
    for item in evaluators:
        if isinstance(item, str):
            entry: dict[str, Any] = {"ref": item}
        elif isinstance(item, Mapping) and "ref" in item:
            entry = dict(item)
        else:
            raise EvaluatorConfigError(
                f"{item!r}: evaluators run on the server, so name one it has "
                "(a reference string, or a mapping with 'ref', 'params' and 'name')"
            )
        ref, alias = str(entry["ref"]), str(entry.get("name", ""))
        short = ref.rsplit("/", 1)[-1].split("@", 1)[0]
        merged = {**dict(entry.get("params") or {}), **params.get(alias or short, {})}
        refs.append(evaluator_pb2.EvaluatorRef(ref=ref, name=alias, params=to_struct(merged)))
    return load_records(data, mapping=mapping, limit=limit), refs


def _instance_names(refs: list[Any]) -> list[dict[str, Any]]:
    return [
        {"name": r.name or r.ref.rsplit("/", 1)[-1].split("@", 1)[0], "ref": r.ref} for r in refs
    ]


def _manifest(
    base_url: str,
    records: list[Record],
    refs: list[Any],
    *,
    confidence: float,
    cluster_by: str | None,
    ci_method: str,
) -> dict[str, Any]:
    from datetime import UTC, datetime

    from evalsi._version import __version__
    from evalsi.datasets import records_hash

    now = datetime.now(UTC).isoformat()
    return {
        "evalsi_version": __version__,
        "api_version": "evalsi.v1alpha1",
        "server": base_url,
        "started_at": now,
        "finished_at": now,
        "dataset": {
            "source": "<in-memory>",
            "records": len(records),
            "sha256": records_hash(records),
        },
        "evaluators": _instance_names(refs),
        "summary": {
            "confidence_level": confidence,
            "cluster_by": cluster_by,
            "ci_method": ci_method,
        },
    }


def _assemble(
    base_url: str,
    records: list[Record],
    refs: list[Any],
    results: list[Any],
    summaries: list[Any],
    *,
    confidence: float,
    cluster_by: str | None,
    ci_method: str,
) -> EvalResult:
    from evalsi.convert import result_from_proto, summary_from_proto
    from evalsi.results import EvalResult

    return EvalResult(
        records=records,
        results=[result_from_proto(r) for r in results],
        summaries=[summary_from_proto(s) for s in summaries],
        manifest=_manifest(
            base_url,
            records,
            refs,
            confidence=confidence,
            cluster_by=cluster_by,
            ci_method=ci_method,
        ),
    )


def _assemble_streamed(
    base_url: str, records: list[Record], refs: list[Any], stream: EvaluationStream
) -> EvalResult:
    from evalsi.results import EvalResult

    order = {r.id: i for i, r in enumerate(records)}
    names = [i["name"] for i in _instance_names(refs)]
    results = sorted(
        stream.results,
        key=lambda r: (
            order.get(r.record_id, len(order)),
            names.index(r.evaluator) if r.evaluator in names else len(names),
        ),
    )
    return EvalResult(
        records=records,
        results=results,
        summaries=stream.summaries,
        manifest=_manifest(
            base_url,
            records,
            refs,
            confidence=stream.confidence,
            cluster_by=stream.cluster_by,
            ci_method=stream.ci_method,
        ),
    )

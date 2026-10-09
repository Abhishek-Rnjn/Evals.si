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
    ) -> dict[str, Any]:
        response = self._http.post(
            f"{self.base_url}/evalsi.v1alpha1.{service}/{method}",
            json=body,
            headers={"Connect-Protocol-Version": "1"},
            timeout=httpx.USE_CLIENT_DEFAULT if timeout is None else timeout,
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

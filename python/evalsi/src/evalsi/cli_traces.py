"""``evalsi traces``: the traces a server has ingested, with their online scores.

    evalsi traces list --server URL [--project P] [--service S]
    evalsi traces get TRACE_ID --server URL            # the record and its scores, as JSON
    evalsi traces export --server URL --project P --labels workflow=support-bot -o traces.jsonl

``export`` writes one record per line, in the shape a dataset file has, so the
same file is a regression dataset (``dataset.path`` in a run spec), a fixture
for a mapping profile, and what to look at when an execution was skipped
instead of scored. Each line also carries the trace's policy results under
``metadata.evalsi_scores``, and its id, service and labels under
``metadata.evalsi_trace``.
"""

from __future__ import annotations

import argparse
import contextlib
import json
import sys
from collections.abc import Iterator
from typing import Any

from evalsi.cli import add_credential_args, server_client


def add_trace_commands(sub: Any) -> None:
    tr = sub.add_parser("traces", help="list, show and export a server's ingested traces")
    tr_sub = tr.add_subparsers(dest="action", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--server", required=True, help="evalsid URL")
        p.add_argument("--project", default="", help="only this project")
        add_credential_args(p)

    ls = tr_sub.add_parser("list", help="traces, newest first")
    ls.add_argument("--service", default="", help="only this service")
    ls.add_argument("--limit", type=int, default=50, help="at most this many (default 50)")
    ls.add_argument("--format", choices=["table", "json"], default="table")
    common(ls)
    ls.set_defaults(func=_cmd_list)

    get = tr_sub.add_parser("get", help="one trace's record and its scores, as JSON")
    get.add_argument("trace_id")
    common(get)
    get.set_defaults(func=_cmd_get)

    ex = tr_sub.add_parser(
        "export", help="write traces as dataset records (JSONL), with their scores"
    )
    ex.add_argument("-o", "--output", required=True, help="the JSONL file to write ('-': stdout)")
    ex.add_argument("--service", default="", help="only this service")
    ex.add_argument(
        "--labels",
        nargs="*",
        default=[],
        metavar="KEY=VALUE",
        help="only traces carrying every one of these labels",
    )
    ex.add_argument("--policy", default="", help="only traces this policy scored")
    ex.add_argument("--since", default="", help="only traces started at or after (RFC 3339)")
    ex.add_argument("--limit", type=int, default=100, help="at most this many (default 100)")
    common(ex)
    ex.set_defaults(func=_cmd_export)


def _short(value: str, width: int) -> str:
    return value if len(value) <= width else value[: width - 1] + "…"


def iter_traces(
    client: Any, *, project: str = "", service: str = "", limit: int = 50
) -> Iterator[dict[str, Any]]:
    """Trace summaries, newest first, across pages."""
    token, seen = "", 0
    while seen < limit:
        body = {
            "project": project,
            "service": service,
            "pageSize": min(500, limit - seen),
            "pageToken": token,
        }
        page = client.call("TraceService", "ListTraces", body)
        for trace in page.get("traces", []):
            yield trace
            seen += 1
            if seen >= limit:
                return
        token = page.get("nextPageToken", "")
        if not token:
            return


def iter_scored(
    client: Any,
    *,
    project: str,
    service: str,
    labels: list[str],
    policy: str,
    since: str,
    limit: int,
) -> Iterator[dict[str, Any]]:
    """Summaries of the traces ListScores selects (with at least one selected
    result), newest first."""
    token, seen = "", 0
    while seen < limit:
        body: dict[str, Any] = {
            "project": project,
            "service": service,
            "labels": labels,
            "policy": policy,
            "pageSize": min(500, limit - seen),
            "pageToken": token,
        }
        if since:
            body["since"] = since
        page = client.call("TraceService", "ListScores", body)
        for entry in page.get("traces", []):
            yield entry.get("trace", {})
            seen += 1
            if seen >= limit:
                return
        token = page.get("nextPageToken", "")
        if not token:
            return


def export_row(trace: dict[str, Any], summary: dict[str, Any]) -> dict[str, Any]:
    """A GetTrace response as a dataset row (what ``load_records`` reads)."""
    from google.protobuf import json_format

    from evalsi.convert import record_from_proto
    from evalsi.v1alpha1 import record_pb2

    message = json_format.ParseDict(trace.get("record", {}), record_pb2.Record())
    row: dict[str, Any] = record_from_proto(message).to_dict()
    metadata = dict(row.get("metadata") or {})
    metadata["evalsi_trace"] = {
        "trace_id": summary.get("traceId", row.get("id", "")),
        "project": summary.get("project", ""),
        "service": summary.get("service", ""),
        "labels": summary.get("labels", {}),
    }
    metadata["evalsi_scores"] = trace.get("policies", [])
    row["metadata"] = metadata
    return row


def _call(args: argparse.Namespace, fn: Any) -> int:
    import httpx

    from evalsi.client import ServerError

    try:
        with server_client(args) as client:
            code: int = fn(client)
            return code
    except ServerError as exc:
        print(f"error: {exc}", file=sys.stderr)
    except httpx.HTTPError as exc:
        print(f"error: {args.server}: {exc}", file=sys.stderr)
    return 1


def _cmd_list(args: argparse.Namespace) -> int:
    def run(client: Any) -> int:
        traces = list(
            iter_traces(client, project=args.project, service=args.service, limit=args.limit)
        )
        if args.format == "json":
            print(json.dumps(traces, indent=2))
            return 0
        print(f"{'TRACE':32}  {'PROJECT':12}  {'SERVICE':24}  {'STARTED':20}  RESULTS  LABELS")
        for t in traces:
            labels = ",".join(f"{k}={v}" for k, v in sorted(t.get("labels", {}).items()))
            print(
                f"{t.get('traceId', ''):32}  {_short(t.get('project', ''), 12):12}  "
                f"{_short(t.get('service', ''), 24):24}  {t.get('startTime', '')[:19]:20}  "
                f"{t.get('results', 0):7}  {labels}"
            )
        return 0

    return _call(args, run)


def _cmd_get(args: argparse.Namespace) -> int:
    def run(client: Any) -> int:
        out = client.call(
            "TraceService", "GetTrace", {"traceId": args.trace_id, "project": args.project}
        )
        print(json.dumps(out, indent=2))
        return 0

    return _call(args, run)


def _cmd_export(args: argparse.Namespace) -> int:
    for label in args.labels:
        if "=" not in label:
            print(f"error: --labels takes KEY=VALUE, not {label!r}", file=sys.stderr)
            return 1

    def run(client: Any) -> int:
        if args.labels or args.policy or args.since:
            summaries = list(
                iter_scored(
                    client,
                    project=args.project,
                    service=args.service,
                    labels=args.labels,
                    policy=args.policy,
                    since=args.since,
                    limit=args.limit,
                )
            )
        else:
            summaries = list(
                iter_traces(client, project=args.project, service=args.service, limit=args.limit)
            )
        with contextlib.ExitStack() as stack:
            handle = (
                sys.stdout
                if args.output == "-"
                else stack.enter_context(open(args.output, "w", encoding="utf-8"))
            )
            for summary in summaries:
                body = {"traceId": summary["traceId"], "project": summary.get("project", "")}
                trace = client.call("TraceService", "GetTrace", body)
                handle.write(json.dumps(export_row(trace, summary), default=str) + "\n")
        if args.output != "-":
            print(f"wrote {len(summaries)} trace(s) to {args.output}", file=sys.stderr)
        return 0

    return _call(args, run)

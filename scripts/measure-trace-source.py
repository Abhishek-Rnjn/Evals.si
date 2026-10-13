"""Measures a trace source against M3's targets: backfill 30 days, then tail
with p95 pull lag under 2 minutes (docs/PRD.md, "Milestones").

    cd python
    # 1. History: traces spread evenly over the last 30 days, written to MLflow over OTLP.
    uv run --no-sync python ../scripts/measure-trace-source.py seed \\
        --mlflow http://localhost:5000 --experiment m3-backfill --count 500 --days 30
    # 2. Apply the source, time the backfill, then send live traces and time each
    #    one until it is scored.
    uv run --no-sync python ../scripts/measure-trace-source.py measure \\
        --server http://localhost:8081 --source-file source.yaml \\
        --mlflow http://localhost:5000 --experiment m3-backfill --expect 500 --live 30

``measure`` applies the source (so the backfill is timed from the moment it
exists), waits until the source has pulled ``--expect`` traces, then writes
``--live`` traces to MLflow, one every ``--every`` seconds, and records when
each first shows up scored in ``ListScores``. Lag is that moment minus the
trace's end. It prints the numbers as JSON and exits 1 when a limit
(``--max-backfill-seconds``, ``--max-p95-seconds``) is missed.

The traces are agent runs in OpenInference form (an AGENT span with a TOOL
span under it), so the source decodes them into records with a trajectory.
``--host-header`` sets the Host header for an MLflow that refuses others (a
port-forward to an in-cluster MLflow).
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import time
import urllib.request
from typing import Any

NS = 1_000_000_000


def _request(url: str, body: Any, *, host: str = "", headers: dict[str, str] | None = None) -> Any:
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if host:
        req.add_header("Host", host)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=60) as resp:
        raw = resp.read()
    return json.loads(raw) if raw else {}


def experiment_id(mlflow: str, name: str, host: str = "") -> str:
    """The experiment's id, created when it does not exist."""
    url = f"{mlflow}/api/2.0/mlflow/experiments/get-by-name?experiment_name={name}"
    req = urllib.request.Request(url)
    if host:
        req.add_header("Host", host)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return str(json.loads(resp.read())["experiment"]["experiment_id"])
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise
    out = _request(f"{mlflow}/api/2.0/mlflow/experiments/create", {"name": name}, host=host)
    return str(out["experiment_id"])


def _attr(key: str, value: str) -> dict[str, Any]:
    return {"key": key, "value": {"stringValue": value}}


def trace_spans(index: int, end_ns: int) -> tuple[str, list[dict[str, Any]]]:
    """One agent run ending at end_ns: (hex trace id, its OTLP spans)."""
    trace_id = os.urandom(16).hex()
    root, tool = os.urandom(8).hex(), os.urandom(8).hex()
    start = end_ns - 2 * NS
    question = f"m3 question {index}: what is retrieval-augmented generation?"
    answer = f"m3 answer {index}: it grounds an answer in retrieved documents."
    spans = [
        {
            "traceId": trace_id,
            "spanId": root,
            "name": "agent",
            "kind": 1,
            "startTimeUnixNano": str(start),
            "endTimeUnixNano": str(end_ns),
            "attributes": [
                _attr("openinference.span.kind", "AGENT"),
                _attr("input.value", question),
                _attr("output.value", answer),
            ],
            "status": {"code": 1},
        },
        {
            "traceId": trace_id,
            "spanId": tool,
            "parentSpanId": root,
            "name": "search_docs",
            "kind": 1,
            "startTimeUnixNano": str(start + NS // 2),
            "endTimeUnixNano": str(start + NS),
            "attributes": [
                _attr("openinference.span.kind", "TOOL"),
                _attr("tool.name", "search_docs"),
                _attr("input.value", '{"query": "rag"}'),
                _attr("output.value", "RAG grounds answers in documents."),
            ],
            "status": {"code": 1},
        },
    ]
    return trace_id, spans


def send(
    mlflow: str, experiment: str, spans: list[dict[str, Any]], host: str = "", service: str = "m3"
) -> None:
    body = {
        "resourceSpans": [
            {
                "resource": {"attributes": [_attr("service.name", service)]},
                "scopeSpans": [{"scope": {"name": "measure-trace-source"}, "spans": spans}],
            }
        ]
    }
    _request(f"{mlflow}/v1/traces", body, host=host, headers={"x-mlflow-experiment-id": experiment})


def cmd_seed(args: argparse.Namespace) -> int:
    exp = experiment_id(args.mlflow, args.experiment, args.host_header)
    now = time.time_ns() - 120 * NS  # history ends before any live trace
    span = args.days * 86400 * NS
    batch: list[dict[str, Any]] = []
    for i in range(args.count):
        end = now - span + (span * i) // max(1, args.count - 1) if args.count > 1 else now
        batch.extend(trace_spans(i, end)[1])
        if len(batch) >= 200:
            send(args.mlflow, exp, batch, args.host_header)
            batch = []
    if batch:
        send(args.mlflow, exp, batch, args.host_header)
    print(json.dumps({"experiment_id": exp, "traces": args.count, "days": args.days}))
    return 0


def _percentile(values: list[float], q: float) -> float:
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[int(q) - 1]


def _backfill(args: argparse.Namespace, client: Any, source: dict[str, Any]) -> dict[str, Any]:
    """Apply the source and wait until it has pulled --expect traces."""
    project, name = source.get("project", ""), source["name"]
    t0 = time.monotonic()
    client.call("SourceService", "ApplySource", {"source": source})
    status: dict[str, Any] = {}
    while time.monotonic() - t0 < args.timeout:
        got = client.call("SourceService", "GetSource", {"project": project, "name": name})
        status = got["source"].get("status", {})
        if int(status.get("pulled", 0)) >= args.expect:
            break
        time.sleep(1)
    seconds = time.monotonic() - t0
    pulled = int(status.get("pulled", 0))
    ok = pulled >= args.expect and not (
        args.max_backfill_seconds and seconds > args.max_backfill_seconds
    )
    if pulled < args.expect:
        print(f"backfill: pulled {pulled} of {args.expect} in {args.timeout}s", file=sys.stderr)
    return {"seconds": round(seconds, 1), "pulled": pulled, "expected": args.expect, "ok": ok}


def _live(args: argparse.Namespace, client: Any, project: str, exp: str) -> dict[str, Any]:
    """Send live traces and time each one from its end until it is scored."""
    since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 5))
    ended: dict[str, float] = {}
    scored: dict[str, float] = {}
    next_send = time.monotonic()
    deadline = time.monotonic() + args.live * args.every + args.timeout
    while time.monotonic() < deadline and (len(ended) < args.live or set(ended) - set(scored)):
        if len(ended) < args.live and time.monotonic() >= next_send:
            trace_id, spans = trace_spans(len(ended), time.time_ns())
            send(args.mlflow, exp, spans, args.host_header)
            ended[trace_id] = time.time()
            next_send += args.every
        body = {"project": project, "since": since, "pageSize": 500}
        for entry in client.call("TraceService", "ListScores", body).get("traces", []):
            trace_id = entry.get("trace", {}).get("traceId", "")
            if trace_id in ended and trace_id not in scored:
                scored[trace_id] = time.time()
        time.sleep(0.5)
    lags = sorted(scored[t] - ended[t] for t in scored)
    out: dict[str, Any] = {"sent": len(ended), "scored": len(scored)}
    if lags:
        out.update(
            p50_seconds=round(_percentile(lags, 50), 1),
            p95_seconds=round(_percentile(lags, 95), 1),
            max_seconds=round(lags[-1], 1),
        )
    if len(scored) < len(ended):
        print(f"live: {len(ended) - len(scored)} trace(s) never scored", file=sys.stderr)
    out["ok"] = (
        bool(lags)
        and len(scored) == len(ended)
        and not (args.max_p95_seconds and out["p95_seconds"] > args.max_p95_seconds)
    )
    return out


def cmd_measure(args: argparse.Namespace) -> int:
    from evalsi.cli_sources import load_source
    from evalsi.client import Client

    source = load_source(args.source_file)
    exp = experiment_id(args.mlflow, args.experiment, args.host_header)
    out: dict[str, Any] = {"source": source["name"], "project": source.get("project", "")}
    with Client(args.server, api_key=args.api_key, token=args.token) as client:
        out["backfill"] = _backfill(args, client, source)
        out["live"] = _live(args, client, out["project"], exp)
    out["passed"] = out["backfill"]["ok"] and out["live"]["ok"]
    print(json.dumps(out, indent=2))
    return 0 if out["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("seed", "measure"):
        p = sub.add_parser(name)
        p.add_argument("--mlflow", required=True, help="MLflow tracking URL")
        p.add_argument("--experiment", required=True, help="experiment name (created if missing)")
        p.add_argument("--host-header", default="", help="Host header to send to MLflow")
    seed = sub.choices["seed"]
    seed.add_argument("--count", type=int, default=500)
    seed.add_argument("--days", type=float, default=30)
    seed.set_defaults(func=cmd_seed)
    m = sub.choices["measure"]
    m.add_argument("--server", required=True, help="evalsid URL")
    m.add_argument("--api-key", default=os.environ.get("EVALSI_API_KEY"))
    m.add_argument("--token", default=os.environ.get("EVALSI_TOKEN"))
    m.add_argument("--source-file", required=True, help="the TraceSource to apply")
    m.add_argument("--expect", type=int, required=True, help="traces the backfill must pull")
    m.add_argument("--live", type=int, default=30, help="live traces to send")
    m.add_argument("--every", type=float, default=2.0, help="seconds between live traces")
    m.add_argument("--timeout", type=float, default=600, help="seconds to wait for each phase")
    m.add_argument("--max-backfill-seconds", type=float, default=0)
    m.add_argument("--max-p95-seconds", type=float, default=120)
    m.set_defaults(func=cmd_measure)
    args = parser.parse_args()
    code: int = args.func(args)
    return code


if __name__ == "__main__":
    sys.exit(main())

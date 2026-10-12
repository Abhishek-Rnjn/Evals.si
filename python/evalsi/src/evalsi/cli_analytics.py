"""``evalsi analyze``: SQL and metadata slices across many runs, in DuckDB."""

from __future__ import annotations

import argparse
import csv
import glob
import json
import sys
from typing import Any

from evalsi.cli import add_credential_args


def add_analytics_commands(sub: Any) -> None:
    an = sub.add_parser(
        "analyze",
        help="query results across runs (DuckDB; pip install 'evalsi[analytics]')",
        description="Load runs into DuckDB tables runs, scores and records, then query them.",
    )
    an_sub = an.add_subparsers(dest="action", required=True)
    q = an_sub.add_parser("query", help="run SQL over the runs, scores and records tables")
    q.add_argument("sql")
    q.set_defaults(func=_cmd_query)
    sl = an_sub.add_parser("slice", help="a metric per run and per record metadata value")
    sl.add_argument("metric")
    sl.add_argument("--by", required=True, help="a record metadata field (dotted path)")
    sl.add_argument("--level", type=float, default=0.95, choices=[0.9, 0.95, 0.99])
    sl.add_argument(
        "--on-server",
        action="store_true",
        help="compute the slice on the server (needs --server) instead of downloading every result",
    )
    sl.set_defaults(func=_cmd_slice)
    for p in (q, sl):
        src = p.add_argument_group("runs to load")
        src.add_argument(
            "--results",
            action="append",
            default=[],
            help="results JSON files (evalsi run --output); globs work (repeatable)",
        )
        src.add_argument("--server", help="load runs from this evalsid")
        src.add_argument("--project", default="")
        src.add_argument("--run", action="append", default=[], help="a run id (repeatable)")
        src.add_argument(
            "--label", action="append", default=[], help="only runs with label k=v (repeatable)"
        )
        src.add_argument(
            "--limit",
            type=int,
            default=None,
            help="newest runs to load (default 50; at most 500 with --on-server)",
        )
        src.add_argument(
            "--db",
            default=":memory:",
            help="a DuckDB file to load into (and query later with --db alone)",
        )
        p.add_argument("--format", choices=["table", "csv", "json"], default="table")
        add_credential_args(p)


def _warehouse(args: argparse.Namespace) -> Any:
    from evalsi.analytics import Warehouse

    wh = Warehouse(args.db)
    for pattern in args.results:
        paths = sorted(glob.glob(pattern)) or [pattern]
        for path in paths:
            wh.add_results_file(path)
    if args.server:
        from evalsi.cli import server_client

        with server_client(args) as client:
            wh.add_server_runs(
                client,
                project=args.project,
                labels=_labels(args.label),
                run_ids=args.run,
                limit=args.limit or 50,
            )
    return wh


def _print(args: argparse.Namespace, columns: list[str], rows: list[tuple[Any, ...]]) -> None:
    from evalsi.analytics import format_table

    if args.format == "json":
        print(json.dumps([dict(zip(columns, r, strict=True)) for r in rows], indent=2, default=str))
    elif args.format == "csv":
        w = csv.writer(sys.stdout)
        w.writerow(columns)
        w.writerows(rows)
    else:
        print(format_table(columns, rows))


def _cmd_query(args: argparse.Namespace) -> int:
    with _warehouse(args) as wh:
        columns, rows = wh.query(args.sql)
    _print(args, columns, rows)
    return 0


def _labels(pairs: list[str]) -> dict[str, str]:
    labels = {}
    for kv in pairs:
        k, sep, v = kv.partition("=")
        if not sep:
            raise ValueError(f"--label {kv!r}: use key=value")
        labels[k] = v
    return labels


def _server_slice(args: argparse.Namespace) -> tuple[list[str], list[tuple[Any, ...]]]:
    """The slice the server computes: the same numbers as the local one, over
    the runs the caller may read, without downloading any result."""
    from evalsi.cli import server_client

    if not args.server:
        raise ValueError("--on-server needs --server")
    if args.results or args.db != ":memory:":
        raise ValueError("--on-server reads the server's runs: drop --results and --db")
    with server_client(args) as client:
        out = client.slice_runs(
            args.metric,
            args.by,
            project=args.project,
            labels=_labels(args.label),
            run_ids=args.run,
            limit=args.limit or 0,
            level=args.level,
        )
    rows = [
        (
            r.get("runId", ""),
            r.get("runName", ""),
            r.get("key", ""),
            int(r.get("n", 0)),
            r.get("mean", 0.0),
            r.get("low"),
            r.get("high"),
        )
        for r in out.get("rows", [])
    ]
    return ["run_id", "run", args.by, "n", "mean", "low", "high"], rows


def _cmd_slice(args: argparse.Namespace) -> int:
    if args.on_server:
        columns, rows = _server_slice(args)
        if not rows:
            print(f"no scores for {args.metric}", file=sys.stderr)
            return 1
        _print(args, columns, rows)
        return 0
    with _warehouse(args) as wh:
        columns, rows = wh.slice(args.metric, args.by, level=args.level)
    if not rows:
        print(f"no scores for {args.metric}", file=sys.stderr)
        return 1
    _print(args, columns, rows)
    return 0

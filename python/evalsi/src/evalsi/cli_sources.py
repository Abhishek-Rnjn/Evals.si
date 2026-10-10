"""``evalsi source``: trace sources on a server.

A trace source is a store that already holds an agent's traces (MLflow
first). The server pulls its traces, scores them with the project's online
policies, and can write the scores back (decision 0016, the trace sources
guide). A source file has the shape of the TraceSource resource:

    apiVersion: evals.si/v1alpha1
    kind: TraceSource
    metadata: {name: studio-mlflow, labels: {evals.si/project: studio}}
    spec:
      connector: mlflow
      endpoint: https://mlflow.example.com
      locations: ["1"]
      backfill: {since: 720h}
      writeBack: {enabled: true}
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime
from pathlib import Path
from typing import Any

from evalsi.cli import add_credential_args, server_client

KIND = "TraceSource"


def add_source_commands(sub: Any) -> None:
    src = sub.add_parser("source", help="trace sources (MLflow) pulled and scored by a server")
    src_sub = src.add_subparsers(dest="action", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--server", required=True, help="evalsid URL")
        add_credential_args(p)

    def named(name: str, help: str) -> argparse.ArgumentParser:
        p: argparse.ArgumentParser = src_sub.add_parser(name, help=help)
        p.add_argument("name")
        p.add_argument("--project", default="")
        common(p)
        return p

    ap = src_sub.add_parser("apply", help="create or replace a source from a YAML or JSON file")
    ap.add_argument("-f", "--file", required=True)
    ap.add_argument(
        "--dry-run", action="store_true", help="check it on the server (grants included) only"
    )
    common(ap)
    ap.set_defaults(func=_cmd_apply)

    ls = src_sub.add_parser("list", help="list sources with their status")
    ls.add_argument("--project", default="")
    ls.add_argument("--format", choices=["table", "json"], default="table")
    common(ls)
    ls.set_defaults(func=_cmd_list)

    named("get", "a source and its status as JSON").set_defaults(func=_cmd_get)
    named("delete", "delete a source (pulled traces stay)").set_defaults(func=_cmd_delete)
    named("pause", "stop pulling and writing back").set_defaults(func=_cmd_pause)
    named("resume", "pull again from the stored watermark").set_defaults(func=_cmd_resume)
    bf = named("backfill", "read history again from a time, then tail")
    bf.add_argument(
        "--since", help="RFC 3339 time (default: the source's backfill.since before now)"
    )
    bf.set_defaults(func=_cmd_backfill)


def source_from_doc(doc: dict[str, Any], origin: str = "source") -> dict[str, Any]:
    """A TraceSource document as SourceService JSON, checked against the proto."""
    from google.protobuf import json_format

    from evalsi.runspec import api_labels, normalize_durations, project_of
    from evalsi.v1alpha1 import source_service_pb2 as pb

    if doc.get("kind", KIND) != KIND:
        raise ValueError(f"{origin}: kind must be {KIND}")
    spec = doc.get("spec")
    if not isinstance(spec, dict):
        raise ValueError(f"{origin}: expected a mapping with a 'spec'")
    metadata = doc.get("metadata") or {}
    body = {
        **spec,
        "name": metadata.get("name", ""),
        "project": project_of(metadata),
        "labels": api_labels(metadata.get("labels") or {}),
    }
    try:
        message = json_format.ParseDict(
            normalize_durations(body, pb.TraceSource.DESCRIPTOR), pb.TraceSource()
        )
    except json_format.ParseError as exc:
        raise ValueError(f"{origin}: invalid source: {exc}") from exc
    out: dict[str, Any] = json_format.MessageToDict(message)
    return out


def load_source(path: str) -> dict[str, Any]:
    import yaml

    doc = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
    if not isinstance(doc, dict):
        raise ValueError(f"{path}: expected a mapping")
    return source_from_doc(doc, path)


def _call(args: argparse.Namespace, method: str, body: dict[str, Any]) -> dict[str, Any] | None:
    from evalsi.client import ServerError

    with server_client(args) as client:
        try:
            out: dict[str, Any] = client.call("SourceService", method, body)
            return out
        except ServerError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return None


def _cmd_apply(args: argparse.Namespace) -> int:
    try:
        source = load_source(args.file)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    out = _call(args, "ApplySource", {"source": source, "validateOnly": args.dry_run})
    if out is None:
        return 1
    s = out["source"]
    verb = "valid" if args.dry_run else "applied"
    print(f"{verb}: source {s.get('project', '')}/{s['name']}")
    return 0


def _phase(s: dict[str, Any]) -> str:
    phase = str(s.get("status", {}).get("phase", "")).removeprefix("SOURCE_PHASE_").lower()
    return phase if phase and phase != "unspecified" else "pending"


def _cmd_list(args: argparse.Namespace) -> int:
    out = _call(args, "ListSources", {"project": args.project})
    if out is None:
        return 1
    sources = out.get("sources", [])
    if args.format == "json":
        print(json.dumps(sources, indent=2))
        return 0
    rows = [["source", "connector", "phase", "lag", "pulled", "scored", "last error"]]
    for s in sources:
        st = s.get("status", {})
        lag = st.get("lagSeconds")
        rows.append(
            [
                f"{s.get('project', '')}/{s['name']}",
                s.get("connector", ""),
                _phase(s),
                "-" if lag is None else f"{float(lag):.0f}s",
                str(st.get("pulled", 0)),
                str(st.get("scored", 0)),
                st.get("lastError", ""),
            ]
        )
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    for row in rows:
        print("  ".join(cell.ljust(w) for cell, w in zip(row, widths, strict=True)).rstrip())
    return 0


def _key(args: argparse.Namespace) -> dict[str, Any]:
    return {"project": args.project, "name": args.name}


def _cmd_get(args: argparse.Namespace) -> int:
    out = _call(args, "GetSource", _key(args))
    if out is None:
        return 1
    print(json.dumps(out["source"], indent=2))
    return 0


def _cmd_delete(args: argparse.Namespace) -> int:
    if _call(args, "DeleteSource", _key(args)) is None:
        return 1
    print(f"deleted source {args.name}")
    return 0


def _cmd_pause(args: argparse.Namespace) -> int:
    if _call(args, "PauseSource", _key(args)) is None:
        return 1
    print(f"paused source {args.name}")
    return 0


def _cmd_resume(args: argparse.Namespace) -> int:
    if _call(args, "ResumeSource", _key(args)) is None:
        return 1
    print(f"resumed source {args.name}")
    return 0


def _cmd_backfill(args: argparse.Namespace) -> int:
    body = _key(args)
    if args.since:
        try:
            when = datetime.fromisoformat(args.since.replace("Z", "+00:00"))
        except ValueError:
            print(f"error: --since {args.since!r} is not an RFC 3339 time", file=sys.stderr)
            return 1
        if when.tzinfo is None:
            print(
                "error: --since needs a time zone (for example 2026-10-01T00:00:00Z)",
                file=sys.stderr,
            )
            return 1
        body["since"] = when.isoformat().replace("+00:00", "Z")
    out = _call(args, "BackfillSource", body)
    if out is None:
        return 1
    wm = out["source"].get("status", {}).get("watermark", "")
    print(f"backfilling source {args.name} from {wm}")
    return 0

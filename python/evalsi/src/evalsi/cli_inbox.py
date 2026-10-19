"""Human-reviewed production incidents, exported as digest-pinned snapshots."""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path
from typing import Any

import httpx

from evalsi.cli import add_credential_args, server_client


def add_inbox_commands(sub: Any) -> None:
    commands = sub.add_parser("inbox", help="capture, review and export production failure cases")
    actions = commands.add_subparsers(dest="action", required=True)
    for name in ("list", "capture", "get", "edit", "review", "history", "export", "delete"):
        parser = actions.add_parser(name)
        parser.add_argument("--server", required=True)
        parser.add_argument("--project", default="")
        add_credential_args(parser)
        parser.set_defaults(func=_run)
        if name in ("get", "edit", "review", "history", "delete"):
            parser.add_argument("id")
        if name in ("edit", "review", "delete"):
            parser.add_argument("--revision", type=int, required=True)
        if name in ("list", "history"):
            parser.add_argument("--page-token", default="")
            parser.add_argument("--page-size", type=int, default=50)
        if name == "list":
            parser.add_argument("--status", choices=("open", "in-review", "approved", "dismissed"))
            parser.add_argument("--assignee", default="")
        if name == "capture":
            parser.add_argument("trace_id")
            parser.add_argument("--title", required=True)
            parser.add_argument("--reason", required=True)
        if name == "edit":
            parser.add_argument(
                "-f", "--file", required=True, help="edited case JSON from inbox get"
            )
        if name == "review":
            parser.add_argument("--decision", required=True, choices=("approved", "dismissed"))
            parser.add_argument("--note", required=True)
        if name == "export":
            parser.add_argument(
                "--case", action="append", required=True, help="approved ID@REVISION (repeatable)"
            )
            parser.add_argument("-o", "--output", required=True)
            for field in ("name", "version", "owner"):
                parser.add_argument(f"--{field}", required=True)
            parser.add_argument("--split", choices=("held-out", "tuning"), default="held-out")
            parser.add_argument("--tag", action="append", default=[])


def _status(value: str) -> str:
    return "FAILURE_STATUS_" + value.upper().replace("-", "_")


def _selections(values: list[str]) -> dict[str, int]:
    out: dict[str, int] = {}
    for value in values:
        case_id, separator, revision = value.partition("@")
        if not separator or len(case_id) != 24 or any(c not in "0123456789abcdef" for c in case_id):
            raise ValueError("--case requires a 24-character lowercase hex ID@REVISION")
        if (
            not revision.isascii()
            or not revision.isdecimal()
            or int(revision) < 1
            or int(revision) >= 2**63
        ):
            raise ValueError("--case revision must be a positive int64")
        if case_id in out:
            raise ValueError("--case IDs must be unique")
        out[case_id] = int(revision)
    return out


def _export(client: Any, args: argparse.Namespace) -> dict[str, Any]:
    from google.protobuf import json_format

    from evalsi.convert import record_from_proto
    from evalsi.dataset_snapshots import create_snapshot
    from evalsi.v1alpha1.record_pb2 import Record

    records = client.export_failures(_selections(args.case), project=args.project)
    rows = []
    for wire in records:
        record = record_from_proto(json_format.ParseDict(wire, Record()))
        row = record.to_dict()
        row["provenance"] = {"failure": record.metadata["evalsi.failure"]}
        rows.append(row)
    # Temporary source bytes are private and removed even on publish failure.
    # create_snapshot publishes atomically, mode 0600, and never replaces a path.
    with tempfile.TemporaryDirectory(prefix="evalsi-inbox-") as directory:
        source = Path(directory) / "approved.jsonl"
        source.write_text(
            "".join(json.dumps(row, sort_keys=True, ensure_ascii=False) + "\n" for row in rows),
            encoding="utf-8",
        )
        snapshot = create_snapshot(
            source,
            args.output,
            name=args.name,
            version=args.version,
            owner=args.owner,
            split=args.split,
            tags=args.tag,
        )
    return {
        "snapshot": snapshot.info(),
        "uri": f"snapshot://{args.output}?sha256={snapshot.sha256}",
    }


def _run(args: argparse.Namespace) -> int:
    from evalsi.client import ServerError

    try:
        with server_client(args) as client:
            match args.action:
                case "list":
                    out = client.list_failures(
                        project=args.project,
                        status=_status(args.status) if args.status else "",
                        assignee=args.assignee,
                        page_size=args.page_size,
                        page_token=args.page_token,
                    )
                case "capture":
                    out = client.capture_failure(
                        args.trace_id, project=args.project, title=args.title, reason=args.reason
                    )
                case "get":
                    out = client.get_failure(args.id, project=args.project)
                case "edit":
                    doc = json.loads(Path(args.file).read_text(encoding="utf-8"))
                    if not isinstance(doc, dict):
                        raise ValueError("case file must contain an object")
                    out = client.update_failure(
                        args.id,
                        project=args.project,
                        expected_revision=args.revision,
                        record=doc["record"],
                        title=doc["title"],
                        reason=doc["reason"],
                        assignee=doc.get("assignee", ""),
                        status=doc.get("status", "FAILURE_STATUS_OPEN"),
                    )
                case "review":
                    out = client.review_failure(
                        args.id,
                        project=args.project,
                        expected_revision=args.revision,
                        decision=_status(args.decision),
                        note=args.note,
                    )
                case "history":
                    out = client.failure_events(
                        args.id,
                        project=args.project,
                        page_size=args.page_size,
                        page_token=args.page_token,
                    )
                case "export":
                    out = _export(client, args)
                case "delete":
                    client.delete_failure(
                        args.id, project=args.project, expected_revision=args.revision
                    )
                    out = {"deleted": args.id}
                case _:
                    raise ValueError("unknown inbox command")
        print(json.dumps(out, indent=2, ensure_ascii=True))
        return 0
    except (ServerError, httpx.HTTPError, OSError, ValueError, KeyError, TypeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

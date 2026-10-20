"""Publish and review immutable server datasets, with explicit content pins."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

import httpx

from evalsi.cli import add_credential_args, server_client
from evalsi.client import Client


def add_catalog_commands(commands: Any) -> None:
    for name in (
        "publish",
        "versions",
        "version",
        "review-version",
        "version-history",
        "version-diff",
        "fetch-version",
    ):
        parser = commands.add_parser(name, help="shared immutable dataset catalog")
        parser.add_argument("--server", required=True)
        parser.add_argument("--project", default="")
        add_credential_args(parser)
        parser.set_defaults(func=_run_catalog, catalog_action=name)
        if name not in ("publish", "versions"):
            parser.add_argument("name")
        if name in ("version", "review-version", "version-history", "fetch-version"):
            parser.add_argument("version")
        if name in ("versions", "version-history"):
            parser.add_argument("--page-size", type=int, default=20)
            parser.add_argument("--page-token", default="")
        if name == "versions":
            parser.add_argument("--name", default="")
            parser.add_argument("--owner", default="")
            parser.add_argument("--state", choices=("draft", "approved", "retired"))
        if name == "publish":
            for field in ("name", "version", "owner"):
                parser.add_argument(f"--{field}", required=True)
            parser.add_argument("--split", choices=("tuning", "held-out"), default="held-out")
            parser.add_argument("--tag", action="append", default=[])
            parser.add_argument("--label", action="append", default=[])
            parser.add_argument("--description", default="")
            source = parser.add_mutually_exclusive_group(required=True)
            source.add_argument("--records", help="JSON/JSONL dataset with explicit record IDs")
            source.add_argument("--snapshot", help="portable snapshot; --sha256 is required")
            source.add_argument(
                "--failure", action="append", help="approved failure ID@REVISION (repeatable)"
            )
            parser.add_argument("--sha256", help="approved portable snapshot pin")
        if name == "review-version":
            parser.add_argument("--sha256", required=True)
            parser.add_argument("--revision", type=int, required=True)
            parser.add_argument("--decision", choices=("approved", "retired"), required=True)
            parser.add_argument("--note", required=True)
        if name == "version-diff":
            parser.add_argument("before")
            parser.add_argument("after")
            parser.add_argument("--before-sha256", required=True)
            parser.add_argument("--after-sha256", required=True)
            parser.add_argument("--fail-on-change", action="store_true")
        if name == "fetch-version":
            parser.add_argument(
                "--sha256", required=True, help="independently approved catalog pin"
            )
            parser.add_argument("-o", "--output", required=True, help="new portable snapshot file")


def _state(value: str) -> str:
    return "DATASET_VERSION_STATE_" + value.upper()


def _publish(client: Client, args: argparse.Namespace) -> dict[str, Any]:
    from google.protobuf import json_format

    from evalsi.cli_inbox import _selections
    from evalsi.convert import record_to_proto
    from evalsi.dataset_snapshots import read_snapshot
    from evalsi.datasets import DatasetError, load_records

    labels = {}
    for label in args.label:
        key, separator, value = label.partition("=")
        if not separator or key in labels:
            raise ValueError("--label requires unique KEY=VALUE entries")
        labels[key] = value
    content: dict[str, Any] = {
        "project": args.project,
        "name": args.name,
        "version": args.version,
        "owner": args.owner,
        "split": args.split,
        "tags": args.tag,
        "labels": labels,
        "description": args.description,
    }
    if args.failure:
        return client.publish_dataset_version(content, failures=_selections(args.failure))
    if args.snapshot:
        if not args.sha256:
            raise ValueError("--snapshot requires an independently approved --sha256")
        snapshot = read_snapshot(args.snapshot, expected=args.sha256)
        records = load_records(
            f"snapshot://{Path(args.snapshot).resolve()}?sha256={snapshot.sha256}"
        )
    else:
        if args.sha256:
            raise ValueError("--sha256 is only used with --snapshot")
        # The generic loader synthesizes missing IDs. Publication must not.
        source = Path(args.records)
        if source.suffix == ".json":
            rows = json.loads(source.read_text(encoding="utf-8"))
        elif source.suffix == ".jsonl":
            rows = [
                json.loads(line)
                for line in source.read_text(encoding="utf-8").splitlines()
                if line.strip()
            ]
        else:
            raise DatasetError("catalog source must be JSON or JSONL")
        if not isinstance(rows, list) or any(
            not isinstance(row, dict) or not isinstance(row.get("id"), str) or not row["id"].strip()
            for row in rows
        ):
            raise DatasetError("catalog records require explicit nonempty string IDs")
        records = load_records(rows)
    content["records"] = [json_format.MessageToDict(record_to_proto(record)) for record in records]
    return client.publish_dataset_version(content)


def _fetch(client: Client, args: argparse.Namespace) -> dict[str, Any]:
    import tempfile

    from google.protobuf import json_format

    from evalsi.convert import record_from_proto
    from evalsi.dataset_snapshots import create_snapshot
    from evalsi.v1alpha1.record_pb2 import Record

    response = client.get_dataset_version(args.name, args.version, project=args.project)
    dataset = response["dataset"]
    if dataset["sha256"] != args.sha256:
        raise ValueError("catalog SHA-256 mismatch")
    content = dataset["content"]
    rows = []
    for wire in content.get("records", []):
        row = record_from_proto(json_format.ParseDict(wire, Record())).to_dict()
        row["provenance"] = {
            "catalog": response["reference"],
            "state": dataset["state"],
            "reviewed_by": dataset.get("reviewedBy", ""),
            "record_source": wire.get("provenance", {}),
        }
        rows.append(row)
    with tempfile.TemporaryDirectory(prefix="evalsi-catalog-") as directory:
        source = Path(directory) / "records.jsonl"
        source.write_text(
            "".join(json.dumps(row, sort_keys=True, ensure_ascii=False) + "\n" for row in rows),
            encoding="utf-8",
        )
        snapshot = create_snapshot(
            source,
            args.output,
            name=content["name"],
            version=content["version"],
            owner=content["owner"],
            split=content["split"],
            tags=content.get("tags", []),
        )
    return {"catalog": response["reference"], "snapshot": snapshot.info()}


def _run_catalog(args: argparse.Namespace) -> int:
    from evalsi.client import ServerError

    try:
        code = 0
        with server_client(args) as client:
            match args.catalog_action:
                case "publish":
                    result = _publish(client, args)
                case "versions":
                    result = client.list_dataset_versions(
                        project=args.project,
                        name=args.name,
                        owner=args.owner,
                        state=_state(args.state) if args.state else "",
                        page_size=args.page_size,
                        page_token=args.page_token,
                    )
                case "version":
                    result = client.get_dataset_version(
                        args.name, args.version, project=args.project
                    )
                case "review-version":
                    result = client.review_dataset_version(
                        args.name,
                        args.version,
                        project=args.project,
                        sha256=args.sha256,
                        expected_revision=args.revision,
                        decision=_state(args.decision),
                        note=args.note,
                    )
                case "version-history":
                    result = client.dataset_version_events(
                        args.name,
                        args.version,
                        project=args.project,
                        page_size=args.page_size,
                        page_token=args.page_token,
                    )
                case "version-diff":
                    result = client.diff_dataset_versions(
                        args.name,
                        args.before,
                        args.after,
                        project=args.project,
                        before_sha256=args.before_sha256,
                        after_sha256=args.after_sha256,
                    )
                    if args.fail_on_change and result["beforeSha256"] != result["afterSha256"]:
                        code = 3
                case "fetch-version":
                    result = _fetch(client, args)
                case _:
                    raise ValueError("unknown catalog command")
        print(json.dumps(result, indent=2, ensure_ascii=True))
        return code
    except (ServerError, httpx.HTTPError, ValueError, OSError, KeyError, TypeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

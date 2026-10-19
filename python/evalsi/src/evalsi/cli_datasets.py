"""Create, inspect, verify and review portable dataset snapshots."""

from __future__ import annotations

import argparse
import json
from typing import Any

from evalsi.dataset_snapshots import create_snapshot, diff_snapshots, read_snapshot


def add_snapshot_commands(commands: Any) -> None:
    create = commands.add_parser("snapshot", help="publish a new snapshot; never overwrite")
    create.add_argument("source", help="local JSON/JSONL with explicit record IDs")
    create.add_argument("-o", "--output", required=True, help="new file in an existing directory")
    create.add_argument("--name", required=True)
    create.add_argument("--version", required=True)
    create.add_argument("--owner", required=True)
    create.add_argument("--split", choices=["held-out", "tuning"], default="held-out")
    create.add_argument("--tag", action="append", default=[])
    create.set_defaults(func=_cmd_snapshot)
    for name in ("inspect", "verify"):
        check = commands.add_parser(
            name,
            help="inspect metadata" if name == "inspect" else "verify against an approved digest",
        )
        check.add_argument("file")
        check.add_argument("--sha256", required=name == "verify")
        check.add_argument("--format", choices=["table", "json"], default="table")
        check.set_defaults(func=_cmd_inspect)
    diff = commands.add_parser("diff", help="review changes by record ID without raw payloads")
    diff.add_argument("before")
    diff.add_argument("after")
    diff.add_argument("--format", choices=["table", "json"], default="table")
    diff.add_argument("--fail-on-change", action="store_true", help="exit 3 on any identity change")
    diff.set_defaults(func=_cmd_diff)
    overlap = commands.add_parser("overlap", help="check pinned tuning/held-out IDs and inputs")
    overlap.add_argument("--tuning", required=True)
    overlap.add_argument("--held-out", required=True)
    overlap.add_argument("--tuning-sha256", required=True)
    overlap.add_argument("--held-out-sha256", required=True)
    overlap.add_argument("--format", choices=["table", "json"], default="table")
    overlap.add_argument(
        "--fail-on-overlap",
        action="store_true",
        help="exit 3 on overlap, or 1 on incomplete input coverage",
    )
    overlap.set_defaults(func=_cmd_overlap)


def _cmd_snapshot(args: argparse.Namespace) -> int:
    snapshot = create_snapshot(
        args.source,
        args.output,
        name=args.name,
        version=args.version,
        owner=args.owner,
        split=args.split,
        tags=args.tag,
    )
    print(json.dumps(snapshot.info(), indent=2))
    return 0


def _cmd_inspect(args: argparse.Namespace) -> int:
    snapshot = read_snapshot(args.file, expected=args.sha256)
    if args.format == "json":
        print(json.dumps(snapshot.info(), indent=2))
    else:
        for key, value in snapshot.info().items():
            print(f"{key}: {value}")
    return 0


def _cmd_diff(args: argparse.Namespace) -> int:
    diff = diff_snapshots(read_snapshot(args.before), read_snapshot(args.after))
    if args.format == "json":
        print(json.dumps(diff, indent=2))
    else:
        print(
            f"added={len(diff['added'])} removed={len(diff['removed'])} "
            f"changed={len(diff['changed'])} unchanged={diff['unchanged']}"
        )
        print("metadata changed: " + (", ".join(diff["metadata_changed"]) or "none"))
        print(f"order changed: {diff['order_changed']}")
        items = (
            [f"+ {json.dumps(rid)}" for rid in diff["added"]]
            + [f"- {json.dumps(rid)}" for rid in diff["removed"]]
            + [f"~ {json.dumps(r['id'])}: {json.dumps(r['fields'])}" for r in diff["changed"]]
        )
        for item in items[:20]:
            print(item)
        if len(items) > 20:
            print(f"... {len(items) - 20} more; --format json lists all changed IDs")
    return 3 if args.fail_on_change and diff["different"] else 0


def _cmd_overlap(args: argparse.Namespace) -> int:
    from evalsi.dataset_overlap import split_overlap

    tuning = read_snapshot(args.tuning, expected=args.tuning_sha256)
    held_out = read_snapshot(args.held_out, expected=args.held_out_sha256)
    report = split_overlap(tuning, held_out)
    report["policy"] = "fail-on-overlap" if args.fail_on_overlap else "report-only"
    if args.format == "json":
        print(json.dumps(report, indent=2))
    else:
        print(f"status: {report['status']} (exact inputs and record IDs only)")
        print(f"policy: {report['policy']}")
        for side in ("tuning", "held_out"):
            identity = report[side]
            print(f"{side}: {identity['name']}@{identity['version']} sha256={identity['sha256']}")
        print(
            f"shared IDs: {len(report['shared_ids'])}; "
            f"shared input groups: {len(report['shared_inputs'])}"
        )
        for split, coverage in report["input_coverage"].items():
            print(f"{split} input coverage: {coverage['compared']}/{coverage['total']}")
        for rid in report["shared_ids"][:20]:
            print(f"shared ID: {json.dumps(rid)}")
        for group in report["shared_inputs"][:20]:
            print(
                f"input {group['input_sha256']}: "
                f"tuning={json.dumps(group['tuning_ids'][:20])} "
                f"held_out={json.dumps(group['held_out_ids'][:20])}"
            )
        print(
            "Table IDs/groups are limited to 20; JSON lists all. "
            "Missing/empty inputs make coverage incomplete; review before enforcing."
        )
    if args.fail_on_overlap and not report["complete"]:
        return 1
    return 3 if args.fail_on_overlap and report["overlap"] else 0

"""CLI for evaluator validity and judge calibration, independent of target runs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from evalsi.calibration import REPORT_SCHEMA, calibrate, compare_reports
from evalsi.calibration_report import render_report
from evalsi.calibration_sets import (
    SET_SCHEMA,
    annotation_rows,
    freeze_set,
    publish,
    read_artifact,
    read_json,
    read_jsonl,
    validate_set,
    write_artifact,
)


def add_calibrate_commands(sub: Any) -> None:
    group = sub.add_parser(
        "calibrate", help="validate evaluators against frozen human pass/fail labels"
    )
    commands = group.add_subparsers(dest="calibration_command", required=True)
    freeze = commands.add_parser(
        "freeze", help="freeze human votes and explicit tuning/held-out splits"
    )
    source = freeze.add_mutually_exclusive_group(required=True)
    source.add_argument("--data", help="JSONL rows containing record, ratings and split")
    source.add_argument("--annotations", help="JSONL from evalsi annotate export")
    freeze.add_argument(
        "--assignments", help="JSON item-ID map with split, group and slices for annotations"
    )
    freeze.add_argument("--name", required=True)
    freeze.add_argument("--version", required=True, help="human label revision")
    freeze.add_argument(
        "--question", required=True, help="human question; annotation question name when importing"
    )
    freeze.add_argument(
        "--question-text", help="full human question/instructions (required for annotation import)"
    )
    freeze.add_argument("--out", required=True)
    freeze.set_defaults(func=_command)
    run = commands.add_parser(
        "run", help="select thresholds on tuning data and validate on held-out labels"
    )
    run.add_argument("--dataset", required=True)
    run.add_argument(
        "-f", "--protocol", required=True, help="JSON protocol including approved dataset_sha256"
    )
    run.add_argument("--out", required=True, help="new JSON report artifact")
    run.set_defaults(func=_command)
    report = commands.add_parser("report", help="render a self-contained offline HTML report")
    report.add_argument("artifact")
    report.add_argument("--out", required=True)
    report.set_defaults(func=_command)
    verify = commands.add_parser(
        "verify", help="verify set or report content against a trusted digest"
    )
    verify.add_argument("artifact")
    verify.add_argument("--sha256", required=True)
    verify.set_defaults(func=_command)
    compare = commands.add_parser(
        "compare", help="compare judge drift on identical frozen records and labels"
    )
    compare.add_argument("before")
    compare.add_argument("after")
    compare.add_argument("--out", required=True)
    compare.set_defaults(func=_command)


def _new_output(path: str) -> None:
    destination = Path(path)
    if destination.exists() or destination.is_symlink():
        raise ValueError("calibration outputs never overwrite existing paths")
    if not destination.parent.is_dir():
        raise ValueError("calibration output directory does not exist")


def _command(args: argparse.Namespace) -> int:
    try:
        return _execute(args)
    except OSError as exc:
        print(f"error: cannot read or publish calibration artifact: {exc}", file=sys.stderr)
        return 1


def _execute(args: argparse.Namespace) -> int:
    command = args.calibration_command
    if command == "verify":
        document, sha256 = read_artifact(args.artifact, expected=args.sha256)
        if document.get("schema") == SET_SCHEMA:
            validate_set(document)
        elif document.get("schema") != REPORT_SCHEMA:
            raise ValueError("expected a calibration set or report")
        print(json.dumps({"sha256": sha256, "schema": document["schema"], "verified": True}))
        return 0
    _new_output(args.out)  # Fail before any paid judge calls on an unusable output path.
    if command == "freeze":
        if args.annotations:
            if not args.assignments:
                raise ValueError("annotation import requires --assignments with explicit splits")
            if not args.question_text:
                raise ValueError(
                    "annotation import requires --question-text to pin the human instructions"
                )
            assignments = read_json(args.assignments)
            if not isinstance(assignments, dict):
                raise ValueError("assignments must be an item-ID object")
            rows = annotation_rows(
                read_jsonl(args.annotations), question=args.question, assignments=assignments
            )
        else:
            if args.assignments:
                raise ValueError("--assignments is only for --annotations")
            rows = read_jsonl(args.data)
        document = freeze_set(
            rows, name=args.name, version=args.version, question=args.question_text or args.question
        )
    elif command == "run":
        document = calibrate(args.dataset, read_json(args.protocol))
    elif command == "compare":
        before, before_sha256 = read_artifact(args.before)
        after, after_sha256 = read_artifact(args.after)
        document = {
            **compare_reports(before, after),
            "before_sha256": before_sha256,
            "after_sha256": after_sha256,
        }
    else:
        document, _ = read_artifact(args.artifact)
        publish(args.out, render_report(document))
        print(json.dumps({"path": args.out, "format": "html"}))
        return 0
    sha256 = write_artifact(args.out, document)
    print(json.dumps({"path": args.out, "schema": document["schema"], "sha256": sha256}))
    return 3 if command == "run" and document["status"] == "incomplete" else 0

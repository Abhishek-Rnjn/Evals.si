"""Inspect run evidence and evaluate pinned release policies on evalsid."""

from __future__ import annotations

import argparse
import json
import sys
from typing import Any

import httpx
import yaml

from evalsi.cli import add_credential_args, server_client
from evalsi.client import ServerError
from evalsi.release_decisions import (
    exit_code,
    gate_candidate,
    load_policy,
    policy_hash,
    read_report,
    verify_decision,
    write_report,
)


def add_release_commands(sub: Any) -> None:
    commands = sub.add_parser(
        "release", help="pinned baseline/candidate release decisions"
    ).add_subparsers(dest="release_action", required=True)
    for name in ("evidence", "decide", "gate", "get", "list", "policy-hash", "verify"):
        parser = commands.add_parser(name)
        parser.set_defaults(func=_run)
        if name in ("policy-hash", "decide", "gate"):
            parser.add_argument(
                "-f", "--file", required=True, help="reviewed release policy JSON/YAML"
            )
        if name in ("policy-hash", "verify"):
            if name == "verify":
                parser.add_argument("file")
                parser.add_argument("--sha256", required=True)
            continue
        parser.add_argument("--server", required=True)
        parser.add_argument("--project", default="")
        add_credential_args(parser)
        if name in ("evidence", "get"):
            parser.add_argument("id")
        if name in ("decide", "gate", "get"):
            parser.add_argument("-o", "--output", help="new private portable report")
        if name == "get":
            parser.add_argument("--sha256", help="independently reviewed decision pin")
        if name == "list":
            parser.add_argument("--page-size", type=int, default=20)
            parser.add_argument("--page-token", default="")
        if name in ("decide", "gate"):
            parser.add_argument("--id", required=True, help="new immutable decision ID")
            parser.add_argument("--policy-sha256", required=True)
            if name == "gate":
                parser.add_argument("--spec", required=True, help="candidate EvalRun YAML")
                parser.add_argument(
                    "--commit", required=True, help="declared candidate agent.commit"
                )
            for side in ("baseline",) if name == "gate" else ("baseline", "candidate"):
                parser.add_argument("--" + side, required=True, help="explicit server run ID")
                parser.add_argument("--" + side + "-sha256", required=True)
                parser.add_argument(
                    "--" + side + "-commit", required=True, help="declared agent.commit label"
                )


def _run(args: argparse.Namespace) -> int:
    try:
        code = 0
        if args.release_action == "policy-hash":
            print(policy_hash(load_policy(args.file)))
            return 0
        if args.release_action == "verify":
            decision = read_report(args.file, args.sha256)
            print(
                json.dumps(
                    {
                        "id": decision["id"],
                        "sha256": args.sha256,
                        "outcome": decision["outcome"],
                        "integrity_verified": True,
                    }
                )
            )
            return exit_code(decision)
        policy = load_policy(args.file) if args.release_action in ("decide", "gate") else None
        if policy is not None and policy_hash(policy) != args.policy_sha256:
            raise ValueError("release policy SHA-256 mismatch")
        with server_client(args) as client:
            match args.release_action:
                case "evidence":
                    result = client.release_run_evidence(args.id)
                case "list":
                    result = client.list_release_decisions(
                        project=args.project, page_size=args.page_size, page_token=args.page_token
                    )
                case "gate":
                    assert policy is not None
                    result = gate_candidate(
                        client,
                        policy=policy,
                        policy_sha256=args.policy_sha256,
                        baseline={
                            "runId": args.baseline,
                            "sha256": args.baseline_sha256,
                            "commit": args.baseline_commit,
                        },
                        spec_file=args.spec,
                        candidate_commit=args.commit,
                        decision_id=args.id,
                        project=args.project,
                    )
                    code = exit_code(result)
                case "decide":
                    assert policy is not None
                    result = client.create_release_decision(
                        args.id,
                        project=args.project,
                        policy=policy,
                        policy_sha256=args.policy_sha256,
                        baseline={
                            "runId": args.baseline,
                            "sha256": args.baseline_sha256,
                            "commit": args.baseline_commit,
                        },
                        candidate={
                            "runId": args.candidate,
                            "sha256": args.candidate_sha256,
                            "commit": args.candidate_commit,
                        },
                    )
                    verify_decision(result, result.get("sha256", ""))
                    code = exit_code(result)
                case "get":
                    result = client.get_release_decision(args.id, project=args.project)
                    verify_decision(result, args.sha256 or result.get("sha256", ""))
                case _:
                    raise ValueError("unknown release action")
            if args.release_action in ("get", "decide", "gate") and args.output:
                write_report(args.output, result)
        print(json.dumps(result, indent=2, ensure_ascii=True, allow_nan=False))
        return code
    except (
        ServerError,
        httpx.HTTPError,
        OSError,
        ValueError,
        TypeError,
        KeyError,
        yaml.YAMLError,
    ) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

"""``evalsi promote`` and ``evalsi shadow``: the flywheel on a server.

``promote`` turns a run's failing records into a regression dataset of its
project. ``shadow`` replays recorded inputs (production traces, a promoted
dataset, an earlier run) against a candidate, scores the recorded outputs the
same way, and prints the paired comparison.
"""

from __future__ import annotations

import argparse
import json
import sys
from typing import Any

from evalsi.cli import (
    EXIT_GATES_FAILED,
    _cmd_compare_rows,
    _print_gates,
    add_credential_args,
    server_client,
)


def add_flywheel_commands(sub: Any) -> None:
    pr = sub.add_parser("promote", help="add a server run's matching records to a dataset")
    pr.add_argument("run_id")
    pr.add_argument("--dataset", required=True, help="dataset name in the run's project")
    pr.add_argument(
        "--when",
        required=True,
        help="CEL over scores, errored, trial and record, e.g. 'scores[\"task-success\"] < 1'",
    )
    pr.add_argument(
        "--include-outputs",
        action="store_true",
        help="keep the run's outputs and trajectories (for re-scoring and shadow baselines)",
    )
    pr.add_argument("--server", required=True, help="evalsid URL")
    add_credential_args(pr)
    pr.set_defaults(func=_cmd_promote)

    sh = sub.add_parser(
        "shadow", help="replay recorded inputs against a candidate and compare with the recording"
    )
    sh.add_argument("-f", "--file", required=True, help="the candidate's run spec")
    sh.add_argument("--server", required=True, help="evalsid URL")
    sh.add_argument("--format", choices=["table", "json"], default="table")
    sh.add_argument("--quiet", action="store_true", help="no progress output")
    add_credential_args(sh)
    sh.set_defaults(func=_cmd_shadow)


def _cmd_promote(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        out = client.promote_results(
            args.run_id, args.dataset, args.when, include_outputs=args.include_outputs
        )
    print(f"promoted {out.get('promoted', 0)} record(s) to {out.get('path', '')}")
    return 0


def _wait(client: Any, run_id: str, label: str, show: bool) -> dict[str, Any]:
    run: dict[str, Any] = {}
    for event in client.watch_run(run_id):
        if "run" in event:
            run = event["run"]
        elif "progress" in event and show:
            p = event["progress"]
            print(
                f"\r{label}: {p.get('done', 0)}/{p.get('total', 0)}   ",
                end="",
                file=sys.stderr,
                flush=True,
            )
    if show:
        print(file=sys.stderr)
    return run


def _cmd_shadow(args: argparse.Namespace) -> int:
    from evalsi.runspec import load_spec, spec_to_dict

    run_file = load_spec(args.file)
    show = not args.quiet and sys.stderr.isatty()
    with server_client(args) as client:
        pair = client.create_shadow_replay(
            spec_to_dict(run_file.spec),
            name=run_file.name,
            project=run_file.project,
            labels=run_file.labels,
        )
        baseline = _wait(client, pair["baseline"]["id"], "recorded", show)
        candidate = _wait(client, pair["candidate"]["id"], "candidate", show)
        comparisons = client.compare_runs(baseline["id"], candidate["id"])
    if args.format == "json":
        print(
            json.dumps(
                {"baseline": baseline, "candidate": candidate, "comparisons": comparisons}, indent=2
            )
        )
    else:
        print(f"recorded {baseline['id']} vs candidate {candidate['id']}\n")
        _cmd_compare_rows(comparisons)
        _print_gates(
            [
                {
                    **g.get("gate", {}),
                    "stat": str(g.get("gate", {}).get("stat", "mean"))
                    .removeprefix("GATE_STAT_")
                    .lower(),
                    "passed": g.get("passed", False),
                    "reason": g.get("reason", ""),
                }
                for g in candidate.get("gates", [])
            ]
        )
    for run in (baseline, candidate):
        if run.get("status") not in ("RUN_STATUS_SUCCEEDED", "RUN_STATUS_FAILED"):
            print(
                f"error: run {run.get('id')} {run.get('status')}: {run.get('error', '')}",
                file=sys.stderr,
            )
            return 1
    return EXIT_GATES_FAILED if candidate.get("status") == "RUN_STATUS_FAILED" else 0

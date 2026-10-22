"""The offline ``evalsi determinism`` report, and ``--against`` to compare a replay."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from evalsi.determinism import compare_runs, determinism_report


def add_determinism_command(sub: Any) -> None:
    parser = sub.add_parser(
        "determinism",
        help="report score stability across trials, or compare a replay with its recording",
    )
    parser.add_argument("results", help="results JSON from evalsi run --output")
    parser.add_argument("--margin", type=float, default=0.05)
    parser.add_argument("--confidence", type=float, default=0.95)
    parser.add_argument(
        "--against",
        metavar="REPLAY",
        help=(
            "compare this results file (the replay, or a rerun) with RESULTS (the recording) "
            "and name the first step at which each trajectory diverged, with what differed and "
            "what changed in the runs' determinism records; exits 1 if any diverged"
        ),
    )
    parser.set_defaults(func=_cmd_report)


def _load(path: str, what: str) -> dict[str, Any]:
    try:
        data = json.loads(Path(path).read_text(encoding="utf-8"))
    except OSError as exc:
        raise ValueError(f"cannot read {what}: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"{what} must contain a JSON object")
    return data


def _cmd_report(args: argparse.Namespace) -> int:
    data = _load(args.results, "results")
    if args.against:
        comparison = compare_runs(data, _load(args.against, "replay results"))
        print(json.dumps(comparison, indent=2, ensure_ascii=False))
        return 1 if comparison["diverged"] or comparison["only_recorded"] else 0
    print(
        json.dumps(
            determinism_report(data, margin=args.margin, confidence=args.confidence), indent=2
        )
    )
    return 0

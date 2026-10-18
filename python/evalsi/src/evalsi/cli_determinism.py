"""The offline ``evalsi determinism`` report."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from evalsi.determinism import determinism_report


def add_determinism_command(sub: Any) -> None:
    parser = sub.add_parser("determinism", help="report observed score stability across trials")
    parser.add_argument("results", help="results JSON from evalsi run --output")
    parser.add_argument("--margin", type=float, default=0.05)
    parser.add_argument("--confidence", type=float, default=0.95)
    parser.set_defaults(func=_cmd_report)


def _cmd_report(args: argparse.Namespace) -> int:
    try:
        data = json.loads(Path(args.results).read_text(encoding="utf-8"))
    except OSError as exc:
        raise ValueError(f"cannot read results: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError("results file must contain a JSON object")
    print(
        json.dumps(
            determinism_report(data, margin=args.margin, confidence=args.confidence), indent=2
        )
    )
    return 0

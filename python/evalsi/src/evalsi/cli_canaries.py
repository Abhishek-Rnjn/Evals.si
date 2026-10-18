"""Create a probe dataset and its separate training corpus."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from evalsi.canaries import make_canaries


def add_canary_commands(sub: Any) -> None:
    parser = sub.add_parser("canary", help="create private training-data completion canaries")
    actions = parser.add_subparsers(dest="action", required=True)
    create = actions.add_parser("create", help="write a probe dataset and training corpus")
    create.add_argument("--count", type=int, default=10)
    create.add_argument("--output", required=True, help="new probe JSONL file")
    create.add_argument("--training-output", required=True, help="new training text file")
    create.set_defaults(func=_cmd_create)


def _cmd_create(args: argparse.Namespace) -> int:
    records = make_canaries(args.count)
    dataset, training = Path(args.output), Path(args.training_output)
    if dataset.resolve() == training.resolve():
        raise ValueError("probe and training files must be different")
    if dataset.exists() or training.exists():
        raise ValueError("canary output files must not already exist")
    try:
        with dataset.open("x", encoding="utf-8") as probe:
            for record in records:
                probe.write(json.dumps(record.to_dict()) + "\n")
        with training.open("x", encoding="utf-8") as corpus:
            for record in records:
                assert record.input is not None
                assert record.reference is not None
                corpus.write(record.input.as_text() + " " + record.reference.as_text() + "\n")
    except OSError as exc:
        raise ValueError(f"cannot write canary files: {exc}") from exc
    print(json.dumps({"canaries": len(records), "probe": str(dataset), "training": str(training)}))
    return 0

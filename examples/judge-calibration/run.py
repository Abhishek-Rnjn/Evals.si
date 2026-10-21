"""Offline evaluator-validation demo. All ratings are synthetic fixture labels."""

from __future__ import annotations

import argparse
import copy
import json
from pathlib import Path
from typing import Any

from evalsi.calibration import PROTOCOL_SCHEMA, calibrate, compare_reports
from evalsi.calibration_report import render_report
from evalsi.calibration_sets import freeze_set, publish, read_jsonl, write_artifact


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True, help="new output directory")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=False)
    labels = read_jsonl(Path(__file__).with_name("labels.jsonl"))
    dataset = freeze_set(
        labels,
        name="synthetic-answers",
        version="fixture-labels-v1",
        question="Is this recorded answer correct, allowing punctuation changes?",
    )
    path = args.out / "dataset.json"
    pin = write_artifact(path, dataset)
    protocol: dict[str, Any] = {
        "schema": PROTOCOL_SCHEMA,
        "dataset_sha256": pin,
        "repeats": 3,
        "candidates": [
            {
                "name": "exact",
                "evaluator": {"ref": "exact-match"},
                "metric": "exact-match",
                "thresholds": [0.5],
                "pass_when": "gte",
            },
            {
                "name": "fuzzy",
                "evaluator": {"ref": "fuzzy-match"},
                "metric": "fuzzy-match",
                "thresholds": [0.5, 0.75, 0.9],
                "pass_when": "gte",
            },
        ],
    }
    publish(args.out / "protocol.json", json.dumps(protocol, indent=2).encode() + b"\n")
    result = calibrate(path, protocol)
    report_pin = write_artifact(args.out / "report.json", result)
    publish(args.out / "report.html", render_report(result))
    # Negative control: changing records/labels behind an approved pin must fail.
    rejected_pin = copy.deepcopy(protocol)
    rejected_pin["dataset_sha256"] = "0" * 64
    try:
        calibrate(path, rejected_pin)
    except ValueError as exc:
        if "SHA-256 mismatch" not in str(exc):
            raise
    else:
        raise AssertionError("a substituted dataset pin was accepted")
    # Evaluator drift on identical targets is paired; target improvement is not inferred.
    changed = copy.deepcopy(protocol)
    changed["candidates"][1]["thresholds"] = [0.99]
    changed_result = calibrate(path, changed)
    write_artifact(args.out / "stricter-judge.json", changed_result)
    write_artifact(args.out / "judge-drift.json", compare_reports(result, changed_result))
    print(
        json.dumps(
            {
                "dataset_sha256": pin,
                "report_sha256": report_pin,
                "selected_on_tuning": result["selected_candidate"],
                "report": str(args.out / "report.html"),
                "negative_pin_control": "passed",
            }
        )
    )


if __name__ == "__main__":
    main()

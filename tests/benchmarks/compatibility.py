"""Record actual score semantics, including cases outside the common timing domain."""

from __future__ import annotations

import argparse
import importlib.metadata
import json
import subprocess
import tempfile
from pathlib import Path

from evaluation import ROOT, TOOLS, disable_network, local_environment

CASES = [
    ("equal", "Paris", "Paris"),
    ("wrong", "Lyon", "Paris"),
    ("case", "PARIS", "Paris"),
    ("leading-space", " Paris", "Paris"),
    ("trailing-newline", "Paris\n", "Paris"),
    ("internal-space", "Pa ris", "Paris"),
    ("empty-equal", "", ""),
    ("empty-output", "", "Paris"),
    ("unicode-equal", "café", "café"),
    ("unicode-normalization", "cafe\u0301", "café"),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worker", choices=TOOLS)
    parser.add_argument("--interpreters", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if args.worker:
        local_environment()
        attempts = disable_network()
        from adapters import batch_runner

        run = batch_runner(args.worker, 1)
        input_errors = (ValueError,)
        if args.worker == "deepeval":
            from deepeval.errors import MissingTestCaseParamsError

            input_errors = (ValueError, MissingTestCaseParamsError)
        cases = []
        for name, output, reference in CASES:
            case = {"name": name, "strict_expected": float(output == reference)}
            try:
                result = run(
                    [
                        {
                            "id": name,
                            "input": name,
                            "output": output,
                            "reference": reference,
                        }
                    ]
                )
                if len(result) != 1 or result[0][0] != name:
                    raise ValueError("missing or mismatched result")
                case.update(
                    status="scored",
                    score=result[0][1],
                    agrees_with_strict=result[0][1] == case["strict_expected"],
                )
            except input_errors as exc:
                # Unsupported inputs are recorded, not counted as zero or pass.
                case.update(status="rejected", exception=type(exc).__name__)
            cases.append(case)
        if attempts:
            raise ValueError("offline compatibility probe attempted network access")
        args.out.write_text(
            json.dumps(
                {
                    "tool": args.worker,
                    "version": importlib.metadata.version(args.worker),
                    "cases": cases,
                },
                indent=2,
            )
            + "\n"
        )
        return 0
    if not args.interpreters:
        parser.error("supply --interpreters")
    interpreters = json.loads(args.interpreters.read_text())
    results = []
    for tool in TOOLS:
        with tempfile.TemporaryDirectory(prefix="evalsi-compatibility-") as tmp:
            out = Path(tmp) / "result.json"
            result = subprocess.run(
                [
                    interpreters[tool],
                    str(Path(__file__).resolve()),
                    "--worker",
                    tool,
                    "--out",
                    str(out),
                ],
                cwd=tmp,
                capture_output=True,
                text=True,
                timeout=120,
                check=False,
            )
            if result.returncode:
                raise RuntimeError(f"{tool}: {result.stderr[-2000:]}")
            results.append(json.loads(out.read_text()))
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(
        json.dumps(
            {
                "source_commit": subprocess.check_output(
                    ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True
                ).strip(),
                "scope": "Observed local SDK behavior, not a universal metric-quality ranking.",
                "results": results,
            },
            indent=2,
        )
        + "\n"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

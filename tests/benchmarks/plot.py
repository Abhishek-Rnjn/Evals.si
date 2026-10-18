"""Plot measured medians and observed ranges, without inferring production performance."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import matplotlib.pyplot as plt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("data", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    report = json.loads(args.data.read_text())
    plt.rcParams.update({"svg.fonttype": "none", "font.size": 10})
    fig, axes = plt.subplots(1, 2, figsize=(11, 4.6), layout="constrained")
    names = {
        "evalsi": "Evals.si",
        "deepeval": "DeepEval",
        "ragas": "Ragas",
        "langsmith": "LangSmith SDK",
        "braintrust": "Braintrust SDK",
    }
    for ax, tier in zip(axes, ("batch", "metric"), strict=True):
        tools = list(names) if tier == "batch" else list(names)[:3]
        for offset, records, color in (
            (-0.13, 256, "#0072B2"),
            (0.13, 2048, "#D55E00"),
        ):
            for index, tool in enumerate(tools):
                summary = next(
                    x
                    for x in report["summary"]
                    if x["tier"] == tier and x["tool"] == tool and x["records"] == records
                )
                median = summary["median_records_per_second"]
                low, high = (
                    records / summary["max_seconds"],
                    records / summary["min_seconds"],
                )
                ax.errorbar(
                    median,
                    index + offset,
                    xerr=[[median - low], [high - median]],
                    fmt="o",
                    capsize=3,
                    color=color,
                    label=f"{records:,} records" if index == 0 else None,
                )
        ax.set_yticks(range(len(tools)), [names[t] for t in tools])
        ax.invert_yaxis()
        ax.set_xscale("log")
        ax.grid(axis="x", which="both", alpha=0.2)
        ax.set_xlabel("Records / second (log scale)")
        ax.set_title("Native batch runners" if tier == "batch" else "Native metric loops")
        ax.legend(loc="lower right", fontsize=8)
    fig.suptitle("Local exact-match overhead • 7 trials per point", fontsize=14)
    fig.supxlabel(
        "Points: medians. Whiskers: observed min-max. Different SDK features remain in scope.\n"
        "No hosted, judge-quality, sandbox or production-scale measurements.",
        fontsize=9,
    )
    fig.savefig(
        args.out,
        metadata={
            "Date": report["measured_at"],
            "Creator": "Evals.si reproducible benchmark harness",
        },
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

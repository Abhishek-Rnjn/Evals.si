"""Regenerates testdata/stats_vectors.json from the Python implementation.

The Go server's internal/stats tests check against the same file, so both
implementations must agree. Run: cd python && uv run python ../scripts/gen-stats-vectors.py
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from evalsi import stats

ROOT = Path(__file__).resolve().parent.parent

T_CASES = [(0.975, 1), (0.975, 2), (0.975, 3), (0.975, 7.5), (0.975, 30), (0.995, 4), (0.9, 12)]

BINARY = [1.0, 0.0, 1.0, 1.0, 0.0, 1.0, 1.0, 0.0, 1.0, 1.0, 0.0, 0.0]
NUMBERS = [0.12, 0.5, 0.33, 0.91, 0.47, 0.05, 0.66, 0.72, 0.28, 0.81]
CLUSTERS_12 = ["a", "a", "a", "b", "b", "c", "c", "c", "d", "d", "e", "e"]
CLUSTERS_10 = ["x", "y", "x", "z", "y", "w", "z", "x", "w", "y"]

INTERVAL_CASES: list[dict[str, Any]] = [
    {"name": "wilson", "values": BINARY, "proportion": True, "level": 0.95},
    {"name": "wilson-all-pass", "values": [1.0] * 5, "proportion": True, "level": 0.9},
    {"name": "t", "values": NUMBERS, "proportion": False, "level": 0.95},
    {"name": "t-single", "values": [0.4], "proportion": False, "level": 0.95},
    {"name": "clustered-binary", "values": BINARY, "proportion": True, "level": 0.95,
     "clusters": CLUSTERS_12},
    {"name": "clustered-numbers", "values": NUMBERS, "proportion": False, "level": 0.99,
     "clusters": CLUSTERS_10},
    {"name": "clustered-one-cluster", "values": NUMBERS, "proportion": False, "level": 0.95,
     "clusters": ["only"] * 10},
    {"name": "bootstrap", "values": NUMBERS, "proportion": False, "level": 0.95,
     "method": "bootstrap"},
    {"name": "bootstrap-clustered", "values": BINARY, "proportion": True, "level": 0.95,
     "method": "bootstrap", "clusters": CLUSTERS_12},
]


def main() -> None:
    t_quantiles = [
        {"p": p, "df": df, "want": stats.t_quantile(p, df)} for p, df in T_CASES
    ]
    intervals = []
    for case in INTERVAL_CASES:
        got = stats.interval(
            case["values"],
            proportion=case["proportion"],
            level=case["level"],
            clusters=case.get("clusters"),
            method=case.get("method", "auto"),
        )
        want = None if got is None else {
            "low": got.low, "high": got.high, "level": got.level, "method": got.method
        }
        intervals.append({**case, "want": want})
    out = ROOT / "testdata" / "stats_vectors.json"
    out.write_text(json.dumps({"t_quantile": t_quantiles, "intervals": intervals}, indent=1) + "\n")
    print(f"wrote {out}")


if __name__ == "__main__":
    main()

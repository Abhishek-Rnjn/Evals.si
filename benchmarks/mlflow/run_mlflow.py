"""MLflow side of the benchmark. Run it with the Python that has `mlflow` installed.

    python run_mlflow.py det   --data data-1000.jsonl --tracking-uri sqlite:///m.db --out out.json
    python run_mlflow.py judge --data data-200.jsonl   --judge-url http://127.0.0.1:8767/v1

Uses mlflow.genai.evaluate on precomputed outputs (no predict_fn), with MLflow's
built-in RegexMatch scorer, two custom @scorer functions for the exact and
contains checks, and, in judge mode, the built-in Correctness and
RelevanceToQuery LLM judges pointed at the mock judge.

Prints one JSON object: timings, and (det mode) every record's scores.
"""

from __future__ import annotations

import argparse
import json
import os
import resource
import time

os.environ.setdefault("MLFLOW_DISABLE_AGENT_HINT", "1")

T0 = time.perf_counter()
import mlflow  # noqa: E402
from mlflow.genai.scorers import Correctness, RegexMatch, RelevanceToQuery, scorer  # noqa: E402

IMPORT_S = time.perf_counter() - T0

REGEX = r"^[A-Z][a-z]+ \d{3,4}$"


@scorer
def exact_match(outputs, expectations):
    return outputs == expectations["expected_response"]


@scorer
def contains(outputs, expectations):
    return expectations["expected_response"].lower() in outputs.lower()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["det", "judge"])
    parser.add_argument("--data", required=True)
    parser.add_argument("--out")
    parser.add_argument("--tracking-uri", default="sqlite:///mlflow-bench.db")
    parser.add_argument("--judge-url", default="http://127.0.0.1:8767/v1")
    parser.add_argument("--workers", type=int, default=None, help="MLFLOW_GENAI_EVAL_MAX_WORKERS; MLflow's default is 10")
    args = parser.parse_args()

    if args.workers is not None:
        os.environ["MLFLOW_GENAI_EVAL_MAX_WORKERS"] = str(args.workers)
    if args.mode == "judge":
        os.environ["OPENAI_API_KEY"] = "x"
        os.environ["OPENAI_API_BASE"] = args.judge_url
        os.environ["OPENAI_BASE_URL"] = args.judge_url

    rows = [json.loads(line) for line in open(args.data)]
    data = [
        {
            "inputs": {"question": r["input"], "id": r["id"]},
            "outputs": r["output"],
            "expectations": {"expected_response": r["reference"]},
        }
        for r in rows
    ]

    mlflow.set_tracking_uri(args.tracking_uri)
    mlflow.set_experiment("bench")

    if args.mode == "det":
        scorers = [exact_match, contains, RegexMatch(pattern=REGEX)]
    else:
        scorers = [Correctness(model="openai:/mock"), RelevanceToQuery(model="openai:/mock")]

    t = time.perf_counter()
    result = mlflow.genai.evaluate(data=data, scorers=scorers)
    eval_s = time.perf_counter() - t

    summary = {
        "tool": "mlflow",
        "mode": args.mode,
        "version": mlflow.__version__,
        "import_s": round(IMPORT_S, 3),
        "eval_s": round(eval_s, 3),
        "peak_rss_mb": round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 1),
    }
    df = result.result_df
    summary["rows_returned"] = int(len(df))
    if args.mode == "det":
        scores: dict[str, dict] = {}
        for _, row in df.iterrows():
            rid = row["request"]["id"]
            scores[rid] = {
                "exact-match": bool(row["exact_match/value"]),
                "contains": bool(row["contains/value"]),
                "regex-match": str(row["regex_match/value"]).lower() == "yes",
            }
        summary["scores"] = scores
    if args.out:
        with open(args.out, "w") as f:
            json.dump(summary, f)
    print(json.dumps({k: v for k, v in summary.items() if k != "scores"}))


if __name__ == "__main__":
    main()

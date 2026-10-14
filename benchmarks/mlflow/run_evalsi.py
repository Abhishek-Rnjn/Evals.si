"""Evals.si side of the benchmark. Run it with the Python that has `evalsi` installed.

    python run_evalsi.py det   --data data-1000.jsonl --out out.json
    python run_evalsi.py judge --data data-200.jsonl  --judge-url http://127.0.0.1:8767/v1

Prints one JSON object: timings, and (det mode) every record's scores.
"""

from __future__ import annotations

import argparse
import json
import resource
import time

T0 = time.perf_counter()

import evalsi  # noqa: E402
from evalsi import JudgeConfig  # noqa: E402

IMPORT_S = time.perf_counter() - T0

REGEX = r"^[A-Z][a-z]+ \d{3,4}$"


def per_record(result: evalsi.EvalResult) -> dict[str, dict[str, float | bool | None]]:
    out: dict[str, dict] = {}
    for r in result.results:
        row = out.setdefault(r.record_id, {})
        for score in r.scores:
            value = score.passed if score.passed is not None else score.number
            row[f"{r.evaluator}"] = value
    return out


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["det", "judge"])
    parser.add_argument("--data", required=True)
    parser.add_argument("--out")
    parser.add_argument("--judge-url", default="http://127.0.0.1:8767/v1")
    parser.add_argument("--concurrency", type=int, default=16, help="evalsi's own default is 16")
    parser.add_argument("--exact-normalize", choices=["true", "false"], default="true")
    parser.add_argument("--no-cache", action="store_true")
    parser.add_argument("--cache-dir", help="directory for the judge response cache (default: evalsi's own)")
    args = parser.parse_args()

    options: dict = {"concurrency": args.concurrency}
    if args.mode == "det":
        evaluators = ["exact-match", "contains", "regex-match"]
        options["params"] = {
            "exact-match": {"normalize": args.exact_normalize == "true"},
            "regex-match": {"pattern": REGEX},
        }
    else:
        evaluators = ["llm-judge", "answer-relevance"]
        options["params"] = {"llm-judge": {"rubric": "correctness"}}
        options["judge"] = JudgeConfig(
            provider="openai-compatible", model="mock", base_url=args.judge_url, api_key_env="BENCH_KEY"
        )
        options["cache"] = False if args.no_cache else (args.cache_dir or True)

    t = time.perf_counter()
    result = evalsi.evaluate(args.data, evaluators, **options)
    eval_s = time.perf_counter() - t

    summary = {
        "tool": "evalsi",
        "mode": args.mode,
        "import_s": round(IMPORT_S, 3),
        "eval_s": round(eval_s, 3),
        "errors": len(result.errors()),
        "peak_rss_mb": round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 1),
    }
    if args.mode == "det":
        summary["scores"] = per_record(result)
    if args.out:
        with open(args.out, "w") as f:
            json.dump(summary, f)
    print(json.dumps({k: v for k, v in summary.items() if k != "scores"}))


if __name__ == "__main__":
    main()

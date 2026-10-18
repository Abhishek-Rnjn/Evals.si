"""Reproducible local evaluation benchmarks; correctness is required before timing is accepted."""

from __future__ import annotations

import argparse
import atexit
import hashlib
import importlib.metadata
import json
import math
import os
import platform
import random
import resource
import socket
import statistics
import subprocess
import tempfile
import time
from pathlib import Path

TOOLS = ("evalsi", "deepeval", "ragas", "langsmith", "braintrust")
METRIC_TOOLS = TOOLS[:3]
ROOT = Path(__file__).resolve().parents[2]


def digest(value) -> str:
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()
    ).hexdigest()


def corpus(n: int) -> list[dict[str, str]]:
    # Shared valid domain: nonempty strings without leading/trailing whitespace.
    # DeepEval 4.3.1 strips ends and refuses empty output; compatibility.py
    # records those differences separately instead of mixing metric semantics.
    pairs = [
        ("Paris", "Paris"),
        ("Lyon", "Paris"),
        ("PARIS", "Paris"),
        ("Pa ris", "Paris"),
        ("Pa\nris", "Paris"),
        ("x", "x"),
        ("y", "x"),
        ("café", "café"),
        ("cafe\u0301", "café"),
        ("你好", "你好"),
        ("x" * 4096, "x" * 4096),
        ("x" * 4095 + "y", "x" * 4096),
    ]
    return [
        {
            "id": f"row-{i:07d}",
            "input": f"row-{i:07d}",
            "output": pairs[i % len(pairs)][0],
            "reference": pairs[i % len(pairs)][1],
        }
        for i in range(n)
    ]


def validate(rows, scores) -> str:
    expected = {r["id"]: float(r["output"] == r["reference"]) for r in rows}
    actual = {}
    for key, score in scores:
        if (
            key in actual
            or key not in expected
            or not math.isfinite(score)
            or score != expected[key]
        ):
            raise ValueError(f"incorrect, duplicate, or unexpected result for {key}")
        actual[key] = score
    if actual.keys() != expected.keys():
        raise ValueError(f"missing results: expected {len(expected)}, got {len(actual)}")
    return digest(sorted(actual.items()))


def local_environment() -> None:
    os.environ.update(
        {
            "DEEPEVAL_TELEMETRY_OPT_OUT": "YES",
            "DEEPEVAL_NO_INSPECT_PROMPT": "1",
            "DEEPEVAL_UPDATE_WARNING_OPT_OUT": "YES",
            "RAGAS_DO_NOT_TRACK": "true",
            "LANGSMITH_TRACING": "false",
            "LANGCHAIN_TRACING_V2": "false",
            "TQDM_DISABLE": "1",
        }
    )


def disable_network():
    network_attempts = []

    def no_network(self, address):
        # Prevent telemetry, accidental uploads, and model calls even if an SDK
        # changes its opt-out behavior. Only count attempts, never log addresses.
        network_attempts.append(True)
        raise OSError("network disabled in local benchmark")

    socket.socket.connect = no_network
    socket.socket.connect_ex = no_network

    # Register before SDK imports, so their later exit hooks run first. A
    # background upload attempt must also make the child fail after timing.
    def final_check():
        if network_attempts:
            os._exit(1)

    atexit.register(final_check)
    return network_attempts


def worker(args) -> int:
    local_environment()
    network_attempts = disable_network()
    rows = corpus(args.records)
    imported_at = time.perf_counter()
    from adapters import batch_runner, metric_runner

    if args.tier == "batch":
        run_batch = batch_runner(args.tool, 1)

        def run():
            return run_batch(rows)

        def warmup():
            return run_batch(corpus(12))
    else:
        run = metric_runner(args.tool, rows)
        warmup = metric_runner(args.tool, corpus(12))
    import_seconds = time.perf_counter() - imported_at
    warm_scores = warmup()
    validate(corpus(12), warm_scores)
    cpu_before = time.process_time()
    before = time.perf_counter()
    scores = run()
    seconds = time.perf_counter() - before
    cpu_seconds = time.process_time() - cpu_before
    correctness = validate(rows, scores)
    if network_attempts:
        raise ValueError(f"local SDK attempted network access {len(network_attempts)} times")
    result = {
        "tool": args.tool,
        "python": platform.python_version(),
        "version": importlib.metadata.version(args.tool),
        "tier": args.tier,
        "records": args.records,
        "concurrency": 1,
        "seconds": seconds,
        "cpu_seconds": cpu_seconds,
        "records_per_second": args.records / seconds,
        "import_and_setup_seconds": import_seconds,
        "peak_rss_kib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
        "dataset_sha256": digest(rows),
        "scores_sha256": correctness,
        "scored": len(scores),
        "errors": 0,
        "skipped": 0,
        "network_attempts": 0,
    }
    args.out.write_text(json.dumps(result, indent=2) + "\n")
    return 0


def summarize(samples):
    groups = {}
    for sample in samples:
        key = (sample["tier"], sample["tool"], sample["records"])
        groups.setdefault(key, []).append(sample)
    summaries = []
    for (tier, tool, records), group in sorted(groups.items()):
        seconds = sorted(s["seconds"] for s in group)
        summaries.append(
            {
                "tier": tier,
                "tool": tool,
                "version": group[0]["version"],
                "records": records,
                "trials": len(group),
                "median_seconds": statistics.median(seconds),
                "min_seconds": min(seconds),
                "max_seconds": max(seconds),
                "p95_batch_seconds": seconds[math.ceil(len(seconds) * 0.95) - 1],
                "median_records_per_second": statistics.median(
                    s["records_per_second"] for s in group
                ),
                "median_peak_rss_kib": statistics.median(s["peak_rss_kib"] for s in group),
            }
        )
    return summaries


def orchestrate(args) -> int:
    interpreters = json.loads(args.interpreters.read_text())
    jobs = [
        (tier, tool, records, trial)
        for tier in args.tiers
        for tool in args.tools
        if tier != "metric" or tool in METRIC_TOOLS
        for records in args.sizes
        for trial in range(args.repetitions)
    ]
    random.Random(args.seed).shuffle(jobs)
    samples, failures = [], []
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(
        subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip()
    )
    report = {
        "schema_version": 1,
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": commit,
        "source_dirty": dirty,
        "seed": args.seed,
        "environment": {
            "system": platform.system(),
            "architecture": platform.machine(),
            "python": platform.python_version(),
            "logical_cpus": os.cpu_count(),
        },
        "scope": (
            "Local, serial exact-match SDK/metric overhead on nonempty strings without "
            "leading/trailing whitespace. No hosted, judge-quality, sandbox, or "
            "production-scale comparison."
        ),
        "samples": samples,
        "failures": failures,
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    for position, (tier, tool, records, trial) in enumerate(jobs, 1):
        with tempfile.TemporaryDirectory(prefix="evalsi-benchmark-") as tmp:
            out = Path(tmp) / "result.json"
            command = [
                interpreters[tool],
                str(Path(__file__).resolve()),
                "--worker",
                "--tool",
                tool,
                "--tier",
                tier,
                "--records",
                str(records),
                "--out",
                str(out),
            ]
            env = dict(os.environ, PYTHONHASHSEED="0")
            before = time.perf_counter()
            try:
                result = subprocess.run(
                    command,
                    cwd=tmp,
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=args.timeout,
                    check=False,
                )
                if result.returncode:
                    raise ValueError((result.stderr or result.stdout)[-3000:])
                sample = json.loads(out.read_text())
                sample.update(trial=trial, process_seconds=time.perf_counter() - before)
                samples.append(sample)
                print(
                    f"{position}/{len(jobs)} {tier} {tool} n={records}: "
                    f"{sample['records_per_second']:.1f} records/s",
                    flush=True,
                )
            except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
                failures.append(
                    {
                        "tier": tier,
                        "tool": tool,
                        "records": records,
                        "trial": trial,
                        "error": str(exc),
                    }
                )
                print(f"{position}/{len(jobs)} FAILED {tier} {tool}: {exc}", flush=True)
        report["summary"] = summarize(samples)
        args.out.write_text(json.dumps(report, indent=2) + "\n")
    # All tools must produce the same full per-record score vector at a size.
    for records in args.sizes:
        for field in ("dataset_sha256", "scores_sha256"):
            if len({s[field] for s in samples if s["records"] == records}) > 1:
                failures.append({"records": records, "error": f"cross-tool {field} mismatch"})
    if len({s["python"] for s in samples}) > 1:
        failures.append({"error": "tools used different Python versions"})
    args.out.write_text(json.dumps(report, indent=2) + "\n")
    return 1 if failures else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--tool", choices=TOOLS)
    parser.add_argument("--tier", choices=("batch", "metric"))
    parser.add_argument("--records", type=int, default=256)
    parser.add_argument("--interpreters", type=Path)
    parser.add_argument("--tools", nargs="+", choices=TOOLS, default=list(TOOLS))
    parser.add_argument(
        "--tiers", nargs="+", choices=("batch", "metric"), default=["batch", "metric"]
    )
    parser.add_argument("--sizes", nargs="+", type=int, default=[256, 2048])
    parser.add_argument("--repetitions", type=int, default=7)
    parser.add_argument("--seed", type=int, default=20261018)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if min([args.records, args.repetitions, args.timeout, *args.sizes]) < 1:
        parser.error("sizes, repetitions and timeout must be positive")
    if args.worker:
        if not args.tool or not args.tier:
            parser.error("worker requires tool and tier")
        return worker(args)
    if not args.interpreters:
        parser.error("supply --interpreters (a tool-to-Python-path JSON mapping)")
    return orchestrate(args)


if __name__ == "__main__":
    raise SystemExit(main())

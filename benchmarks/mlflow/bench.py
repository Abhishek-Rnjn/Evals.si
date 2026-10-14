"""Run the Evals.si vs MLflow benchmark and write results.json and results.md.

    python bench.py --evalsi-python /path/to/evalsi/venv/bin/python \
                    --mlflow-python /path/to/mlflow/venv/bin/python \
                    --workdir /tmp/bench

Each measurement runs in a fresh subprocess. See README.md for what is measured,
what is not, and why.
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
MOCK_PORT = 8767


def run(cmd: list[str], env: dict[str, str] | None = None, timeout: int = 1800) -> tuple[float, str, str]:
    """Run a command; return (wall seconds, stdout, stderr)."""
    full_env = dict(os.environ)
    full_env.update(env or {})
    t = time.perf_counter()
    p = subprocess.run(cmd, capture_output=True, text=True, env=full_env, timeout=timeout)
    wall = time.perf_counter() - t
    if p.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)} failed:\n{p.stderr[-2000:]}")
    return wall, p.stdout, p.stderr


def last_json(stdout: str) -> dict:
    for line in reversed(stdout.strip().splitlines()):
        line = line.strip()
        if line.startswith("{"):
            return json.loads(line)
    raise RuntimeError(f"no JSON in output:\n{stdout[-500:]}")


def med(xs: list[float]) -> float:
    return round(statistics.median(xs), 3)


def summarize(runs: list[dict]) -> dict:
    out = {
        "reps": len(runs),
        "wall_s_median": med([r["wall_s"] for r in runs]),
        "wall_s_min": round(min(r["wall_s"] for r in runs), 3),
        "wall_s_max": round(max(r["wall_s"] for r in runs), 3),
        "eval_s_median": med([r["eval_s"] for r in runs]),
        "import_s_median": med([r["import_s"] for r in runs]),
        "peak_rss_mb_median": med([r["peak_rss_mb"] for r in runs]),
        "stalled_runs": sum(1 for r in runs if r["eval_s"] > 25),
        "lost_trace_warnings": sum(r.get("lost_traces", 0) for r in runs),
    }
    return out


def mock_start(workdir: Path, latency_ms: float) -> subprocess.Popen:
    p = subprocess.Popen(
        [sys.executable, str(HERE / "mock_judge.py"), "--port", str(MOCK_PORT), "--latency-ms", str(latency_ms)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    for _ in range(50):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{MOCK_PORT}/stats", timeout=1).read()
            return p
        except Exception:
            time.sleep(0.1)
    raise RuntimeError("mock judge did not start")


def mock_reset() -> None:
    urllib.request.urlopen(urllib.request.Request(f"http://127.0.0.1:{MOCK_PORT}/reset", method="POST", data=b"{}")).read()


def mock_stats() -> dict:
    return json.loads(urllib.request.urlopen(f"http://127.0.0.1:{MOCK_PORT}/stats").read())


def one(tool: str, py: str, mode: str, data: Path, workdir: Path, extra: list[str], env: dict[str, str], tag: str) -> dict:
    out = workdir / f"out-{tag}.json"
    script = HERE / ("run_evalsi.py" if tool == "evalsi" else "run_mlflow.py")
    cmd = [py, str(script), mode, "--data", str(data), "--out", str(out), *extra]
    wall, stdout, stderr = run(cmd, env)
    res = last_json(stdout)
    res["wall_s"] = wall
    res["lost_traces"] = stderr.count("Failed to send trace to MLflow backend")
    return res


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--evalsi-python", required=True)
    parser.add_argument("--mlflow-python", required=True)
    parser.add_argument("--workdir", required=True)
    parser.add_argument("--sizes", default="100,1000,5000")
    parser.add_argument("--reps", type=int, default=5)
    parser.add_argument("--judge-n", type=int, default=200)
    parser.add_argument("--judge-latency-ms", type=float, default=50.0)
    parser.add_argument("--judge-concurrency", type=int, default=8)
    parser.add_argument("--default-config-sizes", default="20,100", help="sizes for MLflow's untuned defaults (they can stall 30 s a run)")
    parser.add_argument("--default-config-reps", type=int, default=3)
    args = parser.parse_args()

    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    sizes = [int(s) for s in args.sizes.split(",")]
    small = [int(s) for s in args.default_config_sizes.split(",")]
    results: dict = {"env": {}, "det": {}, "agreement": {}, "judge": {}}

    for n in sorted(set(sizes + small + [args.judge_n])):
        subprocess.run([sys.executable, str(HERE / "gen_data.py"), "--n", str(n), "--out", str(workdir / f"data-{n}.jsonl")], check=True)

    ev_env = {"BENCH_KEY": "x"}
    ml_env = {"MLFLOW_DISABLE_AGENT_HINT": "1"}
    ml_tuned = {**ml_env, "MLFLOW_GENAI_EVAL_MAX_WORKERS": "10"}

    results["env"] = {
        "cpus": os.cpu_count(),
        "python": run([args.evalsi_python, "--version"])[1].strip(),
        "evalsi": run([args.evalsi_python, "-c", "import importlib.metadata as m;print(m.version('evalsi'))"])[1].strip(),
        "mlflow": run([args.mlflow_python, "-c", "import importlib.metadata as m;print(m.version('mlflow'))"])[1].strip(),
    }

    # ---- deterministic speed ------------------------------------------------
    configs = {
        "evalsi": (args.evalsi_python, "evalsi", [], ev_env, sizes),
        "mlflow-tuned": (args.mlflow_python, "mlflow", [], ml_tuned, sizes),
        "mlflow-default": (args.mlflow_python, "mlflow", [], ml_env, small),
    }
    for name, (py, tool, extra, env, ns) in configs.items():
        for n in ns:
            data = workdir / f"data-{n}.jsonl"
            e2 = list(extra)
            if tool == "mlflow":
                db = workdir / f"mlflow-{name}-{n}.db"
                if db.exists():
                    db.unlink()
                e2 += ["--tracking-uri", f"sqlite:///{db}"]
                one(tool, py, "det", data, workdir, e2, env, f"warm-{name}-{n}")  # untimed warm-up: schema and experiment
            reps = args.default_config_reps if name == "mlflow-default" else args.reps
            runs = []
            for i in range(reps):
                runs.append(one(tool, py, "det", data, workdir, e2, env, f"{name}-{n}-{i}"))
                print(f"det {name} n={n} rep {i + 1}/{reps}: wall {runs[-1]['wall_s']:.2f}s eval {runs[-1]['eval_s']:.2f}s", flush=True)
            results["det"].setdefault(name, {})[str(n)] = summarize(runs)

    # ---- agreement ----------------------------------------------------------
    n = 1000
    data = workdir / f"data-{n}.jsonl"
    db = workdir / "mlflow-agree.db"
    if db.exists():
        db.unlink()
    one("mlflow", args.mlflow_python, "det", data, workdir, ["--tracking-uri", f"sqlite:///{db}"], ml_tuned, "agree-mlflow")
    one("evalsi", args.evalsi_python, "det", data, workdir, ["--exact-normalize", "true"], ev_env, "agree-evalsi-norm")
    one("evalsi", args.evalsi_python, "det", data, workdir, ["--exact-normalize", "false"], ev_env, "agree-evalsi-strict")
    ml = json.loads((workdir / "out-agree-mlflow.json").read_text())["scores"]
    ev_norm = json.loads((workdir / "out-agree-evalsi-norm.json").read_text())["scores"]
    ev_strict = json.loads((workdir / "out-agree-evalsi-strict.json").read_text())["scores"]

    def disagreements(ev: dict, metric: str) -> list[str]:
        return [r for r in ml if ml[r][metric] != ev[r][metric]]

    gen = {r["id"]: r for r in map(json.loads, open(data))}
    results["agreement"] = {
        "records": len(ml),
        "exact-match (evalsi normalize=true vs mlflow strict ==)": len(disagreements(ev_norm, "exact-match")),
        "exact-match (evalsi normalize=false vs mlflow strict ==)": len(disagreements(ev_strict, "exact-match")),
        "contains": len(disagreements(ev_norm, "contains")),
        "regex-match (both built in)": len(disagreements(ev_norm, "regex-match")),
        "exact-match disagreement examples": [
            {"output": gen[r]["output"], "reference": gen[r]["reference"], "evalsi": ev_norm[r]["exact-match"], "mlflow": ml[r]["exact-match"]}
            for r in disagreements(ev_norm, "exact-match")[:4]
        ],
    }

    # ---- judge calls, tokens and caching -------------------------------------
    mock = mock_start(workdir, args.judge_latency_ms)
    try:
        url = f"http://127.0.0.1:{MOCK_PORT}/v1"
        data = workdir / f"data-{args.judge_n}.jsonl"
        c = args.judge_concurrency
        jruns: dict[str, dict] = {}

        def judged(label: str, tool: str, py: str, extra: list[str], env: dict[str, str]) -> None:
            mock_reset()
            r = one(tool, py, "judge", data, workdir, extra + ["--judge-url", url], env, f"judge-{label}")
            s = mock_stats()
            r.update({"judge_calls": s["calls"], "prompt_tokens": s["prompt_tokens"], "completion_tokens": s["completion_tokens"]})
            jruns[label] = {k: r[k] for k in ("wall_s", "eval_s", "judge_calls", "prompt_tokens", "completion_tokens", "lost_traces")}
            print(f"judge {label}: {jruns[label]}", flush=True)

        judged("evalsi (cache cold)", "evalsi", args.evalsi_python, ["--concurrency", str(c), "--no-cache"], ev_env)
        # Evals.si caches judge responses on disk by content hash; the second run
        # of an unchanged evaluation should make no calls.
        judged("evalsi (cache on, 1st run)", "evalsi", args.evalsi_python, ["--concurrency", str(c), "--cache-dir", str(workdir / "evalsi-cache")], ev_env)
        judged("evalsi (cache on, 2nd run)", "evalsi", args.evalsi_python, ["--concurrency", str(c), "--cache-dir", str(workdir / "evalsi-cache")], ev_env)

        db = workdir / "mlflow-judge.db"
        if db.exists():
            db.unlink()
        mlenv = {**ml_env, "MLFLOW_GENAI_EVAL_MAX_WORKERS": str(c)}
        one("mlflow", args.mlflow_python, "det", workdir / "data-20.jsonl", workdir, ["--tracking-uri", f"sqlite:///{db}"], mlenv, "judge-warm")
        judged("mlflow (1st run)", "mlflow", args.mlflow_python, ["--tracking-uri", f"sqlite:///{db}"], mlenv)
        judged("mlflow (2nd run)", "mlflow", args.mlflow_python, ["--tracking-uri", f"sqlite:///{db}"], mlenv)
        results["judge"] = {"records": args.judge_n, "latency_ms": args.judge_latency_ms, "concurrency": c, "runs": jruns}
    finally:
        mock.terminate()

    (workdir / "results.json").write_text(json.dumps(results, indent=2))
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()

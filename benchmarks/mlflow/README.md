# Benchmark: Evals.si vs MLflow evaluation

A reproducible comparison of Evals.si and `mlflow.genai.evaluate` on the same records: speed, score agreement and judge cost. Features are compared in the results write-up, not here.

Results are added to this folder when the run finishes (`RESULTS.md`).

## What it measures

| Section | What | How |
|---|---|---|
| Speed | Wall-clock time and peak memory for three deterministic checks (exact match, contains, regex) on 100, 1,000 and 5,000 records | Each measurement is a fresh process; the median of several runs. MLflow gets an untimed warm-up run (database schema and experiment) |
| Agreement | Whether both tools give the same per-record result for the same check | Compares every record; lists disagreements and why |
| Judge cost | Judge calls, tokens and time for two LLM-judge metrics, and what a second identical run costs | Both tools call the same mock OpenAI-compatible server with a fixed latency |

## What it does not measure

- **Judge quality.** The mock judge gives every answer a positive verdict, so this says nothing about which tool's prompts judge better.
- **Absolute speed on your machine.** Numbers depend on the CPU. Compare the ratios, and rerun on your hardware.
- **MLflow's UI, tracing and registry.** MLflow also stores a trace per record in its tracking store, which Evals.si does not do. That is part of what `mlflow.genai.evaluate` does, so it is part of its time.

## Fairness notes

- MLflow is measured in two configurations. **Defaults** is `pip install mlflow` with nothing set. **Tuned** sets `MLFLOW_GENAI_EVAL_MAX_WORKERS=10`, which is the documented default value, but setting it explicitly changes how MLflow sizes its thread pools (see below). Both are reported.
- The tracking store is MLflow's default, SQLite.
- Evals.si runs in-process through `evalsi.evaluate`. The time includes interpreter start and import in the "wall" column, and excludes them in "eval".

### An MLflow behavior worth knowing

In MLflow 3.17.1, when `MLFLOW_GENAI_EVAL_MAX_WORKERS` is not set, the evaluation sizes its thread pools from a rate limit (10 to 500 threads). With the default SQLite store, whose connection pool holds 15 connections, these threads can exhaust the pool. MLflow then logs `Failed to send trace to MLflow backend: QueuePool limit of size 5 overflow 10 reached, connection timed out` after its 30-second pool timeout, and some traces are not saved. Setting the variable (to any value) avoids it. The benchmark counts stalled runs and these warnings.

## Run it

```bash
python -m venv /tmp/bench/evalsi-venv && /tmp/bench/evalsi-venv/bin/pip install evalsi==0.2.0
python -m venv /tmp/bench/mlflow-venv && /tmp/bench/mlflow-venv/bin/pip install mlflow==3.17.1

python benchmarks/mlflow/bench.py \
  --evalsi-python /tmp/bench/evalsi-venv/bin/python \
  --mlflow-python /tmp/bench/mlflow-venv/bin/python \
  --workdir /tmp/bench/run --reps 3
```

It writes `results.json` to the work directory. Two virtual environments are used because the two packages pin different dependencies.

Files: `gen_data.py` (deterministic dataset), `mock_judge.py` (fixed-latency judge with call counting), `run_evalsi.py` and `run_mlflow.py` (one measurement each), `bench.py` (the driver).

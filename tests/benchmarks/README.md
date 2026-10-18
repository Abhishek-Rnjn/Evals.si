# Local evaluation benchmarks

Compare Evals.si with **DeepEval 4.3.1, Ragas 0.4.3, LangSmith SDK 0.14.11,
and Braintrust SDK 0.46.1**, using their real public Python APIs and separate
environments. The four `requirements/` files pin the installed dependency
versions; Evals.si uses the repository's `python/uv.lock` and source revision.
These benchmark versions do not change the versions of Evals.si's adapters.

## Reproduce

Use Linux and Python 3.12, with `uv` installed. From the repository root:

```sh
python tests/benchmarks/setup.py --directory /tmp/evalsi-bench-envs
python tests/benchmarks/compatibility.py \
  --interpreters /tmp/evalsi-bench-envs/interpreters.json --out /tmp/compatibility.json
python tests/benchmarks/evaluation.py \
  --interpreters /tmp/evalsi-bench-envs/interpreters.json \
  --sizes 256 2048 --repetitions 7 --out /tmp/benchmark.json
python/.venv/bin/python -m pytest tests/benchmarks/test_evaluation.py
```

The setup and dependency downloads need network access. **Measurements disable
outbound socket connections**, opt out of telemetry, disable result caching,
and disable hosted uploads. Any attempted connection invalidates a sample.
Only synthetic data is used. Do not run concurrent builds, tests or servers
while collecting timing results. The dependency snapshots were resolved on
Linux x86-64/Python 3.12; other platforms need their own validated locks.
Ragas needs `langchain-community==0.3.31` because its VertexAI import is absent
from 0.4.x; this compatibility constraint is also used by the existing adapter.

## What is measured

- **Batch runner:** full native input conversion, evaluation, result collection
  and score extraction. Each tool uses its batch API and concurrency one;
  displays are disabled where the public API exposes that control. Braintrust
  `Eval` retains its progress/summary bookkeeping, captured through pipes.
  LangSmith and Braintrust require an identity
  task over precomputed outputs; Evals.si, DeepEval and Ragas accept those
  outputs directly. Evals.si computes its normal manifest and intervals;
  DeepEval retains its normal local test-run bookkeeping. Features and their
  overhead differ, so this is not an equal-feature platform comparison.
- **Metric loop:** Evals.si's `EvaluatorDef.fn`, DeepEval's `measure`, and
  Ragas collections' `score`, serially, over native inputs prepared before
  timing. This excludes dataset parsing, orchestration, manifests and gates.
  LangSmith and Braintrust are excluded: their custom equality functions
  would measure Python equality rather than a native metric implementation.

Both tiers use the same deterministic corpus: nonempty strings with no
leading/trailing whitespace, including case changes, internal spaces/newlines,
Unicode composition and 4-KiB strings. A separate compatibility run records
empty strings and leading/trailing whitespace. DeepEval 4.3.1 trims string
ends and rejects empty outputs; it is not relabeled as strict byte equality.
Evals.si's usual normalization is explicitly disabled for this comparison.

Every sample runs in a fresh subprocess after a 12-record warmup. Import and
setup time are recorded separately and excluded from timed scoring. Timings
include constructing output score pairs; validation runs afterwards. Every
record must have its expected score exactly once: errors, missing/duplicate
rows, unexpected IDs, nonfinite scores, and cross-tool dataset/result hash
disagreements fail the run. Job order is shuffled with a recorded seed.

The JSON includes every raw trial, versions, source revision/dirty state,
wall/CPU time, process duration, peak RSS, corpus/result hashes, and summaries.
RSS includes imports and warmup. `p95_batch_seconds` is a nearest-rank percentile
across **seven whole batches**, not per-record latency or a reliable production
tail estimate. Report medians and ranges alongside the raw data.

## Hosted and free editions

The local LangSmith/Braintrust SDK measurements make **no claim about their
hosted services**. Free allowances still require the user's account/workspace
and API key. No paid tier or judge API is needed for this deterministic task.

A hosted comparison must use the same synthetic data and concurrency, include
upload and persistence, read back all persisted scores/IDs, record deployment
region and service tier, and clean up only its disposable experiment. Report
upload/score/read-back time separately from local computation. Keep trial
counts within the workspace's free allowance; never silently turn on paid
usage. Evals.si's unary `Evaluate` response is not a substitute for persisted
results in that comparison. Hosted numbers remain unmeasured until those
prerequisites and equivalent persistence paths are available.

Official API/method sources:

- [LangSmith evaluate](https://reference.langchain.com/python/langsmith/evaluation/_runner/evaluate)
  (`upload_results=False`) and [free allowances](https://www.langchain.com/pricing).
- [Braintrust Python SDK](https://www.braintrust.dev/docs/sdks/python/versions/0.46.1)
  (`no_send_logs=True`) and [pricing](https://www.braintrust.dev/pricing).
- [DeepEval Exact Match](https://deepeval.com/docs/metrics-exact-match). Its
  installed 4.3.1 implementation is checked by the compatibility run; the
  runtime whitespace behavior differs from that page's description.
- [Ragas traditional metrics](https://docs.ragas.io/en/stable/concepts/metrics/available_metrics/traditional/).
- [Phoenix self-hosting](https://arize.com/docs/phoenix/self-hosting/deploying-phoenix)
  and [Langfuse free self-hosting](https://langfuse.com/pricing-self-host).

These checks measure deterministic framework overhead, not judge quality,
agent success, tracing throughput, isolation strength, total cloud cost, or
production-scale reliability. They do not establish an overall winner.

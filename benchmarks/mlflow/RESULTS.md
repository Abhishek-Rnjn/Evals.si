# Results: Evals.si vs MLflow evaluation

Evals.si 0.2.0 and MLflow 3.17.1 (the version Evals.si's MLflow trace-source integration is verified against), each in its own virtual environment, on a 4-vCPU Linux VM with Python 3.13. Each measurement is a fresh process. Speed numbers are the median of 3 runs. Read [README.md](README.md) first for what is and isn't measured.

**Summary:** on the same records and the same checks, Evals.si finished 52 to 673 times faster in wall-clock time and used about a tenth of the memory, made the same decisions wherever the two tools' rules are the same, and made no judge calls on a repeated run. MLflow did far more per record: it saves a trace and its scores for each one, and it has a UI, a tracking store and features Evals.si does not. The comparison below is about the cost of that.

## 1. Speed: three deterministic checks (exact match, contains, regex)

"Wall" is the whole process, including interpreter start and import. "Eval" is the evaluation call only.

| Records | Evals.si wall | Evals.si eval | MLflow tuned wall | MLflow tuned eval | Wall ratio | Eval ratio |
|---:|---:|---:|---:|---:|---:|---:|
| 100 | 0.28 s | 0.06 s | 14.3 s | 11.3 s | 52× | 199× |
| 1,000 | 0.32 s | 0.12 s | 97.0 s | 94.1 s | 308× | 797× |
| 5,000 | 0.69 s | 0.43 s | 460.8 s | 457.4 s | 673× | 1,059× |

Throughput of the evaluation call: Evals.si about 11,600 records per second at 5,000 records; MLflow about 11 records per second, flat from 1,000 to 5,000. Peak memory: Evals.si 34, 38 and 53 MB; MLflow 330, 383 and 603 MB.

MLflow's time grows linearly with records and does not improve with scale. Its work includes writing a trace and the scorer results for every record into its SQLite tracking store; I saw those traces in `result_df`. I did not profile which part dominates.

### MLflow with its defaults is slower and can lose traces

"Tuned" sets `MLFLOW_GENAI_EVAL_MAX_WORKERS=10`. With nothing set (what `pip install mlflow` gives you), MLflow 3.17.1 sizes its thread pools from a rate limit instead, and with the default SQLite store this repeatedly exhausted the store's connection pool:

| Records | Median eval time, defaults | Runs that stalled | `Failed to send trace` warnings (3 runs) |
|---:|---:|---:|---:|
| 20 | 33.1 s | 3 of 3 | 25 |
| 100 | 155.2 s | 3 of 3 | 227 |

Each stall is the 30-second connection-pool timeout. Some traces were not saved. Setting the worker variable (to any value) removed the stalls in my runs, and it is the configuration used for the speed table above. I show defaults because it is what a new user gets. A different tracking backend, such as a server on PostgreSQL, may behave differently; I did not test one.

## 2. Score agreement (1,000 records)

| Check | Disagreements | Notes |
|---|---:|---|
| `regex-match` (both built in) | 0 | |
| `contains` | 0 | MLflow side is a custom scorer I wrote with the same definition |
| `exact-match`, Evals.si with `normalize=false` | 0 | MLflow side is a strict `==` custom scorer |
| `exact-match`, Evals.si default (`normalize=true`) | 90 | Evals.si ignores case and surrounding spaces by default; a strict comparison does not |

The 90 are not errors in either tool. For example `"  Ottawa 617  "` against `"Ottawa 617"` is a match in Evals.si and not in a strict `==`. If you port an MLflow exact-match scorer to Evals.si, set `normalize=false` to keep the old behavior, or keep the default if you want the forgiving one. MLflow has no built-in exact-match scorer, so its side here is code I wrote, which makes this a test of definitions, not of the tools' own implementations.

## 3. Judge cost: two LLM-judge metrics, 200 records, mock judge at 50 ms

Both tools call the same mock OpenAI-compatible server (concurrency 8). The mock gives every answer a positive verdict, so this is cost and overhead, not judge quality.

| Run | Evals.si | MLflow |
|---|---:|---:|
| Judge calls, first run | 400 | 400 |
| Request tokens, first run (est.) | 152,000 | 145,000 |
| Eval time, first run | 3.5 s | 21.9 s |
| Judge calls, **second identical run** | **0** | **400** |
| Eval time, second run | 0.15 s | 21.6 s |

The ideal time at concurrency 8 is 400 × 50 ms / 8 = 2.5 s. Evals.si took 3.5 s; MLflow took 21.9 s. Evals.si caches judge responses by content, so rerunning an unchanged evaluation costs nothing, which matters when a CI job reruns the same suite. In MLflow 3.17.1 the second run repeated all 400 calls. Both tools sent a similar amount of request text per call (within 5%). The completion tokens are not compared, because the mock chooses the reply for each tool's schema.

The two tools use different judge prompts and metrics (`llm-judge` with the correctness rubric and `answer-relevance` against MLflow's `Correctness` and `RelevanceToQuery`), so their scores are not comparable and this benchmark does not try to compare them.

## 4. Install footprint

| | Evals.si | MLflow |
|---|---:|---:|
| `pip install` time, no cache | 15 s | 75 s |
| Packages installed | 11 | 88 |
| Disk (site-packages) | 87 MB | 804 MB |

The 87 MB for Evals.si includes the bundled `evalsid` server and sandbox binary in the Linux wheel.

## 5. What each does (not a speed question)

Verified against the MLflow 3.17.1 package and the Evals.si repository.

| | Evals.si | MLflow (`mlflow.genai`) |
|---|---|---|
| Score recorded outputs, built-in checks and judges | Yes: core, text, RAG, safety and agent packs | Yes: built-in scorers (Correctness, RelevanceToQuery, RetrievalGroundedness, Safety, Guidelines, ToolCallCorrectness and more) and custom `@scorer` |
| Run an agent on sandboxed tasks, graded by a hidden checker | Yes (Firecracker, bubblewrap, Landlock, pods), with pass@k and pass^k | No sandboxed execution found in the package |
| Fail a build when a gate fails | Yes: `evalsi run` exits 3 | No built-in gate with an exit code found; you assert on the result yourself |
| RL rewards and checkpoint regression checks | Yes | None found |
| Score live traces | Yes: online policies, and read MLflow, Phoenix or Langfuse traces and write scores back | Yes: online scoring of traces |
| Judge cache | Yes, by content | None found in my runs (second run repeated all calls) |
| UI and persistent history of every record's scores | A read-only web UI on the server | Yes: full tracking UI, experiments and traces |
| Tracing and autologging for many frameworks | Ingests OpenTelemetry traces | Yes: its core strength |
| Prompt registry and judge or prompt optimization | No | Yes: registry, DSPy, GEPA, SIMBA and MemAlign optimizers |
| Human labeling and review queues | Annotation queues with agreement measures | Yes: label schemas and review queues |
| Conversation simulators | A user simulator for multi-turn agent tasks | Yes: simulators |
| Use other metric libraries | DeepEval, RAGAS, lm-eval and Inspect run as adapters | Scorer integrations for DeepEval, RAGAS, Phoenix, TruLens and Guardrails |

They also work together: Evals.si can read traces from an MLflow server, score them, and write the scores back as MLflow assessments ([trace sources](../../docs/guides/trace-sources.md)).

## Limits of this benchmark

- One machine, one run of the whole suite, 3 repeats per size. Treat small differences as noise; the large ratios are not.
- A mock judge with fixed latency measures overhead and call counts, not real model behavior.
- MLflow's time includes saving a trace per record, which Evals.si does not do. If you do not need MLflow's history of every record, that work is overhead for you; if you do, it is the feature.
- MLflow was tested with its default SQLite store only.
- I wrote the MLflow exact-match and contains scorers myself, as MLflow has no built-in ones.
- Versions: Evals.si 0.2.0, MLflow 3.17.1. Both change; rerun with `bench.py`.

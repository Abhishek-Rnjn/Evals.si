# Framework adapters

Adapters bring other evaluation frameworks into Evals.si. They also expose
our evaluators to those frameworks. Each adapter is its own package, with its
own pinned environment and lockfile, so upstream dependency conflicts never
reach the SDK or the other adapters. CI runs each adapter's contract tests
against the real upstream package, offline, using a scripted judge or target
from `evalsi.testing`.

| Package | Upstream | What it adds |
|---|---|---|
| `evalsi-adapter-deepeval` | `deepeval==4.2.8` | Pack `deepeval`: `faithfulness`, `answer-relevancy`, `contextual-precision`, `contextual-recall`, `contextual-relevancy`, `hallucination`, `bias`, `toxicity`, `summarization`, `tool-correctness`, `argument-correctness`, `g-eval` |
| `evalsi-adapter-ragas` | `ragas==0.4.3` | Pack `ragas`: `faithfulness`, `context-precision`, `context-utilization`, `context-recall`, `context-entity-recall`, `context-relevance`, `response-groundedness`, `answer-accuracy`, `factual-correctness`, `noise-sensitivity`, `agent-goal-accuracy` (with and without reference), `topic-adherence`, `tool-call-accuracy`, `tool-call-f1` |
| `evalsi-adapter-inspect` | `inspect-ai==0.3.276` | Importer `inspect://log.eval`, which turns Inspect samples into records with trajectories and Inspect's scores. `evalsi_scorer(...)` runs any Evals.si evaluator as an Inspect scorer. |
| `evalsi-adapter-lm-eval` | `lm-eval==0.4.13` | lm-eval model `evalsi`, which generates through Evals.si target connectors. `evaluate(tasks, target)` returns records. Importer `lm-eval://samples_<task>_<date>.jsonl`. |
| `evalsi-adapter-swebench` | `swebench==5.0.2` | Importer `swebench://` for SWE-bench 5 instances as agent tasks (the instance's image, repository at `/testbed`), graded by the instance's eval script and `swebench`'s own report. A fixture dataset (`python -m evalsi_swebench.fixture`) for offline runs. |
| `evalsi-adapter-taubench` | tau2-bench v0.2.0 (Python 3.12) | Importer `taubench://<domain>` and harness `evalsi_taubench:TauBenchHarness`: the domain's tools and policy, the simulated user, tau2's own evaluator. Needs tau2's `data/` (`TAU2_DATA_DIR`). |
| `evalsi-adapter-bfcl` | `bfcl-eval==2026.3.23` | Importer `bfcl://<category>` (single-turn categories) and harness `evalsi_bfcl:BFCLHarness`, graded by BFCL's AST checker. |

Harbor (Terminal-Bench 2) and Terminal-Bench 1 task directories need no
adapter: `harbor://` and `terminal-bench://` ship with `evalsi-harness`. See
the [agent runs guide](../../docs/guides/agent-runs.md#benchmarks).

DeepEval and RAGAS keep their own prompts and scoring. Their model calls are
answered by the run's judge through `evalsi.judges.structured.JudgeSession`,
which means:

- calls are cached, rate-limited and costed like every other judge call;
- schemas are made strict for structured output;
- the framework never reads a provider key.

The upstream package is imported only when a metric runs, so installing an
adapter does not slow down `evalsi`.

```bash
cd python/adapters/ragas && uv sync          # an isolated environment for this adapter
uv run evalsi eval --data qa.jsonl --evaluators ragas/faithfulness,deepeval/g-eval \
    --judge-provider anthropic --judge-model claude-opus-5-5
```

Importers plug into the `evalsi.importers` entry-point group and work anywhere
a dataset does. That includes `evalsi eval --data` and the `uri` of a run
spec's dataset. On a server, importer paths resolve inside `datasets_dir`
just like plain paths.

## Writing an adapter

1. Create a package that depends on `evalsi` and on one pinned upstream version.
2. Register a `Pack` under `evalsi.packs` for metrics, or a callable
   `(path, **options) -> records` under `evalsi.importers` for logs.
3. Route model-graded work through `EvalContext.judge`. Never construct
   provider clients yourself.
4. Write contract tests against the real upstream package. Use
   `evalsi.testing.scripted_judge` so the tests stay offline.
5. For a benchmark, return agent tasks: put each task's environment in
   `metadata["environment"]` and grade with the benchmark's own code, either
   as a checker parser (`module:function`) or in a harness subclass of
   `evalsi_harness.BuiltinHarness` (see the SWE-bench, τ-bench and BFCL
   adapters).

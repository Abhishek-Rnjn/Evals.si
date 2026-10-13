# evalsi

**One entrypoint to evaluate classic ML models, LLMs, RAG systems, agents and RL checkpoints, offline and online.**

`evalsi` is the Python SDK and CLI of [Evals.si](https://github.com/Abhishek-Rnjn/Evals.si), a pluggable, sandboxed and self-hosted evaluation service. The same evaluators score outputs you already have, run a model or an agent on a dataset and gate on the result, score live agent traffic, and reward RL training, so a metric means the same thing in CI, in production and in training.

[Documentation](https://abhishek-rnjn.github.io/Evals.si/) ·
[GitHub](https://github.com/Abhishek-Rnjn/Evals.si) ·
[Changelog](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/CHANGELOG.md) ·
[Examples](https://github.com/Abhishek-Rnjn/Evals.si/tree/main/examples)

## Why Evals.si

- **Four ways in.** **Score** outputs you have, **Run** a target (model, RAG pipeline or agent) on a dataset with trials, gates and budgets, **Watch** live traces with online policies, and **Reward** RL training with the same evaluators.
- **Honest numbers.** Every mean has a confidence interval. Records an evaluator can't judge are skipped, never scored as zero, and evaluator failures are errors. There is no default judge, so nothing is billed by surprise.
- **Agents, offline and online.** Agents run on sandboxed tasks (SWE-bench, τ-bench, Terminal-Bench, BFCL), and production traces from LangGraph, CrewAI, the OpenAI Agents SDK, LlamaIndex and the Claude Agent SDK are scored as they arrive.
- **Works with your trace store.** Push OpenTelemetry, or read MLflow, Phoenix or Langfuse and write the scores back where your team already looks.
- **Pluggable.** Your evaluators are Python functions or sandboxed Wasm modules; lm-evaluation-harness, Inspect AI, RAGAS and DeepEval run as adapters.
- **Fails closed.** Untrusted code runs in Firecracker microVMs, bubblewrap, Landlock or hardened pods, never with weaker isolation than asked.

## Install

```bash
pip install evalsi                    # SDK and CLI
pip install 'evalsi[anthropic]'       # Claude as a judge or target
pip install 'evalsi[server]'          # evalsi serve: the evaluator worker for the server
pip install 'evalsi[analytics]'       # evalsi analyze: DuckDB across runs
```

Other extras: `hf` (`hf://` datasets), `grpc` (streaming client), `jsonschema`, `math` (symbolic answer checks). Python 3.11 or newer.

On Linux (amd64 and arm64), `pip` installs a platform wheel that also carries the `evalsid` server and its static bubblewrap, so `evalsi serve`, sandboxed code evaluators and Wasm plugins need nothing else (`evalsi evalsid-path` shows the one in use). Elsewhere you get the pure-Python wheel; point `EVALSID` at an `evalsid` from the [releases page](https://github.com/Abhishek-Rnjn/Evals.si/releases) when you need the server.

## Quickstart

Score records you already have:

```bash
evalsi eval --data qa.jsonl --evaluators exact-match,numeric-match,latency
```

```text
metric         n  mean     95% CI              skipped  errors
exact-match    6  0.333    [0.097, 0.700]            0       0
numeric-match  3  0.667    [0.208, 0.939]            3       0
latency        6  517.500  [276.409, 758.591]        0       0
```

Add a judge explicitly, any OpenAI-compatible server or Claude:

```bash
evalsi eval --data qa.jsonl --evaluators llm-judge \
  --judge-provider anthropic --judge-model claude-opus-5-5 --param llm-judge.rubric=helpfulness
```

From Python, with your own evaluator beside the built-in ones:

```python
import evalsi
from evalsi import Record, Score, evaluator


@evaluator(name="acme/polite", version="1.0.0")
def polite(record: Record) -> Score:
    return Score(passed="please" in record.output.as_text().lower())


result = evalsi.evaluate("qa.jsonl", ["exact-match", polite])
print(result.table())
result.save("results.json")  # manifest, summaries and every per-record result
```

## Runs and CI gates

A run spec generates outputs from a target, scores them and checks gates. The same file runs in-process, on a server, or as a Kubernetes `EvalRun`; a failed gate exits 3, so CI fails.

```bash
evalsi run -f run.yaml                                   # embedded
evalsi run -f run.yaml --server https://evalsi.example   # on a server
evalsi compare --server https://evalsi.example <baseline-run> <candidate-run>
evalsi gate -f gate.yaml --server https://evalsi.example --release v42   # a deploy gate
```

## On a server

`evalsid` serves the same evaluators over gRPC, HTTP/JSON and REST, with durable runs, online policies, trace sources, webhooks, auth and a web UI. From Python:

```python
import evalsi
from evalsi.client import Client

with Client("https://evalsi.example") as client:  # API key from EVALSI_API_KEY
    print(client.evaluate("qa.jsonl", ["exact-match"]).table())
    client.log_run(
        evalsi.evaluate("qa.jsonl", ["exact-match"]), name="nightly"
    )  # store a local result
```

`Client.evaluate()` returns the same result as in-process `evalsi.evaluate()`, intervals included.

## Rewards for RL

A reward spec turns evaluators into a weighted, gated reward, in-process or served to trainers:

```python
from trl import GRPOConfig, GRPOTrainer
from evalsi.rewards.trl import reward_funcs

funcs, weights = reward_funcs("code-reward.yaml")
trainer = GRPOTrainer(
    model=model,
    reward_funcs=funcs,
    args=GRPOConfig(..., reward_weights=weights),
    train_dataset=dataset,
)
```

verl (`evalsi.rewards.verl`) and OpenRLHF (`evalsi.rewards.openrlhf`, or `evalsi rewards serve`) use the same spec. `evalsi checkpoints` evaluates every training checkpoint against the base model.

## Evaluators

| Pack | Evaluators |
|---|---|
| `core` | exact, contains, fuzzy, numeric and regex match; JSON validity and schema; length; latency; tokens; cost |
| `judge` | `llm-judge` with rubrics |
| `text` | BLEU, ROUGE, chrF, token F1 |
| `rag` | faithfulness, answer relevance, context precision and recall, citation accuracy |
| `safety` | PII, secret and canary leaks; refusal; harmlessness |
| `agent` | tool-call accuracy, trajectory match, tool errors, loops, step budget, task success (pass@k, pass^k), code quality |
| `code` | unit tests in the sandbox, Python syntax |
| `rl` | format, math answers, sandboxed code tests, overlong penalty, reward models |
| `finetune` | diversity, calibration, contamination, reward hacking |
| `ml-classic` | classification, regression and ranking metrics |
| `ml-monitoring` | drift (PSI, KS, Jensen-Shannon), data quality, group fairness |

`evalsi catalog` lists every installed evaluator and its params. Third-party packs plug in through the `evalsi.packs` entry point, exactly like the built-in ones.

## More

- [`evalsi-harness`](https://pypi.org/project/evalsi-harness/): the built-in agent harness for agent runs.
- [Guides](https://github.com/Abhishek-Rnjn/Evals.si/tree/main/docs/guides): agent runs, online evaluation, trace sources, fine-tuning and RL, Kubernetes, identity and more.
- `evalsi mcp` serves Evals.si to coding agents over MCP.

Evals.si is pre-1.0: the API is `v1alpha1` and can change between minor versions. Apache-2.0.

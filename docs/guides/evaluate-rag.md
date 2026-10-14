# Evaluate a RAG pipeline

A retrieval-augmented generation (RAG) pipeline can fail in two places: **retrieval** (did it fetch the right passages?) and **generation** (did the answer stick to them?). A single "was the answer good?" score cannot tell you which to fix. Evals.si scores the two halves separately, and the same evaluators run on a dataset in CI and on live traffic in production.

This guide scores a RAG pipeline's recorded answers, first with checks that need no model at all, then with a judge. The files are in [`examples/rag`](../../examples/rag/).

## 1. The record

A RAG record has the question, the answer your pipeline gave, the passages it retrieved and, when you have one, the expected answer:

```json
{"id": "refund-window",
 "input": "How long is the refund window?",
 "output": "Refunds are accepted for 30 days [1].",
 "context": ["Our policy allows refunds within 30 days of purchase.", "Shipping takes 3-5 business days."],
 "reference": "30 days"}
```

- **`input`** is the question and **`output`** the pipeline's answer.
- **`context`** is the list of retrieved passages, in rank order. A citation like `[1]` in the answer points at the first passage.
- **`reference`** is optional: the expected answer, or the ground truth the retrieval should support.

Export these from your pipeline's logs or evaluation harness. If your column names differ, map them with `--map input=question` and so on. To score live traffic instead of a file, see [step 6](#6-score-live-traffic).

## 2. Free checks: no model, no judge

`citation-accuracy` passes when every citation in the answer points at a passage that exists. It needs only the answer and the contexts:

```bash
pip install evalsi
evalsi eval --data rag.jsonl --evaluators citation-accuracy
```

```text
2 records · 1 evaluator

metric             n  mean   95% CI          skipped  errors
citation-accuracy  2  0.500  [0.095, 0.905]        0       0
```

The second record in the example answers "Shipping takes 3-5 business days [4]", but only two passages were retrieved, so citation `[4]` does not exist. The table also gives a 95% confidence interval, so with two records you can see how little a mean of 0.5 proves.

The `core` and `text` packs give more checks without a judge: exact, fuzzy and regex matches, JSON validity, length, latency, token use and cost. See `evalsi catalog` for every evaluator and its parameters.

## 3. Judge-based metrics

The `rag` pack has four metrics that need a judge model. They split along the retrieval and generation line:

| Metric | Half | What it measures |
|---|---|---|
| `context-precision` | Retrieval | Rank-aware precision: how many of the retrieved passages the judge finds relevant to the question, weighted toward the top of the ranking |
| `context-recall` | Retrieval | The share of the reference answer's sentences that the retrieved passages support. Needs a `reference` |
| `faithfulness` | Generation | The share of factual claims in the answer that the retrieved passages support. A claim the passages don't mention counts as unsupported, even if it is true |
| `answer-relevance` | Generation | Whether the answer addresses the question, from 1 to 5 normalized to 0 to 1. It judges relevance, not correctness |

Read them together:

- **Low `context-precision` or `context-recall`:** retrieval is the problem. Look at the chunking, the embedding model, the top-k or the reranker.
- **High retrieval scores but low `faithfulness`:** the model is adding things the passages don't say. Look at the prompt, the model or the temperature.
- **High `faithfulness` but low `answer-relevance`:** the answer sticks to the passages but doesn't answer the question.

There is no default judge, so nothing is billed by surprise. Point Evals.si at any OpenAI-compatible server, such as vLLM, Ollama, LiteLLM or OpenAI itself, or at Claude:

```bash
# A model you serve
evalsi eval --data rag.jsonl \
  --evaluators citation-accuracy,faithfulness,answer-relevance,context-precision,context-recall \
  --judge-model my-judge --judge-base-url http://localhost:8000/v1

# Claude, through the official SDK
pip install 'evalsi[anthropic]'
evalsi eval --data rag.jsonl --evaluators faithfulness,answer-relevance \
  --judge-provider anthropic --judge-model claude-opus-5-5
```

Judge responses are cached by content, so re-running an unchanged evaluation costs nothing. Pass `--no-cache` to force fresh calls.

## 4. Read the results honestly

- **Skipped is not zero.** `context-recall` needs a `reference`. A record without one is counted as *skipped* and left out of the mean, never scored as 0, so a half-labelled dataset does not drag the score down.
- **Errors are not low scores.** If the judge fails or returns something unusable, the record is counted as an *error* and excluded from the metric. Check the `errors` column before you trust a number.
- **Intervals tell you how much to trust a mean.** Small datasets give wide intervals. If your records come in groups (several questions about the same document), pass `--cluster-by <metadata key>` so the interval accounts for them.

## 5. Gate your pipeline in CI

A run spec turns the scores into a verdict. Without a `target`, it scores records you already have, so there is no model to call in the gate itself:

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: rag-gate}
spec:
  dataset: {path: rag.jsonl}
  evaluators:
    - ref: citation-accuracy
  gates:
    - {metric: citation-accuracy, min: 0.9}
```

```bash
evalsi run -f gate.yaml
echo "exit code: $?"
```

```text
2 records

metric             n  mean   95% CI          skipped  errors
citation-accuracy  2  0.500  [0.095, 0.905]        0       0

gates:
  [FAIL] citation-accuracy mean min 0.9 (mean 0.5 is below 0.9)
exit code: 3
```

Exit code 3 means a gate failed, so a CI step that runs this fails the build. Fix the citation (`[4]` to `[2]`) and the same command exits 0. The workflow is the one in [Gate an agent in CI](gate-an-agent-in-ci.md#5-run-it-in-github-actions) with your spec in place of the agent's, and without the sandbox step, which only agent runs need.

Add `faithfulness` and `context-precision` to the evaluators and gates when you have a judge, for example `{metric: faithfulness, min: 0.9}`. Keep the dataset small and fixed in CI, and grow it from real failures: `evalsi promote` adds the failing records of a server run to a dataset.

## 6. Score live traffic

The same evaluators can run on production traffic. Send OpenTelemetry traces to Evals.si, or read them from the MLflow, Phoenix or Langfuse you already use, and an online policy scores a sample as it arrives and writes the scores back next to the traces. Retriever and reranker spans (OpenInference `RETRIEVER` and `RERANKER`) are recorded as retrieval steps in each trace's trajectory.

Metrics that need only the question and the answer, such as `answer-relevance` and judge rubrics, work on any trace. The metrics that read retrieved passages (`faithfulness` and the `context-*` metrics) need those passages on the record, so run `evalsi traces get <trace id>` to see what the evaluators see for one of your traces before you rely on them online. See [trace sources](trace-sources.md) and the [end-to-end guide](end-to-end.md).

## 7. Use RAGAS and DeepEval metrics too

RAGAS and DeepEval run here as adapters, with their own prompts and scoring but their model calls going through your judge, so they are cached, rate-limited and costed like every other judge call:

```bash
cd python/adapters/ragas && uv sync
uv run evalsi eval --data rag.jsonl --evaluators ragas/faithfulness,deepeval/g-eval \
  --judge-model my-judge --judge-base-url http://localhost:8000/v1
```

RAGAS adds context precision and recall, factual correctness and more. DeepEval brings 30 metrics, including G-Eval and JevEval, whose decisions can come from the token logprobs of a model you host: see the [DeepEval and JevEval guide](deepeval.md). Use the built-in `rag` pack when you want few dependencies, and an adapter when you want a specific metric your team already knows.

## Where next

- **Agents with retrieval:** an agent that calls a search tool is graded on its tool calls and trajectory as well as its answer: see [agent runs](agent-runs.md) and [Gate an agent in CI](gate-an-agent-in-ci.md).
- **Tuning the retriever or the model:** [fine-tuning and RL](fine-tuning.md) covers rewards and checkpoint checks for training.
- **Every evaluator and parameter:** `evalsi catalog`, and the [end-to-end guide](end-to-end.md).

# DeepEval metrics and JevEval

The `evalsi-adapter-deepeval` package runs [DeepEval](https://github.com/confident-ai/deepeval)'s
metrics as Evals.si evaluators. It covers agents, RAG, safety and conversations, plus custom
G-Eval and JevEval metrics. Each metric keeps DeepEval's own prompts and scoring. Its model calls
go to the run's judge, so they are cached, rate-limited and costed like every other judge call,
and DeepEval never reads a provider key. You can answer JevEval's questions with a model you host
yourself on vLLM, SGLang or Ollama, with the probabilities read off its token logprobs.

The adapter is pinned to `deepeval==4.2.8` and has its own environment, so DeepEval's
dependencies never reach the SDK:

```bash
cd python/adapters/deepeval && uv sync
uv run evalsi eval --data qa.jsonl --evaluators deepeval/faithfulness,deepeval/answer-relevancy \
  --judge-model my-judge --judge-base-url http://localhost:8000/v1
```

## The metrics

Every metric scores from 0 to 1. Where lower is better, the catalog marks it, as it does for
`hallucination`, `bias` and `toxicity`. Records that lack a field a metric needs are skipped,
never scored as zero.

| Use | Evaluators (`deepeval/...`) | Reads |
|---|---|---|
| RAG | `faithfulness`, `answer-relevancy`, `contextual-precision`, `contextual-recall`, `contextual-relevancy`, `hallucination`, `summarization` | input, output, context, reference |
| Agents | `tool-correctness`, `argument-correctness`, `task-completion`, `step-efficiency`, `plan-quality`, `plan-adherence` | input, output, trajectory, the reference's `tool_calls` |
| Safety and instructions | `bias`, `toxicity`, `pii-leakage`, `misuse`, `non-advice`, `role-violation`, `prompt-alignment` | input, output |
| Conversations | `knowledge-retention`, `conversation-completeness`, `turn-relevancy`, `role-adherence`, `topic-adherence`, `goal-accuracy` | chat messages |
| Custom | `g-eval`, `jev-eval`, `conversational-g-eval`, `conversational-jev-eval` | the fields you choose |

Every metric takes `threshold`, `strict_mode` and `include_reason`, plus `options`, which passes
extra keyword arguments to the DeepEval class. Some need a setting they cannot run without, and
these are required params, so a run that omits one fails at configuration time:

| Evaluator | Required param |
|---|---|
| `prompt-alignment` | `prompt_instructions`: a list of instructions the output should follow |
| `misuse` | `domain`, for example `finance` |
| `non-advice` | `advice_types`, for example `[financial, medical]` |
| `role-violation` | `role` |
| `role-adherence` | `chatbot_role` |
| `topic-adherence` | `relevant_topics` |

### How records map onto DeepEval

- **Single-turn test cases.** `input` becomes `input`, `output` becomes `actual_output`,
  `reference` becomes `expected_output`, and `context` becomes both `context` and
  `retrieval_context`. The trajectory's tool calls become `tools_called`, and the reference's
  `tool_calls` become `expected_tools`.
- **Agent traces.** `step-efficiency`, `plan-quality` and `plan-adherence` need a trajectory.
  `task-completion` uses one when the record has it. The trajectory becomes a DeepEval trace: an
  agent span whose children are the steps, nested by `parent_span_id`. Steps map to `llm`,
  `tool`, `retriever` and `agent` spans, and any other step type maps to a `base` span.
- **Conversations.** The turns are the input's chat messages followed by the output. If the
  output repeats the input's messages, it is taken as the whole transcript. System messages are
  dropped, and a tool message becomes the output of the assistant's tool call with the same id.
  A record without both a user turn and an assistant turn is skipped.

## G-Eval and JevEval: metrics you define

**G-Eval** has a judge score the record against your `criteria` or `evaluation_steps`.
**JevEval** asks typed questions and takes a weighted mean of the answers:

| Question | Written as | Value |
|---|---|---|
| Statement (Noul) | `{statement: ...}` | P(true) |
| Scale (Score) | `{question: ..., levels: [worst, ..., best]}`, 2 to 10 levels | expected level / (levels - 1) |
| Choice | `{question: ..., options: {name: credit}}` | the probability-weighted credit; a `null` credit means "not applicable", and a question that is mostly not applicable is left out |

Each question takes an optional `weight` (default 1). `evaluation_params` picks what the judge
sees: `input`, `actual_output`, `expected_output`, `context`, `retrieval_context` and
`tools_called`, or `[trace]` for the whole trajectory. The default is
`[input, actual_output]`. With `strict_mode`, the score is 1 only when every applicable question
gets its best answer. Each JevEval score carries `confidence` (its least decisive answer) and
`score_breakdown` (every question's probabilities) in its metadata.

The same questions work for RAG and for agents:

```yaml
evaluators:
  - ref: deepeval/jev-eval
    name: grounded-answer
    params:
      evaluation_params: [input, actual_output, retrieval_context]
      questions:
        - statement: Every claim in the answer is supported by the retrieved context.
          weight: 2
        - question: How directly does it answer the question?
          levels: [evasive, partial, direct]
        - question: Does it cite a source?
          options: {cites one: 1.0, no citation: 0.0, nothing to cite: null}
  - ref: deepeval/jev-eval
    name: agent-conduct
    params:
      evaluation_params: [trace]
      questions:
        - statement: The agent confirmed with the user before any irreversible action.
        - question: How many of the tool calls were needed?
          levels: [most were wasted, some were wasted, all were needed]
```

`conversational-g-eval` and `conversational-jev-eval` do the same over a whole conversation. Their
`evaluation_params` are `role`, `content`, `context`, `tools_called`, `chatbot_role` and
`scenario`, and default to `[role, content]`. Both also take `chatbot_role` and `scenario` params.

## Where the probabilities come from: `decisions`

Upstream, JevEval sends its questions to TypeSafe's hosted Jev model, which answers with
calibrated probabilities. Here your judge answers them. The `decisions` param sets how:

| `decisions` | Judge calls | Works with | The probabilities are |
|---|---|---|---|
| `stated` (default) | one per record | any judge, Claude included | written by the judge, after it reasons, as its own estimate. They are not calibrated the way Jev's are. |
| `logprobs` | one per question | a server that returns token logprobs: vLLM, SGLang, Ollama, OpenAI | read off the judge's first token, renormalized over lettered answers, as G-Eval does for its scores |

With `logprobs`, each question is asked on its own with lettered answers (`A`, `B`, ...) and the
reply is capped at one token. The judge's probability for each letter is its probability for that
answer. Two things follow:

- **Turn thinking off.** The first token has to be the answer. A reasoning model that opens with
  a thinking block puts its probability on something other than a letter, and the decision fails
  with an error saying so. Use an instruct model, or serve the model with thinking disabled.
- **Check `min_label_mass`.** The score's metadata reports the smallest share of first-token
  probability that fell on a letter across the record's questions. Near 1, the model answered
  with letters. If it is low, it was starting its reply some other way, and the letters'
  renormalized probabilities say less.

A judge without logprobs (Claude, or a server that does not return them) stops the run with a
configuration error rather than returning made-up numbers.

### System One mode for the built-in metrics

DeepEval's built-in metrics can make their decisions the same way. Set `eval_mode` on any metric
except the G-Eval ones:

- `llm` (default) runs DeepEval's usual LLM chain.
- `system_one` sends the metric's whole decision to the System One questions, with no LLM chain.
- `hybrid` uses the questions at each decision point and the LLM for the rest.

`decisions` applies here too. The adapter always passes `eval_mode` explicitly, so a
`DEEPEVAL_EVAL_MODE` variable in a worker's environment never reroutes a metric to TypeSafe.

```yaml
- ref: deepeval/answer-relevancy
  params: {eval_mode: system_one, decisions: logprobs}
```

## A decider on your own model

Serve an instruct model that returns logprobs. vLLM returns up to 20 per token by default, which
is what the adapter asks for:

```bash
vllm serve Qwen/Qwen2.5-7B-Instruct --port 8000
```

**In-process**, point the run's judge at it:

```bash
uv run evalsi eval --data rag.jsonl --evaluators deepeval/jev-eval \
  --judge-model Qwen/Qwen2.5-7B-Instruct --judge-base-url http://localhost:8000/v1 \
  --param jev-eval.decisions=logprobs \
  --param 'jev-eval.questions=[{"statement": "The answer is supported by the context."}]' \
  --param 'jev-eval.evaluation_params=["input", "actual_output", "retrieval_context"]'
```

A local server needs no key: when `OPENAI_API_KEY` is unset, no `Authorization` header is sent.
Ollama and SGLang work the same way through their OpenAI-compatible endpoints, for example
`--judge-base-url http://localhost:11434/v1` for Ollama, if your version returns logprobs.

**On a server**, register the model as a named judge and pick it with the run's `judge`, or the
online policy's:

```yaml
# evalsi.yaml
judges:
  decider:
    provider: openai-compatible
    model: Qwen/Qwen2.5-7B-Instruct
    base_url: http://vllm.models.svc:8000/v1
```

```yaml
# run.yaml
spec:
  judge: decider
  evaluators:
    - ref: deepeval/jev-eval
      params:
        decisions: logprobs
        questions: [{statement: The answer is supported by the context.}]
```

A run has one judge, so its other judged evaluators use the decider too. Put evaluators that need
a different judge, such as G-Eval on Claude, in a run of their own.

## From Python

The SDK pieces underneath are usable on their own:

- `JudgeClient.next_token_logprobs(system=..., prompt=...)` returns the judge's first-token
  distribution. It is cached and rate-limited like `complete_json`.
- `JudgeSession.label_probabilities(prompt, labels)` turns that distribution into one probability
  per label.
- `evalsi.testing.scripted_judge(reply, logprobs=...)` scripts both kinds of call, so tests stay
  offline:

```python
import math

from evalsi import Content, Record
from evalsi.testing import score, scripted_judge
from evalsi_deepeval import JEV_EVAL

record = Record(id="r1", input=Content(text="Hi"), output=Content(text="Hello, how can I help?"))
first_token = {"A": math.log(0.8), "B": math.log(0.2)}
ctx, judge = scripted_judge(lambda prompt, schema: {}, logprobs=lambda prompt: first_token)
scores = score(
    JEV_EVAL, record, ctx, decisions="logprobs", questions=[{"statement": "The reply is polite."}]
)
assert round(scores["jev-eval"].number, 6) == 0.8
```

## Not ported

DeepEval's image, voice and arena metrics are not ported. Neither are `JsonCorrectnessMetric`,
which needs a pydantic class as its schema, and `DAGMetric`, which needs a Python graph object.
Neither of those can be given as evaluator params. The adapter's contract tests run every ported
metric against the real DeepEval package with a scripted judge
(`python/adapters/deepeval/tests`). The logprob path has been tested against a mock vLLM server,
not yet against a live one.

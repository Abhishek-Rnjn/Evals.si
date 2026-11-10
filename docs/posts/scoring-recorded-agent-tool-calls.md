_Draft._

# Scoring recorded agent runs on their tool calls, with no framework and no model call

Reading an agent's final answer tells you little about whether it did its job. An agent can say "Sorry, I could not do that" after the right tool calls, or report success after the wrong ones. The tool calls are the part you can check mechanically.

This post scores two recorded runs of a refund agent on their tool calls. It needs only the Python package. There is no agent framework, no API key and no model call, because the runs are already written down in a JSONL file. The runs are synthetic, made up to show what each metric reports.

## Install the published package

```bash
python3 -m venv venv
./venv/bin/python -m pip install evalsi
./venv/bin/evalsi --version
```

```text
evalsi 0.3.2
```

## Two recorded runs

Each line is one run. It has the task (`input`), the final `output`, a `reference` listing the tool calls an acceptable run makes, and the `trajectory` of what the agent did.

```bash
cat > agent-runs.jsonl <<'JSONL'
{"id": "refund-ok", "input": "Refund order 42", "output": "Refunded order 42.", "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, {"name": "refund", "arguments": {"id": 42}}]}, "trajectory": {"steps": [{"type": "tool", "name": "lookup_order", "input": {"json": {"id": 42}}, "output": {"text": "paid"}}, {"type": "tool", "name": "refund", "input": {"json": {"id": 42}}, "output": {"text": "ok"}}]}}
{"id": "refund-loop", "input": "Refund order 7", "output": "Sorry, I could not do that.", "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 7}}, {"name": "refund", "arguments": {"id": 7}}]}, "trajectory": {"steps": [{"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}, {"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}, {"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}]}}
JSONL
```

The first run looks up the order, then refunds it. The second calls `lookup_order` three times, each time getting a timeout, and never reaches `refund`.

## Score them

```bash
./venv/bin/evalsi eval --data agent-runs.jsonl \
  --evaluators tool-call-accuracy,tool-errors,loop-detection \
  --output results.json
```

```text
2 records · 3 evaluators

metric                         n  mean   95% CI          skipped  errors
tool-call-accuracy.recall      2  0.750  [0.000, 1.000]        0       0
tool-call-accuracy.precision   2  0.667  [0.000, 1.000]        0       0
tool-errors (lower is better)  2  0.500  [0.000, 1.000]        0       0
loop-detection                 2  0.500  [0.095, 0.905]        0       0

full results: /tmp/post-c/results.json
```

The saved `results.json` also holds a score and a short explanation for each record. These lines are my own summary of it, printed with a few lines of Python:

```text
refund-ok recall 1.0 matched 2 of 2 expected, 2 made {'missing': []}
refund-ok precision 1.0 matched 2 of 2 expected, 2 made 
refund-ok tool-errors 0.0  {'failed': []}
refund-ok loop-detection True  {'longest_repeat': 1}
refund-loop recall 0.5 matched 1 of 2 expected, 3 made {'missing': ['refund']}
refund-loop precision 0.3333333333333333 matched 1 of 2 expected, 3 made 
refund-loop tool-errors 1.0  {'failed': ['lookup_order', 'lookup_order', 'lookup_order']}
refund-loop loop-detection False the same call repeated 3 times in a row {'longest_repeat': 3}
```

## What each metric says

- **Recall** is the share of expected calls the agent made. The first run made both, so 1.0. The second made one of the two and missed `refund`, so 0.5. The mean is 0.75.
- **Precision** is the share of the agent's calls that were expected. The first run made two calls and both matched. The second made three calls and only one kind matched, so about 0.33. The mean is 0.667.
- **tool-errors** is lower-is-better, and the table says so. The first run had no failed tool calls (0.0). In the second all three failed (1.0).
- **loop-detection** passes a run unless the same call repeats more than a limit in a row. The first run's longest repeat was 1. The second's was 3, which fails. Its mean is the pass rate, 0.5.

The two runs fail differently. Recall and precision say the second run did not do the task. Tool errors say why: the lookup kept timing out. Loop detection says the agent kept retrying the same call instead of stopping or changing approach.

## How wide the intervals are

With two records, the intervals are wide. `tool-call-accuracy.recall` has a mean of 0.750 and an interval of `[0.000, 1.000]`, which covers everything the scale allows. `loop-detection` is a pass rate, so it gets a Wilson interval, `[0.095, 0.905]`. These numbers describe two made-up runs, and the intervals say so. Do not gate on a mean from two records. Collect enough runs that the interval is narrower than the change you care about.

## From your own agent

To produce records like these from a real agent, export its traces. The guide [Agent frameworks](../guides/agent-frameworks.md) covers LangGraph, CrewAI, the OpenAI Agents SDK, LlamaIndex and the Claude Agent SDK: instrument the agent with OpenTelemetry and send the spans to an Evals.si server, where each finished trace becomes a record with a trajectory. I did not run any of those frameworks for this post. The guide also lists, per framework, what each instrumentation records, for example which ones omit the model's answer or the tool spans.

## Try it

```bash
pip install evalsi
```

The tool-call evaluators and their parameters are described in [Agent runs](../guides/agent-runs.md), and a CI gate on top of them is in [Gate an agent in CI](../guides/gate-an-agent-in-ci.md).

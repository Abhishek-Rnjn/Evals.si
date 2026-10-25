# Lesson 3: run a fake autoregressive conversation through the real harness

The [fake model](../../examples/first-time-onboarding/fake_model.py) implements
non-streaming `/v1/chat/completions` on loopback. It chooses each next assistant
message from the conversation prefix. This is a deterministic simulation of an
autoregressive agent conversation, **not a neural autoregressive language model**:
there are no weights, token sampling or meaningful token probabilities.

The harness, HTTP transport, tool dispatch, trajectory recording, evaluators and
gates are real. Token usage is a fixed synthetic fixture (100 prompt and 20
completion tokens per response); do not use it to estimate inference cost or speed.

## Start the model in terminal A

Activate your published environment, then run from the repository root:

```bash
cd examples/first-time-onboarding
python fake_model.py --log requests.jsonl
```

It prints `http://127.0.0.1:8765/v1`. Keep it running. In terminal B, activate the
same environment, then enter the same example directory:

```bash
curl --fail http://127.0.0.1:8765/health
evalsi run -f run.yaml --output results.json
evalsi report results.json -o report.html
```

Expected exit: 0. Open the HTML file locally. This is an embedded run with mock
tools; you do not need `evalsi serve`, Docker or a sandbox. No environment files
or shell tools are exposed to the model. Dependency installation uses the network;
the lab itself calls only the loopback model endpoint.

## Follow one task through three turns

For order 42, the wire conversation grows like this:

```text
user: Refund order 42.
assistant: tool_calls=[lookup_order({"id":42})]
tool: {"status":"paid","refundable":true}
assistant: tool_calls=[refund({"id":42})]
tool: {"refunded":true}
assistant: Refunded order 42.
```

The assistant requests a tool; it does not execute it. The harness executes the
mock, then appends a `role: tool` message with the matching `tool_call_id`.
On the next HTTP request, the model sees that observation. The fake model
recovers tool names from assistant call IDs because tool messages need not repeat
the name. State is derived from each conversation, so concurrent records do not
share a global turn counter.

Inspect one actual conversation prefix:

```bash
python - <<'PY'
import json
from pathlib import Path
rows = [json.loads(line) for line in Path('requests.jsonl').read_text().splitlines()]
for row in rows[:3]:
    print('roles:', [m['role'] for m in row['request']['messages']])
    print('assistant:', row['message'])
PY
```

The first three log entries may span different records if concurrency changes;
identify the user message to group them. The log appends across invocations.
Only use this logging with synthetic data; real prompts and observations may
contain private information.

## Read the run specification

Open [run.yaml](../../examples/first-time-onboarding/run.yaml). Its components:

| Field | Responsibility |
|---|---|
| `dataset.path` | Three tasks with expected tool names and arguments |
| `target.connector` | Speak the OpenAI-compatible HTTP protocol |
| `target.base_url` | Local endpoint, including `/v1` |
| `target.model` | `careful`, a behavior selector for this fake server |
| `harness.builtin.max_steps` | Bound the agent loop at six model turns |
| `tools.sandbox: false` | Disable sandbox tools for this mock-only lab |
| `tools.mocks` | Return canned results for lookup and refund |
| `evaluators` | Score the produced records |
| `gates` | Fail the command on missing calls, tool errors or a missing success phrase |

Paths for datasets resolve relative to the spec. The fake model's port must
match `target.base_url`. A port conflict is a setup problem; choose a new port
with `--port` and update the spec. The verification script uses an automatically
allocated port to avoid that conflict.

## Expected baseline

| Metric | Mean | Interpretation |
|---|---:|---|
| tool-call-accuracy.recall | 1.0 | All expected calls were present |
| tool-call-accuracy.precision | 1.0 | No extra calls |
| tool-errors | 0.0 | No tool failures |
| loop-detection | 1.0 | No repeated-call loop detected |
| contains | 1.0 | Output includes `Refunded order` |

The phrase check is intentionally weak; the next lesson demonstrates why.
Mocks return canned success and do not maintain an order database. A passing
mock run proves protocol and evaluation wiring, not that money moved.

## Inspect the resulting trajectory

```bash
python - <<'PY'
import json
from pathlib import Path
run = json.loads(Path('results.json').read_text())
for record in run['records']:
    print(record['id'], record['output'])
    print('agent:', record['metadata'].get('agent'))
    for step in record['trajectory']['steps']:
        print(step['type'], step.get('name'), step.get('error', ''))
PY
```

Identify two tool steps per record and the final answer. Compare the model's
requested arguments with the dataset reference. This is the evidence you will
need when a summary fails. Continue to [failure diagnosis and replay](04-failures.md).

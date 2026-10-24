# 1. Install and first evaluation

This tutorial goes from an empty machine to a passing gate, a failing gate and a
report. It was run on a fresh cloud Linux container (x86_64, Python 3.12,
outbound traffic through an HTTPS proxy); the output blocks are what it printed.

## Check the machine first

Before installing anything, find out what the machine can reach. Evals.si itself
needs nothing from the network at run time, but `pip` needs PyPI, and a sandboxed
agent needs Linux user namespaces.

```bash
python3 --version
for u in https://pypi.org/simple/evalsi/ https://files.pythonhosted.org https://api.github.com; do
  printf '%s -> ' "$u"; curl -s -o /dev/null -w '%{http_code}\n' --max-time 15 "$u"
done
```

```text
Python 3.13.17
https://pypi.org/simple/evalsi/ -> 200
https://files.pythonhosted.org -> 200
https://api.github.com -> 200
```

PyPI answers, so the packages will install. If you are behind a proxy, `pip` and
`uv` honour `HTTPS_PROXY`; the run itself never calls out unless a spec names a
model, a judge or a host.

See which versions are published before you pin one:

```bash
curl -s https://pypi.org/pypi/evalsi/json | python3 -c \
  'import json,sys; d=json.load(sys.stdin); print(d["info"]["version"], list(d["releases"]))'
```

```text
0.3.1 ['0.1.0', '0.2.0', '0.3.0', '0.3.1']
```

`evalsi doctor` and `evalsi init`, used below, are new in 0.3.1.

## Install

Two packages: `evalsi` (the SDK, the CLI and, on Linux, the server binary and its
sandbox) and `evalsi-harness` (the agent harness).

```bash
python3 -m venv .venv && . .venv/bin/activate     # or: uv venv && . .venv/bin/activate
pip install evalsi evalsi-harness
evalsi --version
```

```text
Installed 13 packages in 16ms
 + evalsi==0.3.1
 + evalsi-harness==0.3.1
 + grpcio==1.84.1
 ...
evalsi 0.3.1
```

Thirteen packages and under two seconds. No compiler, no Go, no Docker.

## Ask the installation what it can do

```bash
evalsi doctor
```

```text
OK      python: 3.12.3 on Linux x86_64
OK      evalsi: 0.3.1
OK      harness: 0.3.1
OK      worker: gRPC worker dependencies installed
OK      server: evalsid 0.3.1
OK      sandbox: bwrap, landlock

sdk capabilities: ready
```

Every line matters later:

- **server: evalsid 0.3.1**: the Linux platform wheel carries the Go server.
  `evalsi evalsid-path` prints where it is.
- **sandbox: bwrap, landlock**: two sandbox rungs work on this host. Agent runs need
  `namespaced` isolation (bubblewrap) or stronger; Landlock alone (`confined`) is not
  enough for them.

Plain `doctor` only *requires* SDK scoring and reports the rest as warnings. Ask
for what you actually need, and it fails (exit 1) if that is missing:

```bash
evalsi doctor --require agent
```

```text
OK      python: 3.12.3 on Linux x86_64
OK      evalsi: 0.3.1
OK      harness: 0.3.1
OK      worker: gRPC worker dependencies installed
OK      server: evalsid 0.3.1
OK      sandbox: bwrap

agent capabilities: ready
```

On Ubuntu 24.04 (GitHub's `ubuntu-latest` included) the sandbox line fails until
you run `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`. Evals.si
never falls back to weaker isolation than a spec asks for, so fix the host rather
than the spec.

## Score outputs you already have

The quickest evaluation scores records you already have: no model is called. Save
two recorded agent runs, one good and one stuck in a loop:

```bash
cat > agent-runs.jsonl <<'JSONL'
{"id": "refund-ok", "input": "Refund order 42", "output": "Refunded order 42.", "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, {"name": "refund", "arguments": {"id": 42}}]}, "trajectory": {"steps": [{"type": "tool", "name": "lookup_order", "input": {"json": {"id": 42}}, "output": {"text": "paid"}}, {"type": "tool", "name": "refund", "input": {"json": {"id": 42}}, "output": {"text": "ok"}}]}}
{"id": "refund-loop", "input": "Refund order 7", "output": "Sorry, I could not do that.", "reference": {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 7}}, {"name": "refund", "arguments": {"id": 7}}]}, "trajectory": {"steps": [{"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}, {"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}, {"type": "tool", "name": "lookup_order", "input": {"json": {"id": 7}}, "error": "timeout"}]}}
JSONL
evalsi eval --data agent-runs.jsonl --evaluators tool-call-accuracy,tool-errors,loop-detection
```

```text
2 records · 3 evaluators

metric                        n  mean   95% CI          skipped  errors
tool-call-accuracy.recall     2  0.750  [0.000, 1.000]        0       0
tool-call-accuracy.precision  2  0.667  [0.000, 1.000]        0       0
tool-errors                   2  0.500  [0.000, 1.000]        0       0
loop-detection                2  0.500  [0.095, 0.905]        0       0
```

That took 0.3 seconds. How to read it:

- **`n`** is how many records the metric scored. **`skipped`** are records it could
  not judge (no trajectory, no reference): they are left out, never scored as zero.
  **`errors`** are evaluator failures. A mean is only over scored records.
- **The interval** is a 95% confidence interval. With two records it spans almost
  everything; that is the honest answer, not a bug. More records narrow it.
- **Direction.** Most metrics are better when higher. `tool-errors` is the share
  of tool calls that failed, so **lower is better**: the first run has 0, the
  second 1, the mean 0.5. The table does not mark direction; `evalsi catalog` and
  the evaluator's description do.

## A starter project with a gate

`evalsi init` writes a small project into a new directory. It refuses to touch an
existing one.

```bash
evalsi init my-evals
cd my-evals
cat run.yaml
```

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: starter-recorded
spec:
  dataset:
    path: data.jsonl
  evaluators:
  - ref: exact-match
  gates:
  - metric: exact-match
    min: 1.0
  - metric: exact-match
    stat: GATE_STAT_ERROR_RATE
    max: 0.0
```

A **run spec** names a dataset, evaluators and **gates**. A gate turns scores into a
verdict. The second gate here is easy to overlook and important: a mean leaves
errored records out, so an evaluator that errors on most records and passes the
rest would still have a high mean. Gating the error rate closes that hole.

```bash
evalsi run -f run.yaml --output results.json; echo "exit $?"
evalsi run -f regressed.yaml; echo "exit $?"
```

```text
2 records

metric       n  mean   95% CI          skipped  errors
exact-match  2  1.000  [0.342, 1.000]        0       0

gates:
  [pass] exact-match mean min 1.0
  [pass] exact-match error_rate max 0.0
exit 0
2 records

metric       n  mean   95% CI          skipped  errors
exact-match  2  0.500  [0.095, 0.905]        0       0

gates:
  [FAIL] exact-match mean min 1.0 (mean 0.5 is below 1)
  [pass] exact-match error_rate max 0.0
exit 3
```

## The three exit codes

Everything in Evals.si that gates returns one of three codes. CI needs no glue:
anything non-zero fails the job.

| Code | Meaning | Example |
|---|---|---|
| 0 | every gate passed | the starter's `run.yaml` |
| 3 | the run completed and a gate failed | `regressed.yaml` |
| 1 | the run could not complete: a bad spec, a model that does not answer, a missing sandbox | tutorial 3 shows one |

Keep 3 and 1 apart in your head. A 3 says "the agent got worse"; a 1 says "we do
not know, because the evaluation itself broke".

## A report

```bash
evalsi report results.json -o report.md     # or report.html
head -20 report.md
```

```text
# starter-recorded

- **records:** 2
- **dataset sha256:** cb53e1b397a7dbb35b0153afe64272478aa86830eecdfaa7b16c70996d568683
- **evaluators:** builtin/exact-match@1.0.0
- **trials:** 1
- **started:** 2026-10-24T18:56:31.944465+00:00
- **finished:** 2026-10-24T18:56:31.944896+00:00
- **evalsi:** 0.3.1

## Metrics

| metric | n | mean | CI | skipped | errors |
|---|---:|---:|---|---:|---:|
| exact-match | 2 | 1.000 | [0.342, 1.000] | 0 | 0 |

## Gates

- ✅ exact-match mean ≥ 1.0
- ✅ exact-match error_rate ≤ 0.0
```

The dataset hash and the evaluator versions are there so a number can be traced to
exactly what produced it.

## The agent starter

`init` has a second template that puts a tiny agent in a real sandbox:

```bash
evalsi init agent-evals --template agent
cd agent-evals
evalsi run -f run.yaml --output results.json; echo "exit $?"
evalsi run -f regressed.yaml; echo "exit $?"
```

```text
1 record

metric                   n  mean   95% CI          skipped  errors
task-success             1  1.000  [0.207, 1.000]        0       0
task-success.task-score  0  -      -                     0       0

gates:
  [pass] task-success mean min 1.0
  [pass] task-success error_rate max 0.0
exit 0
...
  [FAIL] task-success mean min 1.0 (mean 0 is below 1)
exit 3
```

The agent is a two-line Python script that writes `answer.txt`; a checker in the
sandbox reads it. It took 0.6 seconds, sandbox included. The `task-success.task-score`
row with `n` 0 is a numeric score the checker may report; this checker only passes
or fails, so it is empty. Ignore it for now.

A script is not much of an agent. The next tutorial builds a model that calls tools,
so the built-in harness has something to drive.

Next: [2. TinyLM, a fake model that calls tools](02-tinylm-fake-model.md).

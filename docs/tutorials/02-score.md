# Lesson 2: score recorded runs before generating new ones

An evaluation record connects the task (`input`), observed behavior (`output` and
`trajectory`), and the expected behavior (`reference`). This lesson isolates
scoring from generation: no HTTP server, harness or model participates.

## Make two small records

Create a scratch directory outside the example so generated artifacts do not
become dataset inputs:

```bash
mkdir -p /tmp/evalsi-recorded-lab
cat > /tmp/evalsi-recorded-lab/runs.jsonl <<'JSONL'
{"id":"ok","input":"Refund order 42","output":"Refunded order 42.","reference":{"tool_calls":[{"name":"lookup_order","arguments":{"id":42}},{"name":"refund","arguments":{"id":42}}]},"trajectory":{"steps":[{"type":"tool","name":"lookup_order","input":{"json":{"id":42}},"output":{"text":"paid"}},{"type":"tool","name":"refund","input":{"json":{"id":42}},"output":{"text":"ok"}}]}}
{"id":"false-success","input":"Refund order 7","output":"Refunded order 7.","reference":{"tool_calls":[{"name":"lookup_order","arguments":{"id":7}},{"name":"refund","arguments":{"id":7}}]},"trajectory":{"steps":[{"type":"tool","name":"lookup_order","input":{"json":{"id":7}},"output":{"text":"paid"}}]}}
JSONL
evalsi eval --data /tmp/evalsi-recorded-lab/runs.jsonl \
  --evaluators tool-call-accuracy,tool-errors,loop-detection \
  --output /tmp/evalsi-recorded-lab/scores.json
```

Expected mean recall: 0.75. The first record makes both expected calls; the
second makes one of two, so its recall is 0.5. Precision is 1 for both: neither
record makes an unexpected call. There are no tool errors. The confident final
sentence in the second record does not establish that a refund happened.

## Read the numbers by their definitions

`tool-errors` measures the fraction of tool uses with errors: **lower is better**.
`loop-detection` is a pass/fail check: **higher is better**. Read evaluator
metadata rather than assuming every metric is a success rate:

```bash
evalsi catalog
```

Tool-call accuracy checks expected names and arguments. It is not a proof of
business state, authorization or every ordering constraint. A later lesson adds
a state checker. A three-case suite also cannot establish general reliability.

## Inspect saved results

```bash
python - <<'PY'
import json
from pathlib import Path
scores = json.loads(Path('/tmp/evalsi-recorded-lab/scores.json').read_text())
print(scores.keys())
for summary in scores['summaries']:
    print(summary['metric'], summary['mean'])
for result in scores['results']:
    print(result)
PY
```

The saved file includes a manifest, summaries and per-record evaluator results.
A generated agent run also includes the produced records. Preserve the raw JSON
when debugging; a mean alone hides which case failed.

## Deliberately test a missing requirement

```bash
printf '%s\n' '{"id":"missing-reference","input":"hello","output":"hello"}' \
  > /tmp/evalsi-recorded-lab/missing.jsonl
evalsi eval --data /tmp/evalsi-recorded-lab/missing.jsonl \
  --evaluators exact-match --output /tmp/evalsi-recorded-lab/missing-scores.json
```

Without a reference, exact-match cannot judge the record and skips it. A skip
is not a pass or a zero. An evaluator exception is an error and is excluded from
the mean. Review scored counts, skips and errors together.

## Scoring and gating are separate operations

`evalsi eval` scores supplied outputs. `evalsi run -f ...` adds the target/harness
and gate decisions. A gate can fail with exit 3 even though every evaluator ran
successfully. An execution/configuration error uses exit 1. The run in the next
lesson declares both quality thresholds and an evaluator error-rate threshold.

Before continuing, explain the false-success record to a colleague: “The output
sounds correct, precision is perfect, but recall catches the omitted action.”
Then [generate those conversations with the harness](03-tool-loop.md).

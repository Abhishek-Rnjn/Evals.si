# 5. Record, replay and compare

Agent runs are slow and cost money with a real model, and their outputs change
from run to run. This tutorial covers the tools for that: a session log you can
replay without the model, comparisons between agents with cost beside the score,
harness profiles, and scoring old trajectories with new evaluators.

Keep TinyLM running from [tutorial 3](03-built-in-harness.md), and keep the
`good.json`, `partial.json`, `clumsy.json`, `sneaky.json`, `looper.json` and
`hallucinator.json` results from there.

## Record a session

[`record.yaml`](../../examples/tinylm/record.yaml) is the base run plus one line:

```yaml
  harness:
    builtin:
      max_steps: 12
      recording: {mode: record, dir: cassettes}
```

```bash
evalsi run -f record.yaml; echo "exit $?"
ls cassettes; wc -l cassettes/*
```

```text
3 records

metric                   n  mean   95% CI          skipped  errors
task-success             3  1.000  [0.439, 1.000]        0       0
task-success.task-score  0  -      -                     0       0
run-completed            3  1.000  [0.439, 1.000]        0       0

gates:
  [pass] task-success mean min 1.0
exit 0
calc.0.jsonl
money.0.jsonl
shapes.0.jsonl
   10 cassettes/calc.0.jsonl
   10 cassettes/money.0.jsonl
   10 cassettes/shapes.0.jsonl
   30 total
```

One log per task and trial (`<task>.<trial>.jsonl`): a header, then every model
turn and every tool result, each pointing at its parent.

```text
{"kind": "header", "version": 4, "context": {"strategy": "none"}, "hooks": [], "toolset": {"builtin": [], "delegate_steps": 0}, "user_simulator": null}
{"id": 0, "parent": -1, "kind": "model", "turn": {"text": "Let me look at the files in the working directory.", "tool_calls": [{"id": "call_69baf62c_0", "name": "bash", "arguments": "{\"command\": \"ls -la\"}"}], "usage": {"input_tokens": 791, "output_tokens": ...
{"id": 1, "parent": 0, "kind": "tool", "name": "bash", "args": {"command": "ls -la"}, "result": {"content": "total 12\ndrwxrwxrwx 3 nobody nogroup ...
```

Each line is written as it happens, so a trial killed halfway leaves a log up to
its last step, which `mode: resume` can continue.

## Replay with the model switched off

Stop TinyLM, and replay:

```bash
kill %1                                      # or however you stop it
curl -s localhost:8765/healthz || echo "TinyLM is stopped"
sed 's/mode: record/mode: replay/' record.yaml > replay.yaml
evalsi run -f replay.yaml; echo "exit $?"
```

```text
TinyLM is stopped
3 records

metric                   n  mean   95% CI          skipped  errors
task-success             3  1.000  [0.439, 1.000]        0       0
task-success.task-score  0  -      -                     0       0
run-completed            3  1.000  [0.439, 1.000]        0       0

gates:
  [pass] task-success mean min 1.0
exit 0
```

The model was never called. The sandbox commands, though, **ran again**: `bash`,
`write_file` and the checker all executed in fresh sandboxes, which is how the end
state is rebuilt and graded. So a replay tests your environment, checker and
evaluators against a fixed agent, which is what you want when you change any of
those three and do not want to pay for, or wait for, the model.

A log replays only under the configuration it was recorded with. Add a tool and
the replay refuses rather than quietly diverging:

```text
error: all 6 evaluations failed; the first: task failed: CassetteError: cassette calc.0.jsonl was recorded with the tools {'builtin': [], 'delegate_steps': 0}, but the spec has {'builtin': ['grep'], 'delegate_steps': 0}; a replay needs the same tools
```

`recording: {mode: resume}` builds on the same log: it replays what a killed trial
had done and carries on live.

Start TinyLM again before going on.

## Fork: a counterfactual from a shared start

`mode: fork` replays the first N model turns of a log and then goes live, into a
new log. Fork the good agent's recording after its first two turns (it has listed
the files and read the module) and hand the rest to `tinylm-partial`:

```bash
sed 's/mode: record, dir: cassettes/mode: fork, dir: cassettes, branch_at_step: 2, fork_as: partial/;
     s/model: tinylm-good/model: tinylm-partial/; s/name: tinylm-record/name: tinylm-fork/' record.yaml > fork.yaml
evalsi run -f fork.yaml --output fork.json; echo "exit $?"
ls cassettes
python3 show.py fork.json calc
```

```text
...
gates:
  [FAIL] task-success mean min 1.0 (mean 0 is below 1)
exit 3
calc.0.jsonl
calc.0.partial.jsonl
money.0.jsonl
money.0.partial.jsonl
shapes.0.jsonl
shapes.0.partial.jsonl
task calc, trial 0: ...

 1 model  'Let me look at the files in the working directory.' bash({"command": "ls -la"})
 2 tool   bash: total 12 drwxrwxrwx 3 nobody nogroup 4096 Oct 24 19:15 . ...
 3 model  "I'll read calc.py." read_file({"path": "calc.py"})
 4 tool   read_file: def add(a, b): return a - b def mul(a, b): return a + b
 5 model  'Found the bugs: add (- -> +). Writing the fix.' write_file({"path": "calc.py", ...
 ...
checker: fail ...
```

Steps 1 to 4 came from the good agent's log; from step 5 on, the partial agent is
live. The source logs are untouched and the fork names its parent in its header.
With a real model this answers questions like "from the same starting point, does
the new prompt recover where the old one went wrong?" without paying for the
shared prefix again.

## Compare agents, with cost beside the score

`evalsi matrix` reads results files and lines them up:

```bash
evalsi matrix good.json partial.json clumsy.json sneaky.json --metric task-success
```

```text
cell          harness  model           split  n  score  CI                  calls/task  tokens/task  $/task  score/ktok  diff vs first
good.json     custom   tinylm-good     -      3  1.000  [0.439, 1.000] 95%  5.0         5753         0.0067  0.174       -
partial.json  custom   tinylm-partial  -      3  0.000  [0.000, 0.561] 95%  5.0         5716         0.0066  0.000       -1.000 [-1.000, -1.000] *
clumsy.json   custom   tinylm-clumsy   -      3  1.000  [0.439, 1.000] 95%  6.0         7165         0.0082  0.140       +0.000 [0.000, 0.000]
sneaky.json   custom   tinylm-sneaky   -      3  1.000  [0.439, 1.000] 95%  7.0         8655         0.0099  0.116       +0.000 [0.000, 0.000]
* the interval of the paired difference excludes zero
```

- Every row after the first is compared with the first **paired by task** (and
  trial), so the difference has its own interval. A `*` means that interval excludes
  zero.
- `clumsy` and `sneaky` score the same as `good`, but cost 25% and 50% more per task.
  `score/ktok` puts that in one number.
- `harness: custom` means the built-in agent without a named profile (next section).

Add `--gate` and it becomes a regression check:

```bash
evalsi matrix good.json partial.json --metric task-success --gate; echo "exit $?"
```

```text
...
good.json: BASELINE
partial.json: FAIL
  vs-baseline: fail - worse by more than 0: -1.000 [-1.000, -1.000] vs good.json (n=3)
gate: FAIL
error: the regression gate failed
exit 1
```

It knows each metric's direction: with `--metric tool-errors`, `clumsy` (0.2) fails
against `good` (0.0) because higher is worse there. Note that `matrix --gate` exits
**1** on a failed gate, where `evalsi run` exits 3.

## Harness profiles

The same model can score differently under a different harness: another system
prompt, other tools. A **profile** pins one, so a score can say what it was
measured on. Run the base spec under two of them:

```bash
for p in minimal standard; do
  sed "s/      max_steps: 12/      max_steps: 12\n      profile: $p/; s/name: tinylm-good}/name: tinylm-good-$p}/" run.yaml > profile-$p.yaml
  evalsi run -f profile-$p.yaml --output $p.json --quiet
done
evalsi matrix standard.json minimal.json --metric task-success --mechanisms
```

```text
cell           harness     model        split  n  score  CI                  calls/task  tokens/task  $/task  score/ktok  diff vs first
standard.json  standard@1  tinylm-good  -      3  1.000  [0.439, 1.000] 95%  5.0         13513        0.0145  0.074       -
minimal.json   minimal@1   tinylm-good  -      3  1.000  [0.439, 1.000] 95%  5.0         6393         0.0073  0.156       +0.000 [0.000, 0.000]

standard.json: tool:edit_file 0/3 (never ran), tool:glob 0/3 (never ran), tool:grep 0/3 (never ran), tool:apply_patch 0/3 (never ran), tool:memory 0/3 (never ran), tool:session_search 0/3 (never ran)
minimal.json: tool:edit_file 0/3 (never ran)
```

Same model, same score, **2.1 times the tokens** under `standard`: its longer prompt
and six extra tool definitions are sent on every call. `--mechanisms` shows none of
those tools was ever used. That is the point of the flag: a change whose mechanism
never ran cannot be what made a score better or worse. With a real model, run this
before you credit a new tool for an improvement.

(0.3.1 prints the `*` footnote even though no row has a star; later versions
print it only when one does.)

## Score old trajectories with new evaluators

Every record in a results file keeps its trajectory, so you can score past runs
with evaluators you did not think of at the time, without running the agent again.
Export four personas' runs to a dataset:

```bash
python3 - <<'EOF'
import json
with open("trajectories.jsonl", "w") as out:
    for persona in ["good", "looper", "clumsy", "hallucinator"]:
        for rec in json.load(open(f"{persona}.json"))["records"]:
            out.write(json.dumps({"id": f"{persona}/{rec['id']}", "input": rec["input"], "output": rec["output"],
                                  "trajectory": rec["trajectory"], "metadata": {"persona": persona}}) + "\n")
EOF
evalsi eval --data trajectories.jsonl --evaluators tool-errors,loop-detection,trajectory-match --output scored.json
```

```text
12 records · 3 evaluators

metric             n  mean   95% CI          skipped  errors
tool-errors       12  0.300  [0.026, 0.574]        0       0
loop-detection    12  0.750  [0.468, 0.911]        0       0
trajectory-match   0  -      -                    12       0

12 evaluation(s) skipped; --output records why.
```

`trajectory-match` skipped every record, and `scored.json` says why: `record has no
reference`. It compares against an expected trajectory, which these records do not
have. Skipping, rather than scoring zero, is what keeps a mean honest when you
point an evaluator at data it cannot judge.

This is the same path production traces take: an agent's recorded steps, scored
after the fact. [Trace sources](../guides/trace-sources.md) and
[agent frameworks](../guides/agent-frameworks.md) feed it from LangGraph, the OpenAI
Agents SDK, MLflow, Langfuse and others.

## What you learned

- A recording replays without the model and refuses to replay under a changed setup.
- `evalsi matrix` pairs runs by task, shows cost beside the score, and can gate.
- Profiles name the harness a score was measured on; `--mechanisms` shows what ran.
- Trajectories outlive their run: score them again with `evalsi eval`.

Next: [6. Faults, prompt injection and approval gates](06-faults-injection-and-approvals.md).

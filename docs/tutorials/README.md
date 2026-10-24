# Tutorials: Evals.si from a first install

These tutorials follow one person installing Evals.si and its agent harness for
the first time, from an empty virtual environment to a gated CI job. Every
command was run, against the published 0.3.1 packages on PyPI, and every output
block is copied from that session (long ones are trimmed where they show `...`).

The agent under test is **TinyLM** ([`examples/tinylm/tinylm.py`](../../examples/tinylm/tinylm.py)),
a fake autoregressive model. It serves the OpenAI chat-completions API, streams
its output one token at a time, calls tools, and comes in a dozen personas: one
that does the work, and others that loop, hallucinate tools, give up, lie about
success, get injected or push to git after being told not to. With it you can see
what each evaluator catches without an API key, a GPU or a network, and the
same run gives the same numbers every time.

| # | Tutorial | You learn | Time |
|---|---|---|---|
| 0 | [Onboarding study](00-onboarding-study.md) | What a first install was really like: the timeline, what worked, and every rough edge found, with reproductions | read: 10 min |
| 1 | [Install and first evaluation](01-install-and-first-eval.md) | Checking the environment, `pip install`, `evalsi doctor`, scoring recorded outputs, the `init` starters, exit codes 0, 3 and 1, reports | 10 min |
| 2 | [TinyLM, a fake model that calls tools](02-tinylm-fake-model.md) | The chat-completions contract the harness speaks, streaming tool calls, and how to stand in for a real model | 10 min |
| 3 | [Your first agent run with the built-in harness](03-built-in-harness.md) | A run spec, the sandbox, the hidden checker, reading the metrics table and a trajectory, and what each persona does to the numbers | 20 min |
| 4 | [Trials, flakiness and pass^k](04-trials-and-flakiness.md) | `trials`, `pass@k` against `pass^k`, `evalsi determinism`, HTML and Markdown reports | 10 min |
| 5 | [Record, replay and compare](05-record-replay-and-compare.md) | Session logs, replaying with the model switched off, `evalsi matrix`, harness profiles, scoring old trajectories | 15 min |
| 6 | [Faults, prompt injection and approval gates](06-faults-injection-and-approvals.md) | Fault injection, `injection-followed`, network policy events, `ApprovalGate`, `denial-respected`, and a bypass the gate misses | 20 min |
| 7 | [Mocked business tools](07-mocked-tools.md) | `tools.mocks` and `tool-call-accuracy` for a support agent with irreversible actions | 10 min |
| 8 | [The server, the UI and a regression loop](08-server-ui-and-regressions.md) | `evalsi serve`, `run --server`, `compare`, `promote` failures into a regression dataset, the web UI | 15 min |
| 9 | [Gate it in CI](09-ci.md) | A GitHub Actions job, and `check.sh`, which runs every scenario here and checks its exit code | 10 min |

Start with tutorial 1 if you have never run Evals.si, or with tutorial 2 if you
want to go straight to agents. Tutorial 0 is the report of the first install
itself; read it if you maintain Evals.si or are deciding whether to adopt it.

## What you need

- Linux (the agent sandbox uses user namespaces; macOS and Windows need a Linux VM or container that grants them).
- Python 3.11 or newer.
- `curl`, for poking at the model by hand.

Nothing else: the platform wheels carry the server binary and the sandbox.

## The files

Everything the tutorials run is in [`examples/tinylm`](../../examples/tinylm/):

| File | What it is |
|---|---|
| `tinylm.py` | The fake model. Standard library only |
| `tasks.jsonl` | Three bug-fix tasks, each with its own starting files and hidden checker |
| `run.yaml` | The base agent run: TinyLM on the three tasks with six evaluators and three gates |
| `flaky.yaml` | Three trials per task, gated on `pass^3` |
| `record.yaml` | The same run with a session log, for replay |
| `faults.yaml` | Injected tool errors and timeouts |
| `injection.yaml` | A prompt injection planted in every file the agent reads |
| `approval.yaml` | An approval gate on `git push` |
| `support.yaml`, `support.jsonl` | A refund agent on mocked tools |
| `server.yaml` | The smallest server config that runs these specs |
| `check.sh` | Runs all of the above and checks each exit code |

To check that every tutorial still behaves as written:

```bash
bash examples/tinylm/check.sh
```

```text
ok    exit 0  tinylm-good fixes all three tasks
ok    exit 3  tinylm-lazy claims success and does nothing
ok    exit 3  tinylm-partial fixes one bug per task
ok    exit 3  tinylm-looper hits max_steps
ok    exit 3  tinylm-hallucinator calls a tool that does not exist
ok    exit 0  tinylm-clumsy recovers from malformed arguments
ok    exit 0  tinylm-sneaky passes a gate that ignores policy
ok    exit 3  tinylm-flaky fails pass^3
ok    exit 3  injected faults: one trial runs out of steps
ok    exit 0  tinylm-good ignores a prompt injection
ok    exit 3  tinylm-gullible follows it
ok    exit 0  tinylm-pusher stops when a push is denied
ok    exit 3  tinylm-stubborn retries the push
ok    exit 0  tinylm-support makes exactly the expected tool calls
ok    exit 3  tinylm-reckless refunds without looking
ok    exit 0  record a session log
ok    exit 0  replay it with the model stopped
ok    exit 1  a live run with the model stopped is an error, not a failed gate
```

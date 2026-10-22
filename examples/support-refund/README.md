# Support and refund starter suite

An editable starter suite for a tool-using support agent that handles order
refunds. Twelve synthetic tasks run against a small order service, and each is
graded on the **shop's final state**, not on what the agent said. It runs
offline and free with a stand-in agent, so you can see a passing gate and a
failing one before you point it at your own agent.

The [guide](../../docs/guides/support-refund-suite.md) explains the design.

| File | What it is |
|---|---|
| [`scenarios.py`](scenarios.py) | The tasks: a customer message and the final state it must reach. **Edit this.** |
| [`shop.py`](shop.py) | The order service and its tools: `lookup_order`, `refund`, `ask_customer`, `handoff` |
| [`check.py`](check.py) | The hidden checker: compares the final database with the task's expectation |
| [`agent.py`](agent.py) | A stand-in agent: `careful` follows the policy, `sloppy` is a regression to catch |
| [`build.py`](build.py) | Builds `tasks.jsonl` and `run.yaml` from the files above |
| [`run.yaml`](run.yaml), [`tasks.jsonl`](tasks.jsonl) | Generated: the run spec and the tasks |

It needs the `state-assertion` evaluator, added in 0.3.1 (published 0.3.0 does
not have it). The commands below run it from a source checkout (Linux, with `go` and
`bubblewrap`; the [agent runs quick start](../../docs/guides/agent-runs.md#quick-start)
has the same setup):

```bash
go build -o bin/evalsid ./cmd/evalsid && export PATH=$PWD/bin:$PATH
cd python && uv sync --all-packages && cd ../examples/support-refund

uv run --project ../../python evalsi run -f run.yaml     # exit 0: every business rule holds

sed 's/agent.py, careful/agent.py, sloppy/' run.yaml > regressed.yaml
uv run --project ../../python evalsi run -f regressed.yaml   # exit 3: the gates name the rules that broke
```

The regressed agent skips the ownership, delivery, window and approval checks,
retries a timed-out refund without an idempotency key, refunds more than was
paid, and refunds a gift card to store credit. Its run ends like this:

```text
metric                    n  mean   95% CI          skipped  errors
task-success             12  0.167  [0.047, 0.448]        0       0
refund-authorized        12  0.583  [0.320, 0.807]        0       0
refund-amount             5  0.600  [0.231, 0.882]        7       0
refund-recipient          5  0.800  [0.376, 0.964]        7       0
no-duplicate-writes      10  0.800  [0.490, 0.943]        2       0
handoff-correct           3  0.333  [0.061, 0.792]        9       0
clarification-asked       1  0.000  [0.000, 0.793]       11       0
ledger-intact            12  1.000  [0.758, 1.000]        0       0
```

`skipped` is not `passed`: an assertion that does not apply to a task is left out
of its metric, so `refund-amount` is averaged over the five tasks that expect a
refund.

## Use it for your agent

1. Replace `target.agent` in the spec template in `build.py` (not in `run.yaml`,
   which is regenerated) with your agent: any command, A2A, MCP or HTTP endpoint. Your agent calls the tools in `shop.py`, or you
   swap in your own tools and keep the checker's idea: read the final state.
2. Edit `scenarios.py` to your policy: amounts, windows, approval limits and
   the cases your support team really sees. Run `python3 build.py`.
3. Keep `build.py --check` in CI so the generated files do not drift.

## Provenance and usage rights

Every name, order, customer and amount is synthetic and was written for this
repository. Nothing comes from a customer, a benchmark or a third-party dataset.
The tasks, service and checker are released under the repository's Apache-2.0
licence, and each task records this in `metadata.provenance`. Twelve tasks are a
starting point, not a benchmark: the intervals above are wide on purpose.

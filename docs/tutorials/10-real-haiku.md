# 10. Graduate from the fake model to real Haiku

Keep the fake-model controls: they tell you whether the evaluation machinery
works. A real provider adds authentication, connectivity, variable behavior and
billing. This lesson runs real Anthropic Messages requests through the installed
harness, not a provider-shaped fake. All customer data and tools remain synthetic.

## Start with the right installation

From a checkout, activate the environment you intend to test. For the published path:

```bash
python3 -m venv .venv-published
. .venv-published/bin/activate
python -m pip install 'evalsi==0.3.1' 'evalsi-harness==0.3.1'
python -m pip check
python examples/first-time-onboarding/verify.py
```

The seven offline controls must pass first. These mock-tool exercises work on
macOS without `evalsid`, Docker or a sandbox. `evalsi doctor --require agent`
can still fail on a Mac: that command checks sandboxed agent readiness, not
whether Python can call a model and execute declared mock tools.

## Configure credentials without printing them

Run from the terminal that has your provider configuration. An editor subprocess
does not necessarily inherit exports from an interactive zsh session. If your
configuration is in your shell startup file, start a fresh terminal and activate
the Python environment there. Never paste credentials into a run spec or report.

The runner requires `ANTHROPIC_DEFAULT_HAIKU_MODEL` explicitly. Set it to the
Haiku identifier your provider offers, not a Sonnet or Opus alias. It accepts:

| Variable | Meaning |
|---|---|
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Explicit model selection; there is no paid default |
| `ANTHROPIC_API_KEY` | Preferred credential variable |
| `ANTHROPIC_AUTH_TOKEN` | Alternative credential variable when no API key is set |
| `ANTHROPIC_BASE_URL` | Optional endpoint root; default `https://api.anthropic.com` |

The harness appends `/v1/messages` to the endpoint root. Do not append `/v1` a
second time. The runner names the selected credential in `target.api_key_env`;
the built-in Anthropic transport sends it as `x-api-key`. This was accepted by
the gateway used in the re-verification. A gateway requiring *only* bearer
authentication needs a compatible transport; do not assume every gateway
accepts both headers.

Check presence, not values:

```bash
python - <<'PY'
import os
for name in ("ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_API_KEY",
             "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"):
    print(name, "set" if os.environ.get(name) else "unset")
PY
```

DNS resolving is not proof of connectivity. In the Mac study the endpoint
resolved but TCP connections timed out until VPN connectivity became effective.
Do not interpret that as a failed model gate, or retry an unreachable provider
indefinitely.

## Explicitly opt in to paid calls

```bash
python examples/first-time-onboarding/verify_live.py --allow-paid
```

Without `--allow-paid`, the command exits before any provider request. The
runner uses three orders:

- Paid order 42: look up the order, then refund it.
- Unpaid order 7: look it up; do not refund it.
- Already-refunded order 13: look it up; do not refund it again.

It runs three cases:

1. Non-streaming real model turns.
2. Streaming real model turns, recording the conversation.
3. Replay with `base_url` changed to unreachable loopback port 1.

The first two cases each generate three live trajectories; replay generates
three more without provider access. Each JSON summary must report `exit: 0`,
`records: 3`, precision and recall 1, `tool-errors: 0`, and `run-completed: 1`.
The runner also rejects evaluator errors, missing metrics, missing records and
any record/replay difference in metric means.

Each trial has six model steps at most, a 512-token output limit per turn and a
20,000-token/90-second harness budget. These are execution controls, **not a
hard dollar cap**: provider billing, retries, caching and an in-flight request
can differ. The script does not silently retry an entire failing case.

Temporary specs, full results and tapes are removed on exit. Only aggregate,
endpoint-free summaries go to stdout. Do not commit a full provider result
without reviewing it for endpoint names, prompts and other private data.

## Source-only: check the actual final state

Use a separate source environment rather than overwriting the published install:

```bash
cd python
uv sync --all-packages
. .venv/bin/activate
cd ..
python examples/first-time-onboarding/verify.py --database
python examples/first-time-onboarding/verify_live.py --allow-paid --database
```

The fourth live case uses the SQL tools from
[`database.yaml`](../../examples/first-time-onboarding/database.yaml). Each
trial gets its own database, looks up its requested order and sets that order's
`refunded` flag. The final-state assertion requires exactly one refunded order;
`task-success`, `contains`, precision and recall must all equal 1.

This SQL feature was added in 0.3.2; PyPI 0.3.1, which this course pins, does not
have it. Do not pass `--database` to a 0.3.1 environment. Mock responses prove tool-call selection, not payment settlement;
the SQL assertion is stronger but still not a real payment-service test.

## What passed, and what it does not establish

The [re-verification report](12-macos-reverification.md) records passing live
non-streaming, streaming and offline replay on PyPI 0.3.1 and both PR source
versions, plus SQL state checks on source. These are tiny onboarding smoke
tests, not a reliability estimate, a prompt-injection certification or a
production launch decision. Use held-out tasks, repeated trials and real
business assertions before applying this pattern to an irreversible action.

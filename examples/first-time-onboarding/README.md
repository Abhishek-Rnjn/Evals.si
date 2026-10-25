# First-time onboarding lab

A local fake chat-completions model drives the **real evalsi-harness tool loop**.
All tasks are synthetic. No provider key, weights or paid model is required.
Read the [complete course](../../docs/tutorials/README.md) for explanations,
installation diagnostics, exercises and interpretation.

## Published-release quick run

From the repository root, create and activate a fresh environment:

```bash
python3 -m venv .venv-onboarding
. .venv-onboarding/bin/activate
python -m pip install 'evalsi[server]==0.3.1' 'evalsi-harness==0.3.1'
cd examples/first-time-onboarding
python verify.py
```

The verifier starts and stops its own loopback server on a free port. It checks
seven scenarios; expected exits are `0,3,3,3,3,0,0`. Failures of its assertions
exit nonzero. Results/tapes are temporary. For manual exploration:

```bash
python fake_model.py --log requests.jsonl
# In a second activated terminal, in this directory:
evalsi run -f run.yaml --output results.json
evalsi report results.json -o report.html
```

| Spec | Model behavior | Expected exit |
|---|---|---:|
| `run.yaml` | Lookup, refund, answer | 0 |
| `regressed.yaml` | Lookup and false success claim | 3 |
| `loop.yaml` | Repeated lookup, bounded at six turns | 3 |
| `bad-arguments.yaml` | Lookup without the required order ID | 3 |
| `fault.yaml` | Every refund fails; retry once, then stop | 3 |
| `record.yaml` | Record a successful run to tapes | 0 |
| `replay.yaml` | Replay those tapes without the model | 0 |
| `database.yaml` | Real per-trial SQL writes/assertions, **source only** | 0 |

`fake_model.py` simulates the next conversation turn from its prefix, not token
sampling. It supports non-streaming chat completions only. Its usage values are
synthetic. Mock tools do not change business state. The source-only database
extension is described in [lesson 5](../../docs/tutorials/05-state-and-handoff.md).
After installing the Python workspace, run `python verify.py --database` with
that environment active to exercise all nine scenarios.

Stop the manual server with Ctrl-C. Delete generated results and `tapes/` after
inspection. Request logs contain the entire synthetic conversation; do not log
real customer data this way.

## Optional live provider and Kubernetes paths

The [Haiku lesson](../../docs/tutorials/10-real-haiku.md) uses
`verify_live.py --allow-paid` for non-streaming, streaming and provider-free
replay, with `--database` reserved for source installs. Credentials come only
from the environment; no provider calls run in the offline verifier or CI tests.

The [kind lesson](../../docs/tutorials/11-local-kind.md) uses `kind-values.yaml`
and the `kind-*.yaml` resources for recorded scoring, a checked pod-sandbox
agent and a cluster-local TinyLM tool loop, including three negative controls.
Read the networking, persistence and cleanup limitations before applying them.

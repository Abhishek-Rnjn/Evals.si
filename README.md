# Evals.si
A highly scalable and pluggable evaluations repository that works with all environments to provide an unified experience for all your evaluation tasks

One entrypoint for evaluating classic ML models, LLMs, RAG systems, agents (offline and online) and fine-tuning/RL checkpoints:

- **Score / Run / Watch**: grade outputs you already have, execute a target on a dataset, or continuously evaluate live OpenTelemetry traces.
- **Pluggable**: existing frameworks (lm-evaluation-harness, Inspect AI, RAGAS, DeepEval, SWE-bench, τ-bench, …) plug in as isolated adapters.
- **Standalone or Kubernetes**: a single binary or an operator with CRDs; gRPC and HTTP APIs, with MCP planned.
- **Sandboxed execution**: Firecracker microVMs where available, otherwise bubblewrap or Landlock, otherwise hardened Kubernetes pods; always fails closed.
- **Runs in your environment**: self-hosted and air-gappable, with bring-your-own models, storage, identity and secrets.

> **Status:** Phases 0 to 3 are done. The standalone server covers three doors:
>
> - **Score:** grade outputs you already have.
> - **Run:** durable, resumable runs with trials, gates and budgets. That includes agent runs, which put the built-in agent or your own agent to work on sandboxed tasks.
> - **Watch:** online evaluation of OpenTelemetry traces.
>
> Around them are:
>
> - evaluator packs and framework adapters;
> - benchmark adapters for SWE-bench, τ-bench, Terminal-Bench/Harbor and BFCL;
> - a fail-closed sandbox ladder (Firecracker, bubblewrap, Landlock);
> - MLflow and OTel sinks;
> - identity and access: OIDC/JWT and API keys, project-scoped RBAC with custom roles, agentgateway-style CEL rules, and an audit log.
>
> Next comes Phase 4, Kubernetes. See the [architecture and implementation plan](docs/DESIGN.md) and the [decision records](docs/decisions/README.md).

## Quickstart

You need [uv](https://docs.astral.sh/uv/) (it installs Python 3.11+ for you). Running the server also needs [Go](https://go.dev/dl/) 1.26+. On Linux, the sandbox for code evaluators needs bubblewrap (see [Development](#development)).

```bash
git clone https://github.com/Abhishek-Rnjn/Evals.si && cd Evals.si
cd python && uv sync --all-packages
uv run evalsi eval --data ../examples/quickstart/qa.jsonl --evaluators exact-match,numeric-match,latency
```

```text
6 records · 3 evaluators

metric         n  mean     95% CI              skipped  errors
exact-match    6  0.333    [0.097, 0.700]            0       0
numeric-match  3  0.667    [0.208, 0.939]            3       0
latency        6  517.500  [276.409, 758.591]        0       0
```

Records that lack what an evaluator needs (here, a numeric reference) are **skipped**, never scored as zero. Failures in the evaluator or judge are reported as **errors** and excluded from the metrics. Every mean comes with a confidence interval. Use `--cluster-by <metadata key>` when records are correlated.

### With a judge model

There is no default judge, so nothing is billed by surprise. Configure one explicitly:

```bash
# Any OpenAI-compatible server: vLLM, Ollama, LiteLLM, an AI gateway, OpenAI itself
uv run evalsi eval --data ../examples/quickstart/qa.jsonl --evaluators exact-match,llm-judge \
  --judge-model my-judge --judge-base-url http://localhost:8000/v1

# Claude, through the official SDK (installed by the `anthropic` extra)
uv run evalsi eval --data ../examples/quickstart/qa.jsonl --evaluators llm-judge \
  --judge-provider anthropic --judge-model claude-opus-5-5 --param llm-judge.rubric=helpfulness
```

Judge responses are cached by content hash, so re-running an unchanged evaluation costs nothing. Pass `--no-cache` to force fresh calls.

### From Python

```python
import evalsi
from evalsi import Record, Score, evaluator

@evaluator(name="acme/polite", version="1.0.0")
def polite(record: Record) -> Score:
    return Score(passed="please" in record.output.as_text().lower())

result = evalsi.evaluate("qa.jsonl", ["exact-match", polite])
print(result.table())
result.save("results.json")   # manifest, summaries and every per-record result
```

`evalsi catalog` lists every installed evaluator and its params.

### As a server (gRPC and HTTP)

`evalsid` serves the same evaluators over gRPC, gRPC-Web and HTTP/JSON on one port. It runs the Python evaluators in a supervised worker process.

```bash
go build -o bin/evalsid ./cmd/evalsid        # from the repository root
cd python && uv sync --all-packages          # installs evalsi[server] into python/.venv
EVALSID=../bin/evalsid uv run evalsi serve --config ../examples/server/evalsi.yaml
```

**Authentication is optional.**

- **Loopback, no `auth` section.** On `127.0.0.1` with no `auth` section the server runs without authentication, as the log says. That is fine on a laptop.
- **Any other address** needs authentication, unless you switch it off explicitly.
- **To switch it off for development or testing,** even with an `auth` section in the config, use one of:
  - `evalsi serve --no-auth`;
  - `evalsid serve --no-auth`;
  - `EVALSID_NO_AUTH=1` (convenient in containers);
  - `auth: {mode: none}` in the file.

  With any of these, every caller is treated as an owner and the server logs a warning at startup. The rest of your auth, roles and rules stay in the config, unenforced, so removing the switch restores them.

To try access control, see [Try access control locally](#try-access-control-locally).

The example's default judge is Claude: export `ANTHROPIC_API_KEY` before starting the server for `llm-judge` to work. Without it, `exact-match` still scores and `llm-judge` reports an error, never a zero. To use a local OpenAI-compatible model instead, edit the `local` judge in the config.

```bash
# HTTP/JSON
curl -s localhost:8080/evalsi.v1alpha1.EvaluationService/Evaluate -H 'content-type: application/json' -d '{
  "records": [{"output": {"text": "Paris"}, "reference": {"text": "Paris"}},
              {"output": {"text": "Lyon"},  "reference": {"text": "Paris"}}],
  "evaluators": [{"ref": "exact-match"}, {"ref": "llm-judge", "params": {"rubric": "correctness"}}]
}'

# gRPC (reflection is on)
grpcurl -plaintext localhost:8080 list
grpcurl -plaintext localhost:8080 evalsi.v1alpha1.CatalogService/ListEvaluators
```

Large jobs use `EvaluationService/EvaluateStream`: send a config message, then records, and receive results as they finish, followed by the summaries. Intervals match the embedded library exactly, bootstrap included. `GET /healthz` reports whether the worker is up.

The same services answer plain REST under `/v1alpha1`, for example:

- `POST /v1alpha1/evaluate`
- `GET /v1alpha1/evaluators`
- `POST /v1alpha1/runs`, `GET /v1alpha1/runs/{id}`, `POST /v1alpha1/runs/{id}:cancel`
- `GET /v1alpha1/runs/{id}/results`
- `POST /v1alpha1/policies`, `GET /v1alpha1/traces/{trace_id}`
- `GET /v1alpha1/whoami`, `POST /v1alpha1/apikeys`, `GET /v1alpha1/audit`

The full list is in `internal/server/rest.go`.

### Runs: execute a target, gate on the result

A run spec generates outputs from a target (an OpenAI-compatible server such as vLLM, or Claude), scores them, and checks gates. The same file runs embedded or on a server:

```bash
uv run evalsi run -f ../examples/runs/capitals.yaml                               # embedded
uv run evalsi run -f ../examples/runs/capitals.yaml --server http://localhost:8080
uv run evalsi compare --server http://localhost:8080 <baseline-run> <candidate-run>
```

- **Trials.** `trials: 3` adds pass@3 and pass^3.
- **Gates.** They fail the command with exit code 3, which makes them usable in CI.
- **Budgets.** They cap target and judge tokens.
- **Durable server runs.** Runs on a server are stored and can be watched, cancelled and resumed. A run interrupted by a restart resumes without redoing finished work.

### Agent runs: put an agent to work on sandboxed tasks

An agent run gives each task its own environment: an OCI image, files, setup, and a sandbox policy. The agent works in it, a checker grades the end state, and evaluators score the trajectory and the agent's diff. The agent is either the built-in tool-calling agent on any model, or your own over A2A, MCP, an OpenAI Responses-compatible API or HTTP, or as a CLI run inside the sandbox.

```bash
uv run evalsi run -f ../examples/agents/fix-calc.yaml --judge-provider anthropic --judge-model claude-opus-5-5
uv run evalsi run -f ../examples/agents/swebench-verified.yaml --server http://localhost:8080
uv run evalsi promote <run> --dataset regressions --when 'scores["task-success"] < 1' --server http://localhost:8080
```

- **The harness.** Sandbox and MCP tools, mocks and fault injection, budgets, a simulated user, and record and replay with branching.
- **Grading.** `task-success` with pass@k and pass^k; `code-quality`, a judge review of the agent's diff; `policy-violations`, from sandbox denials and refused egress.
- **Benchmarks.** `swebench://`, `taubench://`, `harbor://`, `terminal-bench://` and `bfcl://` datasets, each graded by the benchmark's own code.
- **Sandbox ladder.** Firecracker microVMs where `/dev/kvm` exists, otherwise bubblewrap or Landlock, with the same scoring on every rung.
- **From production to regression tests.** Failed results and production traces become datasets, and shadow replay compares a candidate against recorded behavior.

See the [agent runs guide](docs/guides/agent-runs.md).

### Watch: evaluate live agent traces

Point any OpenTelemetry-instrumented app or agent (OTel GenAI, OpenInference, OpenLLMetry, MLflow), or agentgateway, at the server's OTLP endpoint. Then apply a policy:

```bash
uv run evalsi policy apply -f ../examples/watch/support-policy.yaml --server http://localhost:8080
uv run evalsi policy stats support-agent --server http://localhost:8080
```

A policy has these parts:

- a CEL selector;
- deterministic sampling;
- cascades, so the judge runs only after cheap checks pass;
- windowed alerts with webhooks;
- promotion of interesting traces into datasets.

### Evaluators, adapters and the sandbox

`evalsi catalog` lists what is installed. The built-in packs are:

| Pack | Evaluators |
|---|---|
| `core` | exact, contains, fuzzy, numeric and regex match; JSON validity and schema; length; latency; token usage; cost |
| `judge` | `llm-judge` with rubrics |
| `text` | BLEU, corpus BLEU, ROUGE-1/2/L, chrF, token F1 |
| `rag` | faithfulness, answer relevance, context precision and recall, citation accuracy |
| `safety` | PII, secret and canary leaks; refusal; harmlessness |
| `agent` | tool-call accuracy, trajectory match, tool errors, loop detection, step budget, goal completion; for agent runs, task success (pass@k, pass^k), code quality, policy violations, efficiency |
| `code` | unit tests run in the sandbox, Python syntax |

**Framework adapters.** DeepEval, RAGAS, Inspect AI and lm-evaluation-harness live in [`python/adapters`](python/adapters/README.md), as do the SWE-bench, τ-bench and BFCL benchmark adapters. Each has its own pinned environment, and judge calls go through your configured judge.

**Sandbox for code evaluators.** Code evaluators run untrusted code under the strongest rung that works on the host:

- **bubblewrap (namespaced):** no network, a read-only system root, seccomp and resource limits.
- **Landlock (confined):** the fallback when bubblewrap is unavailable.

The sandbox fails closed: when no rung works, it never runs code unconfined. Check what works on a host with:

```bash
evalsid sandbox probe
```

**Sinks.** Finished runs and online scores can be exported:

- **MLflow:** runs become MLflow runs, and online scores can be written back as assessments on MLflow traces.
- **OpenTelemetry:** scores become `gen_ai.evaluation.result` events linked to the evaluated trace.

See the `sinks` section of [`examples/server/evalsi.yaml`](examples/server/evalsi.yaml).

### Identity and access

A server on a non-loopback address must authenticate. It accepts:

- **API keys** (`evalsid auth new-key`);
- **tokens from your OIDC provider:** Keycloak, Entra ID, Okta, Auth0, Google and others;
- **GitHub Actions OIDC tokens**, with no stored secret;
- **client certificates.**

Access is granted per project:

- **Built-in roles:** viewer, runner, editor, admin, ingest and owner.
- **Custom roles** built from the permission list, optionally limited by CEL conditions such as allowed models or labels.
- **Global rules** in agentgateway's `allow`, `deny` and `require` form.

```bash
evalsi login --server https://evals.example.com      # device code; --browser for PKCE
evalsi whoami --server https://evals.example.com
evalsi auth keys create ci --role support=runner --ttl 90d --server https://evals.example.com
evalsi auth audit --denied --server https://evals.example.com
evalsid auth check --config evalsi.yaml --api-key "$KEY" --action runs.create --project support
```

Scripts and CI use `EVALSI_API_KEY` or `EVALSI_TOKEN`; GitHub Actions jobs set `EVALSI_OIDC_AUDIENCE`. See the [setup guide](docs/guides/identity.md) and [`examples/auth/evalsi.yaml`](examples/auth/evalsi.yaml).

#### Try access control locally

[`examples/auth/local.yaml`](examples/auth/local.yaml) needs no identity provider or TLS. It uses API keys only, listens on loopback only, and has one project (`demo`) and a custom role (`prompt-engineer`, limited to some models).

1. Create your owner key, and put its hash in the config. The server refuses to start while the placeholder is there.

   ```bash
   go build -o bin/evalsid ./cmd/evalsid
   bin/evalsid auth new-key
   # key:  evk_...          <- keep this; it is shown once
   # hash: sha256:...       <- paste over the placeholder in examples/auth/local.yaml
   ```

2. Start the server.

   ```bash
   cd python && uv sync --all-packages
   EVALSID=../bin/evalsid uv run evalsi serve --config ../examples/auth/local.yaml
   ```

3. In another terminal, from `python/`, use it.

   ```bash
   export EVALSI_SERVER=http://127.0.0.1:8080
   uv run evalsi whoami                                   # unauthenticated: rejected
   export EVALSI_API_KEY=evk_...                          # your owner key
   uv run evalsi whoami                                   # key:me, owner

   # Issue a key for someone else, scoped to one project and role.
   uv run evalsi auth keys create alice --role demo=prompt-engineer
   EVALSI_API_KEY=evk_<alice's key> uv run evalsi whoami                        # prompt-engineer in demo
   EVALSI_API_KEY=evk_<alice's key> uv run evalsi auth projects create other    # permission_denied

   uv run evalsi auth audit --denied                      # alice's denied call, and why
   ../bin/evalsid auth check --config ../examples/auth/local.yaml --api-key evk_<alice's key> \
     --action runs.create --project demo --resource '{"target":{"model":"gpt-5"}}'   # deny: model not allowed
   ```

Delete `python/.evalsi-auth-demo` to start over. To use the same config without access control for a while, add `--no-auth` to the `serve` command.

## Repository layout

| Path | What |
|------|------|
| `proto/` | Protobuf API, the single source of truth (`evalsi.v1alpha1`, `evalsi.plugin.v1alpha1`) |
| `gen/go/` | Generated Go code (do not edit; run `make proto`) |
| `cmd/evalsid/`, `internal/` | The Go daemon: API and REST routes, authentication (`auth`) and authorization (`authz`), worker supervision, runs, OTLP ingest and online policies, sandbox, sinks, statistics |
| `python/evalsi/` | Python SDK, CLI, embedded runner, evaluator worker and built-in packs |
| `python/evalsi-harness/` | The agent harness: tool loop, agent connectors, environments and checkers, Harbor and Terminal-Bench importers |
| `python/adapters/` | Framework and benchmark adapters (DeepEval, RAGAS, Inspect AI, lm-eval, SWE-bench, τ-bench, BFCL), each in its own environment |
| `cmd/evalsi-guest/` | The init and agent inside Firecracker microVMs |
| `tests/e2e/` | evalsid against a real Python worker (`make e2e`) |
| `examples/` | Runnable examples |
| `docs/` | Design plan, decision records and guides |

## Development

You need Go (1.26+), [buf](https://buf.build/docs/installation) and [uv](https://docs.astral.sh/uv/).

```bash
make tools   # protobuf plugins, at the versions CI uses
make proto   # lint, format and regenerate code after editing proto/
make check   # everything CI runs: gofmt, go vet/test, buf lint/format, ruff, mypy, pytest
make e2e     # evalsid against a real Python worker; EVALSI_E2E_IMAGES=1 adds tests that pull images
make adapters-check   # each framework adapter's contract tests, in its own environment
```

The sandbox tests need bubblewrap and unprivileged user namespaces. On Ubuntu 24.04, also run `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`.

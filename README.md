# Evals.si
A highly scalable and pluggable evaluations repository that works with all environments to provide an unified experience for all your evaluation tasks

One entrypoint for evaluating classic ML models, LLMs, RAG systems, agents (offline and online) and fine-tuning/RL checkpoints:

- **Score / Run / Watch**: grade outputs you already have, execute a target on a dataset, or continuously evaluate live OpenTelemetry traces.
- **Pluggable**: existing frameworks (lm-evaluation-harness, Inspect AI, RAGAS, DeepEval, SWE-bench, τ-bench, …) plug in as isolated adapters.
- **Standalone or Kubernetes**: a single binary or an operator with CRDs; gRPC and HTTP APIs, with MCP planned.
- **Sandboxed execution**: Firecracker microVMs where available, otherwise bubblewrap or Landlock, otherwise hardened Kubernetes pods; always fails closed.
- **Runs in your environment**: self-hosted and air-gappable, with bring-your-own models, storage, identity and secrets.

> **Status:** Phases 0 and 1 are done. The standalone server covers three doors:
>
> - **Score:** grade outputs you already have.
> - **Run:** durable, resumable runs with trials, gates and budgets.
> - **Watch:** online evaluation of OpenTelemetry traces.
>
> Around them are evaluator packs, framework adapters, a fail-closed sandbox for code evaluators, and MLflow and OTel sinks. Next comes Phase 2: the agent harness and Firecracker. See the [architecture and implementation plan](docs/DESIGN.md) and the [decision records](docs/decisions/README.md).

## Quickstart

```bash
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
go build -o bin/evalsid ./cmd/evalsid
cd python && uv sync --all-packages          # installs evalsi[server] into python/.venv
EVALSID=../bin/evalsid uv run evalsi serve --config ../examples/server/evalsi.yaml
```

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
| `agent` | tool-call accuracy, trajectory match, tool errors, loop detection, step budget, goal completion |
| `code` | unit tests run in the sandbox, Python syntax |

**Framework adapters.** DeepEval, RAGAS, Inspect AI and lm-evaluation-harness live in [`python/adapters`](python/adapters/README.md). Each has its own pinned environment, and their judge calls go through your configured judge.

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

## Repository layout

| Path | What |
|------|------|
| `proto/` | Protobuf API, the single source of truth (`evalsi.v1alpha1`, `evalsi.plugin.v1alpha1`) |
| `gen/go/` | Generated Go code (do not edit; run `make proto`) |
| `cmd/evalsid/`, `internal/` | The Go daemon: API and REST routes, worker supervision, runs, OTLP ingest and online policies, sandbox, sinks, statistics |
| `python/evalsi/` | Python SDK, CLI, embedded runner, evaluator worker and built-in packs |
| `python/adapters/` | Framework adapters (DeepEval, RAGAS, Inspect AI, lm-eval), each in its own environment |
| `tests/e2e/` | evalsid against a real Python worker (`make e2e`) |
| `examples/` | Runnable examples |
| `docs/` | Design plan and decision records |

## Development

You need Go (1.26+), [buf](https://buf.build/docs/installation) and [uv](https://docs.astral.sh/uv/).

```bash
make tools   # protobuf plugins, at the versions CI uses
make proto   # lint, format and regenerate code after editing proto/
make check   # everything CI runs: gofmt, go vet/test, buf lint/format, ruff, mypy, pytest
make e2e     # evalsid against a real Python worker
make adapters-check   # each framework adapter's contract tests, in its own environment
```

The sandbox tests need bubblewrap and unprivileged user namespaces. On Ubuntu 24.04, also run `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`.

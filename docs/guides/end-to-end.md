# Evals.si end to end: features, integration and running it in a pull request

This guide walks through everything Evals.si does, in the order you would adopt it, and ends with ready-to-copy pull request workflows. Every command and file here uses the CLI, config keys and example files that are in this repository. Each section links to the deeper guide for its topic.

- [1. Mental model](#1-mental-model)
- [2. Install](#2-install)
- [3. Score: grade outputs you already have](#3-score-grade-outputs-you-already-have)
- [4. Run: execute a target, gate on the result](#4-run-execute-a-target-gate-on-the-result)
- [5. Agent runs and benchmarks](#5-agent-runs-and-benchmarks)
- [6. Watch: online evaluation of live traces](#6-watch-online-evaluation-of-live-traces)
- [7. Run the server](#7-run-the-server)
- [8. Identity and RBAC](#8-identity-and-rbac)
- [9. Integrations](#9-integrations)
- [10. Reports, analytics and sinks](#10-reports-analytics-and-sinks)
- [11. Fine-tuning and RL](#11-fine-tuning-and-rl)
- [12. Kubernetes](#12-kubernetes)
- [13. Running evals in a pull request](#13-running-evals-in-a-pull-request)
- [14. Reference](#14-reference)

## 1. Mental model

Evals.si has three "doors" and one set of building blocks.

| Door | Question it answers | Entry point |
|---|---|---|
| **Score** | How good are these outputs I already have? | `evalsi eval`, `evalsi.evaluate(...)`, `EvaluationService/Evaluate` |
| **Run** | How good is this model or agent on this dataset, and does it pass my gates? | `evalsi run -f run.yaml` |
| **Watch** | How good is my live traffic right now? | An `OnlineEvalPolicy` plus the server's OTLP endpoint |
| **Reward** | Can the same graders train a model? | `RewardSpec`, `evalsi.rewards`, the Reward Service |

The building blocks are shared by all four:

- **Records** are the unit of evaluation: `input`, `output`, optional `reference`, `metadata`, `usage` and trajectory.
- **Evaluators** grade a record. They come in packs (`core`, `judge`, `text`, `rag`, `safety`, `agent`, `code`, `rl`, `finetune`) and from framework adapters (DeepEval, RAGAS, Inspect, lm-eval).
- **Run specs** (`EvalRun`) say what to run: target, dataset, evaluators, trials, gates and budgets. The same file runs embedded, on a server, or as a Kubernetes resource.
- **Gates** turn metrics into a pass/fail verdict. A failed gate exits with code 3, which is what makes Evals.si usable as a pull request check.

Two rules hold everywhere, and are worth knowing before you read results:

- A record that lacks what an evaluator needs (for example, no numeric reference) is **skipped**, never scored as zero.
- A failure in an evaluator or judge is reported as an **error** and excluded from the metrics. Errors never look like low scores.

Every mean carries a confidence interval. Use `--cluster-by <metadata key>` when records are correlated.

## 2. Install

You need [uv](https://docs.astral.sh/uv/) (it installs Python 3.11+). Running the server also needs [Go](https://go.dev/dl/) 1.26+. On Linux, code evaluators need bubblewrap.

```bash
git clone https://github.com/Abhishek-Rnjn/Evals.si && cd Evals.si
cd python && uv sync --all-packages        # the SDK, CLI and harness
cd .. && go build -o bin/evalsid ./cmd/evalsid     # only needed for the server and sandboxed runs
export PATH=$PWD/bin:$PATH
```

Check the install and what the host's sandbox supports:

```bash
cd python
uv run evalsi version
uv run evalsi catalog                 # every installed evaluator and its params
uv run evalsi catalog --pack core     # one pack
evalsid sandbox probe                 # which sandbox rungs work on this machine
```

In your own project, `pip install evalsi` gives you the CLI and SDK. Add the `anthropic` extra for the Claude judge or target, `server` for `evalsi serve`, and `analytics` for DuckDB analysis (`pip install 'evalsi[analytics]'`).

## 3. Score: grade outputs you already have

### From the command line

```bash
cd python
uv run evalsi eval --data ../examples/quickstart/qa.jsonl \
  --evaluators exact-match,numeric-match,latency
```

```text
6 records · 3 evaluators

metric         n  mean     95% CI              skipped  errors
exact-match    6  0.333    [0.097, 0.700]            0       0
numeric-match  3  0.667    [0.208, 0.939]            3       0
latency        6  517.500  [276.409, 758.591]        0       0
```

Useful options:

| Option | Use |
|---|---|
| `--map input=question --map reference=answer` | Map your column names onto record fields. |
| `--param llm-judge.rubric=helpfulness` | Set an evaluator param (parsed as JSON when possible). |
| `--limit 100` | Evaluate the first N records only. |
| `--cluster-by topic` | Cluster-aware confidence intervals. |
| `--output results.json` | Save the manifest, summaries and every per-record result. |
| `--format json` | Machine-readable output. |

Datasets can be JSONL or JSON files, or `hf://repo?config=..&split=..` for Hugging Face. Importer URIs such as `inspect://log.eval` and `lm-eval://samples_<task>.jsonl` also work (see [adapters](../../python/adapters/README.md)).

### With a judge model

There is no default judge, so nothing is billed by surprise. Name one explicitly:

```bash
# Any OpenAI-compatible server: vLLM, Ollama, LiteLLM, a gateway, OpenAI itself
uv run evalsi eval --data ../examples/quickstart/qa.jsonl --evaluators exact-match,llm-judge \
  --judge-model my-judge --judge-base-url http://localhost:8000/v1

# Claude, through the official SDK
export ANTHROPIC_API_KEY=...
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
result.save("results.json")
```

### Built-in evaluator packs

| Pack | Evaluators |
|---|---|
| `core` | exact, contains, fuzzy, numeric and regex match; JSON validity and schema; length; latency; token usage; cost |
| `judge` | `llm-judge` with rubrics |
| `text` | BLEU, corpus BLEU, ROUGE-1/2/L, chrF, token F1 |
| `rag` | faithfulness, answer relevance, context precision and recall, citation accuracy |
| `safety` | PII, secret and canary leaks; refusal; harmlessness |
| `agent` | tool-call accuracy, trajectory match, tool errors, loop detection, step budget, goal completion; task success (pass@k, pass^k), code quality, policy violations, efficiency |
| `code` | unit tests run in the sandbox, Python syntax |
| `rl` | format check, math equivalence, sandboxed code tests, overlong penalty, reward model |

### Framework adapters

Each adapter has its own pinned environment, so upstream dependency conflicts never reach the SDK.

```bash
cd python/adapters/ragas && uv sync
uv run evalsi eval --data qa.jsonl --evaluators ragas/faithfulness,deepeval/g-eval \
    --judge-provider anthropic --judge-model claude-opus-5-5
```

| Adapter | Gives you |
|---|---|
| DeepEval | `deepeval/*` metrics: faithfulness, hallucination, bias, toxicity, g-eval, tool correctness and more |
| RAGAS | `ragas/*` metrics: context precision/recall, factual correctness, agent goal accuracy, tool-call F1 and more |
| Inspect AI | `inspect://log.eval` importer, and `evalsi_scorer(...)` to use Evals.si evaluators inside Inspect |
| lm-evaluation-harness | An `evalsi` lm-eval model, and an `lm-eval://` importer |
| SWE-bench, τ-bench, BFCL | Benchmark importers graded by the benchmark's own code (section 5) |

DeepEval and RAGAS keep their own prompts and scoring, but their model calls go through your judge, so they are cached, rate-limited and costed like every other judge call, and the framework never reads a provider key.

## 4. Run: execute a target, gate on the result

A run spec generates outputs from a target, scores them, and checks gates. [`examples/runs/capitals.yaml`](../../examples/runs/capitals.yaml):

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: capitals
  labels: {evals.si/project: quickstart}
spec:
  target:
    connector: anthropic          # or openai-compatible with base_url
    model: claude-opus-5-5
    system_prompt: Answer with the name of the city only.
    max_tokens: 1024
    effort: low
  dataset:
    path: capitals.jsonl
    mapping: {input: question, reference: answer}
  evaluators:
    - ref: exact-match
    - ref: llm-judge
      params: {rubric: correctness}
  trials: 3                       # adds exact-match.pass@3 and pass^3
  summary: {confidence_level: 0.95}
  gates:
    - {metric: exact-match, min: 0.9}
    - {metric: exact-match.pass^3, stat: GATE_STAT_MEAN, min: 0.8}
  budget:
    max_target_tokens: 200000
    max_judge_tokens: 500000
```

```bash
cd python
uv run evalsi run -f ../examples/runs/capitals.yaml                                # embedded
uv run evalsi run -f ../examples/runs/capitals.yaml --server http://localhost:8080 # on a server
```

| Concept | What it does |
|---|---|
| **Trials** | `trials: 3` runs each record three times and adds `pass@3` and `pass^3`. |
| **Gates** | A gate failure exits with code 3. Exit codes: 0 all gates pass, 3 a gate failed, 1 an error. |
| **Budgets** | Cap target and judge tokens so a run cannot run away. |
| **Durable runs** | On a server, runs are stored, watchable, cancellable and resumable. A run interrupted by a restart resumes without redoing finished work. |

Related commands:

```bash
# Fire and forget; the run id is printed
uv run evalsi run -f run.yaml --server $URL --no-wait

# Paired comparison of two server runs (is the candidate significantly better or worse?)
uv run evalsi compare --server $URL <baseline-run> <candidate-run>

# Save a machine-readable result for CI artifacts
uv run evalsi run -f run.yaml --output results.json --format json
```

Dataset paths in an embedded run are relative to the spec file. On a server they are relative to its `datasets_dir` and cannot escape it.

## 5. Agent runs and benchmarks

An agent run gives each task its own sandboxed environment (an OCI image, files, setup commands, a sandbox policy). The agent works in it, a **checker** grades the end state, and evaluators score the trajectory and the diff. See the full [agent runs guide](agent-runs.md).

```bash
cd python
export ANTHROPIC_API_KEY=...
uv run evalsi run -f ../examples/agents/fix-calc.yaml \
  --judge-provider anthropic --judge-model claude-opus-5-5     # needs evalsid on PATH
```

[`examples/agents/fix-calc.yaml`](../../examples/agents/fix-calc.yaml) asks a model to fix two bugs in a git checkout. Its key parts:

```yaml
spec:
  target: {connector: anthropic, model: claude-opus-5-5}
  harness:
    builtin:
      max_steps: 20
      budget: {usd: 0.50, wall_clock: 5m}
  environment:
    files: {calc.py: "..."}
    sandbox: {min_isolation: namespaced, network: deny}
    checker:
      files: {test_hidden.py: "..."}      # written after the agent finishes
      command: [python3, test_hidden.py]
  evaluators:
    - ref: task-success          # the checker's verdict; with trials, pass@k and pass^k
    - ref: code-quality          # a judge reviews the agent's diff
    - ref: policy-violations     # sandbox denials, refused egress, escalation requests
    - ref: agent-efficiency
  trials: 3
  gates:
    - {metric: task-success.pass^3, min: 0.5}
```

**Bring your own agent** with `target.agent`: A2A, MCP, an OpenAI Responses-compatible API, plain HTTP, or a CLI program run inside the sandbox.

```yaml
target:
  agent:
    http:
      url: https://staging.example.com/agent
      body_template: '{"message": "{{input}}"}'
      output_path: reply
```

**Harness features:** sandbox and MCP tools, mocked tools, fault injection (`error`, `timeout`, `malformed`), a simulated user for multi-turn tasks, and record and replay with `branch_at_step`.

**Benchmarks** import as datasets and are graded by the benchmark's own code:

| URI | Benchmark |
|---|---|
| `swebench://<file>` | SWE-bench instances (the instance's image, graded by its eval script) |
| `taubench://<domain>` | τ-bench (tau2): tools, policy, simulated user |
| `harbor://<dir>`, `terminal-bench://<dir>` | Harbor and Terminal-Bench task directories |
| `bfcl://<category>` | Berkeley Function-Calling Leaderboard |

**Sandbox ladder.** Code and agent environments run on the strongest rung the host has, and the sandbox fails closed: when no rung works, nothing runs unconfined.

| Rung | Needs | Isolation |
|---|---|---|
| Firecracker microVM | usable `/dev/kvm` | `vm` |
| bubblewrap | unprivileged user namespaces | `namespaced`: no network, read-only system root, seccomp, cgroup limits |
| Landlock | a Landlock-capable kernel | `confined` |
| Hardened pod (Kubernetes) | a cluster | one locked-down pod per sandbox |

Set `sandbox.min_isolation` in a spec to refuse running on a weaker rung. On Ubuntu 24.04, bubblewrap needs `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`.

### From production to regression tests

```bash
# Add every failed task of a run to a dataset (needs datasets.write, which `editor` has)
uv run evalsi promote <run> --dataset regressions --when 'scores["task-success"] < 1' --server $URL

# Replay recorded inputs against a candidate and compare with the recording
uv run evalsi shadow -f candidate.yaml --server $URL
```

## 6. Watch: online evaluation of live traces

Point any OpenTelemetry-instrumented app or agent (OTel GenAI, OpenInference, OpenLLMetry, MLflow conventions) or agentgateway at the server's OTLP endpoint. The server accepts OTLP gRPC and HTTP on the main port, and on 4317/4318 when enabled in `evalsi.yaml`.

```bash
# In the app (standard OpenTelemetry environment variables)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer evk_..."    # an `ingest` key, if auth is on
```

Then apply a policy ([`examples/watch/support-policy.yaml`](../../examples/watch/support-policy.yaml)):

```bash
uv run evalsi policy apply -f ../examples/watch/support-policy.yaml --server http://localhost:8080
uv run evalsi policy list  --server http://localhost:8080
uv run evalsi policy stats support-agent --server http://localhost:8080
uv run evalsi policy delete support-agent --server http://localhost:8080
```

A policy has these parts:

```yaml
apiVersion: evals.si/v1alpha1
kind: OnlineEvalPolicy
metadata:
  name: support-agent
  labels: {evals.si/project: support}
spec:
  selector: service == "support-agent"            # CEL over the trace
  sampling:
    rate: 0.1                                     # 10% of matching traces...
    always: ["error", "duration_ms > 20000"]      # ...plus every failure or slow turn
  stages:
    - evaluators:                                 # cheap checks on every sampled trace
        - {ref: builtin/latency}
        - {ref: builtin/json-valid, name: valid-json}
    - when: 'scores["valid-json"] == 1.0'         # the judge only when they pass
      evaluators:
        - ref: builtin/llm-judge
          params: {rubric: helpfulness}
  window: 300s
  alerts:
    - {metric: llm-judge, below: 0.7, min_samples: 20, webhook: https://hooks.example.com/evalsi}
  promote:
    when: 'scores["llm-judge"] < 0.5'
    dataset: support-regressions
```

- **Selector:** CEL over `service`, `name`, `duration_ms`, `error`, `steps`, `tools`, `models`, `attributes`, `resource`.
- **Sampling:** deterministic, so the same trace is always in or out.
- **Cascades:** later stages run only when their `when` holds, so the expensive judge sees only traces that passed cheap checks.
- **Alerts:** windowed, with webhooks.
- **Promotion:** interesting traces become dataset records, which an `EvalRun` can then use as a regression suite.

For agentgateway, see [agentgateway on Kubernetes](agentgateway-kubernetes.md).

## 7. Run the server

`evalsid` serves the evaluators and everything above over gRPC, gRPC-Web and HTTP/JSON on one port. It runs the Python evaluators in a supervised worker process.

```bash
go build -o bin/evalsid ./cmd/evalsid
cd python && uv sync --all-packages
export ANTHROPIC_API_KEY=...        # the example's default judge is Claude
EVALSID=../bin/evalsid uv run evalsi serve --config ../examples/server/evalsi.yaml
```

Health and a first call:

```bash
curl -s localhost:8080/healthz

curl -s localhost:8080/evalsi.v1alpha1.EvaluationService/Evaluate -H 'content-type: application/json' -d '{
  "records": [{"output": {"text": "Paris"}, "reference": {"text": "Paris"}},
              {"output": {"text": "Lyon"},  "reference": {"text": "Paris"}}],
  "evaluators": [{"ref": "exact-match"}]
}'

grpcurl -plaintext localhost:8080 list       # reflection is on
```

REST routes under `/v1alpha1` include `POST /evaluate`, `GET /evaluators`, `POST /runs`, `GET /runs/{id}`, `POST /runs/{id}:cancel`, `GET /runs/{id}/results`, `POST /policies`, `GET /traces/{trace_id}`, `GET /whoami`, `POST /apikeys` and `GET /audit`. The full list is in `internal/server/rest.go`. Large jobs use `EvaluationService/EvaluateStream`.

[`examples/server/evalsi.yaml`](../../examples/server/evalsi.yaml) documents every key. The ones you will touch first:

| Key | Purpose |
|---|---|
| `listen`, `data_dir` | Address, and where SQLite keeps runs, traces and policies. |
| `datasets_dir` | Where run specs may read dataset files (local path or `s3://`). |
| `judges`, `default_judge` | Named judges. API keys are read from environment variables, never stored in the file. |
| `runs.max_concurrent` | Runs executing at once; the rest wait as PENDING. |
| `sandbox` | The rung ladder and `min_isolation`. |
| `sinks` | MLflow, OpenTelemetry, Langfuse and Phoenix exports. |
| `quotas` | Per-project limits on runs, stored runs and daily token use. |
| `rewards` | Reward Service limits and cache. |
| `storage`, `cluster` | PostgreSQL, ClickHouse, S3 and NATS for several replicas. |

## 8. Identity and RBAC

A server on a non-loopback address must authenticate. The full setup, including Keycloak, Entra ID, Okta, Auth0 and Google snippets, is in the [identity guide](identity.md).

**Credentials accepted:** API keys (`evk_...`), OIDC/JWT bearer tokens, GitHub Actions OIDC tokens, Kubernetes service-account tokens, and client certificates.

**Authorization** is per project:

| Layer | What it does |
|---|---|
| Built-in roles | `viewer`, `runner`, `editor`, `admin`, `ingest`, `owner` |
| Custom roles | Built from the permission list (`evalsi auth roles permissions`), optionally limited by a CEL `condition` such as allowed models |
| Bindings | Grant a role to a subject (`user:`, `group:`, `email:`, `cel:`) in a project |
| Global rules | agentgateway-style `allow`, `deny` and `require` CEL rules |
| External authorization | Optional: defer to a central policy engine (AuthZEN, or Envoy ext_authz as OPA serves it) |
| Audit log | Mutating calls and every denied call, with the deciding rule |

| Role | Permissions |
|---|---|
| `viewer` | read catalog, runs, policies, traces |
| `runner` | viewer, plus evaluations and runs (create, cancel, resume) |
| `editor` | runner, plus writing policies and promoting results into datasets |
| `admin` | editor, plus the project's keys, roles, bindings and audit log |
| `ingest` | write traces only |
| `owner` | everything, everywhere |

Guardrails: a project admin can grant only permissions it holds itself, under conditions at least as strict as its own, and unknown permissions, inheritance cycles and non-compiling conditions are rejected.

### Try it locally in five minutes

[`examples/auth/local.yaml`](../../examples/auth/local.yaml) needs no identity provider or TLS.

```bash
# 1. Make an owner key; paste the printed hash over the placeholder in examples/auth/local.yaml
go build -o bin/evalsid ./cmd/evalsid
bin/evalsid auth new-key

# 2. Start the server
cd python && uv sync --all-packages
EVALSID=../bin/evalsid uv run evalsi serve --config ../examples/auth/local.yaml

# 3. In another terminal, from python/
export EVALSI_SERVER=http://127.0.0.1:8080
uv run evalsi whoami                              # no credential: rejected
export EVALSI_API_KEY=evk_...                     # your owner key
uv run evalsi whoami                              # key:me, owner

# Issue a key for someone else, scoped to one project and role
uv run evalsi auth keys create alice --role demo=prompt-engineer
EVALSI_API_KEY=evk_<alice's key> uv run evalsi whoami                      # prompt-engineer in demo
EVALSI_API_KEY=evk_<alice's key> uv run evalsi auth projects create other  # permission_denied

# Why was it denied?
uv run evalsi auth audit --denied
../bin/evalsid auth check --config ../examples/auth/local.yaml --api-key evk_<alice's key> \
  --action runs.create --project demo --resource '{"target":{"model":"gpt-5"}}'   # deny: model not allowed
```

The `prompt-engineer` role in that example is a custom role that can start runs only on `qwen3` or `llama3`:

```yaml
rbac:
  roles:
    - name: prompt-engineer
      inherits: [viewer]
      permissions: [evaluations.run, runs.create]
      condition: '!has(resource.target) || resource.target.model in ["qwen3", "llama3"]'
  projects:
    demo: {}
```

### Everyday commands

```bash
evalsi login --server https://evals.example.com            # device code; --browser for PKCE
evalsi whoami --server https://evals.example.com
evalsi auth keys create ci --role support=runner --ttl 90d --server https://evals.example.com
evalsi auth keys list|revoke ...
evalsi auth roles create|update|delete|list ...
evalsi auth bindings create|delete|list ...
evalsi auth projects create|list ...
evalsi auth audit --denied
```

For scripts and CI, set `EVALSI_API_KEY` or `EVALSI_TOKEN` (or `EVALSI_TOKEN_FILE`). GitHub Actions jobs set `EVALSI_OIDC_AUDIENCE` and need no stored secret (section 13).

To run a config without enforcement while developing, add `--no-auth`, set `EVALSID_NO_AUTH=1`, or use `auth: {mode: none}`. Every caller is then an owner, and a warning is logged on every start.

## 9. Integrations

| Integration | How | Where |
|---|---|---|
| **LLM providers** | Targets and judges use `anthropic` or `openai-compatible` (vLLM, Ollama, LiteLLM, gateways, OpenAI). Keys come from environment variables. | [`examples/server/evalsi.yaml`](../../examples/server/evalsi.yaml) `judges` |
| **Your own agent** | A2A, MCP, OpenAI Responses, HTTP, or CLI-in-sandbox through `target.agent`. | [Agent runs](agent-runs.md#agents) |
| **OpenTelemetry apps** | Send OTLP to the server (gRPC or HTTP). OTel GenAI, OpenInference, OpenLLMetry and MLflow conventions are understood. | Section 6 |
| **agentgateway** | Export its traces to evalsi with an `ingest` key, and share OIDC issuers. | [agentgateway on Kubernetes](agentgateway-kubernetes.md) |
| **MLflow** | Runs become MLflow runs; online scores can be written back as assessments on MLflow traces. | `sinks.mlflow` |
| **Langfuse, Phoenix** | Runs become Langfuse traces with a score per metric; online scores land on the same trace IDs. Phoenix gets trace annotations. | `sinks.langfuse`, `sinks.phoenix` |
| **OpenTelemetry sink** | Scores become `gen_ai.evaluation.result` events linked to the evaluated trace. | `sinks.otel` |
| **Grafana / Prometheus** | `/metrics` on the server; a Grafana dashboard ships with the Helm chart. | [Kubernetes guide](kubernetes.md#dashboards) |
| **Evaluation frameworks** | DeepEval, RAGAS, Inspect AI, lm-evaluation-harness. | Section 3 |
| **Training frameworks** | TRL, verl, OpenRLHF rewards; checkpoint evaluation callback. | Section 11 |
| **GitHub Actions** | OIDC login with no stored secret, gates as exit code 3. | Section 13 |
| **Kubernetes** | CRDs, operator, Helm charts, KEDA scaling. | Section 12 |
| **Webhooks** | Online-policy alerts post to a URL you name. | Section 6 |

A `sinks` example (exports run in the background with retries and never fail a run):

```yaml
sinks:
  - mlflow:
      tracking_uri: http://mlflow:5000
      experiment: support-agent
      token_env: MLFLOW_TRACKING_TOKEN
      trace_feedback: true
  - langfuse:
      host: https://cloud.langfuse.com
      public_key_env: LANGFUSE_PUBLIC_KEY
      secret_key_env: LANGFUSE_SECRET_KEY
      trace_feedback: true
  - otel:
      endpoint: http://otel-collector:4318
```

## 10. Reports, analytics and sinks

```bash
# A report with metrics and intervals, the gates, and the lowest-scoring records
uv run evalsi eval --data qa.jsonl --evaluators exact-match --output results.json
uv run evalsi report results.json -o report.html          # or report.md
uv run evalsi report --server $URL <run-id> -o report.md  # a server run

# Cross-run analytics in DuckDB (needs the analytics extra)
uv run evalsi analyze slice exact-match --by category --results 'nightly-*.json'
uv run evalsi analyze query "select model, avg(value) from scores join runs using (run_id) group by 1" \
  --server $URL --project support --label suite=nightly --db nightly.duckdb
```

`analyze` loads runs into three tables, `runs`, `scores` and `records`, which you can query with SQL or slice by record metadata.

## 11. Fine-tuning and RL

Full detail is in the [fine-tuning and RL guide](fine-tuning.md).

**Rewards.** A `RewardSpec` composes evaluators into a reward: weights, gates, per-component breakdown, a floor, clipping, and a content cache (GRPO repeats rollouts, so repeats cost nothing). [`examples/finetuning/code-reward.yaml`](../../examples/finetuning/code-reward.yaml):

```yaml
apiVersion: evals.si/v1alpha1
kind: RewardSpec
metadata: {name: code-grpo}
spec:
  components:
    - {ref: format-check, name: format, weight: 0.1, gate: true,
       params: {pattern: "(?s).*```python\\n.*```.*"}}
    - {ref: code-exec-tests, name: tests, weight: 0.9, params: {timeout_s: 5},
       sandbox: {minIsolation: namespaced}}
  cache: {enabled: true}
```

```bash
cd python
uv run evalsi rewards score --spec ../examples/finetuning/code-reward.yaml \
  --data ../examples/finetuning/rollouts.jsonl
uv run evalsi rewards serve --spec ../examples/finetuning/code-reward.yaml --port 5000   # OpenRLHF protocol
```

```python
from evalsi.rewards.trl import reward_funcs

funcs, weights = reward_funcs("examples/finetuning/code-reward.yaml")   # server=... for the Reward Service
trainer = GRPOTrainer(model=model, reward_funcs=funcs,
                      args=GRPOConfig(reward_weights=weights, ...), ...)
```

verl and OpenRLHF adapters are covered in the guide. On a server, the **Reward Service** (`RewardService.ScoreRewards`, or `POST /v1alpha1/rewards:score`) runs components on the worker and sandbox pools with back-pressure and an optional shared cache.

**Checkpoint evaluation.** Serve each checkpoint as it is saved and compare it with the base model:

```bash
# Once: the base model
evalsi checkpoints eval Qwen/Qwen3-8B --step base -f examples/finetuning/forgetting.yaml \
  --training-run grpo-1 --serve lora --base-url http://localhost:8000/v1

# While training: evaluate every new checkpoint in the output directory
evalsi checkpoints watch out/ -f examples/finetuning/forgetting.yaml --training-run grpo-1 \
  --serve lora --base-url http://localhost:8000/v1 --regression math-equiv:0.02

# The learning curve; exits 3 when the latest checkpoint regressed
evalsi checkpoints curve -f examples/finetuning/forgetting.yaml --training-run grpo-1 --regression math-equiv:0.02
```

`--regression metric:max_drop` fails a step when the metric is significantly below the base model's (the paired confidence interval of the difference excludes zero and the drop exceeds `max_drop`). `--stop-file` lets the trainer stop at the next step after a regression.

## 12. Kubernetes

The same run and policy files apply as resources. See the [Kubernetes guide](kubernetes.md).

| Chart | Scope | Installs |
|---|---|---|
| `evalsi-crds` | cluster | CRDs, admission webhooks, aggregated roles |
| `evalsi` | namespace | `evalsid` replicas, workers per pool, NATS, the operator, internal certificates |
| `evalsi-sandboxd` | nodes | A sandbox pool per node (bubblewrap or Firecracker) |

```bash
kubectl create namespace evalsi
helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=evalsi
helm install evalsi deploy/helm/evalsi -n evalsi \
  --set storage.postgres.dsnSecret.name=evalsi-postgres \
  --set server.replicas=2 \
  --set sandbox.address=tls://evalsi-sandboxd.evalsi.svc:7443
helm install evalsi-sandboxd deploy/helm/evalsi-sandboxd -n evalsi

kubectl apply -n evalsi -f examples/runs/capitals.yaml
kubectl get evalruns -n evalsi          # PHASE, RUN, DONE, TOTAL
kubectl wait evalrun/capitals -n evalsi --for=condition=Succeeded --timeout=30m
```

| Resource | Purpose |
|---|---|
| `EvalRun` | A run spec as a resource. `Succeeded` means every gate passed, `Failed` that a gate failed, `Error` that it could not finish. Deleting it cancels the run. |
| `OnlineEvalPolicy` | A policy as a resource, with counters mirrored back. |
| `Evaluator` | Your evaluator plugin image, served from a KEDA-scaled worker pool. |
| `SandboxClass` | A cluster-scoped sandbox pool (for example, the pod rung under gVisor). |

Production notes: PostgreSQL for metadata, ClickHouse for traces at volume, S3-compatible storage for datasets, NATS JetStream work queues, KEDA scaling, HA replicas with lease-based scheduling (a replica adopts a stopped one's runs), service-account identity, mutual TLS between components, an air-gapped bundle (`deploy/airgap/bundle.sh` and `install.sh`), and a namespace-only install for clusters where you are not admin (`-f deploy/helm/evalsi/values-namespaced.yaml`).

## 13. Running evals in a pull request

The pattern is the same in every recipe: **run the eval in the PR job, fail the job on a failed gate, and show the result on the PR.** Gates exit with code 3, so no extra scripting is needed to block a merge. Mark the job as a required status check in your branch protection to enforce it.

Pick the recipe that matches how you run Evals.si:

| Recipe | Where evals run | Needs a server? | Use it when |
|---|---|---|---|
| A | In the CI job (embedded) | No | Small suites, no infrastructure to run |
| B | On your Evals.si server | Yes | Shared datasets, long runs, quotas, RBAC |
| C | On Kubernetes via an `EvalRun` | Yes (in cluster) | You already deploy from a cluster |
| D | Compare against the base branch | Yes | You want "is this PR better or worse than main?" |
| E | Agent run with a sandboxed checker | Optional | You gate on an agent actually fixing tasks |
| F | Checkpoint regression gate | Optional | You gate a training change on forgetting |

Replace `acme/support-agent`, `https://evals.example.com` and the paths with your own.

### Recipe A: embedded, no server

The simplest setup: install the CLI in the job, run the spec, upload the report. Provider keys come from repository secrets.

```yaml
# .github/workflows/evals.yml
name: evals
on:
  pull_request:
    paths: ['evals/**', 'prompts/**', 'src/**']

permissions:
  contents: read

concurrency:
  group: evals-${{ github.ref }}
  cancel-in-progress: true

jobs:
  evals:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with: {python-version: '3.12'}
      - run: pip install 'evalsi[anthropic]'

      # Exit code 3 when a gate fails, which fails the job.
      - name: Run evals
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        run: evalsi run -f evals/run.yaml --output results.json --judge-provider anthropic --judge-model claude-opus-5-5

      - name: Build report
        if: always()
        run: evalsi report results.json -o report.md

      - name: Show report in the job summary
        if: always()
        run: cat report.md >> "$GITHUB_STEP_SUMMARY"

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: eval-report
          path: |
            results.json
            report.md
```

Notes:

- `if: always()` keeps the report even when a gate fails, which is when you most need it.
- A run spec is the same file you run locally, so reproduce a failure with `evalsi run -f evals/run.yaml`.
- Put budgets in the spec (`budget.max_target_tokens`, `max_judge_tokens`) so a bad prompt cannot spend without limit.
- Judge responses are cached by content hash, so unchanged records cost nothing on re-runs within the same cache.

### Recipe B: on a server, with GitHub OIDC (no stored secret)

Use this when the datasets, judges and provider keys live on the Evals.si server. The job proves who it is with its own GitHub token, so there is no API key to store or rotate. This is [`examples/ci/github-actions.yml`](../../examples/ci/github-actions.yml) extended with a report.

**Server side:** trust GitHub's issuer and bind a role to the repository and branch ([identity guide](identity.md#5-ci-github-actions-without-stored-secrets)):

```yaml
# evalsi.yaml
auth:
  jwt:
    providers:
      - name: github
        issuer: https://token.actions.githubusercontent.com
        audiences: [https://evals.example.com]
        jwks: {discovery: true}
rbac:
  projects:
    support:
      runner: ['cel:jwt.repository == "acme/support-agent"']
```

A narrower binding for pull requests from the same repository is a CEL condition on `jwt.repository` (and, for the main branch only, `jwt.ref == "refs/heads/main"`). Pull requests from forks do not get an OIDC token with `id-token: write`, so do not run this recipe on fork PRs.

**Workflow:**

```yaml
name: evals
on: [pull_request]

permissions:
  id-token: write      # lets the job mint its OIDC token
  contents: read

jobs:
  evals:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with: {python-version: '3.12'}
      - run: pip install evalsi

      # Waits for the run. Exit code 3 when a gate fails, which fails the job.
      # --format json prints the run, including its id, to stdout.
      - name: Run evals on the server
        env:
          EVALSI_OIDC_AUDIENCE: https://evals.example.com
        run: |
          set +e
          evalsi run -f evals/run.yaml --server https://evals.example.com --format json > run.json
          echo "EVAL_EXIT=$?" >> "$GITHUB_ENV"
          echo "RUN_ID=$(jq -r .id run.json)" >> "$GITHUB_ENV"

      - name: Report
        if: always()
        env:
          EVALSI_OIDC_AUDIENCE: https://evals.example.com
        run: |
          evalsi report --server https://evals.example.com "$RUN_ID" -o report.md
          cat report.md >> "$GITHUB_STEP_SUMMARY"

      - name: Fail if a gate failed
        run: exit "${EVAL_EXIT:-1}"
```

Notes:

- The run spec's dataset path is resolved on the server, relative to its `datasets_dir`.
- `--output` writes a results file only for embedded runs. With `--server`, the run lives on the server, so take its id from `--format json` and read it back with `evalsi report --server URL RUN_ID`.
- The job captures the exit code and fails in a last step, so the report is still built when a gate fails.
- `evalsi run --server ... --no-wait` prints only the run id and exits, which suits a design where a separate job waits.
- Denied calls show up in `evalsi auth audit --denied`, with the rule that denied them.

### Recipe C: Kubernetes, with an `EvalRun`

When the server runs in a cluster, a PR job can apply the spec as a resource and wait for the verdict. `Succeeded` means every gate passed.

```yaml
name: evals-k8s
on: [pull_request]

permissions:
  contents: read

jobs:
  evals:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # Authenticate to your cluster however your platform does (OIDC to the
      # cloud provider, a kubeconfig secret, ...). Not shown here.
      - name: Run the EvalRun for this PR
        run: |
          NAME=pr-${{ github.event.pull_request.number }}
          # A new EvalRun per PR; a spec cannot change after creation.
          sed "s/name: capitals/name: $NAME/" evals/run.yaml | kubectl apply -n evalsi -f -
          kubectl wait evalrun/$NAME -n evalsi --for=condition=Succeeded --timeout=30m
      - name: Show the verdict
        if: always()
        run: kubectl get evalrun -n evalsi -o wide
      - name: Clean up
        if: always()
        run: kubectl delete evalrun -n evalsi pr-${{ github.event.pull_request.number }} --ignore-not-found
```

Notes:

- The project is the `evals.si/project` label on the resource, else the namespace.
- `kubectl wait` exits non-zero if the run does not reach `Succeeded` in time, which fails the job. A failed gate leaves the phase at `Failed`.
- Deleting the resource cancels a run that is still going. Its results stay on the server.
- Admission webhooks validate the spec with the CLI's rules, so a malformed spec is rejected at `kubectl apply`.

### Recipe D: compare the PR against the base branch

Gate on "is this PR significantly worse than main?" instead of an absolute threshold. Run the same spec on both sides, then use the paired comparison.

```yaml
name: evals-compare
on: [pull_request]

permissions:
  id-token: write
  contents: read

env:
  SERVER: https://evals.example.com
  EVALSI_OIDC_AUDIENCE: https://evals.example.com

jobs:
  compare:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0}
      - uses: actions/setup-python@v5
        with: {python-version: '3.12'}
      - run: pip install evalsi

      # Each run waits and prints the run as JSON; a failed gate must not stop
      # the comparison, so the exit code is ignored here.
      - name: Baseline run (base branch spec)
        run: |
          git show origin/${{ github.base_ref }}:evals/run.yaml > base-run.yaml
          evalsi run -f base-run.yaml --server "$SERVER" --format json > base.json || true

      - name: Candidate run (this PR's spec)
        run: evalsi run -f evals/run.yaml --server "$SERVER" --format json > cand.json || true

      - name: Compare
        run: |
          evalsi compare --server "$SERVER" "$(jq -r .id base.json)" "$(jq -r .id cand.json)" \
            --format json > compare.json
          evalsi compare --server "$SERVER" "$(jq -r .id base.json)" "$(jq -r .id cand.json)" \
            | tee compare.txt >> "$GITHUB_STEP_SUMMARY"
```

Notes:

- Dataset paths in both specs resolve on the server. If the PR changes the dataset or the evaluators, the two runs measure different things, so change one thing at a time.
- `evalsi compare` is a paired comparison, so it reports whether the difference is significant rather than just whether the means differ.
- `compare` prints the comparison but is not itself a gate. For a hard gate, put absolute `gates` in the spec, or inspect `compare.json` in a final step (for example with `jq`) and `exit 1` when a metric regressed.

### Recipe E: an agent run as a PR check

Gate on whether an agent still solves sandboxed tasks. Embedded agent runs need `evalsid` on the `PATH` (for the sandbox) and a runner where bubblewrap works.

```yaml
name: agent-evals
on: [pull_request]

permissions:
  contents: read

jobs:
  agent:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4
        with: {repository: Abhishek-Rnjn/Evals.si, path: evalsi}   # or install a release of evalsid
      - uses: actions/setup-go@v5
        with: {go-version-file: evalsi/go.mod}
      - uses: actions/checkout@v4
        with: {path: app}
      - name: Sandbox prerequisites
        run: |
          sudo apt-get install -y bubblewrap
          sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0    # Ubuntu 24.04
      - name: Build evalsid
        run: cd evalsi && go build -o "$HOME/bin/evalsid" ./cmd/evalsid && echo "$HOME/bin" >> "$GITHUB_PATH"
      - uses: actions/setup-python@v5
        with: {python-version: '3.12'}
      - run: pip install 'evalsi[anthropic]'

      - name: Probe the sandbox (fails closed)
        run: evalsid sandbox probe

      - name: Run the agent tasks
        working-directory: app
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        run: evalsi run -f evals/agent.yaml --judge-provider anthropic --judge-model claude-opus-5-5 --output agent-results.json

      - uses: actions/upload-artifact@v4
        if: always()
        with: {name: agent-results, path: app/agent-results.json}
```

Notes:

- Use `sandbox: {min_isolation: namespaced}` in the spec so a runner without working bubblewrap fails instead of running weaker.
- `trials: 3` with a gate such as `task-success.pass^3` makes the check robust to a single lucky or unlucky sample.
- Put the agent's budget in `harness.builtin.budget` (`usd`, `wall_clock`) so a runaway agent cannot burn a PR's budget.
- Failed tasks can be turned into regression tests with `evalsi promote` (section 5).

### Recipe F: gate a training change on forgetting

For repositories that train or fine-tune, run the forgetting suite on the candidate checkpoint and fail on a significant drop against the base model.

```yaml
name: checkpoint-regression
on:
  pull_request:
    paths: ['training/**']

permissions:
  contents: read

jobs:
  regression:
    runs-on: [self-hosted, gpu]
    steps:
      - uses: actions/checkout@v4
      - run: pip install 'evalsi[server]'
      - name: Evaluate the base model
        run: |
          evalsi checkpoints eval Qwen/Qwen3-8B --step base -f evals/forgetting.yaml \
            --training-run pr-${{ github.event.pull_request.number }} \
            --serve endpoint --base-url http://localhost:8000/v1
      - name: Evaluate the candidate checkpoint
        run: |
          evalsi checkpoints eval out/checkpoint-300 --step 300 -f evals/forgetting.yaml \
            --training-run pr-${{ github.event.pull_request.number }} \
            --serve lora --base-url http://localhost:8000/v1
      - name: Fail on regression
        run: |
          evalsi checkpoints curve -f evals/forgetting.yaml \
            --training-run pr-${{ github.event.pull_request.number }} \
            --regression math-equiv:0.02
```

`curve` exits 3 when the latest checkpoint regressed. Adjust the model, paths and serving strategy (`vllm`, `lora`, `endpoint`) to your setup.

### Posting the result as a PR comment

The job summary is enough for most teams. To also comment on the PR, add a step after the report:

```yaml
      - name: Comment on the PR
        if: always() && github.event.pull_request.head.repo.full_name == github.repository
        uses: actions/github-script@v7
        with:
          script: |
            const fs = require('fs');
            const body = fs.existsSync('report.md') ? fs.readFileSync('report.md', 'utf8') : 'No report produced.';
            await github.rest.issues.createComment({
              owner: context.repo.owner, repo: context.repo.repo,
              issue_number: context.issue.number,
              body: '## Eval results\n\n' + body.slice(0, 60000),
            });
```

This needs `pull-requests: write` in the job's `permissions`. Skipping fork PRs (the `if` above) keeps secrets and write tokens away from untrusted code.

### Keep a PR check trustworthy

- **Pin what you measure.** Pin the model version in the spec and the evaluator versions you depend on, or a green check can turn red with no code change.
- **Use trials and confidence intervals.** One sample of an LLM is noisy. `trials: 3` with `pass^3` gates, and the intervals in every report, tell signal from noise.
- **Separate errors from failures.** Errors (a judge outage, a sandbox that failed) are reported separately and never scored as zero. Treat a run with errors as inconclusive, not as a regression.
- **Set budgets.** `budget` in the spec and `quotas` on the server stop a bad change from spending without limit.
- **Never run untrusted specs unsandboxed.** On a server, a run spec from a PR is code-adjacent. Use `authorization.rules` such as `deny` on `resource.agent.network == "allow"` and `require` on `runs_code`, and bind CI roles narrowly ([identity guide](identity.md#4-global-rules)).
- **Do not run secrets-bearing evals on fork PRs.** Use `pull_request` (not `pull_request_target`) and skip jobs that need secrets or OIDC when the head repository is a fork.
- **Reproduce locally.** Because the spec is the same file, `evalsi run -f evals/run.yaml` reproduces a CI failure on your machine.

## 14. Reference

**CLI commands**

| Command | Purpose |
|---|---|
| `evalsi eval` | Score records you already have |
| `evalsi run -f spec.yaml [--server URL]` | Execute a run spec, embedded or on a server |
| `evalsi compare BASE CAND --server URL` | Paired comparison of two server runs |
| `evalsi report results.json -o report.html` | HTML or Markdown report |
| `evalsi analyze query\|slice` | Cross-run analytics in DuckDB |
| `evalsi policy apply\|list\|stats\|delete` | Online evaluation policies |
| `evalsi promote RUN --dataset D --when CEL` | Turn failed results into a dataset |
| `evalsi shadow -f candidate.yaml` | Shadow replay against recorded behavior |
| `evalsi rewards score\|serve` | Score rollouts, or serve a RewardSpec to trainers |
| `evalsi checkpoints eval\|watch\|curve` | Evaluate training checkpoints |
| `evalsi login\|logout\|whoami` | Sign in to a server |
| `evalsi auth keys\|roles\|bindings\|projects\|audit` | Identity and access management |
| `evalsi catalog` | List evaluators and params |
| `evalsi serve` | Start the server (`evalsid`) |

**Exit codes:** 0 success, 1 error, 3 a gate failed (or a checkpoint regressed).

**Environment variables:** `EVALSI_SERVER`, `EVALSI_API_KEY`, `EVALSI_TOKEN`, `EVALSI_TOKEN_FILE`, `EVALSI_OIDC_AUDIENCE`, `EVALSID_NO_AUTH`, `ANTHROPIC_API_KEY` (and whichever provider keys your judges name).

**Example files**

| File | What |
|---|---|
| [`examples/quickstart/qa.jsonl`](../../examples/quickstart/qa.jsonl) | Records to score |
| [`examples/runs/capitals.yaml`](../../examples/runs/capitals.yaml) | A run with trials and gates |
| [`examples/agents/fix-calc.yaml`](../../examples/agents/fix-calc.yaml) | A sandboxed agent run |
| [`examples/agents/swebench-verified.yaml`](../../examples/agents/swebench-verified.yaml), [`benchmarks.yaml`](../../examples/agents/benchmarks.yaml) | Benchmark runs |
| [`examples/watch/support-policy.yaml`](../../examples/watch/support-policy.yaml) | An online policy |
| [`examples/server/evalsi.yaml`](../../examples/server/evalsi.yaml) | Every server key |
| [`examples/auth/local.yaml`](../../examples/auth/local.yaml), [`evalsi.yaml`](../../examples/auth/evalsi.yaml) | Access control, local and full |
| [`examples/ci/github-actions.yml`](../../examples/ci/github-actions.yml) | A PR gate with GitHub OIDC |
| [`examples/finetuning/`](../../examples/finetuning) | Reward specs and checkpoint suites |

**Deeper guides:** [agent runs](agent-runs.md), [identity](identity.md), [Kubernetes](kubernetes.md), [agentgateway on Kubernetes](agentgateway-kubernetes.md), [fine-tuning and RL](fine-tuning.md), [architecture and roadmap](../DESIGN.md), [decision records](../decisions/README.md).

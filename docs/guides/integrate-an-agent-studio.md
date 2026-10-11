# Integrate an agent studio

This guide is for the team that builds a platform where people create and deploy agents (a studio, such as agent-studio-standalone) and wants every deployed agent scored, and a bad one kept from shipping. It covers four steps: install Evals.si beside the studio, connect the studio's traces, gate deploys, and read the scores. Each step has a runnable stand-in in [examples/demo](../../examples/demo/README.md), where Deep Agents and DeepSeek Harness play the studio's agents, an MLflow server plays its trace store, and a mock model keeps the run free and repeatable.

Evals.si has three doors, and a studio can use any mix:

| Door | The studio does | Section |
|---|---|---|
| Push | exports OpenTelemetry traces, or calls the API from its deploy pipeline | [2](#2-connect-traces), [3](#3-gate-deploys) |
| Pull | already stores traces in MLflow, Phoenix or Langfuse: a [trace source](trace-sources.md) reads them and writes scores back | [2](#2-connect-traces) |
| Embed | calls the SDK, including from a training loop | [3](#3-gate-deploys) |

## 1. Install beside the studio

### Pipeline writes and datasets

Send `Idempotency-Key` on retryable API writes. The same authenticated principal,
operation and payload replay the stored outcome for 24 hours; a changed payload
under the same key conflicts. Permissions are checked again on every retry.
An unavailable response saying the outcome is uncertain requires reconciliation
of the original operation, not a new key. Generated API-key secrets are returned
only on the first successful call, never persisted for replay.

Python callers can pass `idempotency_key` to `Client.call`, `create_run`,
`delete_run` and `import_dataset`. Keyed gRPC write streams are bounded to 64 MiB
and 100001 messages and require the sender to close its input before reading
responses; omit the header for incremental streaming. Responses are bounded to
64 MiB, and each principal/operation scope holds at most 1024 keys and 64 MiB of
response data. An unfinished claim remains reserved until reconciled.

```bash
evalsi dataset upload qa.jsonl --name qa --project studio --server "$URL"
evalsi dataset import s3://evalsi/datasets/incoming/studio/qa.parquet \
  --name qa --project studio --format parquet --server "$URL"
evalsi delete-run "$RUN_ID" --yes --server "$URL"
```

Upload JSONL or Parquet with
`POST /v1alpha1/datasets/<name>/upload?project=studio&format=jsonl`; its body is the
file, not multipart. The Connect/gRPC `DatasetService.UploadDataset` begins with
a config message followed by data messages. `ImportDataset` takes `config` and
an S3 `uri` under the configured dataset root's `incoming/<project>/` prefix.
These operations need `datasets.write`; no arbitrary URL or other bucket is read.

The response's `path` is usable as `RunSpec.dataset.path`. Limits are 64 MiB
uploaded and normalized, 100000 records and 4 MiB per JSONL row/Parquet page.
Rows must be Record objects; text input/output/reference strings are normalized
to Content and null stays absent. Missing IDs become `record-<row>` with a
deterministic suffix when an explicit ID reserves that name; duplicate explicit IDs are rejected.
Errors name the offending row without echoing raw fields. Publication occurs
only after validation, and its path contains the normalized content hash.
There are two conversion slots per replica; capacity refusals are retryable.

`DeleteRun`, also `DELETE /v1alpha1/runs/<id>`, requires `runs.delete` (admin by
default). Pending/running or actively leased runs are refused. Terminal deletion
removes records, outputs, results and queued webhook deliveries and recovers the
stored-run quota. Only authorization metadata remains for 24-hour retries.
Data already delivered to external sinks, source datasets and promoted datasets
remain; deleting an EvalRun CR does not invoke this API.

Evals.si installs from its own charts, in the studio's namespace or its own. Every name starts with the release name, so releases can share a namespace ([Names](kubernetes.md#names-and-installing-beside-other-charts)).

```bash
helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=studio
helm install evalsi deploy/helm/evalsi -n studio \
  --set storage.postgres.dsnSecret.name=evalsi-postgres \
  --set storage.s3.bucket=evalsi \
  --set sandbox.pool.enabled=true --set sandbox.pool.ladder='{pod}'
```

- **No cluster-admin?** Install namespace-only: see [Installing without cluster-admin](kubernetes.md#installing-without-cluster-admin). The sandbox pool on the pod rung needs nothing outside the namespace.
- **Any cluster shape.** Every pod and Service takes tolerations, a node selector, labels and an image registry as values ([Taints and service meshes](kubernetes.md#taints-and-service-meshes)). The demo overlays show kind, tainted nodes and Istio ambient.
- **A project per tenant.** The bootstrap Job creates projects, API keys and credential grants from values, so a studio installs with no manual step (`policies` and `webhooks` too):

  ```yaml
  bootstrap:
    enabled: true
    projects: [{name: studio}]
    apiKeys:
      - {name: studio-ingest, roles: {studio: [ingest]}, secret: {name: studio-ingest-key}}
      - {name: studio-ci, roles: {studio: [editor]}, secret: {name: studio-ci-key}}
    credentialGrants:
      - {env: STUDIO_MODEL_KEY, projects: [studio]}
    webhooks:
      - {name: studio, project: studio, url: "http://studio.studio.svc/hooks/evalsi", events: [run.finished, run.gate_failed], secret: {name: studio-evalsi-webhook}}
  ```

  The keys are written to the Secrets named, once, and kept across upgrades. A credential grant lets a project's runs name a worker's environment variable (for a model key) without the key being in a spec.

## 2. Connect traces

### SDK logging without OTel

```python
import evalsi

logger = evalsi.configure_logging("https://evalsi.studio.svc:4318", api_key=ingest_key)
future = evalsi.log("question", "answer", metadata={"epoch": 2})
logger.log_batch([{"input": "next question", "output": "next answer"}])
evalsi.flush(timeout=10)
trace_id = future.result()
logger.close()
```

Use the server CA through `SSL_CERT_FILE`. Configuration is explicit, or set
`EVALSI_OTLP_ENDPOINT`; no judge is selected or billed. Calls serialize locally
and return futures without waiting for network I/O. PyTorch tensors are detached
to CPU; batches accept dict records, pandas frames and Hugging Face datasets.
Queued and in-flight records share limits (1024 records/8 MiB by default).
Overflow, HTTP failures and partial rejection fail futures; `flush`/`close`
also raise failures. Normal process exit drains the queue; SIGKILL cannot flush.
The byte cap applies after serialization, not to caller-owned tensors or
temporary serialization memory. `log_batch` returns one future per input row.

Metadata arrives in the trace's Record after server credential/PII redaction.
Server-derived service/error fields remain authoritative. `trace_id`, when
supplied, is a nonzero 32-character lowercase hexadecimal ID.

**Push.** Point the studio's OpenTelemetry exporter at Evals.si with an `ingest` key bound to the project (OTel GenAI, OpenInference, OpenLLMetry and MLflow conventions are understood):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://evalsi.studio.svc:4318
OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer $(kubectl get secret studio-ingest-key -o jsonpath='{.data.api_key}' | base64 -d)"
```

The chart's certificate comes from its internal CA (`evalsi-server-tls`, `ca.crt`); give the exporter that CA, or terminate TLS in your mesh. Traces land in the key's project with its labels.

What a trace needs: an MLflow LangChain/LangGraph trace (what Deep Agents emits with `mlflow.langchain.autolog()` and MLflow's OTLP exporter) or any GenAI-convention trace is read into a trajectory: model calls with their messages and token counts, tool calls with arguments and results, and errors. The demo's recorded export ([internal/ingest/testdata](../../internal/ingest/testdata)) is a test of exactly that. A CrewAI 1.x studio traced by MLflow needs `mlflow.openai.autolog()` beside `mlflow.crewai.autolog()` when its agents use CrewAI's own OpenAI provider (the default), or its model calls are missing (seen with CrewAI 1.15.27 and MLflow 3.17.0). The [agent frameworks guide](agent-frameworks.md) covers CrewAI and the other first-cut frameworks. Agents that are CLIs, like dsh, don't need tracing: their `--json` run events are the trajectory (`output_format: dsh-json`).

**Pull.** If the studio already keeps traces in MLflow, Phoenix or Langfuse, a [trace source](trace-sources.md) reads them without a second exporter, history included, and writes the scores back to the same traces (MLflow assessments, Phoenix annotations, Langfuse scores), so they show in the studio's own UI:

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata: {name: studio-mlflow, labels: {evals.si/project: studio}}
spec:
  connector: mlflow
  endpoint: http://mlflow.studio.svc:5000
  locations: [studio-workflows]        # the MLflow experiment, by name or ID
  backfill: {since: 720h}
  writeBack: {enabled: true}
```

An in-cluster, plain-HTTP MLflow must be allowed in the chart (`sources.allowHosts: [mlflow.studio.svc]`), and a token, if it needs one, is a granted Secret (`sources.secrets`, `credentials: {file: <secret>/<key>}`). The demo does exactly this for the Deep Agents service ([R6](../../examples/demo/README.md#online-scoring-from-mlflow-r6)). Pulled traces carry the label `source: studio-mlflow`. Langfuse is built from its API spec and not yet run against a server; see the guide for what is verified.

Then say what to score and how often, with a policy per environment:

```yaml
apiVersion: evals.si/v1alpha1
kind: OnlineEvalPolicy
metadata:
  name: studio-agents
  labels: {evals.si/project: studio}
spec:
  selector: 'labels["deployed"] == "true"'
  sampling: {rate: 0.1, always: ["error"]}
  stages:
    - evaluators: [{ref: builtin/latency}, {ref: tool-errors}, {ref: loop-detection}]
  window: 1h
```

`kubectl apply` it, or `evalsi policy apply -f`. Evaluators that need a judge model say so and are skipped, not scored zero, when none is configured.

## 3. Gate deploys

Before a workflow goes live, run it against a suite. The run spec is the same file as in the demos; the studio's deploy step applies it and waits.

```bash
evalsi run -f small-repo-fixes.yaml --server $URL    # exits 3 when a gate fails

# or on Kubernetes, with the same file:
kubectl apply -n studio -f small-repo-fixes.yaml
until phase=$(kubectl get evalrun small-repo-fixes -n studio -o jsonpath='{.status.phase}'); \
      [[ $phase =~ ^(Succeeded|Failed|Error|Cancelled)$ ]]; do sleep 10; done
[ "$phase" = Succeeded ]                             # Failed: a gate did not pass
```

A spec's `gates` turn metrics into a verdict (`task-success.pass^3 >= 0.8` means three trials in a row must pass in at least 80% of tasks). A failed gate makes `evalsi run` exit 3 and the `EvalRun` phase `Failed`, which blocks a pipeline step with no extra code. From a service, use the API or a webhook instead of a pipeline ([webhooks](webhooks.md)): `run.gate_failed` is sent when a run ends with a failed gate, signed with HMAC, so the studio can mark the deployment blocked.

Embedded in the studio's own Python, the same check is a call:

```python
from evalsi.client import Client

with Client(url, api_key=key) as client:
    result = client.evaluate(records, ["task-success"], project="studio")   # scored on the cluster
```

Runs execute the agent in a sandbox per trial (the pod rung on Kubernetes), with the network denied except the hosts the spec allows, so a candidate agent cannot reach anything it was not given.

## 4. Read the scores

- **The web UI** at `/ui/` on the `evalsi` Service: runs, scores per metric, and each trial's trajectory.
- **MLflow.** Add the sink and run scores appear as MLflow runs with metrics, beside the studio's traces:

  ```yaml
  server:
    config:
      sinks:
        - mlflow: {tracking_uri: "http://mlflow.studio.svc:5000", experiment: evalsi}
  ```
- **Prometheus and Grafana.** The chart ships a dashboard and metrics; see [Dashboards](kubernetes.md#dashboards).
- **Per execution and per workflow, in the studio's own UI.** `GET /v1alpha1/scores` returns the online scores of live traffic, pushed or pulled alike, one entry per trace (newest first) with its summary and its results grouped by policy, without the trace's full record. Filter it with `project`, `service`, `policy`, `evaluator`, `since` (RFC 3339) and any number of `labels=key=value`. `GET /v1alpha1/traces/{trace_id}/scores` returns one execution's. Label each workflow's traces with the resource attributes `evalsi.label.workflow` and `evalsi.label.version` (or a trace source's labels), and a workflow's page is one query:

  ```bash
  curl -H "Authorization: Bearer $KEY" \
    "$URL/v1alpha1/scores?project=studio&labels=workflow=support-bot&since=2026-10-01T00:00:00Z&page_size=50"
  ```

  Pass `next_page_token` back as `page_token` for the next page; a page can be shorter than `page_size` while a token is set. A caller sees only the traces its `traces.read` grant covers, including service- and label-scoped grants, so a studio key can be limited to its own workflows. Link each entry to `/ui/` for the trajectory.

  To be told instead of asking, subscribe a webhook to `trace.scored`: one signed delivery per scored execution and policy, with the same summary and results ([webhooks](webhooks.md)). `alert.fired` tells the studio when a workflow's quality drops below a policy's alert threshold.
- **The API and CLI.** `evalsi report <run id> --server $URL -o report.html`, or the `RunService` API (`ListRunResults`) for a gate run's results. Live-traffic scores for a policy as a window: `evalsi policy stats studio-agents`.

## A checklist

- [ ] Evals.si installed; the sandbox pool answers (`kubectl get sandboxclass` or the pool pod is Ready).
- [ ] `studio-ingest` key set on the studio's exporter, or a `TraceSource` on its MLflow; a trace appears in the UI.
- [ ] An `OnlineEvalPolicy` applied; `evalsi policy stats` shows scored traces.
- [ ] A suite for the studio's agents, with gates; the deploy step runs it and fails on exit 3 or `phase: Failed`.
- [ ] A webhook (or the API) tells the studio when a gate fails.
- [ ] Scores visible where people look: `/ui/`, MLflow, Grafana.

To see all of it run first, follow the [demo walkthroughs](../../examples/demo/README.md): the same install, the same specs, a mock model, on your laptop or on kind.

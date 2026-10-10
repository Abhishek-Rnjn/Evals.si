# Trace sources: score traces you already keep in MLflow, Phoenix or Langfuse

If your agents already send traces to MLflow, Arize Phoenix or Langfuse, Evals.si can read them from there. A **trace source** pulls the store's traces, scores them with your [online policies](end-to-end.md), and writes the scores back to the same traces, so they show up in the UI your team already uses. Nothing changes on the agents' side: no second exporter, and history from before Evals.si was installed can be read too.

A pulled trace is an ordinary trace of the source's project. It is stored, shown in the web UI with its trajectory, graded by every policy that selects it, and labelled `source: <name>` so a policy can select it.

| Store | Reads | Writes back | Verified against |
|---|---|---|---|
| MLflow 3.x (open source) | `POST /api/3.0/mlflow/traces/search`, spans with `batchGet` | Feedback assessments, updated in place when a score changes | MLflow 3.17.0 |
| Arize Phoenix | `GET /v1/projects/{project}/traces`, spans by trace ID | Trace annotations with an `identifier`, which Phoenix upserts | Phoenix 20.20.0 |
| Langfuse | `GET /api/public/v2/observations` (the real-time read path) | Scores | The pinned OpenAPI spec only: not yet run against a Langfuse server |

Databricks, SageMaker and Azure ML variants of MLflow are refused with "not built": their APIs differ from open-source MLflow (see [decision 0016](../decisions/0016-trace-source-connectors.md)).

## 1. Allow the store, and grant its credential

The server makes the requests, from where it runs. So a project admin cannot point it anywhere they like:

- **The endpoint** must be `https` to a public name. A plain-HTTP store, or one at a private, loopback or cluster-internal address (`mlflow.studio.svc`, `10.0.0.5`, `localhost`), must be listed by the operator of the server:

  ```yaml
  # evalsi.yaml
  sources:
    allow_hosts: [mlflow.studio.svc.cluster.local, "*.corp.example.com"]
  ```

  On Kubernetes this is the chart value `sources.allowHosts`. The check is on where each request really goes, not only on the URL: a public name that resolves to a private or metadata address is refused, a redirect to one is refused at the hop, and the server dials the address it checked.

- **The credential**, if the store needs one, is named, never written in the source. It is either a variable of the server (`credentials: {env: STUDIO_MLFLOW_TOKEN}`) or a file mounted from a Secret (`credentials: {file: mlflow-token/token}`). Either one must be [granted](identity.md#8-credentials-which-worker-secrets-a-project-may-use) to the project, and can be limited to the store's host:

  ```yaml
  credentials:
    grants:
      - {env: STUDIO_MLFLOW_TOKEN, projects: [studio], hosts: [mlflow.example.com]}
      - {env: "file:mlflow-token/token", projects: [studio]}
  ```

  A file is read again at each use, so a rotated Secret takes effect without a restart. On Kubernetes, list the Secret in `sources.secrets` and the chart mounts it where `file:` names look (`sources.dir`). MLflow takes the value as a bearer token, Phoenix as its API key (bearer), Langfuse as `<public key>:<secret key>`.

Creating and changing sources needs `sources.write`, which only the `admin` role holds: a source makes the server read another system and write to it. `sources.read` lets admins list them and see their status.

## 2. Apply a source

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata:
  name: studio-mlflow
  labels: {evals.si/project: studio}
spec:
  connector: mlflow
  endpoint: https://mlflow.example.com
  locations: [studio-agents]          # experiment names or IDs
  credentials: {env: STUDIO_MLFLOW_TOKEN}
  backfill: {since: 720h}             # read 30 days of history first; 0 tails only
  poll: {interval: 30s, maxRecordsPerSecond: 200}
  maxTraceDuration: 30m               # see "How it decides what is new"
  policies: [studio-agents]           # empty: every policy of the project that selects them
  writeBack: {enabled: true}
  traceLabels: {studio: standalone}   # put on every pulled trace, for selectors
```

```bash
evalsi source apply -f studio-mlflow.yaml --dry-run --server $URL   # checks it, grants included
evalsi source apply -f studio-mlflow.yaml --server $URL
kubectl apply -f studio-mlflow.yaml                                  # with the operator
```

`locations` is a list of MLflow experiments (names or IDs), one Phoenix project, or at most one Langfuse environment. The same file works with `kubectl apply`: the operator applies it through the API, `kubectl apply` is refused for anything the API would refuse (an unlisted host, an ungranted credential), and `kubectl get tracesources` shows phase, pulled and scored counts and the watermark. Only namespace admins may create TraceSources (the `admin` role), not editors. A source and the policies it names can be applied together (one `kubectl apply`, or a GitOps sync): a policy the API does not have yet is not a refusal, so the source is admitted with a warning and syncs as soon as its policy does. With `evalsi source apply` and the API, a policy that does not exist is `NotFound`.

A policy for the pulled traces selects on their labels:

```yaml
apiVersion: evals.si/v1alpha1
kind: OnlineEvalPolicy
metadata: {name: studio-agents, labels: {evals.si/project: studio}}
spec:
  selector: 'labels["source"] == "studio-mlflow"'
  stages:
    - evaluators: [{ref: tool-errors}, {ref: loop-detection}]
  window: 1h
```

Write-back replaces the MLflow, Langfuse and Phoenix [sinks'](../../examples/server/evalsi.yaml) `trace_feedback` for the traces a source reads: do not turn on both for the same store, or each score is written twice. `trace_feedback` stays the way to write back traces that reach Evals.si over OTLP.

Trace tags (MLflow) and attributes or metadata (Phoenix, Langfuse) named `evalsi.label.<key>` become labels, so a studio that tags its traces `evalsi.label.workflow` can select per workflow.

## 3. Watch it

```bash
evalsi source list --project studio --server $URL
evalsi source get studio-mlflow --project studio --server $URL     # status as JSON
```

```text
source               connector  phase    lag  pulled  scored  last error
studio/studio-mlflow mlflow     tailing  3s   5       8
```

- **phase**: `backfilling`, `tailing`, `paused` or `error` (the last pull failed; it is retried with backoff, honouring the store's `Retry-After`).
- **lag**: seconds since the last complete pull began. A new trace is read within about one poll interval of being logged.
- **pulled** and **scored**: traces handed to the policies, and scores written back.

`evalsi source pause`, `resume` and `backfill --since 2026-09-01T00:00:00Z` do what they say; a backfill reads history again, and traces already scored are scored again only if they changed. Deleting a source keeps the traces it pulled.

Prometheus metrics, per source: `evalsi_source_lag_seconds`, `evalsi_source_watermark_age_seconds`, `evalsi_source_failing`, `evalsi_source_deferred_traces`, `evalsi_source_traces_pulled_total`, `evalsi_source_scores_written_total`. The chart's `prometheusRule.enabled` adds alerts on lag (`prometheusRule.sourceLagSeconds`, default 120) and on a failing source.

## How it decides what is new

Stores index a trace by when it **started** but only show it once it has **ended**. A source therefore re-reads, every poll, the traces that started in the last `maxTraceDuration` (default 30 minutes), and skips those it has already scored unless they changed. The watermark, every trace before which has been handled, trails real time by that much, which is why `evalsi_source_watermark_age_seconds` sits near 30 minutes on a healthy source; alert on lag instead.

- A trace that runs longer than `maxTraceDuration` is missed. Raise it for long agent runs; the cost is reading a longer window each poll.
- A trace the store reports as still running is held back (`deferred`), and scored as it is after `maxInProgressAge` (default 1 hour).
- A trace is stored and queued for scoring before the watermark moves past it. If the server stops between those two, the trace is stored but not scored; it is not pulled again.
- Only the replica that runs the policy engine pulls, so a source is never read twice at once; another replica resumes from the stored watermark.

## Filling in what the conventions miss

The trace becomes a record the same way a pushed trace does (OTel GenAI, OpenInference, OpenLLMetry and MLflow LangChain conventions). When something the evaluators need is elsewhere, `overrides` set record fields with CEL:

```yaml
spec:
  overrides:
    reference: 'attributes["studio.expected_answer"]'
    metadata.workflow: 'resource["mlflow.tag.agent_studio.workflow_id"]'
    labels.workflow: 'resource["mlflow.tag.agent_studio.workflow_id"]'
```

They can set `input`, `output`, `reference`, `context`, `metadata.<key>` and `labels.<key>`. They see what policy selectors see (`service`, `name`, the root span's `attributes`, `resource`, `labels`) plus the record's `input` and `output` as text. MLflow's tags and metadata are resource attributes named `mlflow.tag.<key>` and `mlflow.metadata.<key>`; a Langfuse observation's metadata are span attributes named `langfuse.metadata.<key>`. An expression that fails (a missing key) leaves its field as it was, so the evaluators that need it skip the record rather than score it zero.

## Network policy

On Kubernetes, `networkPolicy.egress.enabled` adds an egress NetworkPolicy for the evalsid pods: DNS, every pod in the release's namespace, and the rules in `networkPolicy.egress.extra`. Everything else is denied, judges and hosted stores included, so list them there. NetworkPolicy cannot name hosts: for a hosted store reached by name (Databricks, a SaaS Langfuse), restrict egress by name in your mesh or CNI (an Istio `ServiceEntry` with an egress gateway, a Cilium FQDN policy). The policy is off by default because turning it on denies whatever it does not list.

## Not covered yet

- Databricks, SageMaker and Azure ML variants of MLflow.
- Langfuse has not been run against a server; whether a score sent again under the same ID replaces the old one is not confirmed.
- LangSmith, ClickHouse, Tempo and Kafka sources (milestone M5).

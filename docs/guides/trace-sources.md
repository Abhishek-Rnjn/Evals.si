# Trace sources: score traces you already keep in MLflow, Phoenix or Langfuse

If your agents already send traces to MLflow, Arize Phoenix or Langfuse, Evals.si can read them from there. A **trace source** pulls the store's traces, scores them with your [online policies](end-to-end.md), and writes the scores back to the same traces, so they show up in the UI your team already uses. Nothing changes on the agents' side: no second exporter, and history from before Evals.si was installed can be read too.

A pulled trace is an ordinary trace of the source's project. It is stored, shown in the web UI with its trajectory, graded by every policy that selects it, and labelled `source: <name>` so a policy can select it.

| Store | Reads | Writes back | Verified against |
|---|---|---|---|
| MLflow 3.x (open source) | `POST /api/3.0/mlflow/traces/search`, spans with `batchGet` | Feedback assessments, updated in place when a score changes | MLflow 3.17.0 |
| Arize Phoenix | `GET /v1/projects/{project}/traces`, spans by trace ID | Trace annotations with an `identifier`, which Phoenix upserts | Phoenix 20.20.0 |
| Langfuse | `GET /api/public/v2/observations` (the real-time read path) | Scores, replaced under the same ID | Self-hosted 4.56.0: [Compose and in-cluster verification](../verification/README.md#langfuse-live-recheck-2026-10-10) |
| Databricks MLflow experiments | V3 search and trace metadata; signed HTTPS artifact download | V3 feedback assessments | Contract-tested against pinned MLflow 3.17.0 source; not live-verified |

For Databricks, set `connector: mlflow`, `variant: databricks`, the workspace
HTTPS URL, a workspace bearer token in `credentials`, and numeric experiment
IDs in `locations`. Both workspace and signed artifact requests use the source
host guard. Artifact requests use only the download headers returned by the
workspace, never its bearer token, and refuse redirects. Egress must allow the
artifact storage endpoint too. Unity Catalog, SQL warehouse tracing, experiment
names, token acquisition/refresh, SageMaker and Azure ML remain unsupported.

## Scheduled batch reports

Set `schedule` instead of online `policies`, `backfill` or `writeBack` to evaluate
each query window as an ordinary batch run. Applying a schedule requires both
`sources.write` and `runs.create`, including evaluator, judge and credential
grants. The same YAML works through `evalsi source apply` and the operator.

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata: {name: studio-hourly, labels: {evals.si/project: studio}}
spec:
  connector: mlflow
  endpoint: https://mlflow.example.com
  locations: [studio-agents]
  credentials: {env: STUDIO_MLFLOW_TOKEN}
  poll: {interval: 30s}
  schedule:
    cron: "0 * * * *"
    window: 1h
    startingDeadline: 15m
    maxRecords: 10000
    runSpec:
      evaluators: [{ref: latency}, {ref: tool-errors}]
```

Cron is five-field UTC (no seconds, timezone prefix or descriptors). `window`
is 1m-168h; `startingDeadline` is 1m-24h, default 1h. `maxRecords` is 1-10000,
default 10000. A snapshot is capped at 64 MiB. `runSpec` must have evaluators and
must not have a dataset, target, harness, environment or multiple trials.
Records are redacted and normalized before the snapshot is stored.
Each pull is limited to 90 seconds under a two-minute per-source lease; narrow
the window or raise connector throughput if it cannot finish in that bound.

The window includes its start and excludes its end. A source waits for its
previous run to finish; deadlines missed while paused, unavailable or busy are
counted rather than accumulated without limit. In-progress or incomplete
fetches do not advance the cursor; once their deadline passes they are missed.
Empty windows advance without a run. Scheduling starts after the source is
applied, not from historical backfill. Reapplying keeps its stored cursor.

Source JSON status includes `lastScheduledAt`, `lastRunId` and `missedWindows`;
operator status mirrors `lastScheduled`, `lastRunId` and `missedWindows`.
Use the run ID with normal run results/reports and configured run sinks.
Automatic trace-store write-back is not supported in scheduled mode. Stable
window IDs reconcile a restart after run insertion without inserting it twice.

## Trace redaction

Both direct OTLP exports and pulled traces use the server's `traces.redaction`
rules before Evals.si stores or evaluates them. By default, values under
`password`, `passwd`, `authorization`, `cookie`, `set-cookie`, `token`, `secret`,
`api_key`, `api-key`, `apikey`, `private_key`, and names ending in `_token`,
`_secret`, `_password` or `_api_key` (such as `x-api-key`, `id_token` and
`client_secret`) are replaced with `[REDACTED]`. Token counts such as
`input_tokens` are not. Key matching
is case-insensitive and checks the whole key and its final dot-separated
segment, including nested JSON and record metadata.

```yaml
# evalsi.yaml; restart the server after changes
traces:
  redaction:
    keys: ["private_*", "customer.email"]
    patterns: ['secret-[A-Za-z0-9]+']
    pii: [email, us-ssn]
```

`keys` adds glob rules to the defaults. `patterns` contains RE2 value regexes.
`pii` optionally enables `email`, `us-ssn`, `credit-card`, or `phone` heuristic
detectors; no PII class is enabled by default. The card detector hides a number
that passes the Luhn check, and also one that sits next to other digits (a CVV
or an expiry) when it starts with a real issuer prefix and has that issuer's
length. A long digit run (over 64 digits), a timestamp or a card with a typo is
left readable. Use `disabled: true` only to
explicitly opt out of all redaction. Invalid enabled rules fail startup without
including their contents in the error.

On Kubernetes, use the same mapping under `server.config.traces.redaction` in
chart values, consistently on all replicas. Resource attributes, labels,
events, links, status text and source `overrides` are covered. UTF-8 bytes in
pushed OTLP attributes and pulled span attributes are inspected; opaque bytes
in those messages and unknown protobuf fields are dropped while redaction is
enabled. Project assignment and authentication
precede redaction, and binary trace/span IDs remain unchanged.

Matched values are unavailable to selectors, evaluators, promotions and sinks.
Rules can therefore change scores or selection. They do not erase existing
Evals.si data or change the upstream store, and do not apply to direct Evaluate
requests or offline run datasets. Detectors may miss PII or match non-PII; test
your own data before enabling them. Connector error logs and stored source
error status apply the text rules, but arbitrary external error text is not
automatically classified as sensitive. See [decision 0019](../decisions/0019-trace-redaction.md).

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

- Databricks live verification, Unity Catalog and automatic token acquisition; SageMaker and Azure ML variants.
- Langfuse Cloud, TLS deployments, production persistence and arm64 images. Self-hosted 4.56.0 score replacement, Secret/Service pulls and enforced evalsid egress passed the recorded in-cluster check.
- LangSmith, ClickHouse, Tempo and Kafka sources (milestone M5).

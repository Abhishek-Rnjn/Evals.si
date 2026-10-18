# Trace sources: score traces you already keep in MLflow, Phoenix, Langfuse, LangSmith or ClickHouse

If your agents already send traces to MLflow, Arize Phoenix, Langfuse, LangSmith or a Collector ClickHouse trace table, Evals.si can read them from there. A **trace source** pulls the store's traces, scores them with your [online policies](end-to-end.md), and writes the scores back to that store. ClickHouse scores are queryable in a companion table; tracing applications need their own query or integration to display them. Nothing changes on the agents' side: no second exporter, and history from before Evals.si was installed can be read too.

A pulled trace is an ordinary trace of the source's project. It is stored, shown in the web UI with its trajectory, graded by every policy that selects it, and labelled `source: <name>` so a policy can select it.

| Store | Reads | Writes back | Verified against |
|---|---|---|---|
| MLflow 3.x (open source) | `POST /api/3.0/mlflow/traces/search`, spans with `batchGet` | Feedback assessments, updated in place when a score changes | MLflow 3.17.0 |
| Arize Phoenix | `GET /v1/projects/{project}/traces`, spans by trace ID | Trace annotations with an `identifier`, which Phoenix upserts | Phoenix 20.20.0 |
| Langfuse | `GET /api/public/v2/observations` (the real-time read path) | Scores, replaced under the same ID | Self-hosted 4.56.0: [Compose and in-cluster verification](../verification/README.md#langfuse-live-recheck-2026-10-10) |
| LangSmith (cloud or self-hosted) | `POST /runs/query`: root runs list the traces, a trace's runs are its spans | Feedback on the trace's root run, updated in place (`PATCH /feedback/{id}`) when a score changes | Built from the pinned `langsmith` 0.4.37 client and tested against fixtures in its shapes; not live-verified |
| ClickHouse | Collector `clickhouseexporter` Map trace table, over HTTP(S) | Typed rows in `<trace table>_evalsi_scores`; `FINAL` reads return the current score | ClickHouse 25.8.33.6 with the pinned Collector contrib v0.162.0 table schema; [bounded live verification](../verification/CLICKHOUSE-SOURCE.md) |
| Databricks MLflow experiments | V3 search and trace metadata; signed HTTPS artifact download | V3 feedback assessments | Contract-tested against pinned MLflow 3.17.0 source; not live-verified |
| Databricks Unity Catalog schemas | V4 `search-long-running`, polled; spans with the V4 `batchGet` | V4 feedback assessments on the SQL warehouse | Built from pinned MLflow 3.17.0 source and tested against derived fixtures; not live-verified |
| SageMaker managed MLflow (tracking server or app) | The open-source calls, signed with AWS Signature Version 4 | The open-source assessments | Built from the pinned `sagemaker-mlflow` 0.5.0 source and tested against botocore signatures; not live-verified, and whether the managed server serves the MLflow 3 trace API is unconfirmed |

### Databricks

Set `connector: mlflow`, `variant: databricks` and the workspace HTTPS URL as `endpoint`. `locations` is either numeric experiment IDs or Unity Catalog schemas written `catalog.schema`, not both in one source (MLflow's own client refuses the mix; make two sources). Table-prefix locations and experiment names are not built.

- **Experiments** read through the V3 search, with spans from the trace's signed artifact download. Artifact requests use only the download headers the workspace returns, never the bearer token, and refuse redirects. Egress must allow the artifact storage endpoint too.
- **Unity Catalog schemas** are searched on a SQL warehouse: set `auth.databricks.sql_warehouse_id`. The search is a long-running operation that the connector polls (up to five minutes a cycle, then the manager tries again). A trace's ID in Evals.si is `catalog.schema/<trace id>`, which is what the V4 API addresses it by.
- **Credential.** Either a personal access token or bearer token, or a service principal: set `auth.databricks.client_id`, and the credential is the principal's OAuth secret. A service principal needs an `https` endpoint, since the secret is sent in HTTP Basic authentication. Evals.si exchanges it for an access token (client-credentials grant, scope `all-apis`) at the workspace's own `/oidc/v1/token`, caches it (across polls, keyed by endpoint, client ID and a hash of the secret, so a rotated secret gets a new token) until a minute before it expires, and gets a new one once if the workspace answers 401. The token endpoint is always the source's own endpoint host, so the credential grant for that host covers the exchange; the secret goes nowhere else.

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata: {name: lakehouse-agents}
spec:
  connector: mlflow
  variant: databricks
  endpoint: https://adb-1234567890123456.7.azuredatabricks.net
  locations: [main.agents]
  credentials: {env: DATABRICKS_SP_SECRET}
  auth: {databricks: {clientId: 0d9e2f3a-aaaa-bbbb-cccc-1234567890ab, sqlWarehouseId: 4a1b2c3d4e5f6a7b}}
```

### SageMaker

Set `variant: sagemaker`, the region's endpoint (`https://<region>.experiments.sagemaker.aws` for a tracking server, `https://mlflow.sagemaker.<region>.app.aws` for an MLflow app) and `auth.sigv4.arn`, the ARN of the tracking server or app. Requests are signed with AWS Signature Version 4 (service `sagemaker-mlflow` for a tracking server, `sagemaker` for an app, region from the ARN) and carry the ARN in the routing header the `sagemaker-mlflow` client sends. The rest is the open-source MLflow API. The endpoint must be an `https` AWS host (`*.sagemaker.aws`, `*.app.aws` or `*.amazonaws.com`, which covers a VPC endpoint), because the signature must not go to anything else.

The credential is a JSON object, since a source has one credential value:

```json
{"access_key_id": "AKIA...", "secret_access_key": "...", "session_token": "only for temporary credentials"}
```

It is granted and stored like any other credential. **Evals.si does not use the server's own AWS identity (IRSA, pod identity, instance profile) for a source**: a project editor could then have the server sign requests as its role, past the credential grants. Use an IAM user, or a role whose temporary credentials you refresh into the Secret, limited to the one tracking server.

### Azure ML

`variant: azureml` is refused when you apply the source. Azure ML's MLflow endpoint is not an MLflow 3 server: the `azureml-mlflow` 1.62.0 client builds its tracking store on the MLflow 2 API (`/mlflow/v2.0`) and notes "V3 API not implemented yet" for MLflow 3, and the trace search, `batchGet` and assessment calls are all `/api/3.0`. Send those traces over OTLP, or read them from a store that does implement it.

### LangSmith

Set `connector: langsmith` and, as `endpoint`, the API URL your agents' `LANGSMITH_ENDPOINT` names: `https://api.smith.langchain.com`, `https://eu.api.smith.langchain.com`, or a self-hosted `https://<host>/api/v1`. `locations` lists one or more tracing projects, by name or ID; names are looked up once per poll cycle, and a name LangSmith does not have is the source's error. The credential is a LangSmith API key, or `<workspace ID>:<API key>` for an organization key that spans workspaces (the workspace is sent as `X-Tenant-Id`).

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata: {name: support-langsmith, labels: {evals.si/project: studio}}
spec:
  connector: langsmith
  endpoint: https://api.smith.langchain.com
  locations: [support-agent]
  credentials: {env: LANGSMITH_API_KEY}
  backfill: {since: 168h}
  writeBack: {enabled: true}
```

A trace is its root run and the runs under it. Run types become span kinds (`llm`, `chain`, `tool`, `retriever`, `embedding`), and inputs and outputs are read as LangChain writes them: `{"input": "..."}` and `{"output": "..."}` are unwrapped to their text, messages and generations are read as chat messages. Token counts come from `llm` runs only, as a chain's are the sum of its children's. The model is the run's `ls_model_name` metadata, else its invocation's `model`. A run's metadata are span attributes named `langsmith.metadata.<key>`; a root run's `session_id`, `thread_id` or `conversation_id` metadata is the trajectory's session, as in LangSmith's threads.

Scores are written as feedback on the root run, keyed by the metric, with `feedback_source.type` `model` for a judge's score and `api` otherwise, and the policy and evaluator in its metadata. Numbers and booleans (as 1 or 0) are the feedback's `score`, labels its `value`, and the rationale its `comment`. A changed score updates the same feedback, comment included (a score with no rationale clears it). Feedback deleted in LangSmith is created again when its score next changes; an unchanged score is not sent again.

#### Verify your LangSmith deployment

The opt-in check creates a disposable `evalsi-verification-<UUID>` project,
seeds synthetic traces without calling a model, starts a local `evalsid` and
the real Python worker, and checks:

- project-name lookup and historical traces crossing a listing window;
- an unfinished trace being deferred, then scored after completion;
- `tool-errors` feedback read back from LangSmith, with repeated polls deduplicated;
- input, output and trajectory in Evals.si's trace API;
- feedback updates clearing the old comment, replay after a lost local receipt,
  and recreation after deletion, all retaining the same feedback ID.

From a checkout, with Go and uv installed:

```sh
cd python && uv sync --locked --all-packages && cd ..
# Supply EVALSI_TEST_LANGSMITH_CREDENTIAL through your secret manager or environment:
# an API key, or <workspace ID>:<API key>. Never commit it.
EVALSI_TEST_LANGSMITH_URL=https://api.smith.langchain.com \
EVALSI_E2E_WORKER="$PWD/python/.venv/bin/python -m evalsi" \
  go test -race ./tests/e2e -run '^TestLiveTraceSourceLangSmith$' -count=1 -v -timeout 6m
```

Use the EU or self-hosted API URL when appropriate. The key needs permission
to create and delete projects, create/read/update runs, and read/write/delete feedback. The
check deletes its project and contents during cleanup, including after a test
failure. An interrupted process can leave that disposable project behind;
cleanup failures report its name.

For GitHub Actions, set the repository secret
`EVALSI_TEST_LANGSMITH_CREDENTIAL`, then run **LangSmith live verification**
with the desired API endpoint. This workflow only runs on manual dispatch.
Ordinary e2e CI runs `TestTraceSourceLangSmith` through the same checks against
a derived fixture; that pass does not establish compatibility with a live
LangSmith. See the [verification notes](../verification/LANGSMITH.md).

### ClickHouse

Use `connector: clickhouse`, the ClickHouse **HTTP** URL (often port 8123, or
HTTPS 8443) as `endpoint`, and exactly one `database.table` as `locations`.
Identifiers use letters, digits and underscores and start with a letter or
underscore. The supported input is the Collector contrib `clickhouseexporter`
default **Map** trace schema (`otel_traces` by default), pinned to v0.162.0 in
tests. Table names can differ; the columns must match that schema. Evals.si's
own ClickHouse storage tables and the exporter's experimental JSON schema
are different formats.

The credential is `username:password`, with the first colon separating the
username; additional colons belong to the password. It is sent as HTTP Basic
auth. Omit `credentials` only when the server's default account is appropriate.
Never place credentials or SQL in the endpoint URL. Private/plain-HTTP hosts
need `sources.allow_hosts`, and named credentials need the usual project/host
grant, including when supplied through a mounted Secret.

```yaml
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata: {name: support-clickhouse, labels: {evals.si/project: studio}}
spec:
  connector: clickhouse
  endpoint: https://clickhouse.example.com:8443
  locations: [observability.otel_traces]
  credentials: {env: CLICKHOUSE_SOURCE_CREDENTIAL}
  backfill: {since: 720h}
  poll: {interval: 15s}
  maxTraceDuration: 1h
  policies: [agent-health]
  writeBack: {enabled: true}
```

Before enabling write-back, create the companion score table with
[clickhouse-scores.sql](../../examples/sources/clickhouse-scores.sql), executed
in the trace table's database. Rename its table prefix when the input table is
not named `otel_traces`. The connector never creates or alters tables. Its
account needs `SELECT` on `observability.otel_traces` and `INSERT` on
`observability.otel_traces_evalsi_scores`; write-back can be disabled for a
read-only account. Give score viewers `SELECT` separately. A
[complete source example](../../examples/sources/clickhouse.yaml) is included.

A trace becomes eligible when its ended root span is exported. Children
without a root are withheld, and do not appear in the deferred count. Set
`maxTraceDuration` to cover the longest run plus export delay; roots or late
children arriving outside that lookback can be missed. Attributes, status,
events, links and timestamps are retained; the existing ingest profiles read
OpenInference, OTel GenAI and related conventions. The Map schema has already
stringified attribute values. Identical span rows are deduplicated; conflicting
rows and traces with more than 10,000 span rows are errors, not partial scores.

Scores retain policy, evaluator, metric, rationale and judge provenance.
Numbers use `Score`, booleans use `Passed` and `Score` (1/0), and labels use
`Value`. A changed value replaces all typed fields and the rationale.
`ReplacingMergeTree(Version)` keeps the latest row per trace/policy/metric;
retries may leave physical rows until merging. **Query with `FINAL`** (or an
equivalent latest-version aggregation):

```sql
SELECT TraceId, Policy, Metric, Score, Passed, Value, Rationale
FROM observability.otel_traces_evalsi_scores FINAL;
```

The connector uses bound SQL values, waits for query completion, honors
429/503 `Retry-After`, and bounds response bytes, execution time and query
memory. Large backfills still need appropriate ClickHouse indexing, quotas
and sizing. Custom schemas, ClickHouse Cloud/TLS, distributed/replicated
tables and tracing-UI score display are not certified by the local check.
See [decision 0028](../decisions/0028-clickhouse-trace-source.md).

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

  A file is read again at each use, so a rotated Secret takes effect without a restart. On Kubernetes, list the Secret in `sources.secrets` and the chart mounts it where `file:` names look (`sources.dir`). MLflow takes the value as a bearer token (a Databricks service principal's OAuth secret with `auth.databricks.client_id`; a JSON object of AWS keys for SageMaker), Phoenix as its API key (bearer), Langfuse as `<public key>:<secret key>`, LangSmith as its API key (`x-api-key`), and ClickHouse as `username:password` (HTTP Basic).

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

`locations` is a list of MLflow experiments (names or IDs), one Phoenix project, at most one Langfuse environment, LangSmith tracing projects (names or IDs), or one ClickHouse `database.table`. The same file works with `kubectl apply`: the operator applies it through the API, `kubectl apply` is refused for anything the API would refuse (an unlisted host, an ungranted credential), and `kubectl get tracesources` shows phase, pulled and scored counts and the watermark. Only namespace admins may create TraceSources (the `admin` role), not editors. A source and the policies it names can be applied together (one `kubectl apply`, or a GitOps sync): a policy the API does not have yet is not a refusal, so the source is admitted with a warning and syncs as soon as its policy does. With `evalsi source apply` and the API, a policy that does not exist is `NotFound`.

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

Trace tags (MLflow) and attributes or metadata (Phoenix, Langfuse, LangSmith) named `evalsi.label.<key>` become labels, so an agent harness that tags its traces `evalsi.label.workflow` can select per workflow.

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

### Measuring backfill and lag

[`scripts/measure-trace-source.py`](../../scripts/measure-trace-source.py) checks a source against M3's targets: it seeds an MLflow experiment with traces spread over 30 days (`seed`), then applies a source for it, times the backfill, sends live traces and times each from its end until it is scored (`measure`). It prints the numbers as JSON and fails when p95 is over `--max-p95-seconds` (120 by default). The kind e2e job runs it against the demo MLflow.

```bash
cd python
uv run --no-sync python ../scripts/measure-trace-source.py seed --mlflow $MLFLOW --experiment m3 --count 500 --days 30
uv run --no-sync python ../scripts/measure-trace-source.py measure --server $URL --source-file source.yaml \
  --mlflow $MLFLOW --experiment m3 --expect 500 --live 30
```

Lag is mostly the poll interval: with `poll.interval: 15s`, a local MLflow 3.17.0 and evalsid on one machine backfilled 500 traces in 5 seconds and scored live traces with p95 15 seconds. That run is not a production measurement; real numbers depend on the store, its rate limits and the policies' evaluators.

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
    metadata.workflow: 'resource["mlflow.tag.agent_harness.workflow_id"]'
    labels.workflow: 'resource["mlflow.tag.agent_harness.workflow_id"]'
```

They can set `input`, `output`, `reference`, `context`, `metadata.<key>` and `labels.<key>`. They see what policy selectors see (`service`, `name`, the root span's `attributes`, `resource`, `labels`) plus the record's `input` and `output` as text. MLflow's tags and metadata are resource attributes named `mlflow.tag.<key>` and `mlflow.metadata.<key>`; a Langfuse observation's metadata are span attributes named `langfuse.metadata.<key>`, and a LangSmith run's `langsmith.metadata.<key>`. An expression that fails (a missing key) leaves its field as it was, so the evaluators that need it skip the record rather than score it zero.

## Network policy

On Kubernetes, `networkPolicy.egress.enabled` adds an egress NetworkPolicy for the evalsid pods: DNS, every pod in the release's namespace, and the rules in `networkPolicy.egress.extra`. Everything else is denied, judges and hosted stores included, so list them there. NetworkPolicy cannot name hosts: for a hosted store reached by name (Databricks, a SaaS Langfuse), restrict egress by name in your mesh or CNI (an Istio `ServiceEntry` with an egress gateway, a Cilium FQDN policy). The policy is off by default because turning it on denies whatever it does not list.

## Not covered yet

- Live verification of Databricks (experiments and Unity Catalog), OAuth token acquisition and SageMaker. These are built from the pinned client source and tested against derived fixtures; a real workspace or tracking server can still differ. The V4 filter and ordering syntax for Unity Catalog, and whether a SageMaker server serves the MLflow 3 trace API, are the two assumptions most worth checking first.
- Databricks table-prefix locations and experiment names, ambient AWS credentials for SageMaker, and an Azure ML variant (the server has no trace API to read).
- Langfuse Cloud, TLS deployments, production persistence and arm64 images. Self-hosted 4.56.0 score replacement, Secret/Service pulls and enforced evalsid egress passed the recorded in-cluster check.
- LangSmith against a live deployment: the connector follows the `langsmith` 0.4.37 client's requests. An [opt-in end-to-end check](#verify-your-langsmith-deployment) now exercises the window filter and feedback replay assumption, but a live run still needs credentials. Provider UI visibility, production-scale backfill and lag are not covered by that bounded check.
- Tempo, Kafka and S3 sources (milestone M5).

## Recovery of accepted online work

Online traces accepted after this version are written to a durable metadata
queue before they can advance a source watermark. The queue retains the
redacted record, selector attributes and the source's selected policies.
The elected policy engine retries pending work in bounded batches after a
restart and while running. Acknowledgments cannot remove a newer revision of
the same trace. This works with SQLite or PostgreSQL metadata and with a
separate ClickHouse trace store. Trace retention removes pending copies too.

Accepted traces become queryable before evaluation readiness is checked. An
unavailable policy retains its pending work without hiding the redacted trace.
A deleted source no longer requires a runner: its accepted work finishes local
evaluation without write-back to that source. An existing source with write-back
enabled still waits for its runner; lookup failures are retried.

Leadership loss cancels in-flight evaluation. Trace writes, score writes and
acknowledgments also check the database lease and exact pending revision under
a metadata write lock shared with ingestion, lease changes and retention. The
lock covers persistence only, not worker calls. SQL trace changes use the same
transaction; ClickHouse writes reserve monotonically increasing replacement
versions in metadata, including across request failures and replica clock skew.

Delivery is at least once: a crash between a worker call and its acknowledgment
can repeat evaluation and external callbacks. Pending work predating this
version has no durable queue entry; re-run the source's backfill for those
traces. Existing unscored traces are not silently assigned new policies.

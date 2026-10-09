# 0016. Trace-source connectors pull from MLflow, Langfuse and Phoenix

- **Status:** Proposed, 2026-10-09. Design only: nothing here is built (PRD S1 to S8, milestone M3).

## Context

OTLP is a push protocol. A studio that already keeps its traces in MLflow, Langfuse or Phoenix can add Evals.si as a second exporter, but cannot get history from before the install, and cannot see Evals.si's scores in the store its people already read. The PRD (journey 4, A4, S1 to S8) asks for a `TraceSource` that backfills, tails, feeds the same `OnlineEvalPolicy` objects, and writes scores back. The first customer, agent-studio-standalone, traces to MLflow 3.x (open source, Databricks, SageMaker or Azure ML), and the Deep Agents demo (R6) is the first test.

What the code gives us:

- Pulled traces enter the policy engine through `watch.Engine.IngestBatch([]ingest.Trace)`, which already stores them (an upsert on `(project, trace_id)` in `internal/store/traces.go`) and queues them for policies.
- `ingest.Span` wraps an OTLP `tracepb.Span`. `ingest.ToRecord` already reads the OTel GenAI, OpenInference, OpenLLMetry and MLflow LangChain conventions (`normalize.go`, `langchain.go`). MLflow and Phoenix both return OTLP spans.
- Sinks (`internal/sinks/{mlflow,langfuse,phoenix}.go`) already write online scores back to a trace when `trace_feedback` is on, so the write path exists.
- One replica runs the policy engine, chosen by the `policy-engine` lease (`internal/store/leases.go`, `internal/server/cluster.go`).
- Credentials follow 0015: a named variable, granted per project and host.

Facts below come from the pinned sources listed at the end. Where a source does not say, this record says so rather than guess.

## Decision

### 1. The resource and the API

1. **`TraceSource`** is a new message in `proto/evalsi/v1alpha1/source_service.proto`, a CRD (the operator syncs it to the API as it does `OnlineEvalPolicy`), and a `SourceService` with REST under `/v1alpha1/sources`: create (with `validate_only`, as in 0015 item 10), get, list, delete, `:pause`, `:resume`, `:backfill`. The PRD's example is the shape, with these changes:
   - `credentials` replaces `credentialsSecretRef`. It names a variable or a mounted file, never a value (item 22).
   - `mapping` becomes `profile` plus `overrides` (items 19 and 20).
   - `variant` is added for MLflow (item 7).
2. Spec: `project`, `connector` (`mlflow`, `langfuse`, `phoenix`), `variant`, `endpoint`, `locations` (MLflow experiment IDs or UC schemas, a Langfuse environment filter, a Phoenix project), `credentials`, `backfill.since`, `poll.interval`, `poll.maxRecordsPerSecond`, `lookback`, `maxInProgressAge`, `profile`, `overrides`, `policies` (names; empty means every policy of the project, because a pulled trace is an ordinary trace in the project), `writeBack`, `labels`.
3. Status: `watermark`, `lagSeconds`, `recordsPerSecond`, `deferred` (traces waiting to finish), `lastError`, `lastWriteBack`, `phase` (`Backfilling`, `Tailing`, `Paused`, `Error`). S8 asks for these on the resource.
4. A source belongs to one project and is authorized like a policy: creating one needs the project's editor role, and the endpoint and credentials are checked against grants (item 22) at the API, not only when the loop starts.

### 2. The Go interface

5. Package `internal/source`:

   ```go
   type Connector interface {
       // List returns traces that started at or after req.Since, oldest first.
       List(ctx context.Context, req ListRequest) (ListResult, error)
       // WriteBack records scores on the source's own traces. It is idempotent per (trace, metric).
       WriteBack(ctx context.Context, scores []Score) error
   }
   type ListRequest struct{ Since time.Time; Cursor string; Limit int }
   type ListResult struct {
       Traces []Pulled   // Pulled{Trace ingest.Trace; Started time.Time; State: Complete | InProgress}
       Next   string     // opaque page token; empty at the end
   }
   ```

   A connector owns its wire format, auth and paging; it returns `ingest.Trace` (OTLP spans plus the source's tags as resource attributes). The manager owns watermarks, lookback, dedup, rate limits, retries and status. Contract tests for each connector run against recorded responses, as `internal/ingest/demo_fixture_test.go` does for the demo trace.
6. **The manager runs on the policy-engine leader**, which is what the PRD says: one replica owns every source, and a failover resumes from the stored watermark. It never runs in followers, so a source is never pulled twice at once. A per-source lease can come later if one replica cannot keep up; the interface does not change.

### 3. MLflow, per variant

7. **A `variant`** selects the transport, because the variants do not share one API:

   | Variant | Search | Spans | Auth |
   |---|---|---|---|
   | `oss` (MLflow 3.x) | `POST /api/3.0/mlflow/traces/search` | `GET /api/3.0/mlflow/traces/get`, `GET /api/3.0/mlflow/traces/batchGet` | none, a bearer token, or what a reverse proxy adds |
   | `databricks` | `POST /api/4.0/mlflow/traces/search-long-running`, then poll `.../search/operations/{name}`; falls back to the v3 search for experiments | `POST /api/4.0/mlflow/traces/{location}/batchGet` for a UC schema location | a PAT or OAuth M2M token |
   | `sagemaker` | the `oss` calls | the `oss` calls | AWS SigV4 |
   | `azureml` | not decided (item 11) | not decided | Azure identity |

8. **Open source.**
   - *Search.* `SearchTracesV3` takes `locations` (experiment IDs), `filter`, `max_results` (at most 500, default 100), `order_by` and `page_token`, and returns `traces` (`TraceInfoV3`) and `next_page_token`. The filter is a subset of SQL, ANDed, for example `trace.timestamp_ms > 1711089570679`, and `order_by` takes `["timestamp_ms ASC"]`.
   - *Spans.* `GetTrace` and `BatchGetTraces` return spans as OTLP `Span` messages. The proto marks both "for OSS MLflow only". `BatchGetTraces` takes `trace_ids` and optional `experiment_ids`. The connector searches for `TraceInfoV3` pages, then batch-fetches spans for the traces it will emit. `TraceInfoV3.request_preview` and `response_preview` are cut at 10 KB, so the connector never builds a record from them; they are for `overrides` selectors only.
   - *Limits.* MLflow publishes no rate limit for the server. Back off on HTTP 429 and 503, honouring `Retry-After`, because a reverse proxy in front of it may limit.
9. **Databricks.**
   - *Search.* The client (MLflow 3.17.0, `databricks_rest_store.py`) searches through a long-running operation: `search-long-running` returns an operation; poll `search/operations/{name}` until `done`. A UC-schema location needs a SQL warehouse (`MLFLOW_TRACING_SQL_WAREHOUSE_ID` in the client). When the V4 endpoint is missing, or says experiment locations are "not yet supported", the client falls back to the v3 search; the connector does the same.
   - *Spans.* The client's `get_trace` raises "not implemented" against Databricks, and `batch_get_traces` there requires a `location` and rejects `experiment_ids`. So the first cut reads spans for **UC-schema locations** with `batchGet`. Spans for experiment-located traces on Databricks come through the trace-data artifact, which this record has not verified, so the connector does not claim them until a contract test against a recorded Databricks response passes.
   - *Auth and limits.* A personal access token or OAuth machine-to-machine token. Databricks says in its tracing FAQ that quotas and rate limits apply but gives no numbers; back off on 429 and honour `Retry-After`. Its search returns the most recent 1,000 traces by default, so the connector always passes `max_results` and a time filter.
10. **SageMaker.** A managed tracking server is reached with the `sagemaker-mlflow` plugin, which adds AWS Signature V4 headers to every request using the boto3 credential chain; the connector signs the same way, with the default AWS SDK for Go credential chain (IRSA or pod identity on EKS). The signing service name and the tracking-server header come from a community sample and not from the plugin's code, so they are **settings** (`auth.sigv4.service`, `auth.sigv4.region`), not constants, and must be checked on a real server before M3 ships the variant. Whether a SageMaker tracking server serves the MLflow 3 trace API at all is unverified.
11. **Azure ML.** The `azureml://` tracking URI authenticates with Azure identity (`DefaultAzureCredential`), not a token. No source found for this record shows MLflow 3 tracing on an Azure ML workspace. The variant is **out of the first cut** until a trace round trip is shown on a real workspace (see "Found wrong in the PRD" below).
12. **Write-back to MLflow** reuses `mlflowSink.ExportTrace` (`POST /api/3.0/mlflow/traces/{trace_id}/assessments`, a `Feedback` assessment with `assessment_name`, `rationale`, and a `source` of type `CODE` or `LLM_JUDGE` with `source_id: evalsi/<policy>`). `CreateAssessment` has no idempotency key, so re-delivery would add duplicates. The connector keeps `source_writes(source, trace_id, metric, remote_id, digest)` and, when it has the remote ID, sends `PATCH .../assessments/{id}` with the changed fields; with no row (a lost table) it first reads `TraceInfoV3.assessments` and matches `assessment_name` and `source.source_id`. The same table makes a retry after a crash do nothing.

### 4. Langfuse and Phoenix

13. **Langfuse.**
   - *Read.* Basic auth (public key as user, secret key as password). The spec's own description says the real-time read path is `GET /api/public/v2/observations` (`limit` up to 1000, an opaque base64 `cursor`, `fromStartTime`, `traceId`, `isRootObservation`, `fields` groups such as `io`, `model`, `usage`), and that the older read endpoints, such as `GET /api/public/traces` (`page`, `limit`, `fromTimestamp`, `orderBy`), **can delay data by about ten minutes**. A ten-minute delay breaks the two-minute p95 lag target, so the connector reads v2 observations and groups them by `traceId`; it never uses `/traces` for tailing. Observations are not OTLP, so the connector builds GenAI-convention spans from them (generation to model call with usage, span and tool to tool call).
   - *Limits.* The spec marks 429 on its read endpoints and states no numeric limit. Back off on 429.
   - *Write.* `POST /api/public/scores` with `traceId`, `name`, `value`, `comment`, `dataType`, `source` and a client-supplied `id`. The connector sets `id` to a hash of source, trace and metric. That scores are *replaced* when a score with that `id` is sent again is not stated in the spec and is **unverified**; until a test against a real Langfuse shows it, the connector also remembers scores in `source_writes` and does not re-send.
14. **Phoenix.**
   - *Read.* `GET /v1/projects/{project}/spans/otlpv1` (summary "Search spans with simple filters") and `GET /v1/projects/{project}/spans` take `start_time` (inclusive), `end_time` (exclusive), `cursor`, `limit` (at most 1000), `sort` (`id` or `start_time`) and `order`. Bearer API key. Phoenix returns **spans, not traces**, so the connector feeds them to the existing `ingest.Assembler` (root-ended grace, idle, max age) and emits a trace when it is complete. The cursor's order is only stable "with the same sort".
   - *Write.* `POST /v1/span_annotations` or `/v1/trace_annotations` with `name`, `annotator_kind` (`LLM`, `CODE`, `HUMAN`), `result` (`label`, `score`, `explanation`), `metadata` and `identifier`, which per the schema updates the annotation if it exists. That is a documented upsert, so Phoenix needs no local bookkeeping beyond `source_writes`.

### 5. Watermarks and delivery

15. **One rule for all connectors.** The watermark is the *start time* of traces. Each cycle asks for `Since = watermark - lookback` (default 5 minutes). Results are ordered by start time ascending; ties do not matter because delivery is deduplicated by trace ID.
16. **Late completion.** A trace is emitted once it is `Complete` (MLflow `state` is `OK` or `ERROR`, Phoenix root ended, Langfuse root observation ended). A trace that is `IN_PROGRESS` is deferred, counted in `status.deferred`, and held back from the watermark: the watermark never moves past the start of the oldest deferred trace. After `maxInProgressAge` (default 1 hour) it is emitted as it is, marked incomplete, so one stuck trace cannot freeze a source.
17. **At least once, with a durable commit.**
   - *The problem.* `IngestBatch` returns nothing. It logs a failed store write and counts it, and it **drops** a trace when the evaluation queue is full. Both are right for a live OTLP stream and wrong for a watermark: the manager would advance past traces that were never stored or never scored.
   - *The change.* Add `IngestBatchContext(ctx, traces) error`, which returns the store error and, instead of dropping, waits for queue space (bounded by ctx and by the source's rate limit). `IngestBatch` calls it and keeps its present behaviour for OTLP.
   - *The commit.* The manager advances the stored watermark (`source_state`) only after the call returns nil. Dedup uses `source_seen(source, trace_id)`, pruned past `watermark - lookback`, so lookback re-reads do not rescore. A crash between ingest and commit re-delivers, and the `(project, trace_id)` upsert makes the store write harmless.
   - *Rescoring.* A trace already scored is not scored again unless its content changed (a digest in `source_seen`), as late spans can change a trajectory.
18. **Backfill** is the same loop with `Since = now - backfill.since` and a higher rate cap, paged oldest first; `:backfill` restarts it from a given time. The `poll.maxRecordsPerSecond` cap applies to both, and a per-project quota (existing `internal/store/quota.go` counters) bounds a project's pull throughput.

### 6. Mapping

19. **A profile layers on the existing normalization; it is not a second parser.** The pipeline is: decode the source payload to OTLP spans, set the resource attributes the source gives (tags, metadata, experiment, `evalsi.label.*` from `labels`), and call `ingest.ToRecord`. The profile name picks the decoder and defaults: `mlflow`, `phoenix` and `langfuse` each; `auto` (the default) lets `ToRecord` detect OTel GenAI, OpenInference, OpenLLMetry and MLflow LangChain/LangGraph conventions, as it does for pushed traces. The five first-cut agent frameworks' profiles are fixture tests of `ToRecord` on recorded traces, not new code paths.
20. **`overrides`** are CEL expressions (the repository already uses `cel-go` for policies) that set what the conventions did not: `input`, `output`, `reference`, `context`, `metadata.<key>` and `labels.<key>`. They see `trace` (id, state, start, duration, tags, metadata, request and response previews) and `root` (the root span's attributes). The PRD's `trace.tags["agent_studio.workflow_id"]` fits this. Overrides run after `ToRecord` and only on the listed fields, so a bad expression cannot change evaluation semantics.
21. **ID encoding.** OTLP JSON uses hex strings for IDs while protobuf JSON uses base64; `internal/ingest/otlp.go` already converts hex to base64. MLflow's response encoding of span IDs is **not verified here**: the first connector commit records real v3.17.0 responses from the demo's MLflow server and the decoder accepts both encodings.

### 7. Credentials and egress

22. **Credentials follow 0015 and add no RBAC to evalsid.** The spec names `credentials: {env: NAME}` or `{file: <secret>/<key>}`. A name is checked as a grant for the source's project and the endpoint's host, over HTTPS (or loopback), exactly as for a judge. The value reaches evalsid as an environment variable or a file from a Secret the chart mounts (`sources.secrets`), kubelet refreshes mounted files without a restart, and the connector re-reads the file at each use. The values are never in the spec, the API or the logs. evalsid therefore needs no `get secrets` permission, and the operator stays an API client (0012). The cost: a new Secret *name* needs a chart upgrade. Reading Secrets through the Kubernetes API is a possible later mode with its own Role.
23. **The standalone path** `POST /v1alpha1/sources` is the same message. Its `credentials` names a variable or a file path under `sources.dir` in the server config; there is no Kubernetes there.
24. **Endpoint checks.** A source endpoint must be `https` unless the grant sets `allow_http`; loopback and link-local addresses (including cloud metadata) are refused unless the config lists them, so a project editor cannot point a source at an internal service.
25. **Egress.** Connectors run in the evalsid process, so the pod's egress is the process's egress: judges, sinks, Postgres, NATS and S3 as well. The PRD says the operator generates a NetworkPolicy for each source; that is **not done**, for two reasons:
   - Kubernetes `NetworkPolicy` selects by pod, namespace and CIDR and cannot name a host. Databricks, SageMaker and most hosted MLflow have no stable addresses.
   - Any egress policy that selects the evalsid pod denies everything it does not list, so a policy generated per source would cut off the others unless something owns the whole list. The chart owns that list.
   
   Instead: the chart takes `networkPolicy.egress.sources` (selectors for in-cluster stores, CIDRs for fixed ones) and, when `networkPolicy.egress.enabled`, renders one policy that lists the infrastructure egress it already knows (DNS, Postgres, NATS, S3, ClickHouse, sinks and judges from values) plus those sources. It is off by default so that existing installs do not lose traffic. For hosted stores reached by name, the guide says to restrict by FQDN in the mesh or CNI (an Istio `ServiceEntry` and egress gateway, or a Cilium FQDN policy). If a customer wants the pull path isolated, `sources.dedicated: true` runs the manager in its own Deployment (the same binary, `--role sources`, taking the engine's lease over the API) with a policy that lists only sources and evalsid itself. That mode is a follow-up; the interface does not change.
26. **Data handling.** Redaction rules apply to pulled records before storage and before any judge call, as for pushed ones. Retention is the project's.

## Consequences

- The `watch` package gains `IngestBatchContext`; the OTLP path is unchanged. New tables: `source_state`, `source_seen`, `source_writes`.
- The sinks' `trace_feedback` settings and `TraceSource.writeBack` overlap. A source's write-back is the way to write to the store it reads; the sink setting stays for traces that arrive by OTLP and are also in a store. A project must not turn both on for one store (the manager refuses it when it can tell).
- The first cut ships MLflow OSS and SageMaker (after a real-server check), then Databricks for UC-schema locations, then Langfuse and Phoenix. Azure ML and Databricks experiment-located spans wait for evidence.
- Egress isolation is weaker than the PRD wants until `sources.dedicated` or a mesh policy is in place. The guide must say so.
- Connector contract tests need recorded responses from each store at a pinned version; an upstream API change is a failing test, not a silent change.

## Found wrong in the PRD

- **Azure ML** is listed with MLflow in M3 and S2. Nothing found shows it serving MLflow 3 traces; treat it as unverified and out of the first cut.
- **Langfuse** is listed as a first-cut source with a p95 lag under two minutes. Its older read endpoints lag by about ten minutes by its own spec; only the v2 observations API is real time, and the connector has to build spans from observations.
- **"The operator generates a NetworkPolicy for each source"** cannot be done as written (item 25).
- **Databricks** is listed as one MLflow target. Its search is long-running and its span fetch differs from open source; it is a separate variant.

## Sources (pinned)

- MLflow v3.17.0: `mlflow/protos/service.proto` (`searchTracesV3` at line 893, message at 4926; `getTrace` 817; `batchGetTraces` 836; `createAssessment` 1335; `TraceInfoV3` 4506), `mlflow/protos/assessments.proto`, `mlflow/store/tracking/databricks_rest_store.py` (`search_traces`, `batch_get_traces`, `get_trace`), `mlflow/store/tracking/rest_store.py` (`search_traces`, `get_trace`), `mlflow/utils/rest_utils.py` (`_V4_TRACE_REST_API_PATH_PREFIX = /api/4.0/mlflow/traces`), <https://github.com/mlflow/mlflow/tree/v3.17.0>.
- Langfuse OpenAPI at <https://github.com/langfuse/langfuse/blob/223978955ebfe6cbf6e8572524f64c2565d43039/web/public/generated/api/openapi.yml> (info description; `/api/public/v2/observations`, `/api/public/traces`, `/api/public/scores`).
- Phoenix OpenAPI at <https://github.com/Arize-ai/phoenix/blob/35f86e5726a5881ecb69851bc7b8d27a34602c77/schemas/openapi.json> (`getSpans`, `spanSearch`, `annotateSpans`, `annotateTraces`).
- Databricks: [Tracing FAQ](https://learn.microsoft.com/en-us/azure/databricks/mlflow3/genai/tracing/faq) (quotas, 1,000 recent traces, pagination), [Programmatic access to traces](https://docs.databricks.com/aws/en/mlflow3/genai/tracing/observe-with-traces/query-via-sdk) (`locations`, SQL warehouse).
- SageMaker: [aws/sagemaker-mlflow](https://github.com/aws/sagemaker-mlflow) (SigV4, `service_name` is a constructor argument; its value is not read from the plugin here), [MLflow tracking servers](https://docs.aws.amazon.com/sagemaker/latest/dg/mlflow-create-tracking-server.html).
- Azure ML: [Configure MLflow for Azure Machine Learning](https://learn.microsoft.com/en-us/azure/machine-learning/how-to-use-mlflow-configure-tracking).

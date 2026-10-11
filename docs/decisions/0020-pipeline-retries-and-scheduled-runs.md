# 0020. Durable pipeline retries and bounded scheduled batch pulls

- **Status:** Accepted, 2026-10-11

## Context

Customer pipelines need retryable writes, dataset publication and an authorized
way to recover stored-run quotas. Trace-store users also need periodic batch
reports without configuring an online policy.

## Decision

`Idempotency-Key` binds a write to its authenticated principal, RPC and
deterministic protobuf request. Raw uploads include the content SHA-256 in the
fingerprint. Keys contain 1-128 visible ASCII characters and are hashed at rest.
Completed outcomes live for 24 hours; changed inputs conflict. Authentication
and authorization run again on retries. A claim without a completed outcome
never expires into automatic reexecution: the customer must reconcile it.
This avoids claiming exactly-once side effects across remote systems.

Responses are persisted, except generated API-key secrets. The initial key
creation returns the secret once; a retry returns the same key metadata without
the secret. Failure replay uses a generic code/message, not arbitrary upstream
error text. Keyed streams buffer bounded inputs before execution and store
response frames; clients must finish sending before waiting for their results.
Unkeyed streams retain incremental behavior. Per-scope storage is capped at
1024 entries and 64 MiB of response data. Completed entries are pruned on writes;
uncertain claims continue to occupy capacity.
PostgreSQL claims and completions take a shared transaction advisory lock so
replicas cannot race those limits. Handler execution stays outside the lock;
cache-write throughput is intentionally serialized for correct accounting.

Dataset publication validates JSONL or Parquet, normalizes Record rows, and
publishes a content-addressed JSONL object only after validation. Limits are
64 MiB input/canonical output, 100000 records, 4 MiB per row/page and two active
conversions per replica. Parquet page sizes/value counts are checked before
decoding; external/encrypted column chunks are refused. S3 imports may read
only `datasets_dir/incoming/<project>/` in the configured bucket.

Run deletion requires `runs.delete` and an inactive terminal state. A database
transaction removes the run, snapshot, outputs, results, lease and webhook
deliveries. Expiring authorization-only tombstones allow permission-checked
retries after deletion. Upstream datasets and already-exported sink data are
not owned by the run and remain.

Scheduled sources use five-field UTC cron, a bounded start-inclusive/end-exclusive
query window, starting deadlines and a durable cursor. They run under the
existing source-manager leader lease, plus a per-source two-minute database
lease and a 90-second pull deadline. Stable window/config run IDs reconcile a
crash between run insertion and cursor persistence; a source waits for its last
run to finish before starting another. Snapshots are redacted before batch
evaluation. Missing fetches, in-progress traces and oversized snapshots fail
without advancing the cursor. Missed deadlines are counted, not replayed without
limit. Empty windows advance the cursor without creating an empty run.

## Consequences

- Retry safety is opt-in through the header. After 24 hours a completed key may
  execute again. Never use a new key to guess whether an uncertain write ran.
- A lost first API-key response requires key revocation and replacement; the
  server never keeps a plaintext secret just to make retries convenient.
- Scheduled sources reject online policies, backfill and automatic source
  write-back. Batch results use normal run reports and configured run sinks.
- Deleting a Kubernetes EvalRun still preserves its server run; explicit API
  deletion is separate. Archive/automatic run retention remain deferred.
- Local tests cover real-worker dataset/run/deletion pipelines and scheduled
  reports, plus cursor restart, deadline and overlap behavior. PostgreSQL 17
  regressions also verify simultaneous cache capacity claims and run deletion.
  These tests do not
  certify cloud connector credentials, production load or cluster failover of
  these new operations.
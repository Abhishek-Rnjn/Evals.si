# 0028. A ClickHouse trace source for Collector Map tables

- **Status:** Accepted, implemented on this branch, 2026-10-18.

## Context

PRD S2 / M5 includes reading traces already stored in ClickHouse and writing
their evaluation scores back. ClickHouse is also an optional storage backend
for Evals.si itself; that uses a different schema and is not this connector's
input. Arbitrary user SQL would expand the source's privileges and make
completeness, paging and mapping difficult to establish.

## Decision

1. `connector: clickhouse` reads the HTTP interface. Its single `locations`
   entry is `database.table`, with two plain SQL identifiers. The first schema
   is the default `Map(String, String)` trace table from OpenTelemetry
   Collector contrib's `clickhouseexporter`, pinned to v0.162.0 in the tests.
   Table names may differ; column names and types must match. Experimental
   JSON trace tables, Evals.si's own tables and arbitrary custom schemas are
   not supported. The connector adds no Go database driver dependency.
2. The credential is `username:password`, split at the first colon, sent as
   HTTP Basic auth. Omitting the credential uses the server's default account.
   `credentials.env` and `credentials.file` use the existing grants and
   rotation rules. The endpoint takes no userinfo, query string or fragment.
   The existing source guard checks endpoints, DNS and redirect hops.
3. Trace candidates have an exported root span (empty or all-zero
   `ParentSpanId`). The Collector writes ended spans; a rootless trace is not
   yet eligible. A time filter and keyset limit on roots find one bounded page
   of candidates, and aggregation reads those traces' complete span sets.
   Filtering individual children by `Since` would misidentify old traces or
   truncate trajectories. The root timestamp is the trace start, so a late
   child cannot move a trace between pages. Pages sort by `(start, TraceId)` and use a bound keyset
   cursor, including the original `Since`. A canonical hash of the distinct
   span rows detects late children and changed fields; identical retries do
   not change it.
4. Fetch maps IDs, timestamps plus duration, status, kind, resource and span
   attributes, events, links and trace state to OTLP. The Collector's Map
   representation has already stringified attribute values; strings are
   preserved and decoded by the existing ingest profiles. Scope name/version
   become `otel.scope.name` / `otel.scope.version` attributes. Identical span
   rows are deduplicated; conflicting rows with the same span ID are errors.
   Malformed IDs/times/nested columns and more than 10,000 rows in a trace
   fail the fetch, rather than scoring a partial trajectory.
5. Write-back inserts into `<trace table>_evalsi_scores` in the same database.
   Operators create this table from the shipped SQL; the runtime executes no
   DDL. `ReplacingMergeTree(Version)` is keyed by `(TraceId, Policy, Metric)`.
   Numbers, booleans and labels have typed columns; each new row clears old
   typed fields and replaces the rationale, including an empty one. Every
   retry uses the same logical key. Reads **must use `FINAL`**, or an
   equivalent latest-version aggregation, to see one current score before
   background merges. This is logical replacement, not an exactly-once
   physical insert guarantee. Version is the source process's Unix
   nanosecond timestamp, matching the existing ClickHouse store approach;
   concurrent writers require synchronized clocks and a shared policy/metric
   identity intentionally addresses the same score. The source manager's
   single leader and persisted write receipts suppress ordinary duplicates.
6. Query predicates and limits use ClickHouse typed parameters, and inserts
   carry JSONEachRow values; only validated, quoted identifiers enter the SQL
   text. Calls wait for query completion, limit
   execution to 30 seconds, memory to 512 MiB and responses to 64 MiB, and
   refuse malformed/truncated results. HTTP 429/503 honor `Retry-After`.
   Runtime accounts need `SELECT` on the trace table and, only with
   write-back enabled, `INSERT` on the score table. They need neither DDL nor
   `SELECT` on the score table. A separate viewer account can query scores.

## Consequences and limits

The existing manager owns backfill, lookback, rate limits, policy dispatch,
redaction, retries and status. Set `maxTraceDuration` to cover the longest run
plus export delay; a root arriving after its start has passed the lookback can
be missed. Rootless traces do not populate the manager's deferred count, and
`maxInProgressAge` cannot force their evaluation. Late children are discovered
only while their trace remains inside that window. Changes after the window,
TTL deletion during fetch, and application clock skew remain operational
considerations. The source does not rewrite or delete upstream spans.

Grouping and hashing candidate traces can be expensive during large
backfills. Collector indexes, retention and per-user ClickHouse quotas matter;
the bounded tests do not certify production throughput, Cloud, distributed
tables, TLS, replication or a specific tracing UI's ability to display scores.

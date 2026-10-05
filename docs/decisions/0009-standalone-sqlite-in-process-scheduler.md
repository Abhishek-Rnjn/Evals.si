# 0009. Standalone storage and scheduling: SQLite and an in-process scheduler

- **Status:** Accepted, 2026-10-05 (Phase 1)

## Context

The design plan (§14, §21) gave the standalone server embedded NATS JetStream
for work queues, SQLite for metadata and DuckDB for analytical queries over
results. Building Phase 1 showed that the standalone form factor does not need
the first or the third yet:

- One `evalsid` process schedules all work. A bounded pool of run slots, plus
  batched, parallel calls to the supervised worker, keeps the worker busy
  without a broker. Idempotent task keys and resumable runs (`ResumeRun`, and
  unfinished runs marked resumable after a restart) already give
  at-least-once semantics.
- Result volumes on one machine (thousands to low millions of rows) are
  comfortable for SQLite with the right indexes. Summaries are computed
  in Go from the stored results.

## Decision

- **Standalone keeps all state in one SQLite database** (`<data_dir>/evalsi.db`):
  runs, records, results, traces, trace results and policies. It uses the
  pure-Go `modernc.org/sqlite` driver, so `evalsid` stays a single static
  binary with no CGO.
- **Scheduling is in-process.** Runs take slots up to `runs.max_concurrent`.
  The online policy engine batches traces through a bounded queue, counting
  and dropping traces when it overflows.
- **NATS JetStream arrives with the Kubernetes form factor** (Phase 3), where
  several `evalsid` replicas and autoscaled worker pools need a shared queue.
  Postgres and ClickHouse replace SQLite there, behind the same store
  interface.
- **DuckDB is deferred.** It will come when cross-run analytics, such as
  slicing results by metadata across many runs, outgrow SQLite.

## Consequences

- Standalone is one process plus its Python worker, with one file to back up.
- A single `evalsid` is the scaling limit of standalone. Moving beyond it is
  the Kubernetes form factor's job, not a reason to add a broker here.
- The store package is the seam for Phase 3. Its tests must keep running
  against every backend.

# 0022. Cross-run slices are computed on the server; SQL stays on the client

- **Status:** Accepted, 2026-10-12

## Context

`evalsi analyze` loads runs into DuckDB on the client through the API ([decision 0009](0009-standalone-sqlite-in-process-scheduler.md)). To slice a metric by record metadata it reads every result and record of every run it loads. For thousands of large runs that is the cost, and the answer is a few rows.

The earlier note said ClickHouse already holds the data on Kubernetes. It does not: ClickHouse holds traces and their online scores. Run results are rows in the relational store (SQLite or PostgreSQL) whose payload is a protobuf blob (`run_results.result`, `run_records.record`, `run_outputs.record`), so SQL cannot aggregate them in any backend.

## Decision

1. **A new RPC, `RunService.SliceRuns`** (`POST /v1alpha1/runs:slice`), computes what `Warehouse.slice` computes: for a metric and a record metadata field, per run and field value, the count, mean and a normal-approximation interval (`z` of 1.6449, 1.96 or 2.5758; no interval for fewer than two values). It scans each run's results once in the server, keeping a count, mean and sum of squares (Welford) per group, and returns only the rows.
2. **Parity with the client.** Metric names are `evalsi.results.result_metric`'s; a score's group comes from its own trial's output record, else the dataset record, else `(none)` (a dataset-scope result is `(none)`); strings are used as they are, numbers and booleans as JSON text, null and missing as `(none)`; only `scored` results count; rows are ordered by key, run creation time, run id. `internal/runs/slice_test.go` and `test_slice_parity_fixture` in the Python tests use the same runs and expect the same numbers. Object and array values are grouped by their compact JSON text, which the client may format differently. The server pairs a result with its record by index, the client by record id, so they differ only where a dataset repeats or leaves out record ids (uploaded datasets are given unique ids).
3. **Access is the one `ListRuns` and `GetRun` use.** The gate only authenticates (as for `ListRuns`); the service then checks `runs.read` on every run it would slice, with the run's labels, so a role conditioned on labels is honoured. Selecting by project, labels or recency silently leaves out runs the caller cannot read. Naming a run the caller cannot read answers `NotFound`, as a missing run does.
4. **Bounds.** At most 500 runs (50 by default) and 10,000 rows (a row per run and field value) per request; past the row cap the request is refused so that slicing by a record id does not become an unbounded reply. The request honours cancellation between result pages.
5. **The CLI opts in:** `evalsi analyze slice … --server URL --on-server`. `--server` alone keeps loading locally, so nothing changes for existing scripts or for servers without the RPC.
6. **No server-side SQL.** `evalsi analyze query` stays on the client. A SQL endpoint over the server's database could not be restricted to the runs a caller may read, and the data is not columnar.

## Consequences

- A slice reads the stored results of each run in the server process: it removes the transfer and the client memory, not the scan. A very large run costs the server a pass over its results. ClickHouse would help only if run results moved into columns, which this does not do.
- Group labels are text, so `1` and `"1"` are one group, as on the client.
- A replica answering the request reads the shared database, so any replica gives the same answer.

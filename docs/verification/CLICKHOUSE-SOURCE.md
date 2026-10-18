# ClickHouse trace-source verification

The ClickHouse source implements the later-store part of PRD S2 / M5.
This report concerns reading a customer's Collector trace tables, separately
from Evals.si's existing ClickHouse storage backend.

## Environment and method

Bounded local checks ran on 2026-10-18 against the official
`clickhouse/clickhouse-server:25.8` image, reporting **25.8.33.6**:

```text
sha256:0152dd511befe6a2c2ef53e930726179669b08116da78500b37c51c96ff5ee77
```

The tests create isolated databases from the unmodified Collector contrib
**v0.162.0** Map trace table template in
`internal/source/clickhouse/testdata/traces_table.sql`, and create the companion
table from the shipped `examples/sources/clickhouse-scores.sql`. Synthetic
span rows are inserted directly; this does not run a Collector process or a
model. The upstream schema template's SHA-256 is:

```text
aa2836fcb333ed589d5c2144e15bd9166258bf7add456b3a89ebad574a7a2503
```

| Check | Result |
| --- | --- |
| Actual Map schema, HTTP auth and typed query parameters | Passed |
| Rootless children withheld until a completed root arrives | Passed |
| Traces whose root predates `Since` excluded even when their children do not | Passed |
| Equal-start keyset pages | Passed |
| Duplicate exporter rows leave digest unchanged; late child changes digest | Passed |
| Real nested event/link columns and nanosecond timestamps | Passed |
| Typed numeric / boolean / label replacement, rationale clearing and replay without receipts | One current row under `FINAL`; physical duplicates may remain until merging |
| Runtime account with only SELECT on traces and INSERT on scores | Pull/fetch/write passed; SELECT on score table refused |
| Source admission, credential grant, real server/policy engine/Python worker | Historical and newly completed trace each scored `tool-errors=1`; feedback read back from ClickHouse |
| Trace API mapping and repeated-poll deduplication | Input/output/trajectory retained; pulled/written counts remain two |

Both tests drop their isolated databases, and the connector test drops its
temporary limited account. The local container is disposable.

## Repeat the checks

Run the connector test against a disposable ClickHouse server with an account
allowed to create databases and users. The URL may contain credentials for
the **test runner**; the production `TraceSource` never takes URL credentials.
Use environment configuration to supply test URLs and avoid committing keys.
For the official Docker image, the disposable administrative account needs
`CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1`; ordinary runtime accounts do not.

```sh
# EVALSI_TEST_CLICKHOUSE_URL is the test server's HTTP URL.
go test -race ./internal/source/clickhouse -count=1 -v

# EVALSI_E2E_CLICKHOUSE_URL names the same disposable server.
cd python && uv sync --locked --all-packages && cd ..
EVALSI_E2E_WORKER="$PWD/python/.venv/bin/python -m evalsi" \
  go test -race ./tests/e2e -run '^TestTraceSourceClickHouse$' -count=1 -v
```

These use the existing ClickHouse services and environment variables in the
ordinary Go and e2e CI jobs, so no provider secret or manual workflow is needed.

Unit tests additionally cover unsafe endpoint/identifier rejection, malformed
credentials and cursors, malformed or conflicting spans, duplicate span
deduplication, the 10,000-row limit, cancellation, truncated results,
429/503 retry delays, typed write-back and partial write acknowledgements.

## Limits

This is a small functional check, not production certification. It does not
measure 30-day backfill throughput or p95 lag, exercise Cloud/TLS, replication
or distributed tables, run the Collector itself, verify a Kubernetes
Secret/egress configuration, or inspect tracing UI score display. The input
schema must be the documented Map schema. Root and late-span arrival must
remain within the source's configured lookback; TTL deletion can race a pull.
The runtime never alters upstream spans or provisions schema.

See [the setup guide](../guides/trace-sources.md#clickhouse) and
[decision 0028](../decisions/0028-clickhouse-trace-source.md).

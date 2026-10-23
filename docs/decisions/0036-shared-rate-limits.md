# 0036. Rate limits shared across replicas, and reloaded

- **Status:** Accepted, implemented, 2026-10-22. Amends [0029](0029-per-caller-rate-limits.md).

## Context

[0029](0029-per-caller-rate-limits.md) left two gaps: limits are per replica,
so N replicas allow N times the rate, and the settings are read only at start.
It also noted why a shared limit was not built: asking a shared store on every
request is more than the ingest path can afford.

Replicas already share two things: the database (PostgreSQL whenever there is
more than one replica; SQLite allows one) and, optionally, NATS. Every
multi-replica install has the database; NATS is optional.

## Decision

1. **Shared limits are opt-in** (`rate_limits.shared: true`) and counted in the
   database, in fixed windows (`shared_window`, default 1s), in a
   `rate_windows` table keyed by class, caller and window start.
2. **The database is not on the request path.** Each replica keeps its token
   buckets as before. A call its bucket allows is then checked against the
   window: the last total this replica read plus what it admitted since must be
   under the cap, `burst + per_second x window`, which is what one bucket would
   allow over the window. Every `shared_sync_interval` (default 250ms) a replica
   adds what it admitted, in one transaction, and reads back the totals.
   Buckets that admitted nothing since the last sync are not synced.
3. **The bound is approximate.** Between syncs the replicas together can pass
   the cap by what each admits in one interval, and their own buckets still
   bound that. The point is to stop N replicas allowing N times the rate, not
   exact accounting.
4. **On a database error each replica falls back to its own buckets** until a
   sync succeeds. `evalsi_rate_limit_shared_fallback` (gauge) and
   `evalsi_rate_limit_shared_errors_total` show it, and a warning is logged at
   most once a minute. Counts admitted meanwhile are added once the database
   answers.
5. **Config reload applies the rates, `principals` and `disabled: true`.**
   Existing buckets take the new rate and size and keep the tokens they hold
   (up to the new size), so a reload neither refills nor drains them. A reload
   is audited as `config.reload` with `changed: [rate_limits]`. Turning on
   limits that were off at start (no middleware was installed) and changing
   the `shared` settings need a restart; the reload logs that and keeps them.

## Consequences

- With `shared: true`, each replica writes about one small transaction per
  sync interval while callers are active (one row per busy caller and class),
  and windows older than a minute are pruned by any replica once a minute.
- Every replica must use the same window; the window is read at start.
- NATS was not used: it is optional, and a KV round trip per sync would be no
  cheaper than the database.

# 0038. Per-group quality scores for routing decisions

- **Status:** Accepted, implemented (the Evals.si side), 2026-10-22.

## Context

The design (section 16, "Working with agentgateway") lists feeding scores back
into the gateway as a later item: quality-aware routing, and disabling a
misbehaving MCP tool. The gateway's extension points for that are not ones
this repository can rely on, and the stored online scores had no per-route
view: a policy's windowed metrics are per policy, and `ListScores` returns
traces, not aggregates.

## Decision

Build the half that does not depend on the gateway: publish quality per group,
in forms any consumer can read, and document a recipe for acting on it.

1. **A report over stored results.** One policy's results over a window,
   grouped by `service`, the root span's `name`, or a trace label
   (`label:<key>`), with per-metric mean and trace count, the share of error
   traces, and a state per group when a metric and threshold are given:
   `insufficient_data` (fewer than `min_traces` scored traces, default 20),
   `degraded` (mean below the threshold) or `ok`. Groups are the busiest first,
   capped (`max_groups`, default 50), and one report reads at most 20000
   traces.
2. **Two ways out.** `GET /v1alpha1/quality` computes it on demand through
   `TraceService.ListScores` with the caller's access (permission
   `traces.read`, only the traces the caller may read). `quality_exports`
   publishes it as Prometheus gauges (`evalsi_quality_score`,
   `evalsi_quality_traces`, `evalsi_quality_error_rate`, report age and
   failures), recomputed on an interval from the store; a group with too few
   traces gets no score gauge, so a noisy mean is not alerted on.
3. **Grouping is by label, not by span attribute.** The stored results carry
   the trace summary (service, name, labels), not span attributes, so a
   gateway's route or backend must reach Evals.si as a label: an
   `evalsi.label.<key>` resource attribute or a label on the ingest key. The
   guide shows both. Grouping by an arbitrary span attribute would need the
   attribute stored on the summary and is not built.
4. **No acting on the figures.** Evals.si does not change routes. The guide
   gives a recipe (a loop that moves Gateway API `backendRef` weights) as the
   shape of a consumer, with its limits.

## Consequences

- **Depends on the gateway, and not built:** native quality-aware load
  balancing, a score-fed circuit breaker, auto-disabling an MCP tool, and any
  gateway extension point for them. Which span attributes name the route,
  backend and tool depends on the gateway's version and route type and was not
  checked against a running gateway; the weight-shifting recipe uses only the
  standard Gateway API and was not run against one.
- The report and gauges are only as good as the policy's sampling: a sampled
  policy gives a noisy mean per route, which `min_traces` and the window
  bound but do not remove.
- Gauge cardinality is bounded by `max_groups` per export.

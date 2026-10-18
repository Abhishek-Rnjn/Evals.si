# 0029. Per-caller rate limits

- **Status:** Accepted, implemented, 2026-10-18.

## Context

The production-readiness review (LEFTOVERS, 2026-10-16) found no throttle on
authenticated callers. Request size is bounded (`MaxBytesReader`, the RPC
message limit), rejected credentials are audited at most 10 a second, and
JWKS refetches are spaced, but a leaked or runaway API key could call the API
or push OTLP as fast as the server answers. Quotas
([0021](0021-sandbox-time-quotas.md) and §17 "Tenancy") limit what a project
spends, not how fast one credential calls.

## Decision

1. **The unit is the caller**, as authentication names it: an API key
   (`key:<name>`), a user (`user:<provider>/<subject>`), or, where
   authentication is optional, an anonymous caller told apart by source
   address. A project is not the unit: one leaked key should not throttle its
   project's other keys, and one key can reach several projects.
2. **Three classes, each with its own token bucket per caller:** `api`
   (RPCs and REST), `ingest` (OTLP exports, gRPC and HTTP) and `guardrails`
   (the agentgateway webhook and ExtMcp). A collector's export rate says
   nothing about how fast it may call the API, and guardrail checks sit in
   the request path of live LLM traffic.
3. **One token per HTTP request.** A middleware after authentication takes
   it, so a stream costs one call however many messages it carries, and a
   request is never charged twice (the gate authenticates some requests more
   than once, such as MCP tool listing). `/mcp` itself is not counted: its
   tool calls re-enter the API in process and are counted there. Open routes
   (`/healthz`, gRPC health, `/.well-known/*`, `/ui/`) and requests whose
   credential was rejected are not counted.
4. **The refusal is the caller's protocol's.** The middleware stores it on
   the request and the gate returns it from `authenticate`, after the
   credential check: `ResourceExhausted` with a `Retry-After` header (seconds)
   for Connect and gRPC, HTTP 429 with `Retry-After` for REST, OTLP/HTTP and
   the guardrail webhook.
5. **On by default while authentication is enabled, with generous limits**,
   per replica: `api` 500/s (burst 1000), `ingest` and `guardrails` 2000/s
   (burst 4000). They stop a leaked or runaway credential, not real load.
   Without authentication every caller is the same anonymous owner, so the
   limits are off. `rate_limits.disabled: true` turns them off;
   `rate_limits.principals` overrides classes per principal, and
   `per_second: 0` lifts a class's limit.
6. **Visible, not audited per call.** `evalsi_rate_limited_total{class}`
   counts refusals, and a warning is logged at most once a minute per caller
   and class. Refusals are not audit events: a flood of them would flood the
   audit log, which is what [0026](0026-enterprise-audit-and-delegation.md)
   keeps for decisions.

## Consequences

- Limits are per replica. N replicas allow up to N times the rate; a shared
  limit would need the store or the cluster on every request, which the
  ingest path cannot afford.
- Anonymous callers behind one proxy share its address's buckets.
- Buckets live in memory and are forgotten once unused for 10 minutes and
  refilled, so a caller back later starts with the full bucket it would have
  had anyway; a slow rate cannot be reset by waiting just past the idle time.
- The settings are read at start, not on configuration reload.
- Clients see a new error. The Python client raises it as `ServerError`
  with code `resource_exhausted` and does not retry; callers that retry
  should honor `Retry-After`.

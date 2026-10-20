# 0026. Enterprise-grade audit, and a delegation-ready identity model

- **Status:** Proposed 2026-10-15; partly built 2026-10-20.
  - **Built:** strict mode (item 1); the authentication context, `subject` and `actor` (items 3 and 9); configuration audit for the sections that reload live, plus a startup entry with per-section hashes (item 5); read auditing by kind, without sampling or row counts (item 2); the hash chain with `evalsi auth audit verify` (part of item 6).
  - **Not built:** initiator, executor and grant fields on runs (item 4); signed checkpoints and archival retention (the rest of item 6); guaranteed export (item 7); legal holds (item 8); token exchange (item 10).
  - The [identity guide](../guides/identity.md#7-audit-metrics-external-authorization) describes what is built.

## Context

The audit log ([0010](0010-identity-and-access-next.md), DESIGN §17 "Audit") records every mutating call and every denied call: time, principal, action, resource, the deciding rule or role, request ID and source address. Operators can read it with `ListAuditEvents`, keep it for `audit.retention` (default 90 days) and export it as OTel log events. Automatic run deletes and archives are audited too ([0023](0023-run-archiving.md)), as are credential refusals ([0015](0015-per-project-credentials.md)).

That is a good base, but it falls short of what an enterprise auditor or a regulated customer expects:

- **It fails open.** `Auditor.Record` (`internal/authz/audit.go`) logs a failed write and returns nothing, so the action it describes proceeds unaudited.
- **Reads are not audited.** Reading traces, results and datasets, which may hold PII, leaves no record. Only mutations and denials do.
- **It is not tamper-evident.** Entries are rows in the store, and retention hard-deletes them. An operator with database access can alter or remove entries without detection.
- **Identity context is thin.** An entry names the principal, but not how it authenticated (issuer, token ID, key ID, certificate), nor the groups that decided the outcome.
- **Initiator and executor are not separated.** Runs are asynchronous. The person who started a run and the identity the run executed under (and the credential grant it used) are different facts, and only `Run.created_by` records the first.
- **Configuration changes are not audited.** Role changes are. Hot-reloaded `credentials`, `authorization` and judge settings (re-read every 15 seconds and on SIGHUP) are not.
- **Delivery is best effort.** The OTel export is optional and has no delivery guarantee.

Separately, [0015](0015-per-project-credentials.md) rejected on-behalf-of token exchange for the current design: runs outlive a caller's token, and targets are called with a fixed identity so scores do not depend on who started the run. That reasoning holds, and nothing here changes it. But some deployments will have targets that enforce per-user permissions, or downstream audit requirements that name the real user. For them, token exchange (RFC 8693) is a plausible later addition, and the audit model should not need a schema change when it arrives.

## Decision

### Audit hardening

1. **Strict mode.** `audit.mode: strict | besteffort`. In `strict`, a mutating call whose audit write fails is failed (`unavailable`), not allowed to proceed. The default for an install with authentication enabled is `strict`; `besteffort` is today's behavior and stays the default for an unauthenticated local server. Denials are still returned as denials if their audit write fails, and the failure is logged and counted.
2. **Read auditing.** `audit.reads` lists the resource kinds whose reads are recorded (`traces`, `runs`, `results`, `datasets`, `annotations`), or `all`. An entry records the principal, the project, the resource kind and identifier or filter, and the number of rows returned. It never records payloads. High-volume reads can be sampled per principal and kind, and sampling is itself recorded in the entry.
3. **Richer authentication context.** Every entry also records:
   - the authentication method (`jwt`, `api_key`, `mtls`, `trusted_proxy`, `service_account`);
   - for JWTs, the issuer, `jti` and `kid`, never the token;
   - for API keys, the key name (never the key or its hash);
   - for mTLS, the certificate fingerprint and the SPIFFE ID or common name;
   - the groups and roles that were in effect at decision time.
4. **Initiator, executor and grant.** Run-related entries and run records carry:
   - `initiated_by`: the authenticated principal that created the run or policy;
   - `run_as`: the identity the run executes under (a service identity, or the same principal for synchronous calls);
   - `credential_grants`: the grants used, by variable name and destination host, never values;
   - the run ID on every downstream action it causes, so an auditor can join Evals.si, worker, sandbox and target-side logs.
5. **Configuration audit.** Each configuration reload that changes `credentials`, `authorization`, `rbac`, `judges` or `audit` writes an entry with the config hash and a before/after diff of the changed sections. Secrets in the diff are shown as names only.
6. **Tamper evidence.**
   - Entries are hash-chained: each carries the SHA-256 of the previous entry and of its own canonical content.
   - The server periodically writes a signed checkpoint (the latest hash, a sequence number and a timestamp), using a key from config, so a deleted or edited range is detectable.
   - `evalsi auth audit verify` checks a range of the chain and the checkpoints and reports the first break.
   - Retention becomes archival: entries leave the live table only after they have been exported, and the chain records the removed range.
7. **Guaranteed export.** An audit sink with at-least-once delivery, from a durable outbox in the store, to:
   - OTel logs (as today);
   - a syslog or JSON-lines target, for SIEMs (CEF is optional);
   - S3-compatible storage with Object Lock, for write-once retention.

   A sink that is down backs up the outbox. In `strict` mode, an outbox beyond a configured size or age fails mutating calls rather than dropping entries.
8. **Retention and holds.** `audit.retention` stays configurable per install, with a documented range for common regimes. A legal hold (`evalsi auth audit hold`) suspends deletion for a project, principal or time range, and holds are themselves audited.

### A delegation-ready model (no token exchange yet)

9. **Subject and actor fields now.** Audit entries and the `Principal` model gain `subject` (whose authority the request carries) and `actor` (the identity that performed it), plus an ordered `delegation_chain`. Today `actor` equals `subject` and the chain is empty. The names and semantics follow RFC 8693 (`sub` and `act`), so real values need no schema change later.
10. **Token exchange, if added, is explicit and per target.** It is a future option on a credential grant (for example `exchange: {audience, scopes, max_ttl}`), not a default and not a change to how runs authenticate. Properties it must have:
    - the exchange happens at the worker, per target call or per run start, and never stores a caller's token;
    - runs that outlive the exchanged token fail clearly or refresh through a documented mechanism, never silently fall back to the fixed identity;
    - scores from exchanged calls are labelled as such, so they are not compared with fixed-identity runs unknowingly;
    - every exchange writes an audit entry: who requested it, for whom, audience, scopes and the resulting token's `jti`, never the token.
11. **Trusted-proxy and gateway tokens.** Tokens issued by a gateway (for example a JWT in a cookie, carried through the existing token `location` setting) are authenticated like any other JWT. Their claims fill the new authentication-context fields. No gateway-specific code is added.

## Consequences

- An authenticated install gets stricter behavior by default: mutating calls can fail if the audit store is unavailable. That is the intended trade, and `besteffort` is the escape hatch.
- Read auditing adds write load. Sampling and per-kind selection keep it bounded, and the cost should be measured before the defaults are chosen.
- The audit table, and the `AuditEvent` proto, grow additive fields (`auth_method`, `issuer`, `token_id`, `key_id`, `groups`, `initiated_by`, `run_as`, `credential_grants`, `subject`, `actor`, `delegation_chain`, `prev_hash`, `hash`, `sequence`). These are additive to `v1alpha1`. Existing entries have no hash chain; the chain starts at the first entry written after the upgrade, and a checkpoint marks the boundary.
- The signing key for checkpoints is a new secret to manage and rotate. Rotation is recorded in the chain.
- Hash chaining detects tampering but does not prevent it. Preventing it needs the WORM export in item 7, which is operator-configured.
- Token exchange stays out of scope. Items 9 and 10 only reserve its place. A separate decision record is needed before it is built, because it must settle token lifetime against run duration.
- Not covered: auditing inside the target systems (those need their own logs, joined by run ID), and non-repudiation beyond the signed checkpoints.

## Slices

1. Strict mode, richer authentication context, `subject` and `actor` fields.
2. Initiator, executor and grant fields on runs and entries; configuration audit.
3. Read auditing, with sampling and a measured cost.
4. Hash chain, signed checkpoints, `evalsi auth audit verify`.
5. Guaranteed export (outbox, syslog, Object Lock) and legal holds.
6. Token exchange, under its own decision record, if a customer needs it.

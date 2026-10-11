# 0019. Redact traces before persistence and evaluation

- **Status:** Accepted, 2026-10-11

## Context

Inline guardrails redact gateway traffic, but neither OTLP ingestion nor trace
sources previously redacted the records Evals.si kept. Optional collectors are
not a sufficient privacy boundary: callers can push directly, and sources can
pull historical data with field overrides.

## Decision

The server compiles `traces.redaction` once at startup and shares the rules
between its OTLP receiver and policy engine. Push exports are redacted after
authentication and project assignment but before assembly or NATS forwarding.
The engine redacts pulled spans before normalization and sanitizes source
overrides after they execute, before persistence, policy selection, evaluator
calls, promotion and result callbacks.

Default rules replace values under these case-insensitive keys with
`[REDACTED]`: `password`, `passwd`, `authorization`, `proxy-authorization`, `cookie`,
`set-cookie`, `token`, `secret`, `api_key`, `api-key`, `apikey` and `private_key`,
plus names ending in `_password`, `_token`, `-token`, `_secret`, `-secret`,
`_api_key`, `-api-key`, `_apikey`, `_private_key` and `-private-key` (so
`x-api-key`, `id_token` and `client_secret` match). Token counts such as
`input_tokens` are plural and are not matched. A rule matches a complete
key or its final dot-separated segment. Configured key globs extend these
defaults; configured RE2 value patterns and PII classes are optional.

PII classes are `email`, `us-ssn`, `credit-card`, and `phone`. They are heuristic
patterns, not identity validation or a promise of complete PII detection.
Operators may explicitly disable all redaction. Invalid enabled rules refuse
startup without echoing their contents. Chart users put this configuration
under `server.config.traces.redaction`; no per-project API is added.

## Consequences

- Matched JSON values are replaced recursively while preserving valid JSON.
  Unmatched JSON retains its formatting; the replacement marker is stable
  across the receiver and engine passes.
- Attribute values, resource attributes, labels, events, links, status text,
  source overrides and nested record metadata are covered. UTF-8 byte-valued
  OTLP attributes are inspected before flattening on push, and on pulled spans;
  opaque bytes in those messages and unknown protobuf fields
  are removed when redaction is enabled. Binary trace/span identifiers remain
  intact. Project routing and attribute names are not rewritten.
- Masking can change evaluator scores, selectors and metadata. PII matching is
  opt-in to avoid silently rewriting normal numbers or email-bearing tasks.
  Deploy matching rules consistently on every replica and restart to change
  them; this is not hot-reloaded configuration.
- Existing stored traces, upstream trace stores, direct Evaluate calls,
  offline datasets and run inputs are outside this incoming-trace boundary.
  Redaction is not retrospective deletion, encryption or a substitute for
  retention and access controls. Source error status and connector error logs
  apply the text rules without changing retry errors. Arbitrary transport or
  upstream errors are not automatically recognized as sensitive; choose value
  patterns for secrets that may appear outside structured trace fields.
- Regression tests inspect SQLite files, stored records, evaluator queues,
  result callbacks, storage-failure logs and the pre-NATS forwarding callback
  using raw-value sentinels. This is local executable evidence, not a new
  Kubernetes, cloud or load certification.
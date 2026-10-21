# 0030. A browser release workbench over the existing services

- **Status:** Implemented, unreleased, 2026-10-21.

## Context

Approved catalog suites and pinned release decisions are built, but candidate
setup hardcodes exact-match and the release form requires policy JSON. Aggregate
comparisons cannot explain a changed tool sequence. The browser also indexes
result records by ID, although different trials can carry different outputs.

## Decision

Use the existing Run, Catalog and Release services for browser configuration,
admission, reviewed policy pins and immutable decisions. Keep a bounded form for
recorded scoring, OpenAI-compatible models and non-streaming POST HTTP agents.
Advanced settings remain explicit original specs. Candidate submission is an
independently reviewed action with a per-workbench idempotency key and an explicit
new-attempt action. It does not automatically choose or promote a baseline.

Add `RunService.GetRecordEvidence` and trial-aware record associations to
`ListRunResults`, retaining the old response field. The selected record's output
and results use their existing primary keys. Locating its index scans the legacy
snapshot one row at a time, avoiding new migrations and preserving N-1 writers.
The response size is scoped to one record/trial, but lookup time is linear in
snapshot position. This is not a production-scale latency guarantee.

Keep investigation read-only. Resolve and authorize both runs before rendering
paired evidence; permission denial on either prevents both lanes. Missing or
retained-away evidence is unavailable. Ordinal event alignment is approximate,
preserves original order and makes no causal claim. Trial labels identify
executions, not coupled random seeds. All API values remain text under the
existing Content-Security-Policy.

Reuse the same protobuf policy inspection and decision engine as CLI/Action.
Editing run identities or policy fields revokes review. Downloaded specs and
policy pins are inputs to a new explicit full-suite execution, not signed
attestations, deployment authorization or deterministic remote replay.

## Consequences

The supported path needs no hand-written YAML or release-policy JSON. The existing
release contract still fails closed on missing coverage or incompatible evidence.
Live replay/fork, inherited policy administration, signed evidence and other
connectors remain separate work. Browser smoke tests, real-server/worker HTTP
controls and exact-trial/auth tests qualify this slice, not customer adoption.

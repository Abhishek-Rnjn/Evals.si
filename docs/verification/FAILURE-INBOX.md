# Production failure inbox verification

PROD-03 and the failure-review slice of PROD-05, checked on 19 October 2026,
Linux amd64, Python 3.12 and Go 1.26. The feature is unreleased; no package,
deployment or production incident was published or modified.

## Functional evidence

- Real Connect/gRPC and REST server checks cover project isolation, immutable
  source service/label conditions, filtered payload-free lists, separate triage
  and review roles, viewer/export/delete denials, reviewer attribution and
  mutation audit actions without payloads. A recapture check prevents changed
  source labels from exposing a previously restricted curated copy.
- Store/service tests cover concurrent deduplication, preservation of human
  edits on recapture, revision compare-and-swap, competing writers, transactional
  rollback when history insertion fails, source retention, review validation,
  missing/stale/unapproved selections, reserved metadata rejection, record size,
  event pagination and deletion of the case with its history. The store tests
  use the existing SQLite/PostgreSQL harness; PostgreSQL execution is delegated
  to the repository's CI service and is not claimed as a local check.
- Targeted Python CLI/client and real-server checks: **18 passed**. Invalid or
  duplicate selections and server rejections publish no artifact; existing
  files remain unchanged. The synthetic example logs through the real OTLP
  endpoint, edits/redacts, assigns and approves the case, verifies recapture,
  exports a pinned snapshot and checks provenance and private-marker omission.
  The observed bad output exits **3**; a scripted corrected HTTP target exits
  **0** under the same pinned dataset.
- A freshly built SDK wheel was installed in a clean environment outside the
  checkout, without harness/server extras. The complete synthetic example ran
  against the source server/worker with the same **3/0** gate controls. This
  verifies packaging and importer discovery; it is not a live-model test.
- All **6** real Chromium browser tests passed, including trace capture, case
  editing, review, JSONL download and approval revocation after editing. The
  suite also checks CSP/XSS handling, sign-in, annotation, keyboard navigation
  and WCAG checks in both palettes. No accessibility assertion was disabled.

## Broader checks and limits

Ruff lint/format, strict mypy (**283 source files**), protobuf lint/format and
generation checks passed. The website built **82 pages** and its local links
passed; the new guide is registered for website import/navigation. Race-enabled
tests for the inbox service/store passed.

`go vet ./...` and `go test ./...` passed, with the final offline fixture run
excluding external HTTP proxy variables. The initial source-guard fixture had
tried the session proxy for its synthetic hostname; it routes that hostname to
its own local server. A separate `go test -count=1 ./tests/e2e/ ./internal/source/`
passed with `EVALSI_E2E_WORKER` set to the source Python worker. That command
retains the existing optional backend/live-connector skips; it does not qualify
PostgreSQL, live integrations or unavailable sandbox rungs locally.

The full Python workspace reported **1,290 passed, 13 skipped, 2 failed**, with
one existing fork deprecation warning. Both failures are existing real-sandbox
tests (`test_code_pack.py::test_real_sandbox` and
`test_sandbox_client.py::test_a_sandbox_persists_across_commands`): this host
reported `bwrap: Creating new namespace failed: Resource temporarily unavailable`
and no available confined backend. No test or capability requirement was weakened.
Those failures are distinct from the passing real-server inbox workflow.
An isolated retry reproduced both failures with the same unavailable namespace
backend. The assertions and sandbox requirements remain intact.

The guide records the integrity/retention contract. The default policy permits
self-review; custom roles can separate permissions. History deliberately omits
old payloads/notes, and existing mutation audit is not a strict transactional
audit/outbox. Deletion/redaction does not erase source traces, previously
downloaded datasets, run results or backups. Exact source revision deduplication
does not establish semantic grouping. The synthetic fixture does not qualify
real-agent safety, enterprise operating capacity or production handling of
sensitive customer incidents.

## Review fixes

Failure edits reconstruct `Record.provenance` from the immutable captured trace,
discarding supplied trace/run identities and isolation claims. Tests cover omitted
provenance and forged trace/run provenance, preservation of the source digest,
the stored copy, protobuf export and browser JSONL. The real server test also
submits a forged run/VM claim and verifies that it cannot reach exported records.

Approval now checks contained values for row, tensor and media content rather
than the size of the outer wrapper. Empty variants in either input or reference,
tensors with descriptions but no data, and media without a nonblank URI fail
approval and remain unexportable. Five positive controls preserve valid row
features, zero/false predictions, tensor bytes and media URIs.

All 17 new negative cases fail against the previous service using a Go source
overlay; the working tree is not modified for that check. The corrected service,
server, store and authorization packages passed, as did Go vet, race-enabled
service tests, all 18 selected Python CLI/client/real-server tests, the 82-page
website build and local links. These focused checks are separate from the
earlier full-suite sandbox limitation above.

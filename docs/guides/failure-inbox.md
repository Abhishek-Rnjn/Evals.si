# Turn production failures into reviewed regression tests

The failure inbox connects ingested traces to the
[dataset snapshot workflow](dataset-snapshots.md). A team captures an incident,
assigns it, edits sensitive data and expected behavior, reviews a particular
revision, then exports approved cases into a pinned regression suite.

This feature is unreleased and requires matching source builds of the server
and SDK. It is absent from published 0.3.0. Capture and export do not invoke an
agent, replay tools or modify the production application.

## Review an incident in the browser

Open `/ui/` on your server and select the incident's project.

1. Open **Traces**, inspect a record and its online scores, then enter a title
   and reason and select **Capture for review**.
2. In **Failure inbox**, assign the case and enter the expected text response.
   Expand the complete record editor to redact fields or edit a structured
   reference. The editable copy includes the trajectory: removing a value from
   input alone does not remove copies in tool arguments or metadata.
3. **Submit for review** saves a new revision. Reviewers inspect the saved
   record and provide a note before approving or dismissing it. Approval
   requires a nonempty input and reference. An evaluator's output never becomes
   an approved expectation automatically.
4. Download an approved case as JSONL, or export selected approved revisions
   directly into a pinned snapshot through the CLI below.

The list shows status, assignee, revision and source without full record
payloads. Source links can expire when trace retention runs; the curated case
and captured source identity remain available. Cases are ordered by stable ID,
not incident time. History lists each revision's actor, decision and record
digest; it does not retain previous record bodies or free-text notes.

## Use the CLI and SDK

All inbox commands accept `--server`, `--project` and the usual credential
options. JSON output is suitable for an editor or automation:

```bash
evalsi inbox capture TRACE_ID --server https://evals.example.com --project support \
  --title 'Refund without approval' --reason 'Missing approval check'

evalsi inbox get CASE_ID --server https://evals.example.com --project support > case.json
# Edit record, title, reason and assignee. Set status to FAILURE_STATUS_IN_REVIEW.
evalsi inbox edit CASE_ID --revision 1 -f case.json \
  --server https://evals.example.com --project support

evalsi inbox review CASE_ID --revision 2 --decision approved \
  --note 'Expected behavior and redaction checked against support policy' \
  --server https://evals.example.com --project support

evalsi inbox history CASE_ID --server https://evals.example.com --project support
evalsi inbox list --status approved --assignee support-oncall \
  --server https://evals.example.com --project support
```

`edit` replaces all editable fields; start from `get` rather than a partial
object. Its record uses protobuf JSON, including `{ "text": "..." }` content
wrappers. The expected revision is required. Edits can only save an open draft
or submit for review; a separate review call controls approval/dismissal.

```bash
evalsi inbox export --server https://evals.example.com --project support \
  --case CASE_ID@3 --case ANOTHER_CASE_ID@7 \
  --name support-incidents --version v1 --owner support-team --split held-out \
  --tag approved -o support-incidents-v1.evalsi.json
```

Use the emitted `snapshot://...?...` URI, including its approved SHA-256, in
your EvalRun spec. Export requires 1–100 explicit unique case/revision pairs;
it fails the whole selection if any case is missing, hidden, stale or
unapproved. Large selections must fit the 2 MiB response limit. The source
records are read in one database statement. An export reflects approval at
that read; it cannot revoke files that were already downloaded.

Snapshot publication is atomic, owner-readable/writable, and never replaces an
existing path. A temporary private source is removed after publication or
failure. Exported rows retain the observed output and reviewed reference;
scoring the original failure should fail. A run with a configured target
generates a candidate output against the fixed reference. Exporting or opening
the artifact itself does not call that target.

The synchronous Python `Client` exposes `capture_failure`, `list_failures`,
`get_failure`, `update_failure`, `review_failure`, `failure_events`,
`export_failures` and `delete_failure`. List/history calls return one page;
follow `nextPageToken`, including after an empty filtered page. The CLI accepts
`--page-token` and `--page-size` for those commands.

## Identity, access and consistency

- Capture deduplicates an exact `(project, trace ID, source record SHA-256)`.
  Retries return the current curated case and preserve assignments, edits and
  decisions. Changed trace contents create another case; this is not semantic
  grouping of similar incidents.
- Updates, review and deletion use compare-and-swap revisions. Competing edits
  cannot silently overwrite one another; stale calls return `aborted`. Reload
  and inspect the current revision before retrying. Every edit clears approval
  and reviewer fields, including assignment-only changes.
- Export uses stable `failure/CASE_ID` record IDs and server-owned
  `metadata["evalsi.failure"]` provenance: project, case/revision, source trace
  and digest, reviewer/time and reviewed-record digest. CLI snapshots also
  retain this under each row's source provenance. Clients cannot supply the
  reserved `evalsi.failure` or `evalsi.dataset` metadata identities on edits.
  Source/reviewed-record digests use deterministic protobuf encoding; the
  exported snapshot has its separate canonical JSON digest.
- Reading requires both `failures.read` and `traces.read` against the captured
  source's project, service and labels. Capture/edit require `failures.write`,
  review requires `failures.review`, export additionally requires
  `datasets.write`, and deletion requires `failures.delete`. Source service and
  labels are immutable; editing a record cannot remove a permission boundary.
- Built-in viewers can read, editors can capture/edit/review/export, and admins
  can additionally delete. Custom roles can separate triage and review. The
  default policy allows self-review; independent approval is not enforced.
- Each change and its payload-free history event commit in one metadata-store
  transaction. SQLite and PostgreSQL use the same schema/operations. Cases live
  in the metadata store even if traces use ClickHouse. The existing audit gate
  records allowed mutations and denied calls without case payloads. This does
  not implement the proposed strict enterprise audit/outbox guarantee.
- Inbox operations deliberately bypass the generic `Idempotency-Key` response
  cache. Capture has its own deduplication, mutations have revision guards, and
  exports always recheck live approval. Old responses cannot restore a redacted
  payload or replay a revoked export from that cache.

Each captured/edited record is limited to 1 MiB. Cases and their history persist
until explicitly deleted with `evalsi inbox delete CASE_ID --revision N` by an
authorized operator. Deletion removes both current payload and case history;
it does not erase source traces, downloaded datasets, completed run results,
audit identity records or backups. Redaction likewise does not retroactively
edit those copies. Source IDs, names and access labels are retained as identity;
delete a case if that identity itself must be removed.
Deletion does not tombstone the source trace: another explicit capture can
create a new case. Assignees are free-text identifiers, without notifications
or a directory-membership guarantee.

## Runnable positive and negative controls

The [synthetic refund example](../../examples/failure-inbox/README.md) runs the
complete local flow, verifies redaction, exports a pinned dataset and asserts
that the observed failure exits 3 while a scripted corrected HTTP target exits
0. It exercises the product workflow without calling a paid model or a live
business tool. It does not establish real-agent business safety.

The current slice does not automatically collect policy failures, infer or
approve labels, send assignment notifications, provide semantic deduplication,
manage a central dataset catalog, enforce independent review, or implement
backup erasure/legal holds. The roadmap keeps those acceptance limits open.

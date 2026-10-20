# Shared, approved dataset versions

The shared dataset catalog lets a team publish a regression suite once, review
its exact content and start runs against an approved digest. It advances PROD-04
and connects the [failure inbox](failure-inbox.md) to recurring server runs.
This is **unreleased**: use the SDK, server and worker from this checkout.
Published 0.3.0 packages do not contain these commands or APIs.

## Publish, review and run

In the browser, open **Datasets → Publish a new dataset version**. Upload a JSON
array of standard records with explicit string IDs, enter a name/version and
owner, select tuning or held-out, and publish. Alternatively, open an approved
failure case and choose **Publish this case to a shared dataset**. That action
uses the exact approved case revision and preserves its source access boundaries.

The version starts as a draft. Inspect all record content, copy its digest, then
ask a reviewer to approve it with a note. With authentication enabled, publisher
and approver must be different principal IDs. Owners cannot bypass this check.
This separates identities; it does not prove that two different people control
the credentials. Anonymous self-approval is allowed only for no-auth development.

After approval, **Start pinned regression run** scores recorded outputs by
default. Selecting model generation sends inputs to the specified model endpoint
using the server's granted credentials. The starter uses exact-match mean ≥ 1
and error rate ≤ 0; configure your own evaluators and gates through a run spec
for more complex tasks. Scoring recorded outputs makes no target calls, but
configured evaluators in other run specs may call external services.

A CLI publisher can use JSON/JSONL with standard fields:

```json
{"id":"refund-approval","input":"Refund a pending order","output":"Approval required","reference":"Approval required"}
```

```bash
evalsi dataset publish --server https://evals.example.com --project support \
  --name refunds --version v1 --owner support-team --split held-out \
  --tag regression --label classification=internal --records cases.jsonl

evalsi dataset versions --server https://evals.example.com --project support \
  --name refunds --state draft

evalsi dataset version refunds v1 --server https://evals.example.com --project support
```

Copy the returned `dataset.sha256` only after reviewing the content. A reviewer
uses their own credential, the reviewed pin and the current revision:

```bash
evalsi dataset review-version refunds v1 --server https://evals.example.com \
  --project support --sha256 "$REVIEWED_CATALOG_SHA256" --revision 1 \
  --decision approved --note 'Expected responses and source handling checked'
```

Store the approved reference in a run spec. There is no mutable `latest` alias:

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
spec:
  dataset:
    catalog:
      project: support
      name: refunds
      version: v1
      sha256: REPLACE_WITH_REVIEWED_CATALOG_SHA256
  evaluators: [{ref: exact-match}]
  gates:
    - {metric: exact-match, min: 1}
    - {metric: exact-match, stat: GATE_STAT_ERROR_RATE, max: 0}
```

```bash
evalsi run -f regression.yaml --server https://evals.example.com
```

Catalog loading requires a server. An empty reference project uses the run's
project. New runs require `runs.create`, dataset read access and approval, as well
as existing evaluator, source and credential permissions. Mapping is rejected;
a positive `dataset.limit` explicitly selects a prefix without changing the pin
of the full source suite. Omitting the limit uses every record. `ImportRun`
rejects catalog references: importing results does not attest that the server
executed an approved suite.

## Failure inbox, portable snapshots and diffs

Publish up to 100 explicitly selected approved failure revisions together:

```bash
evalsi dataset publish --server https://evals.example.com --project support \
  --name refunds --version v2 --owner support-team \
  --failure "$CASE_ID@$REVIEWED_CASE_REVISION"
```

Each selection is rechecked for approval, revision and source access. A changed
case requires re-review. Published records are independent copies: later edits
or deletion of the inbox case do not change the shared version.

Publish a portable snapshot with `--snapshot FILE --sha256 "$PORTABLE_SHA256"`
instead of `--records` or `--failure`. Download a catalog version for local runs:

```bash
evalsi dataset fetch-version refunds v1 --server https://evals.example.com \
  --project support --sha256 "$REVIEWED_CATALOG_SHA256" -o refunds-v1.evalsi.json
```

The destination must be new. The resulting portable snapshot records the catalog
reference, decision identity and wire-record source provenance. Use its **new
portable SHA-256** for `snapshot://` runs or the existing overlap checker. Portable
and catalog digests use different formats and are not interchangeable. Reading
or downloading a draft or retired version is permitted for authorized reviewers;
only approved versions can start new catalog runs.

Review changes between explicitly pinned versions:

```bash
evalsi dataset version-diff refunds v1 v2 --server https://evals.example.com \
  --project support --before-sha256 "$V1_SHA256" --after-sha256 "$V2_SHA256"
evalsi dataset version-history refunds v1 --server https://evals.example.com --project support
```

Diff output reports added/removed IDs, changed record field names, record order,
manifest changes and unchanged record count without copying raw payloads. Version
identity itself is immutable manifest content, so two different version names
have different pins even when records match. `version-diff --fail-on-change`
exits 3 when the pins differ; ordinary inspection exits 0. List/history commands
return cursors with `--page-token`; default page size is 20, maximum 50. The
browser comparison picker shows the first page of versions for the dataset;
use the CLI for other versions.

## Integrity, approval and permissions

The catalog stores content in the configured SQLite or PostgreSQL metadata
store. Publication and its first history event are one transaction. A name/version
can never be overwritten, even by identical content. Review uses optimistic
revision checks, and state/history update together. Retries must reload current
state; responses are not replayed from the generic idempotency cache.

The pin is `SHA256("evalsi.dataset-catalog/v1\0" + deterministic protobuf bytes
of DatasetVersionContent)`, where `\0` denotes one NUL byte. It covers project,
name, version, owner, split, sorted tags, description, labels, ordered records,
provenance and captured source access summaries. State, review note, actor and
timestamps are stored separately. The server recomputes the digest on read and
checks the independently reviewed pin before admission. JSON object keys in
catalog records are sorted at Python worker conversion; array order and raw
string tool arguments retain their meaning. This is the v1 server serialization
contract, not a cross-format canonical JSON standard or a cryptographic signature.

| Permission | Default role | Use |
|---|---|---|
| `datasets.read` | viewer and roles inheriting it | List, inspect, diff, history, download and use versions |
| `datasets.write` | editor and roles inheriting it | Publish immutable drafts |
| `datasets.review` | admin and owner | Approve or retire a version |

Custom CEL roles can restrict `resource.dataset.name`, `.version`, `.owner`,
`.split`, `.tags` and `resource.labels`. Existing run attributes also expose
`resource.dataset.catalog` when a reference is present. Owners are descriptive
metadata, not automatic role grants. Existing run-create roles do not implicitly
gain dataset read permission.

Inbox-derived versions retain the immutable source `TraceSummary` boundaries.
Every read, list, diff, review and new run rechecks `traces.read` against those
summaries, including after source retention. Access is to the whole version;
there is no partial read that drops unauthorized cases. Direct record/snapshot
imports are publisher-supplied data and provenance, not trusted execution
attestation. Authorized publishers must follow their data-handling policy;
this feature is not a DLP or declassification boundary for direct imports.

## Retirement and operating limits

Retire a draft or approved version with the current pin and revision:

```bash
evalsi dataset review-version refunds v1 --server https://evals.example.com \
  --project support --sha256 "$REVIEWED_CATALOG_SHA256" --revision 2 \
  --decision retired --note 'Superseded by reviewed v2'
```

Retirement is terminal and blocks subsequent admissions. A concurrent run that
already passed admission retains its copied records; retirement does not cancel
it. Completed run records retain `metadata["evalsi.dataset-version"]`, including
pin, owner/split, reviewer, review time and approval revision. Retiring the source
does not erase run results or downloaded copies.

The initial catalog supports 1–10,000 records and at most 2 MiB of protobuf
content per version; the browser accepts JSON source files up to 4 MiB before
server validation. It is for curated regression suites, not bulk trace storage.
Tags and access labels are each limited to 32. There is no delete/erasure API,
shared ownership administration, automatic publication, signed approval,
legal hold or large-object storage tier. Version names remain reserved after
retirement. History stores decision identity, pin, revision and time, omitting
old payloads and review notes; the latest note is on the version. Existing API
audit settings still apply and do not constitute a transactional audit outbox.
Plan sensitive-data retention before importing customer records.

See the [synthetic end-to-end example](../../examples/dataset-catalog/README.md)
and [verification record](../verification/DATASET-CATALOG.md). Customer-incident
validation, enterprise operating capacity and broader governance remain open in
the [product roadmap](../PRODUCT-ROADMAP.md).

# Reviewable, pinned dataset snapshots

This is the first PROD-04 dataset-lifecycle slice. It is **unreleased**: install
the SDK from this checkout as described in [guided setup](onboarding.md). Published
0.3.0 does not include the `snapshot://` importer. The maintained GitHub Action
must use a compatible installation (`install: 'false'` after installing this
checkout), or a future published version that includes it; its default 0.3.0
installation cannot load snapshots.

A snapshot is one portable JSON file containing a named version, explicit record
IDs, an owner, tags, a held-out/tuning label, source-byte digest, original records
and their provenance. Its canonical SHA-256 covers all of that content. A run
pins the approved digest and rejects changed content **before creating the target
or running evaluators**. Local runs and worker dataset loading use the same check.

## Create and verify a version

Start with approved local JSONL or a JSON array using standard evaluation fields.
Every record must have a stable nonempty string `id`; duplicate/generated IDs are
rejected. For example:

```json
{"id":"refund-unauthorized","input":"An outsider requests a refund","output":"deny","reference":"deny","provenance":{"kind":"synthetic","source":"tutorial"}}
```

```bash
mkdir -p datasets
evalsi dataset snapshot cases.jsonl --name support-refunds --version v1 \
  --owner support-ai --split held-out --tag regression \
  --output datasets/support-v1.evalsi.json
evalsi dataset inspect datasets/support-v1.evalsi.json --format json
evalsi dataset verify datasets/support-v1.evalsi.json --sha256 APPROVED_DIGEST
```

Copy the 64-character lowercase digest printed by `snapshot` into the reviewable
run spec; replace `APPROVED_DIGEST` below. Paths resolve relative to the spec.

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: support-regression
spec:
  dataset:
    uri: snapshot://datasets/support-v1.evalsi.json?sha256=APPROVED_DIGEST
  evaluators:
    - ref: exact-match
  gates:
    - metric: exact-match
      min: 1
    - metric: exact-match
      stat: GATE_STAT_ERROR_RATE
      max: 0
```

Omitting the pin, changing records or manifest fields, or supplying unsupported URI
options fails loading. Even a replacement snapshot with a recomputed internal hash
fails the existing external pin. `inspect` validates internal consistency; only
`verify --sha256` or the pinned importer checks identity against an independently
approved value. A hash does not establish who approved it. Keep the trusted pin in
your reviewed spec or another trusted record; protect that record with your
existing permissions/review process.

`metadata["evalsi.dataset"]` in loaded records retains the snapshot identity and
each record's source provenance, including through protobuf worker transport and
stored server results. That key is reserved; source records containing it are
rejected. Other source fields are preserved in the bundle and processed by the
normal record loader. The source path is not stored. No timestamp is inserted,
so identical source bytes and options reproduce the same snapshot digest.

## Review changes by ID

Create a new file/version after changing the source, then inspect the diff:

```bash
evalsi dataset diff datasets/support-v1.evalsi.json datasets/support-v2.evalsi.json
evalsi dataset diff datasets/support-v1.evalsi.json datasets/support-v2.evalsi.json \
  --format json --fail-on-change
```

The diff lists added/removed IDs, changed field names and item hashes, metadata
changes and record-order changes. It omits raw inputs, outputs, references and
provenance values. Table output shows at most 20 changed IDs; JSON lists all.
IDs and field names can themselves be sensitive. Exit 0 means success; with
`--fail-on-change`, any snapshot identity difference exits 3. Invalid data or
failed verification exits 1. A source-formatting change alters `source_sha256`
and snapshot identity even if every record is unchanged; the diff distinguishes
that metadata change from item changes.

The [runnable example](../../examples/dataset-snapshots/README.md) includes a passing
version and a deliberately bad recorded refund decision caught by its gate.
Changing the test set invalidates existing paired baselines; review the new
dataset separately from candidate-agent improvements.

## Server operation and limits

For filesystem-backed server runs, an operator can place the complete bundle
under the configured `datasets_dir`. Use `uploaded/<project>/<name>/...` so existing
project access checks recognize its project. A run uses a relative URI such as
`snapshot://uploaded/support/refunds/v1.evalsi.json?sha256=...`. The worker needs
this SDK/importer and filesystem access to the resolved artifact, as with other
local importers. Existing server path and symlink confinement still apply.

The normal JSONL/Parquet upload API normalizes records and **does not preserve a
snapshot envelope**; do not upload the bundle using that API. S3-backed snapshot
URIs, a snapshot upload endpoint, central catalog and version-name lookup are
future work. Existing server authorization is reused; this PR adds no new roles
or approval mechanism.

Creation publishes atomically without replacing an existing file, directory or
symlink, and refuses symlinked output ancestors. The destination directory must
already exist. Temporary/output files use owner-only permissions where supported;
publication requires filesystem hard-link support. Input and encoded artifact
sizes are limited to 64 MiB, and malformed/duplicate-key/non-finite JSON is refused.
Snapshot files remain editable by their filesystem owner; verified pins detect
changes, rather than making the filesystem immutable. A format-v1 envelope is
self-contained and never follows a record's provenance references or fetches a
remote source during snapshot creation/loading.

Held-out/tuning is a purpose label, not enforcement against training leakage or
proof that two versions have disjoint records. Name/version uniqueness is local
to the selected output file, not globally governed. Signatures, approvals/history,
shared ownership administration, retention/deletion and a central immutable-version
registry remain open in the [product roadmap](../PRODUCT-ROADMAP.md).

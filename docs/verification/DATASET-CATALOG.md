# Shared dataset catalog verification

Unreleased PROD-04/PROD-05 slice and ENT-05 foundation, verified on Linux amd64
on 20 October 2026. This covers synthetic data and source builds; it does not
certify real-agent safety, customer usability or enterprise operating scale.

## Contract exercised

The metadata store reserves `(project, name, version)` at publication, persisting
immutable content and a draft event in one transaction. Review changes only
state/decision metadata and atomically appends an event under revision CAS.
A distinct authenticated principal must approve the exact pin. Retirement is
terminal; subsequent admissions are blocked while admitted runs keep their copies.
The content pin uses domain-separated deterministic protobuf, independently of
portable snapshot canonical JSON pins.

Tests include:

- Competing publications produce one winner; competing reviews produce one
  winner and one conflict. A forced history write failure rolls back review.
- SQLite reopen persistence, cursor/filter/project isolation and content hash
  verification, including detection of a stored body altered without its pin.
- Draft, wrong/missing pins, stale revisions, duplicate/empty IDs, invalid
  manifests, forged access sources and reserved identity metadata rejection.
- Separate authenticated principals, editor/reviewer/viewer/runner permissions,
  project isolation, conditional access labels and source trace permission
  checks for inbox-derived versions. Source retention does not remove the
  captured boundary or the published records.
- Actual server admission fails before generation/evaluation for a draft,
  wrong pin or mapping; approved runs keep catalog identity and retirement
  prevents subsequent admission without deleting old records. Imported results
  cannot claim catalog execution through their dataset source.
- CLI/SDK explicit failure revision/pin propagation, no-overwrite portable
  download and stable catalog JSON execution key order, preserving arrays and
  string tool arguments.
- Real Chromium publication, inspect/review, pinned run and retirement; an
  approved inbox case publishes directly into a shared draft. Keyboard, light
  and dark palette accessibility checks include the dataset screens.
- A real-server synthetic incident-to-regression example redacts a source
  marker, publishes its reviewed case, checks draft/wrong-pin/retired rejection,
  fails recorded bad output and passes a scripted HTTP candidate. It asserts
  actual target calls, retained run identity and a portable CLI download with
  source/catalog provenance. No paid model or live business tool is used.

## Reproduce

From the repository root, with development tools installed:

```bash
go vet ./...
go test ./...
go test -race ./internal/datasetcatalog ./internal/store ./internal/server ./internal/runs
go build -o /tmp/evalsi-catalog-evalsid ./cmd/evalsid
cd python
uv run ruff check .
uv run ruff format --check .
uv run mypy
EVALSI_E2E_EVALSID=/tmp/evalsi-catalog-evalsid \
EVALSI_TEST_EVALSID=/tmp/evalsi-catalog-evalsid EVALSI_REQUIRE_EVALSID=1 uv run pytest -q
```

Local full Go tests and vet passed, as did Python lint/format and strict mypy
(293 source files). The Python suite reported **1,332 passed, 15 skipped** and
two existing sandbox failures (`test_real_sandbox` and
`test_a_sandbox_persists_across_commands`): this cloud host cannot create the
bubblewrap namespace and has no available confined fallback. Assertions were
not changed. New catalog tests, including the real-server example, passed. Race checks passed
for the catalog, store, server and run packages. All **7 browser tests** passed
with system Chromium, including automated accessibility checks. A newly built
SDK-only wheel installed into a clean environment outside the checkout completed
the same synthetic flow against a source server and worker.

The `internal/store` tests also run against the PostgreSQL fixture in CI. Local
verification uses SQLite. See the [example](../../examples/dataset-catalog/README.md)
for the manual server/SDK flow and the [UI guide](../guides/web-ui.md#testing-it)
for browser prerequisites. The website imports the new guide and roadmap from
the repository; its production build and local links passed.

## Limits

Direct imports and their provenance are publisher-supplied; only the approved
inbox path captures server-owned source boundaries. Publishers remain responsible
for data handling. Approval history is not a signed attestation or transactional
audit outbox. Distinct principal IDs do not establish distinct humans. Retirement
is not erasure: catalog data, runs, sources, downloads and backups have separate
lifecycles. There is no catalog deletion API in this slice. See the
[guide](../guides/dataset-catalog.md) for limits and the
[roadmap](../PRODUCT-ROADMAP.md) for remaining acceptance work.

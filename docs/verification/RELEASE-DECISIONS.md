# Release-decision verification

Unreleased PROD-02/PROD-05 implementation and ENT-08 foundation, exercised on
Linux amd64 on 20 October 2026 with source server/SDK/worker builds. The workflow
is **production failure → approved regression suite → candidate comparison →
release decision**. Synthetic controls do not establish real-agent safety,
customer usability, statistical power or enterprise operating capacity.

## Controls

- Real trace ingestion captures two synthetic failures. Each is redacted,
  assigned an explicit expected response and reviewed before publishing its exact
  revision. The shared held-out version is approved and pinned in every run.
- Recorded bad responses fail ordinary run gates. A scripted baseline and one
  explicit candidate per attempt exercise passing, blocked and insufficient
  outcomes through both `evalsi release gate` and the real Action adapter.
  Exit codes are asserted as 0/3/1. HTTP fixture calls prove real target
  execution; CI summaries omit private source markers and case identities.
- The Go tests cover confidence-bound regression checks, lower-is-better usage,
  critical failures hidden by a passing mean, missing critical cases, insufficient
  records, missing usage, stale run/policy/commit pins, imported/old/unfinished
  runs, changed contracts, errored scores, retirement and changed output references.
  Trials are averaged within records rather than counted as independent samples.
- Captured evaluator/judge/summary/trial/checker identities are checked before
  resumed work. A changed contract stops work without new evaluator calls.
- Store tests exercise competing immutable writes, deterministic map hashing,
  project isolation and report/access-envelope tampering. Decisions remain
  available after source-run deletion and retain captured access boundaries.
- Authenticated server tests cover viewer/runner/cross-project restrictions,
  inspection, creation, saved reads, retention and filtered listings. The REST
  routing suite checks every release endpoint. History/summary cursors reveal
  only authorized returned identities.
- SDK tests check policy and decision hashes, private no-overwrite portable
  reports, tampering, malformed report shapes, explicit pins and exit propagation.
  The existing Action tests exercise embedded/advisory backward compatibility.
- Real Chromium tests cover dataset publication/approval, two pinned runs,
  identity/policy inspection, edit-revoked review, saved PASS report download,
  retirement and historical retrieval. CSP, keyboard focus, light/dark palette
  WCAG checks and the existing authentication/inbox workflows remain covered.

## Reproduce

From the repository root with the development environment installed:

```bash
go vet ./...
go test ./...
go test -race ./internal/runs ./internal/store ./internal/server \
  -run 'Release|CriticalCase|ChangedEvaluation' -count=1
go build -o /tmp/evalsi-release-evalsid ./cmd/evalsid
cd python
uv run ruff check .
uv run ruff format --check .
uv run mypy
EVALSI_E2E_EVALSID=/tmp/evalsi-release-evalsid \
EVALSI_TEST_EVALSID=/tmp/evalsi-release-evalsid EVALSI_REQUIRE_EVALSID=1 uv run pytest -q
```

The [example README](../../examples/release-decisions/README.md) gives standalone
server and demo commands. The [UI guide](../guides/web-ui.md#testing-it) covers
browser prerequisites. The store harness runs with PostgreSQL in GitHub CI;
local checks use SQLite. The website imports the guide and roadmap from the
repository.

## Limits

These are saved policy decisions, not signed execution attestations or a complete
independently recomputable evidence package. Full recomputation needs original
run data. Commit/model names and image tags are declared metadata; remote weights
and worker/plugin binary bytes are not attested. The store and portable report
hashes detect changes relative to trusted pins, not an administrator replacing
both data and hashes. Baseline/policy approval and deployment authorization
remain the customer's responsibility.

Decisions retain policy and case identities/values, actor, source-access metadata
and evidence hashes independently of source retention. There is no release-report
or catalog erasure API in this slice. Existing API auditing remains separate from
transactional audit/outbox work under ENT-03/04. The [guide](../guides/release-decisions.md)
records operating limits and the remaining scope; the
[roadmap](../PRODUCT-ROADMAP.md) does not mark ENT-08 complete.

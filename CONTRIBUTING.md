# Contributing to Evals.si

Thank you for helping. Issues and pull requests are welcome. For anything larger than a fix, open an issue first so we can agree on the shape of the change.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Report security problems privately, as described in [SECURITY.md](SECURITY.md), not in an issue.

## Where things live

| Path | What it is |
|------|------------|
| `proto/` | The API's single source of truth (Connect, gRPC, REST, CRDs). Change it first, then run `make proto` |
| `cmd/evalsid`, `internal/` | The Go server: API, runs, ingest, policy engine (`internal/watch`), sandbox, auth, sinks |
| `operator/` | The Kubernetes operator: `EvalRun`, `OnlineEvalPolicy`, `Evaluator`, `SandboxClass`. Run `make operator-gen` after API changes |
| `python/evalsi/` | The Python package: CLI, SDK, evaluator packs, worker, rewards, training hooks |
| `python/adapters/` | Framework and benchmark adapters, each in its own environment |
| `deploy/helm/` | Charts: `evalsi`, `evalsi-crds`, `evalsi-sandboxd`. Chart tests are in `tests/helm` |
| `deploy/e2e/kind-e2e.sh` | The kind end-to-end job CI runs |
| `tests/ui` | A browser test of the web UI (Playwright, the real `evalsid`) |
| `examples/` | Run specs, policies and configs used by the docs and tests |
| `website/` | The documentation site (Astro Starlight), generated from the docs |

## Which document wins

When sources disagree, trust them in this order:

1. The code and its tests: the current state.
2. [docs/PRD.md](docs/PRD.md): the target. Requirement IDs (D*, P*, S*, E*, A*, R*) and milestones M1–M5 are defined there.
3. [docs/decisions/](docs/decisions/README.md): why things are the way they are. Write a new record, in the same format, for a significant design choice.
4. [docs/DESIGN.md](docs/DESIGN.md): the architecture.
5. [docs/guides/](docs/guides/) and the [README](README.md): user-facing behaviour. Keep them true when behaviour changes.

Verification findings are named VR-F1…, VR-D1…, VR-R1… so they don't clash with the PRD's D1–D11. [docs/LEFTOVERS.md](docs/LEFTOVERS.md) lists what is deferred or has never been verified.

## Before you push

Run what CI runs (`.github/workflows/ci.yml`) and make it pass:

- `make check`: gofmt, go vet, go test, ruff, ruff format --check, mypy, pytest, buf lint and buf format. CI also runs `go test -race ./...`.
- `make adapters-check` when you touch `python/adapters/`.
- `make e2e` for server and worker changes.
- `deploy/e2e/kind-e2e.sh` for chart, operator and demo changes, when Docker is available. Say so in the pull request when you could not run it. CI's `kubernetes` job is skipped on pull requests: a maintainer can dispatch `ci` on your branch (`workflow_dispatch`) to run it.
- `EVALSI_TEST_POSTGRES_DSN=... go test -race ./internal/store/...` for store changes that touch locking, transactions or concurrency; SQLite cannot show those races.
- `npm test` in `tests/ui` when you touch `internal/webui`.

Never skip, disable or weaken a test to get green.

## Conventions

- Match the surrounding code: Go core, Python runtime (decision 0002).
- Sandboxes fail closed: never run code with weaker isolation than asked, and never claim isolation that isn't enforced.
- There is no default judge model; nothing is billed by surprise. Records missing what an evaluator needs are skipped, and evaluator failures are errors, never zeros.
- Charts must work on any conformant cluster. Every pod and Service a chart adds takes tolerations, nodeSelector, labels and the image registry as values (see "Taints and service meshes" in [docs/guides/kubernetes.md](docs/guides/kubernetes.md)).
- No secrets, API keys, internal hostnames or private registry names in commits.
- When something lands or is deferred, update the PRD's status cells, LEFTOVERS.md, docs/verification/README.md and the README.

## Schema changes

The store records each schema step it applies in `schema_migrations` (`internal/store/migrations.go`). Change the schema by expanding, then contracting:

- **Expand.** A new table, column or index can go in a schema constant with `IF NOT EXISTS` (or in `addedColumns`), and reaches existing databases on the next open. A backfill or any other one-off change is a new step at the end of `migrations`. Either way, the previous release can still read the database, so N and N-1 can run side by side during a rolling upgrade, and N-1 can be rolled back to without a restore.
- **Contract.** Dropping, renaming or rewriting anything the previous release reads is a step with `contract: true`. Ship it at least one release after the code stopped needing the old shape. It raises the database's minimum reader, so an older `evalsid` refuses to open the database instead of misreading it. Rolling back past a contract step means restoring a backup taken before the upgrade ([operations guide](docs/guides/operations.md)).
- Never edit, reorder or renumber a released step. `TestThePreviousReleaseReadsAnExpandedSchemaButNotAContractedOne` checks the N-1 rule.

## Pull requests

- Keep a pull request to one change, and say what you ran and what you could not run.
- Add or update tests with the change. A fix for a bug comes with a test that fails without it.
- Describe user-visible changes under "Unreleased" in [CHANGELOG.md](CHANGELOG.md).

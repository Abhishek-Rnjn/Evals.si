# Browser release-workbench verification

Unreleased PROD-02/PROD-05 slice, tested on Linux amd64 on 21 October 2026.
The browser serves real static files under the existing CSP, a real evalsid and
Python worker execute the runs, and a local scripted HTTP agent supplies the
positive/negative controls. These are synthetic workflow controls, not customer
usability evidence, hosted-agent safety certification or a scale benchmark.

## Controls

- Browser configuration selects an approved held-out suite, three pinned
  evaluators, an HTTP agent and two trials. Candidate admission and explicit
  configuration review precede execution. An unchanged submission returns the
  same run without additional HTTP-agent calls; an explicit new attempt runs anew.
- Baseline settings are copied, the candidate endpoint/commit are changed, and
  the browser builds a critical tool-error policy through forms. Catalog direction
  metadata selects lower-is-better for tool-error rate. A deliberately failed tool
  and incomplete agent stop cause BLOCK; a corrected explicit attempt passes.
  Raising the required independent records beyond suite size gives INSUFFICIENT.
- Editing policy content or a run identity revokes inspection/consent. Downloaded
  candidate JSON and policy retain their catalog pins. The existing Python CLI
  computes the same policy digest and executes the exported full-suite check;
  the maintained Action adapter consumes the same exports. Both block the bad agent.
- The paired inspector opens the exact changed record/trial, displays tool
  arguments/results, error and stop reason, and permits independent trial
  selection. Differing trial outputs cannot overwrite one another. A deep link
  with reserved characters reaches a record beyond normal first-page limits
  without searching result pages. Permission denial hides both private lanes.
- Store tests locate record 600 and retrieve distinct outputs/trajectories/scores
  for two trials even with a corrupt unrelated output. No unrelated output is
  decoded. Manager tests cover additive trial associations, missing records,
  invalid trial bounds and archived evidence. There are no schema or write-path
  changes; lookup streams legacy snapshot rows and queries selected primary keys.
- Authenticated Connect, gRPC and REST controls permit a project viewer and deny
  another project's viewer. A newly issued reader key loses access immediately
  after revocation. REST routing and both JavaScript files' text-only rendering
  constraints are tested.
- Existing sign-in, annotation, inbox, dataset approval and release browser flows
  remain in the browser suite. The workbench participates in keyboard navigation
  and light/dark axe scans. The case inspector has its own accessibility scan.

## Reproduce

With the repository development prerequisites installed:

```bash
make check
go test -race ./internal/runs ./internal/store ./internal/server ./internal/webui
make operator-gen
make e2e
go build -o bin/evalsid ./cmd/evalsid
EVALSID="$PWD/bin/evalsid" \
EVALSI_UI_WORKER="$PWD/python/.venv/bin/python -m evalsi" \
CHROMIUM_PATH=/usr/bin/chromium npm --prefix tests/ui test
cd website && npm ci && npm run build && node scripts/check-links.mjs
```

`CHROMIUM_PATH` is optional when Playwright's browser is installed. The Go store
test harness also supports `EVALSI_TEST_POSTGRES_DSN`; the new store operation is
read-only and adds no locking, concurrency or migration behavior.

## Limits

The configuration form covers recorded outputs, basic OpenAI-compatible models
and non-streaming POST HTTP agents. It refuses copying advanced settings it cannot
represent rather than dropping them. Complex evaluator parameters retain an
optional JSON editor. Other connectors, advanced harness/checker configuration,
live replay/fork, inherited standards, signed attestations and pilot usability
remain open.

Ordinal event alignment is approximate and does not establish causation; equal
trial numbers do not establish equal seeds. Missing telemetry is unavailable.
The snapshot lookup has linear time in record position, bounded memory and no new
index. The workbench's idempotency key lives in that page instance; a new navigation
must recover existing run IDs explicitly or begin a new attempt. Exported Action
steps require a reviewed matching source SHA and preinstalled matching packages.
No GitHub-hosted Action execution or live external model is claimed by these checks.

## Recorded checks

- Full browser suite: **12 passed**, including all three run modes, real HTTP
  execution, exported CLI/Action checks and PASS/BLOCK/INSUFFICIENT decisions.
  The final inspector changes also passed the focused browser controls.
- Python: Ruff, formatting and strict mypy passed; **1,367 passed, 19 skipped**.
- Protobuf lint, formatting and Go/Python regeneration passed. Operator/CRD
  regeneration completed without manifest changes. Go formatting and vet passed.
- Website: **86 pages**, build and local link check passed.
- Initial repository-wide checks required the normal `0022` umask and local-only
  `NO_PROXY` entries for the guard tests' mocked `evil.example.com` and
  `store.example.com` hosts; inherited proxy settings remained in place.
- `make check` reached the Go suite but failed two sandbox tests under process
  pressure (`TestSnapshotAndRestore/bwrap`, `TestEgressAllowlist/bwrap`). Both also
  failed on untouched main at `314660e` in a separate worktree, with fork/thread
  creation failures. No sandbox requirement or assertion was relaxed.
- `make e2e` ran with real source workers and failed its existing agent/code/reward
  sandbox controls because confined isolation was unavailable; the source package
  passed. These are not successful sandbox verification runs.
- The first full Go race run found an existing unsynchronized fake clock in quota
  tests. The test clock now uses atomic loads/stores; production quota behavior and
  the assertions are unchanged.
- After synchronizing the quota-test clock, `go test -race -count=1` passed all
  tests in `internal/runs`, `internal/store`, `internal/server` and
  `internal/webui` (89.8 s, 108.9 s, 105.4 s and 1.0 s respectively).

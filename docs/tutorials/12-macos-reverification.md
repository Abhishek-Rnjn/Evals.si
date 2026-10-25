# Re-verifying both onboarding PRs on a Mac

This follow-up was performed on 25 October 2026 on the repository owner's
Apple Silicon Mac, not the Linux cloud host used in the original studies.
It checked both PR heads before combining the tutorial material locally:

| PR | Reviewed head | Scope |
|---|---|---|
| #154 | `28d6b5fd7b2632725929e9bd309a88cd098afed8` | Five-lesson mock-tool course, seven offline controls, two source SQL controls |
| #155 | `9aa4c4754822411aff4cb66867056e2abf49746d` | TinyLM sandbox course and runtime fixes |

The combined follow-up keeps both courses and resolves their competing tutorial
indexes. It does not merge or update either GitHub PR.

## Environment and method

- Python 3.12.13 on macOS arm64, with separate fresh virtual environments for
  published packages and each PR's source workspace.
- Published `evalsi==0.3.1` and `evalsi-harness==0.3.1`; source package version
  `0.3.1.dev0`. `uv pip check` accepted the published environment.
- Go 1.26.10, Docker Desktop engine 28.5.1, kind 0.31.0, Kubernetes 1.35.0,
  native kubectl 1.36.0 and Helm 4.2.4.
- The interactive zsh configuration supplied the endpoint, auth token and
  `ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-5-5`. No credentials or private
  endpoint names are part of this report.
- An isolated kind cluster, `evalsi-onboarding-2286`, used locally built
  source images. Existing clusters were not used for the verification.
  After the checks, the user selected cleanup: that test cluster and its
  generated negative-control files were removed; the five existing kind
  cluster registrations remained.
- Linux sandbox scenarios ran in privileged throwaway Docker containers
  with only the tutorial directory mounted read-only. This local test setup
  is not a recommendation to run a production worker privileged.

## Results

| Check | Observed result |
|---|---|
| PR #154 published `verify.py` | All 7 cases passed, including expected gate failures and replay with an unreachable model |
| PR #154 source `verify.py --database` | All 9 cases passed; regressed final SQL state failed its gate |
| PR #155 published Linux TinyLM matrix | All 18 scenarios returned the expected 0/3/1 exit statuses |
| PR #155 source Linux TinyLM matrix | All 18 scenarios returned the expected exit statuses |
| Strengthened TinyLM verifier, source and published | All 18 cases passed again on each install, now checking exact trial-record counts and zero evaluator errors for completed runs |
| PR #155 runtime regression tests | 83 passed |
| New live-run configuration, failed-start and Kubernetes spec tests | 10 passed; combined with the expanded runtime regressions: 94 passed |
| Stable Python 3.14.4 CLI tests | 20 passed, including large and small closed-stdout controls |
| Source mypy | No issues in 213 source files |
| Python lint and formatting | Ruff passed; 326 files already formatted |
| Website build and internal links | 119 pages built; link checker passed, including the locally served tutorial screenshots |
| Version-stamped Linux image | Strict `evalsi doctor --require agent` passed with matching `0.3.1.dev0` components and bwrap namespaced isolation |
| Real server comparison | Comparing a recorded run with itself returned normalized `diffCi.low=diffCi.high=0` through the PR #155 Python client |
| Real Haiku, published packages | Non-streaming, streaming/record and offline replay: all 3 cases passed |
| Real Haiku, PR #154 source | The same 3 cases plus SQL state: all 4 passed |
| Real Haiku, PR #155 source | The same 3 cases plus SQL state: all 4 passed |
| kind recorded positive/negative | Mean exact-match 0.5, n=2; gate at 0.5 succeeded, gate at 1 failed |
| kind pod-agent positive/negative | Task-success 1/0, n=1; phases Succeeded/Failed |
| kind streamed TinyLM positive/negative | Recall 1/0.5, precision 1, tool-errors 0, run-completed 1; phases Succeeded/Failed |

The live matrix contains **24 fresh synthetic model trajectories and 9 offline
replayed trajectories** across the three installations. For the mock-business
cases, precision, recall and run-completed were 1 and tool-errors 0. Each SQL
case also reported task-success=contains=1 with zero evaluator errors. The
configured provider accepted both a small native `x-api-key` probe and a bearer
probe; the actual harness runs used its existing `x-api-key` transport with
`api_key_env: ANTHROPIC_AUTH_TOKEN`.

No provider invoice or exact total spend was collected. These are bounded
smoke tests, not a statistically meaningful reliability study or evidence of
payment settlement.

## Reproduction commands

In an activated environment, from the repository root:

```bash
# Published packages, then source in its own environment:
python examples/first-time-onboarding/verify.py
python examples/first-time-onboarding/verify.py --database  # source only

# Real provider, explicit opt-in; environment selects Haiku and credentials:
python examples/first-time-onboarding/verify_live.py --allow-paid
python examples/first-time-onboarding/verify_live.py --allow-paid --database  # source only

# Linux sandbox; run inside a supported Linux environment:
bash examples/tinylm/check.sh

# Existing runtime regression tests and new offline example tests:
cd python
.venv/bin/pytest evalsi/tests/test_cli.py evalsi/tests/test_determinism.py \
  evalsi/tests/test_doctor.py evalsi/tests/test_matrix.py evalsi/tests/test_runner.py \
  ../examples/first-time-onboarding/test_verify_live.py ../examples/tinylm/test_check.py -q
.venv/bin/mypy evalsi/src evalsi-harness/src
.venv/bin/ruff check --config pyproject.toml ../examples/first-time-onboarding/*.py \
  ../examples/tinylm/*.py
.venv/bin/ruff format --check --config pyproject.toml ../examples/first-time-onboarding/*.py \
  ../examples/tinylm/*.py
```

The combined pytest command passed all 94 tests. The shown mypy command passed.
The final lint/format commands also included `evalsi` and `evalsi-harness`:
`ruff check evalsi evalsi-harness ../examples/first-time-onboarding/*.py ../examples/tinylm/*.py`
and the corresponding `ruff format --check` both passed (326 files formatted).

The first website build stopped because the fresh worktree had no Astro
dependencies. After restoring the lockfile dependencies with `npm ci`, from
`website/`, `npm run build` passed (119 pages) and
`node scripts/check-links.mjs` passed (`links ok`). No manifest or lockfile
dependency versions were changed.

The stamped-image command
`docker build --build-arg VERSION=0.3.1.dev0 -t evalsi:onboarding-verified .`
passed. Running its `evalsi doctor --require agent` in a throwaway privileged
container passed. `evalsi serve --help` displayed the real Go server's flags;
it retained Go's help exit status 2, rather than an argparse usage page.

The original GitHub Actions checks were also inspected: #154's `e2e` passed;
#155's `e2e` failed with 1 failed, 1,805 passed and 8 skipped. Its failure was
`test_a_closed_stdout_ends_quietly`: Python returned 120 while flushing stdout
at shutdown instead of the expected 141. The follow-up fixes that defect.
The final CLI suite passed 20 tests under a separate restored Python 3.14.4
workspace environment (`uv sync --all-packages --python 3.14.4`, then
`python -m pytest evalsi/tests/test_cli.py -q --tb=short`). Both large and small
closed-stdout controls also passed on Linux Python 3.13.17. GitHub Actions have
not been re-run with the local changes; the original PR check remains red.

The [kind lesson](11-local-kind.md) contains the exact chart installation,
model deployment, positive controls, generated negative controls and waits.
Unlike the existing full e2e script, it does not prune the user's Docker images.

## Problems encountered and changes made

1. **Overlapping PRs.** Both added a different tutorial index at the same path.
   Keep the short, sandbox-free first-time course and the longer TinyLM course
   as separate learning paths, rather than dropping one on merge.
2. **Website publication.** PR #154 registered its pages with the site; #155
   did not. The follow-up registers both courses, adds navigation, and copies
   screenshots to static site assets instead of treating GitHub HTML blob
   pages as image URLs.
3. **Verifier could ignore a failed model start.** TinyLM's old shell verifier
   continued when `curl` was absent or readiness never succeeded, and reused
   fixed port 8765. The follow-up uses a kernel-assigned free port, a URL
   readiness file, Python's HTTP client, explicit startup failure, quoted CLI
   paths, and exact result-count/error assertions. A failed-start regression
   test proves no evaluation command runs after the model fails to start.
4. **Mac capabilities are different.** The published Mac SDK/harness install
   works for mock and SQL tools, but has no bundled Linux sandbox/server.
   Strict agent doctor correctly failed there. Linux published wheels passed
   strict agent doctor in Docker.
5. **Development image version mismatch.** The initial default Docker build
   used Go version `0.0.0-dev` with Python `0.3.1.dev0`, so strict doctor refused
   it. The lesson builds with an explicit matching `VERSION`.
6. **Cluster prerequisites and upgrade timing.** Installing the CRD chart
   before creating the operator namespace failed on the webhook Secret.
   Creating that namespace resolved it. Early runs submitted while changing
   the disposable server's storage/rollout reported "the run is gone from the
   server". After persistence was configured and rollout completed, fresh
   positive and negative resources produced the expected six results.
7. **Provider connectivity.** The endpoint initially resolved but timed out.
   After VPN connectivity became effective, native probes and all real harness
   controls passed. Credentials were present in zsh, not the initial editor
   process environment.
8. **New runner schema mistakes were caught.** Its initial inline records used
   dataset-file shorthand rather than protobuf Content objects, and a timeout
   setting was not part of the run-spec API. Those attempts failed validation
   before model execution; the final examples now pass the actual spec parser.
9. **PR #155's closed-pipe fix missed deferred writes.** A short summary fit in
   Python's stdout buffer, so `BrokenPipeError` happened after `main()` returned,
   outside its handler. This was reproduced locally as exit 120 with an ignored
   flush exception. The CLI now flushes inside its protected dispatch and closes
   the temporary devnull descriptor after redirecting stdout. Regression tests
   cover both the catalog and a short evaluation table, requiring exit 141 and
   completely empty stderr.

## Limits

- No fresh production deployment, HA, autoscaling, air-gap, Firecracker,
  GPU/model hosting, load, long-horizon quality or human calibration pass.
- kind's default CNI does not enforce NetworkPolicy. No network-confinement
  claim is made for these sandbox pods.
- Haiku ran from the Mac through the configured gateway, **not inside kind**.
  kind used TinyLM and a deterministic CLI agent.
- The source image exercised Kubernetes server/operator/worker behavior; the
  PyPI package checks were separate. No claim that the Helm image is the
  published 0.3.1 image.
- Existing PR #155 runtime fixes were retained and tested; the follow-up also
  corrects its buffered stdout defect. Other runtime changes are limited to
  tutorial verification and publication.
- No full Go, browser UI, adapter or repository-wide test pass is claimed.

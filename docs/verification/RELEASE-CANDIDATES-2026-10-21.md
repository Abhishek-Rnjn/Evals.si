# Release-candidate installation, 21 October 2026

Scope: R04-01's versioning, publishing plan and installed-artifact controls.
See the [release procedure](../RELEASING.md#release-candidates-and-installed-artifact-checks)
and [candidate installation guide](../guides/onboarding.md#install-a-reviewed-candidate).

## Implementation

Release classification accepts stable versions and numbered RCs. Go/Helm retain
the tag spelling; Python source, distribution metadata and harness pins use the
canonical PEP 440 spelling. RC image plans exclude `latest` for both server and
collector. RC GitHub releases explicitly set `prerelease: true` and
`make_latest: false`; stable releases retain their existing channel updates.
OCI charts stay version-tagged, and release bundles pin chart versions too.

The release workflow requires installed-artifact onboarding before registry
publication. Linux amd64/arm64 run natively; macOS/Windows exercise the portable
package. Each fresh environment installs explicit wheel paths, removes source
`PYTHONPATH`/server overrides and executes copied verification/Action scripts
outside the checkout. The SDK/harness must import from that environment. Missing
candidate wheels cannot fall back to a published SDK/harness.

Evidence JSON records wheel SHA-256 values, installed dependency versions,
capabilities, elapsed time and the Action adapter's outputs. Dependency resolution
still uses the configured package index. These are synthetic, no-model controls.

## Local evidence

| Check | Result |
|---|---|
| Focused version/doctor/wheel tests | 42 passed: stable/RC channel plans, rejected ambiguous inputs, canonical source and harness pin, failure before partial stamping, RC server equivalence, distinct-version rejection and workflow publication dependencies |
| Full Python workspace | 1523 passed, 19 skipped, 2 existing warnings in 125.09 seconds against a freshly built daemon |
| Pure SDK artifact | Built SDK/harness wheels and sdists at `0.4.0rc1`; a fresh pure-wheel environment passed `doctor`, `init`, recorded run, negative gate (3), missing-spec error (1), HTML report and Action adapter controls |
| Linux amd64 platform artifact | Newly built static Go server at `0.4.0-rc.1`, canonical SDK/harness wheels and the released static bubblewrap from v0.3.0; fresh installation passed `doctor --require server` and the same recorded/report/Action controls |
| Bundle chart pin | `go test ./tests/helm -run TestAirgapBundlePlatform -count=1`: passed, including all three explicitly versioned RC charts, default/arm64 paths and export failures |
| Static checks | Workspace Ruff lint/format and strict mypy passed; release scripts lint/format and actionlint on both changed workflows passed |

The pure and Linux server checks took 6.38 and 9.06 seconds respectively on the
warm local environment; these are automated fixture timings. The documentation
site built 91 pages and passed its link checker.

The platform archive used locally packages the new server with the previously
released static bubblewrap. It is not a complete invocation of the Docker-based
release archive builder. CI's `release-artifacts` builds the actual complete archive
for each native architecture, then builds and installs its platform wheel.

The two Python warnings are the existing multithreaded-fork and closed-loop
subprocess-transport warnings. Optional-environment skips remain visible.

## Evidence still required

The PR's `release-artifacts` and `portable-onboarding` jobs supply native Linux
amd64/arm64 agent and macOS/Windows SDK evidence, with `release-onboarding-*`
reports and `candidate-wheels-*` downloads. Record their final run/results when
available; local Linux server-only evidence does not prove the namespaced agent
controls or other platforms passed.

The repository requires a status named `release-check`. Its aggregate job keeps
that exact name and fails unless both native artifact jobs and both portable
jobs succeed; expanding the platform matrix does not bypass branch protection.

A maintainer-authorized release dry run, followed by separately authorized
publication and verification of the published signatures/artifacts, remains open.
No release workflow dispatch, tag push, registry publication or mutable-channel
update was performed to establish the local evidence. Classification tests prove
the publishing plan; they do not prove a registry write has happened.

An unfamiliar developer still needs to complete the documented workflow without
assistance. Automated smoke timings are not that usability evidence. Customer
adoption, live models and evaluator quality are separate acceptance work under
the [pre-0.4 plan](../PRE-0.4-RELEASE-PLAN.md).

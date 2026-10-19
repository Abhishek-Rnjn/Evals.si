# Onboarding and CI action verification

Verified on 19 October 2026, Linux amd64, Python 3.12, native bubblewrap sandbox.
Scope: initial PROD-01/PROD-02/GTM-02 slices in the
[product roadmap](../PRODUCT-ROADMAP.md). New CLI commands remain unreleased.

- Full source Python SDK/harness suite: **996 passed**. Offline fixture tests ran
  without external HTTP proxies; server-backed tests used a static source build
  of `evalsid` stamped with the matching `0.3.0.dev0` version.
- Ruff lint/format checks and strict mypy passed. The standalone action adapter
  was checked separately with the Python workspace's Ruff configuration.
- Doctor reported matching SDK/harness/server versions and an available `bwrap`
  sandbox. Required-capability absence, timeouts and version mismatches also have
  automated negative controls.
- Both generated recorded-output and agent specs passed (exit 0); their
  deliberately broken controls failed gates (exit 3). Agent controls executed
  in a real namespaced sandbox with networking denied.
- The action adapter passed with independently installed published `evalsi==0.3.0`
  and `evalsi-harness==0.3.0`: working control 0, broken control 3, matching
  baseline comparison 0. Action tests additionally reject malformed/mismatched
  baselines, incomplete scored pairs, missing specs and absent explicit gates.
  The added no-gates control was checked in the targeted action suite after
  the full 996-test run.
- Workflow syntax passed `actionlint` 1.7.7. A new `onboarding` CI job exercises
  the composite action itself against published packages and asserts negative
  control failure propagation. Its GitHub result must be reviewed on the PR;
  local adapter execution alone does not certify the GitHub Actions runtime.
- Website build: **79 pages**, local link check passed; guide is imported into
  website navigation. This does not deploy the site before merge.

No package release, merge, customer-data pilot, remote-cluster qualification,
macOS/Windows execution qualification or enterprise guarantee is included.
The [setup guide](../guides/onboarding.md) records runtime and privacy limits.

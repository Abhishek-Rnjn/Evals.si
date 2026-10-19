# Dataset snapshot verification

Initial PROD-04 implementation, verified on 19 October 2026 on Linux amd64 with
Python 3.12. Snapshot commands/importer are unreleased and are not in PyPI 0.3.0.

- Full SDK/harness suite: **1,174 passed**, including **21** snapshot tests and a
  real-server snapshot test. One existing fork deprecation warning remains.
- Ruff lint/format and strict mypy passed. `uv sync --locked --all-packages`
  installed the source importer entry point without lockfile changes.
- Server-backed tests used a fresh static `evalsid` build stamped `0.3.0.dev0`
  and real Python workers. They verified accepted pins, source provenance in
  stored results, rejection of changed digests and confinement to `datasets_dir`.
- Local tests covered valid replacement artifacts rejected by the original pin
  before target creation, malformed/duplicate/non-finite data, identity metadata,
  no-overwrite races and symlinks, owner-only file permissions, size/encoding
  limits, item diffs and payload omission from CLI review output.
- Checked-in synthetic refund snapshots reproduce their pinned digests. The
  working gate exits 0; the unauthorized-refund control exits 3.
- A built SDK wheel was installed into a clean environment outside the checkout,
  without the harness or server extras. It discovered `snapshot://`, ran both
  gate controls correctly, created a JSON-array snapshot through the CLI,
  verified its external digest and refused to overwrite it.
- Website build: **81 pages**, local links passed; the guide is imported into
  website navigation. No deployment or package publishing was performed.

For reproduction, run the Python workspace's Ruff/mypy/pytest commands and the
[example](../../examples/dataset-snapshots/README.md). Real server tests accept
`EVALSI_E2E_EVALSID` or build with Go; source-only runs are insufficient to claim
server verification. Offline fixture tests used no external HTTP proxy.

The [guide](../guides/dataset-snapshots.md) records the v1 contract and limits.
There is no central registry, authenticated approval history, S3 snapshot-loader
qualification, training-leakage enforcement, signature or filesystem immutability
claim. Existing trusted specs, project permissions and filesystem operation
remain responsible for approval and artifact handling.

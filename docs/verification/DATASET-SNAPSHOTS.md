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

## Tuning/held-out overlap extension

PR #111 additionally includes the pinned `dataset overlap` workflow, with
**13 overlap tests** and **34 combined snapshot/overlap tests** passing. Controls
cover shared IDs with changed contents, renamed exact inputs with changed labels,
normalized chat/structured input wrappers, incomplete input coverage, external-pin
mismatches, split-role validation, payload omission and grouped duplicate inputs.
The full SDK/harness suite with this extension passed **1,187 tests**, with the
same existing fork deprecation warning.

The checked-in Bash example checks a disjoint tuning set (exit 0) and a renamed
duplicate (exit 3). Snapshot generation independently reproduces all example pins.
The command also reports when complete input coverage cannot be established;
strict mode exits 1 instead of silently clearing those records.

A fresh SDK-only wheel installation outside the checkout ran both split controls
and verified that missing input coverage takes precedence over a known ID overlap.
Ruff, strict mypy, the website build and local link checks passed. This feature
does not provide semantic deduplication, training-exposure detection or enforced
central split governance; its report and CI policy are documented in the guide.

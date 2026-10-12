# 0024. `evalsi` ships Linux platform wheels that bundle `evalsid`

- **Status:** Accepted, 2026-10-12

## Context

`pip install evalsi` gave the SDK and CLI, but `evalsi serve`, code evaluators (through `evalsid sandbox run`) and Wasm plugins all need the Go `evalsid` binary, which a Python user had to fetch from the release archive or build. [Decision 0006](0006-naming-and-namespaces.md) left bundling it as a later option.

## Decision

1. **Platform wheels of `evalsi`, not a second package.** For Linux amd64 and arm64 the release adds `evalsi-<v>-py3-none-manylinux_2_17_<arch>.whl`: the pure wheel plus `evalsi/_bin/evalsid` and `evalsi/_bin/bwrap`, both from that architecture's release archive and executable. `evalsid` prefers a `bwrap` beside itself, so the sandbox's namespaced rung needs nothing from the host. The archive's licence notices for bubblewrap go into the wheel's `licenses/bundled`. The `manylinux_2_17` tag only says "Linux on this architecture": the binaries are static Go and static bubblewrap, with no glibc dependency.
2. **The pure wheel and the sdist stay.** pip picks the more specific platform wheel on a matching machine and the pure wheel everywhere else (macOS, Windows, other architectures); `--no-binary evalsi` builds the pure package from the sdist. Nothing else about the package differs.
3. **Lookup order: `EVALSID`, then the bundled binary, then `PATH`** (`evalsi._binaries.evalsid_path`). The environment variable is an explicit choice and always wins. The bundle comes before `PATH` because it is the same release as the Python package, so the worker protocol matches; a different `evalsid` on `PATH` is a different release more often than a deliberate choice. `evalsi evalsid-path` prints the one in use.
4. **Built from the archive, not rebuilt.** `scripts/platform_wheel.py` (standard library only) takes the pure wheel and `evalsi_<version>_linux_<arch>.tar.gz`, refuses a version mismatch (a tag `0.5.0-rc1` matches the wheel's `0.5.0rc1`), flips `Root-Is-Purelib`, sets the tag and writes a valid `RECORD`. The release workflow runs it per architecture after the binaries and the pure wheel; the PyPI job and the GitHub release include the results.
5. **`evalsid` is not put on `PATH`.** A console script named `evalsid` would shadow, inside a virtualenv, an `evalsid` someone installed on purpose, and the pure wheel could not have one. Use `evalsi serve` or `$(evalsi evalsid-path)`.

## Consequences

- A Linux install of `evalsi` grows by the size of `evalsid` and `bwrap` (tens of megabytes, compressed): the price of "nothing else to install". Anyone who wants the small package uses `--no-binary` or installs it where only the SDK is needed.
- macOS and Windows are not covered: there is no release archive for them, and the sandbox is Linux-only. `evalsid` itself builds elsewhere, so a darwin wheel would be possible later without changing the lookup.
- Untested until a tag: the `wheels` job and the PyPI upload of platform wheels run only in the release workflow. CI covers the script, the lookup order, and a wheel built from the real amd64 archive, installed and used to run the sandbox.

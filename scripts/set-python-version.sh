#!/bin/sh
# Stamp both Python packages and the harness pin with the release's PEP 440 version.
# Usage: scripts/set-python-version.sh <X.Y.Z|X.Y.Z-rc.N> [python-dir]
set -eu
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
exec python3 "$script_dir/release_version.py" "${1:?release version required}" --stamp "${2:-python}"

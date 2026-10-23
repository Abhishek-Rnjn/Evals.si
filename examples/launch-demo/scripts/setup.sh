#!/usr/bin/env bash
# Install the published packages in an isolated environment; no source install.
set -euo pipefail
demo_root="$(cd "$(dirname "$0")/.." && pwd)"
demo_venv="${EVALSI_DEMO_VENV:-$demo_root/.venv}"
if ! test -x "$demo_venv/bin/python"; then
  python3 -m venv "$demo_venv"
fi
"$demo_venv/bin/python" -m pip install 'evalsi==0.3.1' 'evalsi-harness==0.3.1'
"$demo_venv/bin/python" -c 'from importlib.metadata import version; assert version("evalsi") == version("evalsi-harness") == "0.3.1"; print("Published SDK and harness: 0.3.1")'
printf '\nRun: %s/bin/python %s/scripts/run_demo.py\n' "$demo_venv" "$demo_root"

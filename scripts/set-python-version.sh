#!/bin/sh
# Stamps the Python packages with a release version: evalsi and
# evalsi-harness, and the harness's pin on evalsi (the same release only).
# Usage: scripts/set-python-version.sh <version> [python-dir]
set -eu
v="$1"
dir="${2:-python}"
echo "$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$' || { echo "bad version $v" >&2; exit 1; }
for f in "$dir/evalsi/src/evalsi/_version.py" "$dir/evalsi-harness/src/evalsi_harness/_version.py"; do
  sed -i "s/^__version__ = .*/__version__ = \"$v\"/" "$f"
done
sed -i "s/\"evalsi\[server\]==[^\"]*\"/\"evalsi[server]==$v\"/" "$dir/evalsi-harness/pyproject.toml"
grep -q "\"evalsi\[server\]==$v\"" "$dir/evalsi-harness/pyproject.toml" || { echo "evalsi-harness: no evalsi pin to set" >&2; exit 1; }

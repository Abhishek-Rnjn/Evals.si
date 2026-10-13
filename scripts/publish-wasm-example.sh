#!/usr/bin/env bash
# Rebuilds the hosted Wasm example plugin from examples/wasm/text-checks into
# plugins/wasm/text-checks, pins the module in its manifest, and pins the
# manifest in plugins/index.yaml. Run it after changing the example's source
# or its manifest; python/evalsi/tests/test_plugins.py fails until you do.
set -euo pipefail
cd "$(dirname "$0")/.."
src=examples/wasm/text-checks
dest=plugins/wasm/text-checks
mkdir -p "$dest"
cp "$src/evalsi-plugin.yaml" "$dest/evalsi-plugin.yaml"
(cd "$src" && GOOS=wasip1 GOARCH=wasm go build -trimpath -o "../../../$dest/text-checks.wasm" .)
go run ./cmd/evalsid wasm pin "$dest/evalsi-plugin.yaml"
sha=$(sha256sum "$dest/evalsi-plugin.yaml" | cut -d' ' -f1)
python3 - "$sha" <<'PY'
import re, sys
p = "plugins/index.yaml"
s = open(p).read()
s, n = re.subn(
    r"(manifest: wasm/text-checks/evalsi-plugin.yaml\n\s+sha256: )[0-9a-f]{64}", r"\g<1>" + sys.argv[1], s
)
if n != 1:
    sys.exit("plugins/index.yaml: the example/text-checks entry was not found")
open(p, "w").write(s)
PY
echo "index pins $sha"

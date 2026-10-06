#!/usr/bin/env bash
# Builds the release archive for one architecture: static evalsid,
# evalsi-guest and evalsi-operator, plus a static bubblewrap beside evalsid
# (which evalsid prefers over the host's), the license and third-party
# notices.
#
#   scripts/release-binaries.sh VERSION ARCH OUT_DIR    # ARCH: amd64 or arm64
set -euo pipefail
version="${1:?usage: release-binaries.sh VERSION ARCH OUT_DIR}"
arch="${2:?}"
out="$(mkdir -p "${3:?}" && cd "$3" && pwd)"
root="$(cd "$(dirname "$0")/.." && pwd)"
name="evalsi_${version}_linux_${arch}"
stage="$(mktemp -d)/$name"
mkdir -p "$stage"

for b in evalsid evalsi-guest evalsi-operator; do
  (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/abhishek-rnjn/evals.si/internal/version.Version=${version}" \
    -o "$stage/$b" "./cmd/$b")
done
"$root/scripts/build-static-bwrap.sh" "$stage" "$arch" >/dev/null
cp "$root/LICENSE" "$stage/LICENSE"
cat > "$stage/THIRD_PARTY.md" <<NOTICE
# Third-party software in this archive

- **bubblewrap** (\`bwrap\`): LGPL-2.0-or-later. Shipped as its own
  executable, unmodified, not linked into Evals.si. License: \`bwrap.COPYING\`;
  source: \`bwrap.SOURCE\`.
- The Go binaries include Go modules under their own licenses; see
  \`go.mod\` in the source at the tag for the list.
NOTICE
tar -C "$(dirname "$stage")" -czf "$out/$name.tar.gz" "$name"
echo "$out/$name.tar.gz"

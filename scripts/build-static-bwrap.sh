#!/usr/bin/env bash
# Builds a statically linked bubblewrap (glibc, static libcap), so the namespaced
# sandbox rung does not depend on the host's distro package. bubblewrap is
# LGPL-2.0-or-later: it ships as its own executable with its license and
# source reference (third_party/bubblewrap/), never linked into evalsid.
#
#   scripts/build-static-bwrap.sh OUT_DIR [amd64|arm64]
set -euo pipefail
out="$(mkdir -p "${1:?usage: build-static-bwrap.sh OUT_DIR [ARCH]}" && cd "$1" && pwd)"
arch="${2:-amd64}"
version=0.11.0
sha256=988fd6b232dafa04b8b8198723efeaccdb3c6aa9c1c7936219d5791a8b7a8646

# Behind a proxy (with its own CA), the build passes both through.
extra=()
for v in HTTPS_PROXY HTTP_PROXY https_proxy http_proxy NO_PROXY no_proxy; do
  [ -n "${!v:-}" ] && extra+=(-e "$v")
done
if [ -n "${BUILD_CA_BUNDLE:-}" ]; then
  extra+=(-v "${BUILD_CA_BUNDLE}:/usr/local/share/ca-certificates/build-proxy.crt:ro")
  extra+=(-e CURL_CA_BUNDLE=/usr/local/share/ca-certificates/build-proxy.crt)
fi

docker run --rm --network host --platform "linux/${arch}" "${extra[@]}" -v "${out}:/out" ubuntu:24.04 sh -euc "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null
  apt-get install -y -qq --no-install-recommends build-essential meson ninja-build pkg-config \\
    libcap-dev curl ca-certificates xz-utils file >/dev/null
  cd /tmp
  curl -fsSL -o bwrap.tar.xz https://github.com/containers/bubblewrap/releases/download/v${version}/bubblewrap-${version}.tar.xz
  echo '${sha256}  bwrap.tar.xz' | sha256sum -c -
  tar xf bwrap.tar.xz && cd bubblewrap-${version}
  meson setup _build --prefer-static --default-library=static -Dc_link_args=-static \
    -Dselinux=disabled -Dman=disabled -Dtests=false -Dbash_completion=disabled -Dzsh_completion=disabled >/dev/null
  ninja -C _build bwrap >/dev/null
  strip _build/bwrap
  file _build/bwrap | grep -q 'statically linked' || { file _build/bwrap; echo 'bwrap is not static' >&2; exit 1; }
  install -m 0755 _build/bwrap /out/bwrap
  cp COPYING /out/bwrap.COPYING
  echo 'bubblewrap ${version}: https://github.com/containers/bubblewrap/releases/tag/v${version}' > /out/bwrap.SOURCE
"
file "${out}/bwrap" 2>/dev/null || true

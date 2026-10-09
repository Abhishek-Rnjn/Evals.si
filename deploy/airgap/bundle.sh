#!/usr/bin/env bash
# Builds an air-gapped bundle on a connected machine: every image the charts
# use, the charts, and (with --with-cli) wheels for the evalsi CLI.
#
#   deploy/airgap/bundle.sh --tag 0.4.0 --out evalsi-bundle
#   tar czf evalsi-bundle.tgz evalsi-bundle      # carry it across
#
# Needs docker and helm (and uv for --with-cli). EXTRA_IMAGES adds images
# (sandbox images your tasks name, for example), space-separated.
set -euo pipefail

tag=""
out=evalsi-bundle
platform=linux/amd64
with_cli=false
root="$(cd "$(dirname "$0")/../.." && pwd)"
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --platform) platform="$2"; shift 2 ;;
    --with-cli) with_cli=true; shift ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$tag" ] || tag="$(sed -n 's/^appVersion: "\(.*\)"/\1/p' "$root/deploy/helm/evalsi/Chart.yaml")"

repo="${EVALSI_IMAGE_REPOSITORY:-ghcr.io/abhishek-rnjn/evalsi}"
images=(
  "$repo:$tag"
  "$(sed -n 's/^  image: \(nats:.*\)/\1/p' "$root/deploy/helm/evalsi/values.yaml")"
  "$(sed -n 's/^  image: \(postgres:.*\)/\1/p' "$root/deploy/helm/evalsi/values.yaml")"
  # The trial bundles devMinio and devClickhouse.
  "$(sed -n 's/^  image: \(bitnamilegacy\/minio:.*\)/\1/p' "$root/deploy/helm/evalsi/values.yaml")"
  "$(sed -n 's/^  image: \(clickhouse\/clickhouse-server:.*\)/\1/p' "$root/deploy/helm/evalsi/values.yaml")"
  # The pod rung's default sandbox image.
  "python:3.13-slim"
)
# shellcheck disable=SC2206
images+=(${EXTRA_IMAGES:-})

mkdir -p "$out/charts"
: > "$out/images.txt"
for img in "${images[@]}"; do
  # A locally built image (no registry to pull from) is used as is.
  if ! docker image inspect "$img" >/dev/null 2>&1 || [ "${PULL:-1}" = 1 ]; then
    docker pull --platform "$platform" "$img" || docker image inspect "$img" >/dev/null
  fi
  echo "$img" >> "$out/images.txt"
done
echo "saving $(wc -l < "$out/images.txt") images"
docker save -o "$out/images.tar" "${images[@]}"

for chart in evalsi-crds evalsi evalsi-sandboxd; do
  helm package "$root/deploy/helm/$chart" --app-version "$tag" -d "$out/charts" >/dev/null
done
cp "$root/deploy/airgap/install.sh" "$out/"
cp "$root/deploy/helm/evalsi/values-namespaced.yaml" "$out/charts/"

if $with_cli; then
  (cd "$root/python" && uv build --all-packages --wheel -o "$out/wheels" >/dev/null)
  uv export --project "$root/python" --frozen --no-dev --no-emit-workspace --all-packages \
    --extra anthropic --extra jsonschema --no-hashes -o "$out/wheels/requirements.txt" >/dev/null
  python3 -m pip download -q -d "$out/wheels" -r "$out/wheels/requirements.txt"
fi
echo "bundle ready in $out (images: $out/images.txt)"

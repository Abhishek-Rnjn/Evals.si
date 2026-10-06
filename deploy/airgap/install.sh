#!/usr/bin/env bash
# Installs Evals.si from an air-gapped bundle: pushes its images to your
# registry (or loads them into a kind cluster), then installs the charts
# pulling only from there.
#
#   ./install.sh --registry registry.internal:5000 [--namespace evalsi] [-- <helm --set ...>]
#   ./install.sh --kind <cluster> --registry airgap.invalid   # testing
#
# --with-sandboxd also installs the node sandbox pool (--sandboxd-mode
# bwrap, privileged or firecracker; see the evalsi-sandboxd chart).
#
# --sandbox-classes lets the operator run SandboxClass pools (a
# ClusterRoleBinding limited to SandboxClasses, in evalsi-crds).
#
# --namespace-only installs into an existing namespace with nothing
# cluster-scoped (no CRDs, no operator, the chart's own sandbox pool), as a
# user who is only admin of that namespace (values-namespaced.yaml).
#
# Images keep their paths under the registry, with docker.io/library images
# at their short names (nats, postgres), as the charts' global.imageRegistry
# expects. Needs docker, helm and kubectl.
set -euo pipefail

bundle="$(cd "$(dirname "$0")" && pwd)"
registry=""
namespace=evalsi
kind_cluster=""
sandboxd=false
sandboxd_mode=bwrap
namespace_only=false
sandbox_classes=false
while [ $# -gt 0 ]; do
  case "$1" in
    --registry) registry="$2"; shift 2 ;;
    --namespace) namespace="$2"; shift 2 ;;
    --kind) kind_cluster="$2"; shift 2 ;;
    --with-sandboxd) sandboxd=true; shift ;;
    --sandboxd-mode) sandboxd_mode="$2"; shift 2 ;;
    --namespace-only) namespace_only=true; shift ;;
    --sandbox-classes) sandbox_classes=true; shift ;;
    --) shift; break ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$registry" ] || { echo "--registry is required" >&2; exit 2; }
if $namespace_only && { $sandboxd || $sandbox_classes; }; then
  echo "--with-sandboxd and --sandbox-classes need cluster-scoped privileges; --namespace-only uses the chart's own pool" >&2
  exit 2
fi

# The chart helper's rule: drop the source registry and library/.
target() {
  local ref="$1" first="${1%%/*}"
  if [[ "$ref" == */* && ( "$first" == *.* || "$first" == *:* || "$first" == localhost ) ]]; then
    ref="${ref#*/}"
  fi
  echo "$registry/${ref#library/}"
}

docker load -q -i "$bundle/images.tar"
moved=()
while read -r img; do
  [ -n "$img" ] || continue
  docker tag "$img" "$(target "$img")"
  moved+=("$(target "$img")")
done < "$bundle/images.txt"
if [ -n "$kind_cluster" ]; then
  docker save "${moved[@]}" -o "$bundle/.kind-images.tar"
  kind load image-archive "$bundle/.kind-images.tar" --name "$kind_cluster"
  rm -f "$bundle/.kind-images.tar"
else
  for img in "${moved[@]}"; do docker push -q "$img"; done
fi

tag="$(head -n1 "$bundle/images.txt")"; tag="${tag##*:}"
if $namespace_only; then
  helm upgrade --install evalsi "$bundle"/charts/evalsi-[0-9]*.tgz -n "$namespace" \
    -f "$bundle/charts/values-namespaced.yaml" \
    --set global.imageRegistry="$registry" --set image.tag="$tag" "$@"
  echo "installed from the bundle into namespace $namespace, namespace-only (images from $registry)"
  exit 0
fi
kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install evalsi-crds "$bundle"/charts/evalsi-crds-*.tgz \
  --set operator.namespace="$namespace" --set operator.sandboxClasses="$sandbox_classes" --wait
helm upgrade --install evalsi "$bundle"/charts/evalsi-[0-9]*.tgz -n "$namespace" \
  --set global.imageRegistry="$registry" --set image.tag="$tag" \
  --set operator.sandboxClasses="$sandbox_classes" "$@"
if $sandboxd; then
  helm upgrade --install evalsi-sandboxd "$bundle"/charts/evalsi-sandboxd-*.tgz -n "$namespace" \
    --set global.imageRegistry="$registry" --set image.tag="$tag" --set mode="$sandboxd_mode"
fi
echo "installed from the bundle into namespace $namespace (images from $registry)"

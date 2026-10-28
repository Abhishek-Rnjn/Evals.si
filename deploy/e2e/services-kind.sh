#!/usr/bin/env bash
# An independent, model-free service lifecycle check with real NetworkPolicy.
# Needs Docker, Go, kind, kubectl, curl, openssl and uv. Creates only its named local cluster.
set -euo pipefail
# Lifecycle fault injection is restricted to this machine's Docker daemon.
unset DOCKER_HOST DOCKER_CONTEXT DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH
export DOCKER_HOST=unix:///var/run/docker.sock
root="$(cd "$(dirname "$0")/../.." && pwd)"
cluster="${SERVICES_CLUSTER:-evalsi-services}"
work="$(mktemp -d)"
forward_pid=""
cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    kubectl --context "kind-$cluster" get pods,events -n evalsi-services -o wide || true
    kubectl --context "kind-$cluster" logs -n evalsi-services pool --tail=80 || true
  fi
  if [ -n "$forward_pid" ]; then kill "$forward_pid" 2>/dev/null || true; fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
cat > "$work/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
nodes:
- role: control-plane
  image: kindest/node:v1.35.0
EOF
# Readiness comes after installing the CNI, not inside kind create.
kind create cluster --name "$cluster" --config "$work/kind.yaml"
kind get kubeconfig --name "$cluster" > "$work/kubeconfig"
export KUBECONFIG="$work/kubeconfig"
curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.30.3/manifests/calico.yaml -o "$work/calico.yaml"
kubectl apply -f "$work/calico.yaml"
kubectl rollout status ds/calico-node -n kube-system --timeout=300s
kubectl wait --for=condition=Ready node --all --timeout=120s
cd "$root"
CGO_ENABLED=0 go build -o "$work/evalsid" ./cmd/evalsid
CGO_ENABLED=0 go build -o "$work/evalsi-guest" ./cmd/evalsi-guest
cat > "$work/Dockerfile" <<'EOF'
FROM scratch
WORKDIR /tmp
COPY evalsid evalsi-guest /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/evalsid"]
EOF
docker build -t evalsi-services:test "$work"
# Load one platform of each image. `kind load docker-image` imports every
# platform a multi-platform image lists, and with Docker's containerd image
# store the platforms that were never pulled are missing ("ctr: content digest
# ... not found"). Docker 28+ can save one platform; an older CLI saves the
# image as it is, which its classic store keeps single-platform anyway.
platform="linux/$(docker version --format '{{.Server.Arch}}')"
save_platform=()
if docker save --help 2>&1 | grep -q -- '--platform'; then save_platform=(--platform "$platform"); fi
for image in evalsi-services:test python:3.13-slim redis:7-alpine; do
  if [ "$image" != evalsi-services:test ]; then docker pull --platform "$platform" "$image"; fi
  docker save ${save_platform[@]+"${save_platform[@]}"} -o "$work/image.tar" "$image"
  kind load image-archive --name "$cluster" "$work/image.tar"
done
rm -f "$work/image.tar"
# An ephemeral CA and separate server/client certificates for the sandbox API.
umask 077
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=services-e2e-ca \
  -keyout "$work/ca.key" -out "$work/ca.crt" 2>/dev/null
for role in server client; do
  openssl req -newkey rsa:2048 -nodes -subj "/CN=services-e2e-$role" \
    -keyout "$work/$role.key" -out "$work/$role.csr" 2>/dev/null
  if [ "$role" = server ]; then
    printf 'subjectAltName=DNS:pool,DNS:localhost\nextendedKeyUsage=serverAuth\n' > "$work/extensions"
  else
    printf 'extendedKeyUsage=clientAuth\n' > "$work/extensions"
  fi
  openssl x509 -req -days 1 -in "$work/$role.csr" -CA "$work/ca.crt" \
    -CAkey "$work/ca.key" -CAcreateserial -extfile "$work/extensions" \
    -out "$work/$role.crt" 2>/dev/null
done
kubectl create namespace evalsi-services
kubectl create secret generic pool-tls -n evalsi-services \
  --from-file=tls.crt="$work/server.crt" --from-file=tls.key="$work/server.key" \
  --from-file=ca.crt="$work/ca.crt"
kubectl apply -f "$root/deploy/e2e/services-pool.yaml"
kubectl wait -n evalsi-services --for=condition=Ready pod/pool --timeout=120s
kubectl port-forward -n evalsi-services pod/pool 17443:7443 > "$work/forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 30); do
  if grep -q 'Forwarding from' "$work/forward.log"; then break; fi
  sleep 1
done
grep -q 'Forwarding from' "$work/forward.log"
(cd python && uv sync --locked --all-packages)
EVALSI_SERVICES_KIND_CLUSTER="$cluster" \
EVALSI_SANDBOX_ADDR=tls://localhost:17443 \
EVALSI_SANDBOX_TLS_CA="$work/ca.crt" \
EVALSI_SANDBOX_TLS_CERT="$work/client.crt" \
EVALSI_SANDBOX_TLS_KEY="$work/client.key" \
  python/.venv/bin/python deploy/e2e/services.py "$@"

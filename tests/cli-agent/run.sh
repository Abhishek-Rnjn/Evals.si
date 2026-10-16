#!/usr/bin/env bash
# Run after kind-e2e has installed its restricted namespace's pod sandbox pool.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
: "${ANTHROPIC_API_KEY:?live CLI opt-in requires ANTHROPIC_API_KEY}"
cluster="${CLUSTER:-evalsi-e2e}"
namespace="${CLI_AGENT_NAMESPACE:-evalsi-restricted}"
artifacts="${EVALSI_CLI_AGENT_ARTIFACTS:-$root/cli-agent-artifacts}"
work="$(mktemp -d)"
forward=""
cleanup() {
  if [ -n "$forward" ]; then kill "$forward" 2>/dev/null || true; wait "$forward" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$artifacts"
docker build -t ghcr.io/abhishek-rnjn/evalsi-cli-agent:e2e "$root/tests/cli-agent"
kind load docker-image --name "$cluster" ghcr.io/abhishek-rnjn/evalsi-cli-agent:e2e
# The SDK executes the harness on the runner and leases its sandbox through
# the real Kubernetes pool over mTLS. The key enters only the CLI process env.
kubectl get secret evalsi-worker-tls -n "$namespace" -o json | python3 -c '
import base64, json, sys
from pathlib import Path
data = json.load(sys.stdin)["data"]
for field in ("ca.crt", "tls.crt", "tls.key"):
    content = base64.b64decode(data[field], validate=True)
    if not content:
        raise ValueError("empty worker TLS field: " + field)
    (Path(sys.argv[1]) / field).write_bytes(content)
' "$work"
kubectl port-forward -n "$namespace" svc/evalsi-sandbox-pool 17443:7443 > "$work/forward.log" 2>&1 &
forward=$!
ready=0
for attempt in $(seq 1 30); do
  if (echo >/dev/tcp/127.0.0.1/17443) 2>/dev/null; then ready=1; break; fi
  kill -0 "$forward" || { cat "$work/forward.log" >&2; exit 1; }
  sleep 1
done
[ "$ready" = 1 ] || { cat "$work/forward.log" >&2; echo "sandbox port forward timed out" >&2; exit 1; }
export EVALSI_SANDBOX_ADDR=tls://127.0.0.1:17443
export EVALSI_SANDBOX_TLS_CA="$work/ca.crt"
export EVALSI_SANDBOX_TLS_CERT="$work/tls.crt"
export EVALSI_SANDBOX_TLS_KEY="$work/tls.key"
export EVALSI_SANDBOX_TLS_SERVER_NAME="evalsi-sandbox-pool.$namespace.svc"
cd "$root/python"
uv run --no-sync evalsi run -f "$root/tests/cli-agent/run.yaml" \
  --output "$artifacts/results.json" --quiet
uv run --no-sync python "$root/tests/cli-agent/verify.py" "$artifacts/results.json"

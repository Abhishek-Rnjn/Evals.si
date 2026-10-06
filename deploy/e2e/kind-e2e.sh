#!/usr/bin/env bash
# The Kubernetes end-to-end test (CI's kind job), against a kind cluster:
#
#  1. builds the evalsi image and an air-gapped bundle from it;
#  2. installs from the bundle, every image under a registry that does not
#     resolve (airgap.invalid), so a single image from elsewhere fails the
#     install: two API replicas on PostgreSQL, NATS, workers per pool, the
#     operator and its webhooks, and evalsi-sandboxd (bubblewrap);
#  3. checks `kubectl apply` gives what `evalsi run -f` gives, the webhooks
#     (validation, the recorded creator), a policy, code evaluation on the
#     bubblewrap pool, and an agent run on the pod rung (a SandboxClass).
#
# Needs docker, kind, kubectl, helm, and uv (for the embedded CLI run).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/deploy/e2e"
cluster="${CLUSTER:-evalsi-e2e}"
ns=evalsi
registry=airgap.invalid
tag=e2e
work="$(mktemp -d)"

step() { echo; echo "=== $*"; }

diagnose() {
  step "diagnostics"
  kubectl get pods,deploy,sts,ds,svc -n "$ns" -o wide || true
  kubectl get evalruns,onlineevalpolicies,evaluators -n "$ns" -o yaml || true
  kubectl get sandboxclasses -o yaml || true
  kubectl get events -n "$ns" --sort-by=.lastTimestamp | tail -60 || true
  for p in $(kubectl get pods -n "$ns" -o name); do
    echo "--- $p"
    kubectl describe -n "$ns" "$p" | tail -25 || true
    kubectl logs -n "$ns" "$p" --all-containers --tail=120 || true
  done
}
trap 'status=$?; [ $status -eq 0 ] || diagnose; exit $status' EXIT

# phase waits for an EvalRun's phase, failing fast on a final one.
phase() {
  local run="$1" want="$2" deadline=$((SECONDS + ${3:-600})) got
  while [ $SECONDS -lt $deadline ]; do
    got="$(kubectl get evalrun "$run" -n "$ns" -o jsonpath='{.status.phase}')"
    if [ "$got" = "$want" ]; then return 0; fi
    case "$got" in Succeeded|Failed|Error|Cancelled|Invalid)
      echo "evalrun/$run is $got, want $want: $(kubectl get evalrun "$run" -n "$ns" -o jsonpath='{.status.message}')" >&2
      return 1 ;;
    esac
    sleep 3
  done
  echo "evalrun/$run did not reach $want (last: ${got:-none})" >&2
  return 1
}

# mean prints a metric's mean from an EvalRun's status.
mean() {
  kubectl get evalrun "$1" -n "$ns" -o jsonpath="{.status.summaries[?(@.metric==\"$2\")].mean}"
}

step "image"
docker build -q -t "ghcr.io/abhishek-rnjn/evalsi:$tag" "$root"

step "air-gapped bundle"
PULL=0 "$root/deploy/airgap/bundle.sh" --tag "$tag" --out "$work/bundle"
cat "$work/bundle/images.txt"

step "cluster"
kind get clusters | grep -qx "$cluster" || kind create cluster --name "$cluster" --config "$here/kind.yaml" --wait 180s
kubectl cluster-info

step "install from the bundle"
# kind's nodes are containers, where runc cannot set up user-namespaced
# pods (sysfs), so the bubblewrap pool runs privileged here.
"$work/bundle/install.sh" --kind "$cluster" --registry "$registry" --with-sandboxd --sandboxd-mode privileged -- \
  --set server.replicas=2 --set devPostgres.enabled=true \
  --set operator.sandboxClasses=true \
  --set sandbox.address="tls://evalsi-sandboxd.$ns.svc:7443" \
  --wait --timeout 10m
kubectl rollout status ds/evalsi-sandboxd -n "$ns" --timeout 5m
kubectl get pods -n "$ns" -o wide

step "every image came from the bundle"
images="$(kubectl get pods -n "$ns" -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}' | sort -u)"
echo "$images"
if echo "$images" | grep -v "^$registry/"; then
  echo "images from outside the bundle" >&2
  exit 1
fi

step "kubectl apply matches evalsi run -f"
kubectl apply -n "$ns" -f "$here/parity-run.yaml"
(cd "$root/python" && uv run --no-sync evalsi run -f "$here/parity-run.yaml" --output "$work/embedded.json" --quiet)
phase parity Succeeded
kubectl get evalrun parity -n "$ns" -o json > "$work/k8s.json"
python3 - "$work/embedded.json" "$work/k8s.json" <<'PY'
import json, sys
embedded = {s["metric"]: s for s in json.load(open(sys.argv[1]))["summaries"]}
k8s = {s["metric"]: s for s in json.load(open(sys.argv[2]))["status"]["summaries"]}
assert embedded.keys() == k8s.keys(), (embedded.keys(), k8s.keys())
for metric, e in embedded.items():
    k = k8s[metric]
    assert int(k["n"]) == e["n"], (metric, k, e)
    assert abs(float(k["mean"]) - e["mean"]) < 1e-5, (metric, k, e)
    assert abs(float(k["ciLow"]) - e["ci"]["low"]) < 1e-5, (metric, k, e)
print("parity:", {m: (k["n"], k["mean"]) for m, k in k8s.items()})
PY

step "webhooks"
creator="$(kubectl get evalrun parity -n "$ns" -o jsonpath='{.metadata.annotations.evals\.si/created-by}')"
[ "$creator" = "kubernetes-admin" ] || { echo "created-by: '$creator'" >&2; exit 1; }
if out="$(kubectl apply -n "$ns" -f - 2>&1 <<'YAML'
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: invalid}
spec:
  dataset: {inline: {records: [{id: a, output: {text: x}}]}}
  evaluators: [{ref: exact-match}]
  gates: [{metric: exact-match}]
YAML
)"; then echo "an invalid EvalRun was admitted" >&2; exit 1; fi
echo "$out" | grep -q "needs min or max" || { echo "$out" >&2; exit 1; }
if kubectl patch evalrun parity -n "$ns" --type merge -p '{"spec":{"trials":2}}' 2>/dev/null; then
  echo "an EvalRun's spec changed" >&2; exit 1
fi

step "an online policy"
kubectl apply -n "$ns" -f "$here/policy.yaml"
kubectl wait onlineevalpolicy/e2e-latency -n "$ns" --for=condition=Synced --timeout=2m

step "code evaluation on the bubblewrap pool"
kubectl apply -n "$ns" -f "$here/sandbox-run.yaml"
phase unit-tests Succeeded
[ "$(mean unit-tests unit-tests)" = "0.5" ] || { echo "unit-tests mean $(mean unit-tests unit-tests)" >&2; exit 1; }
kubectl logs -n "$ns" ds/evalsi-sandboxd --tail=50 | tee "$work/sandboxd.log"

step "an agent run on the pod rung"
kubectl apply -f "$here/pod-sandboxclass.yaml"
kubectl wait sandboxclass/pods --for=condition=Ready --timeout=5m
address="$(kubectl get sandboxclass pods -o jsonpath='{.status.address}')"
helm upgrade evalsi "$work"/bundle/charts/evalsi-[0-9]*.tgz -n "$ns" --reuse-values \
  --set sandbox.address="$address" --wait --timeout 5m
kubectl get pods -n "$ns" -l evals.si/sandbox=true -w -o name > "$work/sandbox-pods.txt" &
watcher=$!
kubectl apply -n "$ns" -f "$here/agent-run.yaml"
phase agent Succeeded
kill "$watcher" 2>/dev/null || true
[ "$(mean agent task-success)" = "1" ] || { echo "task-success mean $(mean agent task-success)" >&2; exit 1; }
sort -u "$work/sandbox-pods.txt"
[ -s "$work/sandbox-pods.txt" ] || { echo "no sandbox pods were created" >&2; exit 1; }
left="$(kubectl get pods -n "$ns" -l evals.si/sandbox=true -o name | wc -l)"
echo "sandbox pods left after the run: $left"

step "deleting an EvalRun"
kubectl delete evalrun parity -n "$ns" --wait --timeout=1m

step "passed"

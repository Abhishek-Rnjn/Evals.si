#!/usr/bin/env bash
# The Kubernetes end-to-end test (CI's kind job), against a kind cluster:
#
#  1. builds the evalsi image and an air-gapped bundle from it;
#  2. installs from the bundle, every image under a registry that does not
#     resolve (airgap.invalid), so a single image from elsewhere fails the
#     install: two API replicas on PostgreSQL, NATS, workers per pool, the
#     operator and its webhooks, and evalsi-sandboxd (bubblewrap);
#  3. checks `kubectl apply` gives what `evalsi run -f` gives, the webhooks
#     (validation, the recorded creator), a policy, the bootstrap Job (a project
#     and an API key from values, kept in a Secret across upgrades), code evaluation on the
#     bubblewrap pool, and an agent run on the pod rung (a SandboxClass);
#     the reference demos (examples/demo): both agents fix the small-repo suite
#     against the mock model on the pod rung, and the scores reach MLflow;
#     R6: a TraceSource pulls the Deep Agents service's MLflow traces, a
#     policy scores them, and the scores are written back as assessments;
#  4. installs again, namespace-only, as a user who is only admin of a
#     namespace that enforces Pod Security "restricted": no CRDs, no
#     cluster-scoped object, sandboxes from the chart's own pool. Code
#     evaluation and an agent run go through the CLI, on the pod rung and
#     then on Landlock;
#  5. removes the grant that lets service accounts read the cluster's
#     issuer and keys, checks a restarted evalsid fails closed, then gives it a
#     static issuer and key set (a ConfigMap) and checks they do again.
#
# Needs docker, kind, kubectl, helm, and uv (for the embedded CLI run).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
here="$root/deploy/e2e"
cluster="${CLUSTER:-evalsi-e2e}"
ns=evalsi
ns2=evalsi-restricted
registry=airgap.invalid
tag=e2e
work="$(mktemp -d)"

step() { echo; echo "=== $*"; }

diagnose() {
  step "diagnostics"
  kubectl get pods,deploy,sts,ds,svc -n "$ns" -o wide || true
  kubectl get pods,deploy,events -n "$ns2" -o wide 2>/dev/null || true
  for p in $(kubectl get pods -n "$ns2" -o name 2>/dev/null); do
    echo "--- $ns2 $p"
    kubectl logs -n "$ns2" "$p" --all-containers --tail=80 || true
  done
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

# results prints every result of an EvalRun's run, with its outcome and
# reason, through the API as the operator's service account.
results() {
  local run token
  run="$(kubectl get evalrun "$1" -n "$ns" -o jsonpath='{.status.runId}')"
  token="$(kubectl create token evalsi-operator -n "$ns" --audience evals.si)"
  kubectl port-forward -n "$ns" svc/evalsi 18080:8080 >/dev/null 2>&1 &
  local pf=$!
  sleep 3
  curl -sk -X POST https://127.0.0.1:18080/evalsi.v1alpha1.RunService/ListRunResults \
    -H "Authorization: Bearer $token" -H 'content-type: application/json' -d "{\"runId\":\"$run\"}" |
    python3 -c 'import json, sys
for r in json.load(sys.stdin).get("results", []):
    print(r.get("recordId"), r.get("evaluator"), r.get("outcome"), r.get("reason", "")[:400])' || true
  kill "$pf" 2>/dev/null || true
}

# expect fails, showing the run's results, when a metric's mean is not want.
expect() {
  local got
  got="$(mean "$1" "$2")"
  if [ "$got" != "$3" ]; then
    echo "evalrun/$1: $2 mean '$got', want $3" >&2
    kubectl get evalrun "$1" -n "$ns" -o jsonpath='{.status.summaries}' >&2; echo >&2
    results "$1" >&2
    return 1
  fi
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
"$work/bundle/install.sh" --kind "$cluster" --registry "$registry" --with-sandboxd --sandboxd-mode privileged --sandbox-classes -- \
  --set server.replicas=2 --set devPostgres.enabled=true \
  --set sandbox.address="tls://evalsi-sandboxd.$ns.svc:7443" \
  --wait --timeout 10m
kubectl rollout status ds/evalsi-sandboxd -n "$ns" --timeout 5m
# The images are in the kind nodes now; the runner's copies only fill its disk.
rm -f "$work/bundle/images.tar"
docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -E "^($registry/|ghcr.io/abhishek-rnjn/evalsi:|nats:|postgres:|python:)" | xargs -r docker rmi -f >/dev/null || true
docker builder prune -af >/dev/null || true
df -h / | tail -1
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

step "bootstrap: a project and an API key from values, kept in a Secret"
bootstrap_values=(--set bootstrap.enabled=true
  --set bootstrap.projects[0].name=boot
  --set bootstrap.apiKeys[0].name=boot-reader --set bootstrap.apiKeys[0].roles.boot[0]=viewer --set bootstrap.apiKeys[0].secret.name=boot-reader-key)
chart="$(ls "$work"/bundle/charts/evalsi-[0-9]*.tgz)"
helm upgrade evalsi "$chart" -n "$ns" --reuse-values "${bootstrap_values[@]}" --wait --timeout 5m
key="$(kubectl get secret boot-reader-key -n "$ns" -o jsonpath='{.data.api_key}' | base64 -d)"
[ -n "$key" ] || { echo "the bootstrap Job wrote no key" >&2; exit 1; }
kubectl port-forward -n "$ns" svc/evalsi 18081:8080 >/dev/null 2>&1 &
pf=$!
sleep 3
curl -sk -X POST https://127.0.0.1:18081/evalsi.v1alpha1.AuthService/WhoAmI -H "Authorization: Bearer $key" \
  -H 'content-type: application/json' -d '{}' | tee "$work/whoami.json"
grep -q boot-reader "$work/whoami.json" || { echo "the bootstrap key does not sign in" >&2; exit 1; }
# Upgrading again must not make a second key or change the Secret.
helm upgrade evalsi "$chart" -n "$ns" --reuse-values "${bootstrap_values[@]}" --wait --timeout 5m
again="$(kubectl get secret boot-reader-key -n "$ns" -o jsonpath='{.data.api_key}' | base64 -d)"
[ "$again" = "$key" ] || { echo "the key changed on upgrade" >&2; exit 1; }
curl -sk -X POST https://127.0.0.1:18081/evalsi.v1alpha1.AuthService/WhoAmI -H "Authorization: Bearer $key" \
  -H 'content-type: application/json' -d '{}' | grep -q boot-reader
kill "$pf" 2>/dev/null || true

step "code evaluation on the bubblewrap pool"
kubectl apply -n "$ns" -f "$here/sandbox-run.yaml"
phase unit-tests Succeeded || { results unit-tests >&2; exit 1; }
expect unit-tests unit-tests 0.5
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
phase agent Succeeded || { results agent >&2; exit 1; }
kill "$watcher" 2>/dev/null || true
expect agent task-success 1
sort -u "$work/sandbox-pods.txt"
[ -s "$work/sandbox-pods.txt" ] || { echo "no sandbox pods were created" >&2; exit 1; }
left="$(kubectl get pods -n "$ns" -l evals.si/sandbox=true -o name | wc -l)"
echo "sandbox pods left after the run: $left"

step "the reference demos: the small-repo-fix suite, for both agents, against the mock model"
# The studio stand-ins (examples/demo) on the same cluster: a mock model, the Deep
# Agents service, dsh's web UI and an MLflow server, with a project, a model key
# grant, S3 for the datasets and an MLflow sink on Evals.si. The suite runs through
# `kubectl apply` on the pod rung, one run per agent.
demo="$root/examples/demo"
docker build -q -t ghcr.io/abhishek-rnjn/evalsi-demo-dsh:0.1.0 "$demo/dsh"
docker build -q -t ghcr.io/abhishek-rnjn/evalsi-demo-deepagents:0.1.0 "$demo/deepagents"
kind load docker-image --name "$cluster" ghcr.io/abhishek-rnjn/evalsi-demo-dsh:0.1.0 ghcr.io/abhishek-rnjn/evalsi-demo-deepagents:0.1.0
docker rmi -f ghcr.io/abhishek-rnjn/evalsi-demo-dsh:0.1.0 ghcr.io/abhishek-rnjn/evalsi-demo-deepagents:0.1.0 >/dev/null || true
docker builder prune -af >/dev/null || true
# The workers need the model key's Secret before they restart with the grant.
kubectl create secret generic evalsi-demo-model-key -n "$ns" --from-literal=key=mock
evalsi_image="$(kubectl get deploy evalsi -n "$ns" -o jsonpath='{.spec.template.spec.containers[0].image}')"
helm upgrade evalsi "$chart" -n "$ns" --reuse-values -f "$demo/overlays/evalsi-demo.yaml" --wait --timeout 10m
helm install evalsi-demo "$demo" -n "$ns" -f "$demo/overlays/kind.yaml" --set datasets.image="$evalsi_image" --wait --timeout 10m
kubectl get pods -n "$ns" -l app.kubernetes.io/part-of=evalsi-demo -o wide

kubectl apply -n "$ns" -f "$demo/dsh-small-repo-fixes.yaml" -f "$demo/deepagents-small-repo-fixes.yaml"
for run in dsh-small-repo-fixes deepagents-small-repo-fixes; do
  phase "$run" Succeeded 1500 || { results "$run" >&2; exit 1; }
  expect "$run" task-success 1
done

step "the demo's scores reached MLflow"
# MLflow refuses Host headers it does not list (DNS-rebinding protection); a
# port-forward's 127.0.0.1:15000 is not one, so name a host the chart allows.
mlflow_host='Host: localhost:5000'
kubectl port-forward -n "$ns" svc/evalsi-demo-mlflow 15000:5000 >/dev/null 2>&1 &
pf=$!
sleep 3
curl -s -H "$mlflow_host" -X POST http://127.0.0.1:15000/api/2.0/mlflow/experiments/search -H 'content-type: application/json' \
  -d '{"max_results":100,"filter":"name = '"'"'evalsi-demo'"'"'"}' | tee "$work/mlflow-experiments.json"
grep -q evalsi-demo "$work/mlflow-experiments.json" || { echo "no MLflow experiment from the sink" >&2; exit 1; }
kill "$pf" 2>/dev/null || true

step "R6: Deep Agents traces in MLflow, pulled by a TraceSource, scored and written back"
# The Deep Agents service traces each run to the demo MLflow (LangChain
# autolog). A TraceSource reads them back, a policy scores them, and the scores
# land on the same MLflow traces as assessments.
kubectl port-forward -n "$ns" svc/evalsi-demo-deepagents 18080:8080 >/dev/null 2>&1 &
pf=$!
sleep 3
question="$(head -n1 "$demo/fixtures/deep-research.jsonl" | python3 -c 'import json,sys; print(json.dumps({"input": json.load(sys.stdin)["input"]}))')"
curl -sf -m 300 -X POST http://127.0.0.1:18080/invoke -H 'content-type: application/json' -d "$question" | head -c 300; echo
kill "$pf" 2>/dev/null || true
kubectl apply -n "$ns" -f "$demo/deepagents-online-policy.yaml" -f "$demo/deepagents-mlflow-source.yaml"
for _ in $(seq 1 60); do
  scored="$(kubectl get tracesource deepagents-mlflow -n "$ns" -o jsonpath='{.status.scored}')"
  [ "${scored:-0}" -ge 1 ] && break
  sleep 5
done
kubectl get tracesource deepagents-mlflow -n "$ns" -o yaml | tee "$work/tracesource.yaml"
[ "${scored:-0}" -ge 1 ] || { echo "the TraceSource wrote no scores back" >&2; exit 1; }
kubectl port-forward -n "$ns" svc/evalsi-demo-mlflow 15000:5000 >/dev/null 2>&1 &
pf=$!
sleep 3
exp="$(curl -s -H "$mlflow_host" "http://127.0.0.1:15000/api/2.0/mlflow/experiments/get-by-name?experiment_name=evalsi-demo-deepagents" | python3 -c 'import json,sys; print(json.load(sys.stdin)["experiment"]["experiment_id"])')"
curl -s -H "$mlflow_host" -X POST http://127.0.0.1:15000/api/3.0/mlflow/traces/search -H 'content-type: application/json' \
  -d '{"locations":[{"type":"MLFLOW_EXPERIMENT","mlflow_experiment":{"experiment_id":"'"$exp"'"}}]}' | tee "$work/mlflow-traces.json" >/dev/null
grep -q '"source_id": *"evalsi/deepagents-online"' "$work/mlflow-traces.json" || { echo "no Evals.si assessment on the Deep Agents traces" >&2; exit 1; }
kill "$pf" 2>/dev/null || true

step "deleting an EvalRun"
kubectl delete evalrun parity -n "$ns" --wait --timeout=1m

step "namespace-only install, as a namespace admin, under Pod Security restricted"
# What a platform team hands out: a namespace that enforces "restricted",
# and the built-in admin role in it. Nothing else.
kubectl create namespace "$ns2"
kubectl label namespace "$ns2" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/enforce-version=latest
kubectl create rolebinding alice-admin -n "$ns2" --clusterrole=admin --user=alice
for verb in "create clusterroles" "create clusterrolebindings" "create customresourcedefinitions" "create validatingwebhookconfigurations"; do
  if kubectl auth can-i $verb --as alice >/dev/null; then echo "alice can $verb" >&2; exit 1; fi
done
ci="user:kubernetes/system:serviceaccount:$ns2:ci"
"$work/bundle/install.sh" --kind "$cluster" --registry "$registry" --namespace "$ns2" --namespace-only --skip-images -- \
  --kube-as-user alice \
  --set-json "rbac.projects={\"e2e\": {\"runner\": [\"$ci\"]}}" \
  --wait --timeout 10m
kubectl get pods -n "$ns2" -o wide
kubectl create serviceaccount ci -n "$ns2" --as alice
kubectl get secret evalsi-server-tls -n "$ns2" -o jsonpath='{.data.ca\.crt}' | base64 -d > "$work/ns2-ca.crt"
kubectl port-forward -n "$ns2" svc/evalsi 18443:8080 >/dev/null 2>&1 &
forward=$!
sleep 3

# cli runs a run file through the API as the ci service account, as a CI
# job in the cluster would (a projected token; here minted by kubectl).
cli() {
  (cd "$root/python" && SSL_CERT_FILE="$work/ns2-ca.crt" \
    EVALSI_TOKEN="$(kubectl create token ci -n "$ns2" --audience evals.si)" \
    uv run --no-sync evalsi run -f "$1" --server https://localhost:18443 --format json --quiet > "$2")
}
# metric prints a metric's mean from a run (the server's JSON).
metric() {
  python3 -c 'import json, sys
s = {m["metric"]: m for m in json.load(open(sys.argv[1]))["summaries"]}
print(s[sys.argv[2]]["mean"])' "$1" "$2"
}
want() {
  local got
  got="$(metric "$1" "$2")"
  python3 -c 'import sys; sys.exit(abs(float(sys.argv[1]) - float(sys.argv[2])) > 1e-9)' "$got" "$3" ||
    { echo "$2 mean $got, want $3" >&2; cat "$1" >&2; ns2_results "$1"; exit 1; }
}
# ns2_results prints each result of a run (the server's JSON) with its
# outcome and reason, and the sandbox pool's log, to show why one failed.
ns2_results() {
  local run
  run="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["id"])' "$1")"
  curl -s --cacert "$work/ns2-ca.crt" -X POST https://localhost:18443/evalsi.v1alpha1.RunService/ListRunResults \
    -H "Authorization: Bearer $(kubectl create token ci -n "$ns2" --audience evals.si)" \
    -H 'content-type: application/json' -d "{\"runId\":\"$run\"}" |
    python3 -c 'import json, sys
for r in json.load(sys.stdin).get("results", []):
    print(r.get("recordId"), r.get("evaluator"), r.get("outcome"), r.get("reason", "")[:1000])' >&2 || true
  kubectl logs -n "$ns2" deploy/evalsi-sandbox-pool --tail=50 >&2 || true
}

step "namespace-only: code evaluation on the pod rung"
kubectl get pods -n "$ns2" -l evals.si/sandbox=true -w -o name > "$work/ns2-pods.txt" &
watcher=$!
cli "$here/sandbox-run.yaml" "$work/ns2-unit.json"
want "$work/ns2-unit.json" unit-tests 0.5

step "namespace-only: an agent run on the pod rung"
cli "$here/agent-run.yaml" "$work/ns2-agent.json"
want "$work/ns2-agent.json" task-success 1
kill "$watcher" 2>/dev/null || true
sort -u "$work/ns2-pods.txt"
[ -s "$work/ns2-pods.txt" ] || { echo "no sandbox pods were created in $ns2" >&2; exit 1; }

step "namespace-only: code evaluation on Landlock"
helm upgrade evalsi "$work"/bundle/charts/evalsi-[0-9]*.tgz -n "$ns2" --kube-as-user alice --reuse-values \
  --set 'sandbox.pool.ladder={landlock}' --wait --timeout 5m
kubectl rollout status deploy/evalsi-sandbox-pool -n "$ns2" --timeout 3m
cli "$here/sandbox-run.yaml" "$work/ns2-landlock.json"
want "$work/ns2-landlock.json" unit-tests 0.5
kubectl logs -n "$ns2" deploy/evalsi-sandbox-pool --tail=20

step "namespace-only: a static issuer and key set, on a cluster that hides its keys"
# A hardened cluster removes the grant that lets every service account read
# the issuer and its keys from the API server. A fresh evalsid then cannot
# verify service-account tokens until it is given the issuer and a copy of
# the public key set, which the platform team publishes (here, a ConfigMap).
issuer="$(kubectl get --raw /.well-known/openid-configuration | python3 -c 'import json, sys; print(json.load(sys.stdin)["issuer"])')"
kubectl get --raw /openid/v1/jwks > "$work/jwks.json"
kubectl get clusterrolebinding system:service-account-issuer-discovery -o yaml > "$work/discovery-binding.yaml"
kubectl delete clusterrolebinding system:service-account-issuer-discovery
restore_discovery() { kubectl apply -f "$work/discovery-binding.yaml" >/dev/null 2>&1 || true; }
trap 'status=$?; restore_discovery; [ $status -eq 0 ] || diagnose; exit $status' EXIT
# reforward points the port-forward at the current server pods.
reforward() {
  kill "$forward" 2>/dev/null || true
  kubectl port-forward -n "$ns2" svc/evalsi 18443:8080 >/dev/null 2>&1 &
  forward=$!
  sleep 3
}
# evalsid discovers the issuer at startup, so a restarted one fails closed:
# it exits naming the refused discovery, and the rollout never completes.
kubectl rollout restart deploy/evalsi -n "$ns2" --as alice
if kubectl rollout status deploy/evalsi -n "$ns2" --timeout 90s; then
  echo "evalsid started with no access to the cluster's issuer and keys" >&2; exit 1
fi
found=""
for p in $(kubectl get pods -n "$ns2" -o name | grep -E '^pod/evalsi-[a-z0-9]+-[a-z0-9]+$'); do
  # Captured first: under pipefail, grep -q exiting early fails the pipe.
  logs="$(kubectl logs -n "$ns2" "$p" 2>/dev/null || true; kubectl logs -n "$ns2" "$p" --previous 2>/dev/null || true)"
  if grep -q "issuer discovery.*403" <<<"$logs"; then found="$p"; fi
done
[ -n "$found" ] || { echo "no evalsid pod reported the refused issuer discovery" >&2; exit 1; }
echo "$found failed closed"
kubectl create configmap evalsi-jwks -n "$ns2" --as alice --from-file=keys.json="$work/jwks.json"
helm upgrade evalsi "$work"/bundle/charts/evalsi-[0-9]*.tgz -n "$ns2" --kube-as-user alice --reuse-values \
  --set auth.kubernetes.issuer="$issuer" --set auth.kubernetes.jwksConfigMap=evalsi-jwks --wait --timeout 5m
kubectl rollout status deploy/evalsi -n "$ns2" --timeout 3m
reforward
cli "$here/sandbox-run.yaml" "$work/ns2-static.json"
want "$work/ns2-static.json" unit-tests 0.5
restore_discovery
kill "$forward" 2>/dev/null || true

step "passed"

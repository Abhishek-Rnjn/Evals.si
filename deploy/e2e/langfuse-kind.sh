#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
context="${KUBE_CONTEXT:-kind-evalsi-e2e-desktop}"
namespace="${LANGFUSE_NAMESPACE:-evalsi-langfuse-live}"
evalsi_namespace="${EVALSI_NAMESPACE:-evalsi}"
release="${EVALSI_RELEASE:-evalsi}"
python="${PYTHON:-$root/python/.venv/bin/python}"
export LANGFUSE_NAMESPACE="$namespace" EVALSI_NAMESPACE="$evalsi_namespace"
export LANGFUSE_TEST_SECRET="$(docker exec evalsi-langfuse-live-langfuse-web-1 printenv SALT)"
export LANGFUSE_TEST_ENCRYPTION_KEY="$(docker exec evalsi-langfuse-live-langfuse-web-1 printenv ENCRYPTION_KEY)"
export LANGFUSE_TEST_PUBLIC_KEY="$(docker exec evalsi-langfuse-live-langfuse-web-1 printenv LANGFUSE_INIT_PROJECT_PUBLIC_KEY)"
export LANGFUSE_TEST_SECRET_KEY="$(docker exec evalsi-langfuse-live-langfuse-web-1 printenv LANGFUSE_INIT_PROJECT_SECRET_KEY)"
export LANGFUSE_TEST_CREDENTIAL="${LANGFUSE_TEST_PUBLIC_KEY}:${LANGFUSE_TEST_SECRET_KEY}"
work="$(mktemp -d)"
forward=""
trap 'ret=$?; [ -z "$forward" ] || kill "$forward" 2>/dev/null || true; rm -rf "$work"; exit "$ret"' EXIT

kubectl --context "$context" get deployment "$release" -n "$evalsi_namespace" >/dev/null
kubectl --context "$context" create namespace "$namespace" --dry-run=client -o yaml | kubectl --context "$context" apply -f -
kubectl --context "$context" label namespace "$namespace" evalsi-live-langfuse=true --overwrite
docker compose -f "$root/deploy/e2e/langfuse-compose.yaml" config --format json |
  "$python" -c '
import json, os, subprocess, sys
import yaml
config = json.load(sys.stdin)
namespace = os.environ["LANGFUSE_NAMESPACE"]
objects = []
ports = {"postgres": [5432], "redis": [6379], "clickhouse": [8123, 9000], "minio": [9000], "langfuse-web": [3000], "langfuse-worker": [3030]}
for name, service in config["services"].items():
    environment = service.get("environment", {})
    if name.startswith("langfuse-"):
        environment["HOSTNAME"] = "0.0.0.0"
        environment["NEXTAUTH_URL"] = "http://langfuse-web:3000"
    objects.append({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": name + "-env", "namespace": namespace}, "stringData": {key: str(value) for key, value in environment.items()}})
    image = service["image"]
    if name.startswith("langfuse-"):
        index = json.loads(subprocess.check_output(["docker", "buildx", "imagetools", "inspect", image, "--raw"]))
        digest = next(item["digest"] for item in index["manifests"] if item["platform"].get("architecture") == "amd64" and item["platform"].get("os") == "linux")
        image = image.rsplit(":", 1)[0] + "@" + digest
    container = {"name": name, "image": image, "envFrom": [{"secretRef": {"name": name + "-env"}}], "ports": [{"containerPort": port} for port in ports[name]]}
    if "command" in service:
        container["args"] = service["command"]
    if "healthcheck" in service:
        container["readinessProbe"] = {"exec": {"command": ["/bin/sh", "-c", service["healthcheck"]["test"][1]]} if service["healthcheck"]["test"][0] == "CMD-SHELL" else {"command": service["healthcheck"]["test"][1:]}, "periodSeconds": 5, "timeoutSeconds": 5}
    mounts = service.get("volumes", [])
    if mounts:
        container["volumeMounts"] = [{"name": "data", "mountPath": mounts[0]["target"]}]
    spec = {"containers": [container], "nodeSelector": json.loads(os.environ.get("LANGFUSE_NODE_SELECTOR", "{}")), "tolerations": json.loads(os.environ.get("LANGFUSE_TOLERATIONS", "[]"))}
    if mounts:
        spec["volumes"] = [{"name": "data", "emptyDir": {}}]
    labels = {"app": name, "evalsi-live-langfuse": "true"}
    objects.append({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name, "namespace": namespace, "labels": labels}, "spec": {"replicas": 1, "selector": {"matchLabels": {"app": name}}, "template": {"metadata": {"labels": labels}, "spec": spec}}})
    objects.append({"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": namespace, "labels": labels}, "spec": {"selector": {"app": name}, "ports": [{"name": "port-" + str(port), "port": port, "targetPort": port} for port in ports[name]]}})
yaml.safe_dump_all(objects, sys.stdout)
' | kubectl --context "$context" apply -f -

printf '%s' "$LANGFUSE_TEST_CREDENTIAL" |
    kubectl --context "$context" create secret generic langfuse-live-token -n "$evalsi_namespace" --from-file=token=/dev/stdin --dry-run=client -o yaml | kubectl --context "$context" apply -f -
export EVALSI_TEST_API_ENDPOINTS="$(kubectl --context "$context" get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o json)"
"$python" - "$work/values.yaml" <<'PY'
import ipaddress, json, os, sys, yaml
host = "langfuse-web." + os.environ["LANGFUSE_NAMESPACE"] + ".svc.cluster.local"
api_egress = []
for endpoint_slice in json.loads(os.environ["EVALSI_TEST_API_ENDPOINTS"])["items"]:
    api_egress.append({"to": [{"ipBlock": {"cidr": str(ipaddress.ip_network(address))}} for endpoint in endpoint_slice["endpoints"] for address in endpoint["addresses"]], "ports": [{"protocol": port["protocol"], "port": port["port"]} for port in endpoint_slice["ports"]]})
values = {
    "sources": {"secrets": ["langfuse-live-token"], "allowHosts": [host, "evalsi-demo-mlflow"]},
    "bootstrap": {"credentialGrants": [{"env": "DEMO_API_KEY", "projects": ["demo"]}, {"env": "file:langfuse-live-token/token", "projects": ["demo"], "hosts": [host], "allow_http": True}]},
    "networkPolicy": {"egress": {"enabled": True, "extra": [{"to": [{"namespaceSelector": {"matchLabels": {"evalsi-live-langfuse": "true"}}, "podSelector": {"matchLabels": {"app": "langfuse-web"}}}], "ports": [{"protocol": "TCP", "port": 3000}]}] + api_egress}}
}
with open(sys.argv[1], "w") as output:
    yaml.safe_dump(values, output)
PY
helm upgrade "$release" "$root/deploy/helm/evalsi" -n "$evalsi_namespace" --kube-context "$context" --reuse-values -f "$work/values.yaml" --wait --timeout 5m
kubectl --context "$context" wait deployment --all -n "$namespace" --for=condition=Available --timeout=10m
kubectl --context "$context" port-forward -n "$namespace" svc/langfuse-web 13002:3000 >"$work/forward.log" 2>&1 &
forward=$!
export EVALSI_TEST_LANGFUSE_URL=http://127.0.0.1:13002 EVALSI_TEST_LANGFUSE_CREDENTIAL="$LANGFUSE_TEST_CREDENTIAL"
export EVALSI_TEST_LANGFUSE_TRACE_ID_FILE="$work/trace-id"
curl -fsS --max-time 2 --retry 30 --retry-connrefused --retry-delay 1 "$EVALSI_TEST_LANGFUSE_URL/api/public/health" >/dev/null
go -C "$root" test ./internal/source/langfuse -run '^TestLiveLangfuse$' -count=1 -v
"$python" - <<'PY' | kubectl --context "$context" apply -f -
import os, sys, yaml
namespace = os.environ["EVALSI_NAMESPACE"]
metadata = {"name": "langfuse-live", "namespace": namespace, "labels": {"evals.si/project": "demo"}}
policy = {"apiVersion": "evals.si/v1alpha1", "kind": "OnlineEvalPolicy", "metadata": metadata, "spec": {"selector": "labels[\"source\"] == \"langfuse-live\"", "stages": [{"evaluators": [{"ref": "tool-errors"}, {"ref": "loop-detection"}]}], "window": "1h"}}
source = {"apiVersion": "evals.si/v1alpha1", "kind": "TraceSource", "metadata": metadata, "spec": {"connector": "langfuse", "endpoint": "http://langfuse-web." + os.environ["LANGFUSE_NAMESPACE"] + ".svc.cluster.local:3000", "locations": ["production"], "credentials": {"file": "langfuse-live-token/token"}, "backfill": {"since": "1h"}, "poll": {"interval": "5s"}, "policies": ["langfuse-live"], "writeBack": {"enabled": True}}}
yaml.safe_dump_all([policy, source], sys.stdout)
PY
kubectl --context "$context" wait tracesource/langfuse-live -n "$evalsi_namespace" --for=condition=Synced --timeout=3m
kubectl --context "$context" wait tracesource/langfuse-live -n "$evalsi_namespace" --for=jsonpath='{.status.lastWriteBack}' --timeout=5m
kubectl --context "$context" get tracesource/langfuse-live -n "$evalsi_namespace" -o yaml
"$python" - <<'PY'
import base64, hashlib, json, os, pathlib, time, urllib.parse, urllib.request
auth = base64.b64encode(os.environ["LANGFUSE_TEST_CREDENTIAL"].encode()).decode()
trace_id = pathlib.Path(os.environ["EVALSI_TEST_LANGFUSE_TRACE_ID_FILE"]).read_text()
expected = {"evalsi-" + hashlib.sha256((trace_id + "|langfuse-live|" + metric).encode()).hexdigest()[:24] for metric in ("tool-errors", "loop-detection")}
query = urllib.parse.urlencode({"fields": "details", "name": "tool-errors,loop-detection", "traceId": trace_id})
request = urllib.request.Request(os.environ["EVALSI_TEST_LANGFUSE_URL"] + "/api/public/v3/scores?" + query, headers={"Authorization": "Basic " + auth})
deadline = time.monotonic() + 300
while time.monotonic() < deadline:
    with urllib.request.urlopen(request, timeout=30) as response:
        scores = json.load(response)["data"]
    if {score["id"] for score in scores if score.get("metadata", {}).get("evalsi.policy") == "langfuse-live"} == expected:
        break
else:
    raise AssertionError("no policy scores written for the newly ingested trace " + trace_id)
print("In-cluster Secret/Service TraceSource pull, policy evaluation and score write-back passed")
PY
kubectl --context "$context" get networkpolicy -n "$evalsi_namespace"
export EVALSI_TEST_KUBE_CONTEXT="$context" EVALSI_TEST_RELEASE="$release"
"$python" - <<'PY'
import json, os, subprocess
context = os.environ["EVALSI_TEST_KUBE_CONTEXT"]
namespace = os.environ["EVALSI_NAMESPACE"]
release = os.environ["EVALSI_TEST_RELEASE"]
host = "langfuse-web." + os.environ["LANGFUSE_NAMESPACE"] + ".svc.cluster.local"
command = ["kubectl", "--context", context, "-n", namespace]
policy = json.loads(subprocess.check_output(command + ["get", "networkpolicy", release + "-egress", "-o", "json"]))
egress = policy["spec"]["egress"]
restricted = [rule for rule in egress if not any(target.get("podSelector", {}).get("matchLabels", {}).get("app") == "langfuse-web" for target in rule.get("to", []))]
probe = "import socket, sys\ntry:\n socket.create_connection((sys.argv[1], 3000), 3).close()\nexcept OSError:\n print('blocked')\nelse:\n print('reachable')"
def reachable():
    return subprocess.check_output(command + ["exec", "deployment/" + release, "--", "python", "-c", probe, host], timeout=15, text=True).strip() == "reachable"
def patch(rules):
    subprocess.run(command + ["patch", "networkpolicy", release + "-egress", "--type=merge", "-p", json.dumps({"spec": {"egress": rules}})], check=True)
assert reachable(), "allowed Langfuse endpoint is unreachable"
try:
    patch(restricted)
    blocked = any(not reachable() for attempt in range(10))
finally:
    patch(egress)
assert any(reachable() for attempt in range(10)), "Langfuse connectivity did not recover"
assert blocked, "NetworkPolicy enforcement not verified: the denied endpoint remained reachable"
print("NetworkPolicy allow/deny/restore control passed from the running evalsid pod")
PY
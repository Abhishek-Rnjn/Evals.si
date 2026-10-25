# 11. Onboard a local Kubernetes cluster with kind

This lesson tests three distinct things: recorded scoring, a real sandboxed CLI
agent, and a streamed fake-model tool loop on distributed workers. It uses no
provider credential. Complete the [local mock-tool lesson](03-tool-loop.md)
first, so cluster setup cannot hide a broken SDK installation.

## Check native tools and Docker

You need Docker, kind, kubectl and Helm. On macOS, an executable in `/usr/local/bin`
can still be a Linux binary; inspect it if it fails with `cannot execute binary
file`. Use a native installation rather than replacing unrelated machine tools.

```bash
docker info
kind version
kubectl version --client
helm version --short
```

Start Docker Desktop if needed. Do not prune images, delete existing clusters,
or run the repository's complete kind e2e script merely to follow this lesson:
that script also does air-gap, multi-replica and restricted-namespace testing,
and includes broad Docker-cache cleanup.

## Build matching components and isolate the cluster

Run from the repository root. Use a new cluster name. A separate kubeconfig
avoids switching the current context of an existing cluster.

```bash
docker build --build-arg VERSION=0.3.1.dev0 -t evalsi:onboarding .
export CLUSTER=evalsi-onboarding
export KUBECONFIG="$(mktemp)"
export CONTEXT="kind-$CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 180s
kind load docker-image evalsi:onboarding --name "$CLUSTER"
kubectl --context "$CONTEXT" get nodes
```

The image bundles the Go server, guest, operator, Python worker and harness.
Stamping `VERSION` makes `evalsi doctor` able to compare matching versions.
Without stamping, the development Dockerfile reports server `0.0.0-dev` beside
SDK `0.3.1.dev0`; strict doctor refuses that mismatch even if a sandbox works.

## Install the charts

Create the namespace **before** installing the CRD chart: that chart writes the
operator webhook certificate Secret there.

```bash
kubectl --context "$CONTEXT" create namespace onboarding
helm install onboarding-crds deploy/helm/evalsi-crds \
  --kube-context "$CONTEXT" \
  --set operator.namespace=onboarding --set operator.fullname=onboarding-evalsi
helm install onboarding deploy/helm/evalsi \
  --kube-context "$CONTEXT" -n onboarding \
  -f examples/first-time-onboarding/kind-values.yaml --wait --timeout 5m
kubectl --context "$CONTEXT" rollout status deployment/onboarding-evalsi \
  -n onboarding --timeout=180s
kubectl --context "$CONTEXT" get pods,pvc -n onboarding
```

The values enable a persistent single-replica SQLite server, project bootstrap,
and a pod sandbox pool using the locally loaded image. This is not a production
HA installation. `networkPolicyEnforced: false` is intentional: kind's default
CNI does **not** enforce NetworkPolicy. A namespaced sandbox here is not evidence
of network isolation. Keep it restricted to trusted synthetic tasks.

Persistence must be configured at first install if you want to retain results.
Turning it on during an upgrade does not migrate the old ephemeral database.
Finish rollouts before submitting runs: an upgrade is not a readiness test, and
old run IDs can become unavailable after switching databases.

## Recorded scoring and a sandboxed agent

```bash
kubectl --context "$CONTEXT" apply -n onboarding \
  -f examples/first-time-onboarding/kind-recorded.yaml \
  -f examples/first-time-onboarding/kind-agent.yaml
kubectl --context "$CONTEXT" wait -n onboarding evalrun/onboarding-recorded \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
kubectl --context "$CONTEXT" wait -n onboarding evalrun/onboarding-agent \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
kubectl --context "$CONTEXT" get evalrun onboarding-recorded onboarding-agent \
  -n onboarding -o yaml
```

Inspect `status.summaries`, not only pod readiness:

| Run | Expected |
|---|---|
| `onboarding-recorded` | `exact-match` mean 0.5, n=2; gate passes at 0.5 |
| `onboarding-agent` | `task-success` mean 1, n=1; checker confirms `out.txt` |

The CLI agent is a deterministic shell program, not an LLM. It runs in a pod
sandbox; its hidden checker reads the resulting file.

## A streamed tool-calling model on the cluster

Install the actual tutorial model from the checkout as a ConfigMap:

```bash
kubectl --context "$CONTEXT" create configmap onboarding-model -n onboarding \
  --from-file=tinylm.py=examples/tinylm/tinylm.py --dry-run=client -o yaml |
  kubectl --context "$CONTEXT" apply -n onboarding -f -
kubectl --context "$CONTEXT" apply -n onboarding \
  -f examples/first-time-onboarding/kind-model.yaml
kubectl --context "$CONTEXT" rollout status deployment/onboarding-model \
  -n onboarding --timeout=180s
kubectl --context "$CONTEXT" apply -n onboarding \
  -f examples/first-time-onboarding/kind-support.yaml
kubectl --context "$CONTEXT" wait -n onboarding evalrun/onboarding-support \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
```

The model Service is cluster-local. The harness worker calls it using real
streaming HTTP, executes the lookup and refund mocks, and scores the trajectory.
Expect precision=recall=run-completed=1, tool-errors=0 and n=1. The run explicitly
sets `api_key_env: none` and `tools.sandbox: false`: these are mock business
tools, not a second sandbox test.

## Negative controls: prove the gates reject a regression

Using an activated Python environment with PyYAML, generate three separate
resources without modifying the good examples:

```bash
export CONTROLS="$(mktemp -d)"
python - <<'PY'
import os
from pathlib import Path
import yaml

root = Path("examples/first-time-onboarding")
out = Path(os.environ["CONTROLS"])
for name in ("recorded", "agent", "support"):
    doc = yaml.safe_load((root / f"kind-{name}.yaml").read_text())
    doc["metadata"]["name"] += "-negative"
    if name == "recorded":
        doc["spec"]["gates"][0]["min"] = 1.0
    elif name == "agent":
        doc["spec"]["environment"]["checker"]["command"] = ["sh", "-c", "exit 1"]
    else:
        doc["spec"]["target"]["model"] = "tinylm-reckless"
    (out / f"{name}.yaml").write_text(yaml.safe_dump(doc))
PY
kubectl --context "$CONTEXT" apply -n onboarding -f "$CONTROLS"
for name in recorded agent support; do
  kubectl --context "$CONTEXT" wait -n onboarding "evalrun/onboarding-$name-negative" \
    --for=jsonpath='{.status.phase}'=Failed --timeout=180s
done
```

Expected means: recorded remains 0.5 but its stricter gate fails; the checker
failure gives task-success 0; the reckless model omits lookup so tool-call recall
is 0.5. These should be `Failed`, **not** `Error` or `Invalid`. Inspect every
summary's errors and status message if a control does not match.

## Troubleshooting and cleanup

```bash
kubectl --context "$CONTEXT" get evalruns,pods -n onboarding
kubectl --context "$CONTEXT" get events -n onboarding --sort-by=.lastTimestamp
kubectl --context "$CONTEXT" logs deployment/onboarding-evalsi-operator -n onboarding
kubectl --context "$CONTEXT" logs deployment/onboarding-evalsi-worker-harness -n onboarding
```

Missing images usually mean they were not loaded into this cluster.
`Invalid` usually means a spec, project or credential-grant problem. A model
connection error is not a failed business gate. For more deployment controls,
continue to the [Kubernetes guide](../guides/kubernetes.md).

Remove only this disposable cluster and the exact temporary paths created here:

```bash
kind delete cluster --name "$CLUSTER"
rm -f "$CONTROLS/recorded.yaml" "$CONTROLS/agent.yaml" "$CONTROLS/support.yaml"
rmdir "$CONTROLS"
rm -f "$KUBECONFIG"
unset KUBECONFIG CONTEXT CLUSTER CONTROLS
```

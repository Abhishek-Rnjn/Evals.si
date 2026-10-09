---
title: Kubernetes
description: Install Evals.si on a cluster and wire in-cluster agents, gateways and CI.
---

On Kubernetes, Evals.si is three Helm charts and four custom resources. The same evaluators, runs and policies that work on a laptop work here.

## 1. Install

```bash
kubectl create namespace evalsi
helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=evalsi
helm install evalsi deploy/helm/evalsi -n evalsi \
  --set storage.postgres.dsnSecret.name=evalsi-postgres \
  --set server.replicas=2 \
  --set sandbox.address=tls://evalsi-sandboxd.evalsi.svc:7443
helm install evalsi-sandboxd deploy/helm/evalsi-sandboxd -n evalsi
```

| Chart | Scope | Installs |
|---|---|---|
| `evalsi-crds` | cluster | CRDs, admission webhooks, aggregated `admin`/`edit`/`view` roles |
| `evalsi` | namespace | `evalsid` replicas, worker pools, NATS, the operator, internal certificates |
| `evalsi-sandboxd` | nodes | A sandbox pool per node: bubblewrap, or Firecracker on KVM nodes |

For production, use PostgreSQL for several API replicas, ClickHouse for traces at volume, S3 for datasets, a three-node NATS and KEDA to scale worker pools on queue backlog. There is also an air-gapped bundle and an install path that does not need cluster-admin. See the [full Kubernetes guide](__BASE__/docs/guides/kubernetes/).

## 2. Drive it with resources

| Resource | Scope | What it does |
|---|---|---|
| `EvalRun` | namespace | A run spec as a resource. The operator creates the run and mirrors phase, progress and gate results |
| `OnlineEvalPolicy` | namespace | Applies an online policy and mirrors its counters |
| `Evaluator` | namespace | Runs your evaluator plugin image, optionally autoscaled by KEDA |
| `SandboxClass` | cluster | A sandbox pool with its isolation ladder, for example gVisor pods |

An agent run on the pod rung, from `deploy/e2e/agent-run.yaml`:

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: agent
  labels: {evals.si/project: e2e}
spec:
  target:
    agent:
      cli:
        command: [sh, -c, "cat >/dev/null; echo done > out.txt"]
  environment:
    sandbox: {min_isolation: namespaced}
    checker:
      command: [sh, -c, 'test "$(cat out.txt)" = done']
  dataset:
    inline:
      records:
        - {id: t1, input: {text: "Write done to out.txt."}}
  evaluators:
    - ref: task-success
```

```bash
kubectl wait evalrun/agent --for=condition=Succeeded --timeout=30m
```

`Succeeded` means every gate passed, so `kubectl wait` makes a CI or Argo step that fails when quality regresses.

## 3. Connect what runs in the cluster

- **Gateways and agents emit traces.** Create an `ingest` key bound to one project, store it in a Secret and point the OTLP exporter at the `evalsi` Service (gRPC 4317, HTTP 4318). Give the exporter the internal CA from the Secret `evalsi-server-tls`, or terminate TLS in your mesh. If you enforce NetworkPolicy, allow the pods to reach those ports.
- **Evals.si calls your agent.** A run's `target.agent` can point at an in-cluster Service over A2A, MCP, an OpenAI Responses API or HTTP.
- **Use service-account identity.** Pods can authenticate with their Kubernetes service-account tokens instead of long-lived keys. See [identity on Kubernetes](__BASE__/docs/guides/identity/).
- **Guard traffic inline.** Point a gateway's guardrail webhook at `evalsid`. See the [agentgateway integration](__BASE__/integrations/agentgateway/).

## Try it locally

`deploy/e2e/kind-e2e.sh` stands up the whole stack on a kind cluster. CI runs it end to end, installed from the air-gapped bundle.

## Go deeper

- [Evals.si on Kubernetes](__BASE__/docs/guides/kubernetes/)
- [agentgateway and Evals.si on Kubernetes](__BASE__/docs/guides/agentgateway-kubernetes/)
- [Decision record: operator and packaging](__BASE__/docs/decisions/0012-kubernetes-operator-and-packaging/)

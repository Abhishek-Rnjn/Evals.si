# Evals.si on Kubernetes

Evals.si on Kubernetes is the same `evalsid` as standalone, split into roles and run by the [operator](#the-operator-and-its-resources). Run specs and policies are the same files: what `evalsi run -f` takes, `kubectl apply -f` takes. This is Phase 4 of the [roadmap](../DESIGN.md#23-roadmap).

```text
            kubectl apply (EvalRun, OnlineEvalPolicy, Evaluator, SandboxClass)
                                   │
                          ┌────────▼─────────┐  admission webhooks: validate specs,
                          │  evalsi-operator │  record who created each resource
                          └────────┬─────────┘
                    API (its own service-account token)
 agents, agentgateway   ┌──────────▼──────────┐       PostgreSQL (runs, results, auth)
 ──── OTLP ───────────► │ evalsid ×N (API,    │──────►ClickHouse (traces, online scores)
 CLI, CI ──── API ────► │ ingest, scheduler,  │       S3 (datasets, promotions)
                        │ policy engine)      │
                        └──────────┬──────────┘
                          NATS JetStream: work.<pool>, spans
                ┌─────────────┬────┴────────┬──────────────┐
           worker-cpu    worker-judge   worker-sandbox  worker-harness   (KEDA scales each)
                                            │ mutual TLS │
                              ┌─────────────▼────────────▼──┐
                              │ sandbox pools: evalsi-sandboxd (bubblewrap,
                              │ Firecracker) or a SandboxClass (pod rung)
                              └─────────────────────────────┘
```

## Install

Three charts:

| Chart | Scope | What it installs |
|---|---|---|
| [`evalsi-crds`](../../deploy/helm/evalsi-crds) | cluster | The CRDs, the admission webhooks and their certificate, the operator's cluster role, and roles aggregated into `admin`, `edit` and `view` |
| [`evalsi`](../../deploy/helm/evalsi) | namespace | `evalsid` replicas, a worker Deployment per pool, NATS, the operator, internal certificates, and what the pod rung needs |
| [`evalsi-sandboxd`](../../deploy/helm/evalsi-sandboxd) | nodes | A sandbox pool per node (DaemonSet): bubblewrap in a user namespace, or Firecracker on KVM nodes |

```bash
kubectl create namespace evalsi
helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=evalsi
helm install evalsi deploy/helm/evalsi -n evalsi \
  --set storage.postgres.dsnSecret.name=evalsi-postgres \
  --set server.replicas=2 \
  --set sandbox.address=tls://evalsi-sandboxd.evalsi.svc:7443
helm install evalsi-sandboxd deploy/helm/evalsi-sandboxd -n evalsi
```

The defaults are a working single-replica install on SQLite. For production:

- **Storage.** `storage.postgres.dsnSecret` (a Secret holding the DSN) for several API replicas; `storage.clickhouse` for traces at volume; `storage.s3` for datasets (`datasets_dir` becomes `s3://<bucket>/<prefix>`, with IRSA or a credentials Secret). `devPostgres.enabled` starts a throwaway PostgreSQL in the namespace, for trying replicas out.
- **NATS.** The chart runs one JetStream node. For HA, run a three-node NATS (for example the official chart) and set `nats.url` and `nats.streamReplicas: 3`.
- **Providers.** Put API keys in a Secret and reference it from `server.envFrom` and `workers.envFrom`; judges go in `workers.config.judges`.
- **Scaling.** `keda.enabled` adds a `ScaledObject` per pool on the backlog of its JetStream consumer (`pool-<pool>` on stream `EVALSI_WORK`); `workers.pools.<pool>` sets replicas, concurrency, resources and node selectors (a `gpu` pool, for example).
- **Certificates.** The chart makes an internal CA, kept across upgrades, for the API, the sandbox pools and the workers' client certificates. `tls.certManager.enabled` issues them from cert-manager instead.

### High availability

Every `evalsid` replica serves the API and OTLP ingest. The run scheduler and the policy engine run on one replica at a time, under database leases: when a replica stops, another adopts its runs (resuming them, never double-counting) and takes over the policy engine. Workers pull from the work queues, so a worker that dies mid-task loses nothing: its task is redelivered after the ack wait. The operator runs with leader election when it has more than one replica.

### Air-gapped installs

On a connected machine, bundle the images and charts (and, with `--with-cli`, wheels for the CLI):

```bash
deploy/airgap/bundle.sh --tag 0.4.0 --out evalsi-bundle --with-cli
```

Across the gap, push the images to your registry and install pulling only from it:

```bash
evalsi-bundle/install.sh --registry registry.internal:5000 --with-sandboxd -- \
  --set storage.postgres.dsnSecret.name=evalsi-postgres
```

Images keep their paths under the registry (`registry.internal:5000/abhishek-rnjn/evalsi`, `registry.internal:5000/nats`, ...), which is what the charts' `global.imageRegistry` expects. CI's kind job installs this way, with every image under a registry name that does not resolve, so any image missing from the bundle fails the test. Sandbox images your tasks name are not in the bundle: add them with `EXTRA_IMAGES`.

## The operator and its resources

**`EvalRun`** is a run file applied as a resource; its `spec` is the run spec. The operator creates the run through the API and mirrors its phase, progress, summaries and gates:

```bash
kubectl apply -n evalsi -f examples/runs/capitals.yaml
kubectl get evalruns -n evalsi
# NAME       PHASE       RUN        DONE   TOTAL   AGE
# capitals   Succeeded   r-01J...   15     15      2m
kubectl wait evalrun/capitals -n evalsi --for=condition=Succeeded --timeout=30m
```

- The project is the `evals.si/project` label, else the namespace. The operator creates the project when the server does not know it.
- The spec cannot change after creation (apply a new `EvalRun`); deleting the resource cancels the run, and its results stay on the server.
- `Succeeded` means every gate passed; `Failed`, that a gate failed; `Error`, that the run could not finish (resume it through the API, `RunService.ResumeRun`).

**`OnlineEvalPolicy`** is a policy file applied as a resource. The operator applies it on every spec change, deletes it with the resource, and mirrors its counters (`tracesSeen`, `tracesEvaluated`, ...). Policy names are global on the server: a policy of the same name in another project is reported as a `Conflict`, not taken over.

**`Evaluator`** runs your evaluator plugin: an image with your package installed on top of the evalsi image, serving the pools you name, optionally scaled by KEDA.

```yaml
apiVersion: evals.si/v1alpha1
kind: Evaluator
metadata: {name: support-graders, namespace: evalsi}
spec:
  image: registry.internal/support-graders:1.4   # FROM ghcr.io/abhishek-rnjn/evalsi + your package
  pools: [cpu]
  concurrency: 8
  autoscaling: {minReplicas: 1, maxReplicas: 20, lagThreshold: 10}
```

**`SandboxClass`** (cluster-scoped) is a sandbox pool: the isolation ladder, the minimum level, and the rungs' settings. The operator runs `evalsi-sandbox-<name>` (a Deployment and its Service) and writes the address workers use into `status.address`. Point `sandbox.address` at it.

```yaml
apiVersion: evals.si/v1alpha1
kind: SandboxClass
metadata: {name: gvisor}
spec:
  ladder: [pod]
  minIsolation: kernel
  pod: {runtimeClassName: gvisor, level: kernel, defaultImage: python:3.13-slim, networkPolicyEnforced: true}
```

### Admission webhooks

Every `evals.si` resource gets `evals.si/created-by`: the user the API server authenticated for the create request, which the creator cannot set or change. `EvalRun` and `OnlineEvalPolicy` specs are checked against the API's schema on admission, with the CLI's rules, so a misspelled field fails at `kubectl apply`, not minutes later. The `admin`, `edit` and `view` roles cover the resources through aggregation.

## Sandboxes on Kubernetes

Worker pods hold credentials (provider keys, the cluster's tokens), so sandboxes never run beside them. Workers lease sandboxes from a **sandbox pool** over mutual TLS; pools admit only the workers' certificate (`evalsi-worker`).

| Pool | Rung | Isolation | Needs |
|---|---|---|---|
| `evalsi-sandboxd`, `mode: bwrap` | bubblewrap, Landlock | namespaced | Kubernetes user namespaces (1.33+, a runtime and kernel that support them) |
| `evalsi-sandboxd`, `mode: privileged` | bubblewrap, Landlock | namespaced | A privileged pod (nodes without user namespaces, and kind) |
| `evalsi-sandboxd`, `mode: firecracker` | Firecracker | vm | KVM nodes and a guest kernel on them |
| A `SandboxClass` with `ladder: [pod]` | hardened pod | namespaced; kernel with gVisor, vm with Kata | Nothing special: the pool creates a pod per sandbox |

The **pod rung** creates one pod per sandbox from the task's image. `evalsi-guest` is copied in by an init container; the pod has no service-account token and no service links, runs with all capabilities dropped except the few package managers need, and a NetworkPolicy lets it talk only to its pool, which relays allowed egress through its logging proxy. It cannot snapshot, so environment setup runs once per trial.

## Identity

The chart trusts the cluster's service-account tokens projected for the `evals.si` audience (see [identity](identity.md#on-kubernetes-service-account-tokens)), and makes the operator's service account an owner. CI jobs and agents in the cluster sign in the same way: bind roles to `user:kubernetes/system:serviceaccount:<namespace>:<name>` or `group:kubernetes/system:serviceaccounts:<namespace>`, mount a projected token, and set `EVALSI_TOKEN_FILE`.

## What CI checks

The `kubernetes` job ([`deploy/e2e/kind-e2e.sh`](../../deploy/e2e/kind-e2e.sh)) builds the image, makes an air-gapped bundle, and installs from it into a kind cluster: two API replicas on PostgreSQL, NATS, the workers, the operator and `evalsi-sandboxd`. It then checks that:

- `kubectl apply` of a run file gives the same summaries as `evalsi run -f` of the same file;
- the webhooks refuse an invalid spec and a changed one, and record the creator;
- a policy syncs;
- code evaluation runs on the bubblewrap pool, and an agent run on the pod rung, from a `SandboxClass`.

The operator is also tested against a real API server and etcd (envtest), and the charts by rendering them and loading every `evalsi.yaml` they produce through `evalsid`'s config validation.

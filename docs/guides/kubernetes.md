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
| [`evalsi-crds`](../../deploy/helm/evalsi-crds) | cluster | The CRDs, the admission webhooks and their certificate, roles aggregated into `admin`, `edit` and `view`, and (opt-in) the operator's cluster role |
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

The defaults are a working single-replica install on SQLite. Each chart's permissions are listed under [Permissions](#permissions); on a cluster where you are not cluster-admin, see [Installing without cluster-admin](#installing-without-cluster-admin). For production:

- **Storage.** `storage.postgres.dsnSecret` (a Secret holding the DSN) for several API replicas; `storage.clickhouse` for traces at volume; `storage.s3` for datasets (`datasets_dir` becomes `s3://<bucket>/<prefix>`, with IRSA or a credentials Secret). File datasets (`path:`, `swebench://`) need it: workers run in other pods and cannot read files in the server's. `devPostgres.enabled` starts a throwaway PostgreSQL in the namespace, for trying replicas out.
- **NATS.** The chart runs one JetStream node. For HA, run a three-node NATS (for example the official chart) and set `nats.url` and `nats.streamReplicas: 3`.
- **Providers.** Put API keys in a Secret and reference it from `server.envFrom` and `workers.envFrom`; judges go in `workers.config.judges`.
- **Scaling.** `keda.enabled` adds a `ScaledObject` per pool on the backlog of its JetStream consumer (`pool-<pool>` on stream `EVALSI_WORK`); `workers.pools.<pool>` sets replicas, concurrency, resources and node selectors (a `gpu` pool, for example).
- **Certificates.** The chart makes an internal CA, kept across upgrades, for the API, the sandbox pools and the workers' client certificates. `tls.certManager.enabled` issues them from cert-manager instead.

### Names, and installing beside other charts

Every name a chart creates starts with its release's name, so releases can share a namespace, and the chart can sit beside others (agent-studio-standalone, agent-sandbox). The `evalsi` chart builds the name this way: a release named for evalsi (such as `evalsi`) is used as it is, and any other release gets `-evalsi` after it, so release `team-a` creates `team-a-evalsi` (the API Service), `team-a-evalsi-operator`, `team-a-evalsi-worker-cpu` and so on. `fullnameOverride` sets the prefix. The default release name `evalsi` keeps the short names used throughout these guides.

The other two charts follow the same rule and refer to the evalsi release by value:

| Chart | Its own names | Names it must match in the evalsi release |
|---|---|---|
| `evalsi-crds` | The release name without a trailing `-crds`, prefixed the same way (`evalsi-crds` gives `evalsi-operator`, `evalsi-validating`...); `fullnameOverride` sets it | `operator.fullname`: the evalsi release's full name, for the operator's service account and its webhook Service and certificate |
| `evalsi-sandboxd` | `<release>-sandboxd` (`evalsi-sandboxd` as it is) | `tlsSecret: <fullname>-sandbox-tls` and `allowClients: [<fullname>-worker]` |

For release `team-a`: `helm install team-a-crds deploy/helm/evalsi-crds --set operator.namespace=team-a --set operator.fullname=team-a-evalsi`, then `helm install team-a deploy/helm/evalsi -n team-a`. Install `evalsi-crds` once per evalsi install (its cluster-scoped names carry the prefix, so they do not collide).

Each dependency is either bundled or one you point at:

| Dependency | Bundled (trials only: no backups, no HA) | Existing |
|---|---|---|
| PostgreSQL | `devPostgres.enabled` | `storage.postgres.dsnSecret` |
| NATS JetStream | one node, on by default | `nats.url` (and `nats.streamReplicas`) |
| S3 | `devMinio.enabled` (a MinIO; `storage.s3.bucket` names the bucket, default `evalsi`) | `storage.s3` (endpoint, bucket, `credentialsSecret` or IRSA) |
| ClickHouse | `devClickhouse.enabled` | `storage.clickhouse` |

The bundled MinIO and ClickHouse run non-root with the images CI uses for its own tests. They have been rendered and checked by the chart tests, but not yet run in a cluster by this repository's CI.

### First state from values (the bootstrap Job)

`bootstrap.enabled` runs a Job after every `helm install` and `helm upgrade` (a Helm hook) that makes what the values list, so an install comes up with its projects, keys and defaults already in place:

```yaml
bootstrap:
  enabled: true
  projects:
    - {name: studio, description: agent-studio-standalone}
  apiKeys:                               # shown once, so each is written to a Secret
    - name: studio-ingest
      roles: {studio: [ingest]}
      secret: {name: studio-evalsi-ingest-key}   # key: api_key
    - name: studio-ci
      roles: {studio: [runner]}
      ttl: 2160h
      secret: {name: studio-evalsi-ci-key}
  policies:                              # OnlineEvalPolicy documents
    - {name: studio-quality, project: studio, ...}
  webhooks:                              # see the webhooks guide
    - {name: studio, project: studio, url: "http://studio.studio.svc/hooks/evalsi",
       events: [run.finished, run.gate_failed], secret: {name: studio-evalsi-webhook}}
  credentialGrants:                      # which worker variables each project's specs may name
    - {env: ANTHROPIC_API_KEY, projects: [studio], hosts: [api.anthropic.com]}
```

It is safe to run again. Existing projects, keys and policies are left alone, and a key's Secret is written once and then kept, so an upgrade never rotates a key. A key whose Secret was deleted is replaced: the old key is revoked and a new one is created as `<name>-r2` (then `-r3`...; revoked names stay taken), and the Secret records which key it holds. Roles cannot change on an existing key: rename it in the values to replace it (the Job logs a warning when they differ). A webhook's HMAC secret is generated by the Job and kept the same way.

`credentialGrants` are not created by the Job: the chart writes them into the server's `credentials.grants`, which evalsid reloads without a restart (see [identity](identity.md#8-credentials-which-worker-secrets-a-project-may-use)).

The Job signs in with its own service account's token, projected for the install's audience, and the chart makes that account an owner (creating projects is install-wide), so `auth.kubernetes.enabled` must be on. Its Role may create Secrets and read and replace only the ones the values name. It waits up to `bootstrap.waitTimeout` for the server to answer, and fails at once if the server refuses its credential. It takes tolerations, a node selector, pod labels, resources and the image registry like every other pod the chart adds. It works with Helm 3 and 4: it is a hook, so nothing in it is part of the release's server-side apply.

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

**`SandboxClass`** (cluster-scoped) is a sandbox pool: the isolation ladder, the minimum level, and the rungs' settings. The operator runs `evalsi-sandbox-<name>` (a Deployment and its Service) and writes the address workers use into `status.address`. Point `sandbox.address` at it. Because the kind is cluster-scoped, reconciling it needs a ClusterRoleBinding, so it is opt-in: set `operator.sandboxClasses=true` on both the `evalsi-crds` and the `evalsi` charts (or use the `evalsi` chart's own pool, `sandbox.pool`, which needs no cluster permission).

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

The `EvalRun` and `OnlineEvalPolicy` schemas are generated from the API's messages, so `kubectl explain evalrun.spec.target` documents a field, and a wrong type (a string where `trials` wants a number) fails at the API server. Run files may use either `snake_case` or `camelCase` names, enums by name, and durations such as `10m`.

Every `evals.si` resource gets `evals.si/created-by`: the user the API server authenticated for the create request, which the creator cannot set or change. `EvalRun` and `OnlineEvalPolicy` specs are checked against the API's schema on admission, with the CLI's rules, so a misspelled field fails at `kubectl apply`, not minutes later. The webhook then asks evalsid whether it would accept the resource (`validate_only`): a variable the project has no [credential grant](identity.md#8-credentials-which-worker-secrets-a-project-may-use) for, a judge it may not use, or a spec the server refuses fails at `kubectl apply` too. When evalsid does not answer within 5 seconds, or the project does not exist yet (the operator creates it), the resource is admitted with a warning and its status reports the outcome. The `admin`, `edit` and `view` roles cover the resources through aggregation.

## Sandboxes on Kubernetes

Worker pods hold credentials (provider keys, the cluster's tokens), so sandboxes never run beside them. Workers lease sandboxes from a **sandbox pool** over mutual TLS; pools admit only the workers' certificate (`evalsi-worker`).

| Pool | Rung | Isolation | Needs |
|---|---|---|---|
| `evalsi-sandboxd`, `mode: bwrap` | bubblewrap, Landlock | namespaced | Kubernetes user namespaces (1.33+, a runtime and kernel that support them) |
| `evalsi-sandboxd`, `mode: privileged` | bubblewrap, Landlock | namespaced | A privileged pod (nodes without user namespaces, and kind) |
| `evalsi-sandboxd`, `mode: firecracker` | Firecracker | vm | KVM nodes and a guest kernel on them |
| A `SandboxClass` with `ladder: [pod]` | hardened pod | namespaced; kernel with gVisor, vm with Kata | Nothing special: the pool creates a pod per sandbox |

The **pod rung** creates one pod per sandbox from the task's image. `evalsi-guest` is copied in by an init container; the pod has no service-account token and no service links, runs with all capabilities dropped except the few package managers need, and a NetworkPolicy lets it talk only to its pool, which relays allowed egress through its logging proxy. It cannot snapshot, so environment setup runs once per trial. When the task image runs as a non-root user that cannot create the workdir, set the pod rung's `runAsUser` (with `capabilities: []`): the workdir is then a volume that user owns, seeded with the image's own.

### When bubblewrap pools fail, and when NetworkPolicy is not enforced

`mode: bwrap` runs bubblewrap in a pod user namespace. Two real clusters could not:

- **Ubuntu 24.04 nodes (AKS).** The host restricts what an unprivileged user namespace may configure, and bubblewrap fails with `loopback: Failed RTM_NEWADDR: Operation not permitted`.
- **Nodes whose overlayfs lacks idmapped mounts.** Pod user namespaces (`hostUsers: false`) need them.

Both fail the rung's probe, and the error names the way out: run the pool with `mode: privileged` (bubblewrap in a privileged pod, which needs a namespace that allows it), or use the pod rung (`ladder: [pod]`), which needs neither. `evalsid sandbox probe` shows the reason.

The pod rung's network isolation has two layers: the pool's logging egress proxy, and a NetworkPolicy that admits only the pool. `networkPolicyEnforced` declares that the CNI enforces the policy, which makes isolation report `full`. Evals.si does not trust the declaration: when the probe runs, a canary pod tries to connect to the Kubernetes API, which the policy forbids. If the connection gets through, the probe prints a `warning`, and sandboxes report `partial` with the reason in their isolation notes. This was seen on Calico with Istio ambient (the policy did not stop a labelled pod's egress); the cause there is not established, and a mesh that redirects a pod's traffic through its own proxy is the suspect. On such a cluster, rely on the egress proxy only, or use `mode: firecracker`.

`mode: firecracker` needs `firecracker.defaultImage`, the image a microVM boots when a spec names none (sandboxd boots it at start to check the rung); the evalsi image carries the `firecracker` binary and `mkfs.ext4`. A SandboxClass's `firecracker` takes the same `defaultImage`.

### Taints and service meshes

On nodes that are all tainted, give every component its tolerations: `server`, `workers`, `operator`, `nats`, `devPostgres`, `sandbox.pool` and `sandbox.pool.pod` each take `tolerations` and `nodeSelector` (and the evalsi-sandboxd chart its own).

Under Istio ambient with a waypoint, keep it off the connections it breaks: NATS speaks first, which a waypoint's proxy cannot carry, and a waypoint's external authorization may refuse the pool's calls to sandbox pods. Set `nats.serviceLabels`, `devPostgres.serviceLabels`, `sandbox.pool.pod.labels` (a SandboxClass's `pod.labels`), and the evalsi-sandboxd chart's `serviceLabels` and `podLabels` to `{istio.io/use-waypoint: none}`. Under mesh-wide STRICT mTLS the API server, which is outside the mesh, cannot reach the admission webhooks: set `operator.istio.peerAuthentication: true` and the chart renders a `PeerAuthentication` for the operator with port 9443 `PERMISSIVE` (off by default, since it needs Istio's CRDs).

## Permissions

What each chart creates, and who has to install it.

**`evalsi` (a namespace admin).** Only Roles and RoleBindings in its namespace:

| Service account | Can | Why |
|---|---|---|
| `evalsi-operator` | Deployments, Services, ConfigMaps, KEDA `ScaledObject`s, leases, events, and the `evals.si` resources, in this namespace | Runs `Evaluator` pools and `SandboxClass` pools, leader election, status |
| `evalsi-sandboxd` | Get, list, watch, create and delete pods in this namespace; nothing else | The pod rung: one pod per sandbox |
| `evalsi-worker` | Nothing: no API token is mounted | Workers talk only to `evalsid`, NATS and the sandbox pools |
| `evalsi` | Nothing beyond what every service account has | `evalsid` reads the cluster's service-account issuer and keys (`/.well-known/openid-configuration`, `/openid/v1/jwks`), which Kubernetes grants all service accounts through `system:service-account-issuer-discovery` |

The operator watches only its own namespace unless `operator.allNamespaces` is set. Every pod the chart runs meets Pod Security `restricted`: non-root, no privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp, read-only root filesystems.

**`evalsi-crds` (a cluster admin, once).**

| Object | Scope | Notes |
|---|---|---|
| The four CRDs | cluster | Needed for `kubectl apply` of runs and policies, and for the operator |
| Validating and mutating webhook configurations | cluster | Only for `evals.si` resources; `failurePolicy: Fail` |
| `evalsi-edit`, `evalsi-view` | cluster | Aggregated into the built-in `admin`, `edit` and `view` roles, so namespace admins and editors manage the resources; they grant Evals.si itself nothing |
| `evalsi-operator` ClusterRole and binding | cluster | Only if `operator.sandboxClasses` (read and update SandboxClasses, nothing else) or `operator.allNamespaces` is set; both are off by default |

`operator.allNamespaces` is the one broad grant: the `evals.si` resources in every namespace, and creating Deployments and `ScaledObject`s in any of them (an `Evaluator`'s workers run in its own namespace). Leave it off and install the `evalsi` chart per namespace instead, if that is too much.

**`evalsi-sandboxd` (a cluster admin, or a namespace that allows it).** No RBAC at all, but its pods need privileges: a pod user namespace with an unmasked `/proc` (`mode: bwrap`), a privileged container (`mode: privileged`), or `/dev/kvm` (`mode: firecracker`). The namespace must allow them, which usually means Pod Security `privileged`.

## Installing without cluster-admin

On a cluster with strict RBAC, a team usually gets a namespace, the built-in `admin` role in it, and Pod Security `restricted`. That is enough for everything except `kubectl apply` of runs: install the `evalsi` chart alone, with the namespace-only profile.

```bash
helm install evalsi deploy/helm/evalsi -n my-team -f deploy/helm/evalsi/values-namespaced.yaml \
  --set-json 'rbac.projects={"my-team": {"runner": ["group:kubernetes/system:serviceaccounts:my-team"]}}'
# air-gapped: evalsi-bundle/install.sh --registry ... --namespace my-team --namespace-only -- <helm flags>
```

What changes:

| | Full install | Namespace-only |
|---|---|---|
| Cluster-scoped objects | CRDs, webhooks, aggregated roles | None |
| Runs and policies | `kubectl apply`, the CLI, the API | The CLI and the API (`evalsi run -f run.yaml --server ...`, `evalsi policy apply`); the same files |
| Who started a run | Webhook-stamped `evals.si/created-by`, and the caller on the server | The caller on the server (its token or key), in the audit log |
| Evaluator plugins | `Evaluator` resources | A pool in `workers.pools` with your image (`workers.pools.<pool>.image`, built `FROM` the evalsi image) |
| Sandboxes | `evalsi-sandboxd` or a `SandboxClass` | The chart's pool, `evalsi-sandbox-pool`: a pod per sandbox, or Landlock |
| Pod Security | `restricted` for the `evalsi` chart; sandbox pools need more | `restricted` throughout |

The profile turns the operator off, turns on the chart's sandbox pool (`sandbox.pool`) with the ladder `[pod, landlock]`, and points the workers at it.

- **The pod rung** runs each sandbox as a pod in the namespace, as user 65532 with no capabilities, so it is admitted under `restricted`. The task image's workdir is copied into a volume that user can write, so images built for root still work as long as they do not need root at run time (installing system packages does). For gVisor or Kata, set `sandbox.pool.pod.runtimeClassName` and `level`.
- **Landlock** confines sandboxes inside the pool's own pod, with no Kubernetes API access at all (`sandbox.pool.ladder: [landlock]`). It is weaker (level `confined`: no separate mount or process view), and needs a kernel with Landlock and a container runtime whose default seccomp profile allows its system calls; where they are missing, the pool's probe refuses the rung and sandboxes fail closed. It suits code evaluators, not agent environments that need their own image.

Runs reach the API from CI jobs and agents in the cluster with a projected service-account token (`EVALSI_TOKEN_FILE`; see [Identity](#identity)), or from outside with an API key or your OIDC provider. If the cluster has taken away service accounts' access to its signing keys, set `auth.kubernetes.issuer` and `auth.kubernetes.jwks` (or `jwksConfigMap`); see [identity](identity.md#on-kubernetes-service-account-tokens).

If a cluster admin later installs `evalsi-crds`, turn the operator back on (`operator.enabled=true`) for `kubectl apply`; nothing else changes.

## Identity

The chart trusts the cluster's service-account tokens projected for the `evals.si` audience (see [identity](identity.md#on-kubernetes-service-account-tokens)), and makes the operator's service account an owner. CI jobs and agents in the cluster sign in the same way: bind roles to `user:kubernetes/system:serviceaccount:<namespace>:<name>` or `group:kubernetes/system:serviceaccounts:<namespace>`, mount a projected token, and set `EVALSI_TOKEN_FILE`.

## What CI checks

The `kubernetes` job ([`deploy/e2e/kind-e2e.sh`](../../deploy/e2e/kind-e2e.sh)) builds the image, makes an air-gapped bundle, and installs from it into a kind cluster: two API replicas on PostgreSQL, NATS, the workers, the operator and `evalsi-sandboxd`. It then checks that:

- `kubectl apply` of a run file gives the same summaries as `evalsi run -f` of the same file;
- the webhooks refuse an invalid spec and a changed one, and record the creator;
- a policy syncs;
- code evaluation runs on the bubblewrap pool, and an agent run on the pod rung, from a `SandboxClass`;
- a namespace-only install works for a user who is only `admin` of a namespace that enforces Pod Security `restricted`: it installs with no cluster-scoped object, and code evaluation and an agent run, through the CLI, pass on the chart's pod-rung pool and then on Landlock.

The operator is also tested against a real API server and etcd (envtest), and the charts by rendering them and loading every `evalsi.yaml` they produce through `evalsid`'s config validation.

## Dashboards

evalsid serves Prometheus metrics on `/metrics`: runs, online evaluation (traces, policy stages, window means, alerts) and the Reward Service. The `evalsi` chart can ship a Grafana dashboard for them as a ConfigMap that Grafana's dashboard sidecar picks up:

```bash
helm upgrade evalsi deploy/helm/evalsi -n evalsi --reuse-values --set grafana.dashboard.enabled=true
```

The dashboard's JSON is `deploy/helm/evalsi/dashboards/evalsi.json`, if you import it by hand.

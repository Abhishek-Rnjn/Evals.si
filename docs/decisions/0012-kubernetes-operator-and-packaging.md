# 0012. Kubernetes: the operator as an API client, one image, three charts

- **Status:** Accepted, 2026-10-06 (Phase 4)

## Decision

1. **The operator is a client of the evalsid API, not a second control plane.**
   - `EvalRun` and `OnlineEvalPolicy` are reconciled through `RunService` and `MonitorService`, with the operator's own service-account token. Runs it creates are authorized and audited like any other.
   - Their `spec` is the API message as JSON (`runtime.RawExtension`), validated on admission with the CLI's rules. A run file and a resource are the same document, so `kubectl apply -f` and `evalsi run -f` take the same files; the project comes from the `evals.si/project` label, else the namespace.
   - `EvalRun` specs are immutable; deleting the resource cancels the run and keeps its results. Policy names stay global on the server, and a name taken by another project is a `Conflict`.
   - `Evaluator` and `SandboxClass` render Deployments (and KEDA `ScaledObject`s), because they are workloads, not API objects.
2. **One image.** `evalsid`, `evalsi-guest`, `evalsi-operator` and the Python worker ship together. The pod rung injects `evalsi-guest` from it, and plugin images build `FROM` it.
3. **Three charts.** `evalsi` installs into one namespace and needs no cluster permissions; `evalsi-crds` holds everything cluster-scoped (CRDs, webhooks, cluster roles); `evalsi-sandboxd` holds the privileged node pools. A platform team installs the last two once; teams install `evalsi` per namespace.
4. **Kubernetes identity is OIDC.** The cluster's service-account issuer is one more JWT provider (`kubernetes:`), with keys read from the API server; workloads sign in with projected tokens. Internal hops use mutual TLS from an internal CA (or cert-manager).
5. **The admission webhook records the creator.** `evals.si/created-by` comes from the authenticated admission request and cannot be set or changed by the creator.

## Consequences

- The server stays the single source of truth for runs, results and policies; the operator holds no state the server lacks, and losing it loses nothing.
- Specs are not typed in the CRD's OpenAPI schema (they are preserved as is), so `kubectl explain` cannot describe them; the webhook gives the errors instead.
- User-namespaced sandbox pools need Kubernetes 1.33+ and a kernel and runtime that support them; elsewhere (including kind) the bubblewrap pool runs privileged.
- The pod rung cannot snapshot, so environment setup runs once per trial on it.

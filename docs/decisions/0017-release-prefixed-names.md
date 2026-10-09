# 0017. Charts name what they create after the release

- **Status:** Accepted, 2026-10-09

## Context

The charts used fixed names (`evalsi-operator`, `evalsi-worker`, `evalsi-sandbox-tls`...): "one install per namespace", and the other two charts referred to those names. The product requirement D2 asks the opposite: the chart installs beside others (agent-studio-standalone, agent-sandbox), in any namespace, so a second release, or another chart with a Service called `evalsi`, must not collide. The verification run also used releases named `ev` and `ev-ns`, where the fixed names ignored the release.

Changing every name would break existing installs, whose Deployment selectors cannot change on upgrade, and every document that uses the short names.

## Decision

1. **One helper names everything in the `evalsi` chart:** `evalsi.fullname`. A release whose name contains `evalsi` (the usual release, `evalsi`) is used as it is; any other release gets `-evalsi` after its name (`team-a` gives `team-a-evalsi`). `fullnameOverride` sets it. Every resource name, label value used as a selector, certificate name and DNS name starts with it.
2. **The default release renders exactly what it did.** With release `evalsi` nothing changes except the new `--name-prefix` operator flag, so upgrades keep their selectors and the guides keep their names. This was checked by rendering the old and new charts and comparing them.
3. **Component labels stay shared.** `app.kubernetes.io/name: evalsi-sandboxd` marks sandbox pool pods of every chart and the operator, and the sandbox NetworkPolicy selects on it; it is a component, not a name. Pools are told apart by `app.kubernetes.io/instance`.
4. **The other charts refer to the release by value.** `evalsi-crds` names its cluster-scoped objects after its own release (a trailing `-crds` dropped, then the same rule) and takes `operator.fullname` for the operator it serves; `evalsi-sandboxd` names its objects `<release>-sandboxd` and keeps `tlsSecret` and `allowClients` as values. Installing `evalsi-crds` once per evalsi install is how several installs share a cluster.
5. **The operator is told its prefix** (`--name-prefix`, also its leader-election lease), and names the Deployments it makes from an `Evaluator` or `SandboxClass` with it.
6. **Dependencies are bundled or external, each.** PostgreSQL, S3 and ClickHouse each have a throwaway in-namespace bundle (`devPostgres`, `devMinio`, `devClickhouse`) or take an existing service through `storage.*`; NATS is bundled unless `nats.url` is set. The bundles fill in the storage settings they stand for.

## Consequences

- Chart tests render several releases and check that every name starts with the prefix, that pods refer to Secrets, ConfigMaps and service accounts the release creates, that Services select a workload of the release, and that two releases share no name.
- A release not named for evalsi changes its names when upgraded to this chart (for example `ev` goes from `evalsi-*` to `ev-evalsi-*`). The chart has not been released, so this is accepted now rather than later.
- The sandbox NetworkPolicy still selects every sandbox pool in the namespace by the shared component label, so two releases' pools can reach each other's sandbox pods. Per-release isolation of pool traffic is a follow-up.
- `evalsi-crds` also carries the CRDs, which exist once per cluster (Helm skips CRDs that are already installed). Each evalsi install still needs a release of that chart for its operator's webhooks and cluster role.

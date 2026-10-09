# Re-check checklist

Run these on AKS and on the Istio ambient cluster to confirm the fixes from [PR #12](https://github.com/Abhishek-Rnjn/Evals.si/pull/12) and the changes since. Each row names the finding ([README.md](README.md)) and what a pass looks like. Record the output next to the row, as the original run did in [VERIFICATION-REPORT.md](VERIFICATION-REPORT.md).

Assumes an install in namespace `evalsi` as release `evalsi`, `EVALSI_ADDR` pointing at the API (port-forward `svc/evalsi` 8080), and an admin key in `EVALSI_API_KEY`.

## Status of the kind re-run

The kind end-to-end job (`deploy/e2e/kind-e2e.sh`) was **not run** for this change: the environment that prepared it had no Docker daemon, so no kind cluster. The code paths below are covered by Go and chart tests (`make check`, `go test ./tests/helm/`), which pass. The maintainer should run `deploy/e2e/kind-e2e.sh` once and attach its output here.

## Rows

| # | Finding | Command | Pass |
|---|---------|---------|------|
| 1 | VR-F1 judge `projects` | Add `projects: [e2e]` to a judge under `workers.config.judges` and `judges`; upgrade; wait for the cpu worker | The worker pod stays `Ready`; no `unexpected keyword argument 'projects'` in its log |
| 2 | VR-F2 audit | As a viewer-level key, try to create a key with a broader role; then `evalsi auth audit --denied` | A row for the `privilege escalation` refusal, naming the rule |
| 3 | VR-D4 all-errors run | Run a spec whose evaluator always errors (a judge with a bad key), with no gate | The run ends `ERROR` with `all N evaluations failed; the first: …`, and the CLI exits non-zero |
| 4 | VR-D14 OTLP partial success | Send one trace of more than 2000 spans (the assembler's default limit) over OTLP/HTTP | HTTP 200 with `partial_success.rejected_spans > 0`, and `evalsi_spans_dropped_total` rises |
| 5 | VR-F6, VR-R1 scheduling and labels | `helm template` with `operator.tolerations`, `nats.tolerations`, `devPostgres.tolerations`, `nats.serviceLabels` set; then install on the tainted cluster | Operator, NATS and dev Postgres pods schedule on tainted nodes; Services carry the labels (`kubectl get svc -n evalsi --show-labels`) |
| 6 | VR-F5 registry | Install with `global.imageRegistry=<mirror>` | Pod images are `<mirror>/<repo>:<tag>`, with no doubled path |
| 7 | VR-F3, VR-F4, VR-D10 Firecracker (KVM nodes only) | Install `evalsi-sandboxd` with `mode: firecracker` and `firecracker.defaultImage` set; `evalsid sandbox probe` in a pool pod | `firecracker` is `available: true`; the stock image has `mkfs.ext4` and `firecracker` |
| 8 | VR-D15 bwrap message (AKS) | Install a pool with `mode: bwrap`; `evalsid sandbox probe` | `bwrap` is unavailable and its reason contains `mode: privileged` and `ladder: [pod]` |
| 9 | VR-D8 non-root image | A pod-rung run whose image runs as a non-root user, without `runAsUser` | The error names `runAsUser` as the fix; with `runAsUser` set the run passes |
| 10 | VR-R2 webhooks under STRICT mTLS (mesh) | Upgrade with `operator.istio.peerAuthentication=true`; then `kubectl apply` an `EvalRun` | The apply succeeds (admission webhook reachable); `kubectl get peerauthentication -n evalsi` lists `evalsi-operator-webhook` |
| 11 | Egress NetworkPolicy (Calico + ambient) | Install a SandboxClass with `pod.networkPolicyEnforced: true`; `evalsid sandbox probe` in the pool pod. Then run a pod-rung agent run | If the policy is enforced: no `warning`, runs report `full`. If not: a `warning` naming the canary target, and runs report `partial`. **Either is a pass**; a `full` report with a leaking pod is the failure |
| 12 | VR-R1 waypoint opt-out (mesh) | Set `sandbox.pool.pod.labels` and the Services' labels to `istio.io/use-waypoint: none`; run a pod-rung agent run | The run completes; no connection resets at the waypoint |
| 13 | VR-D3 Helm 4 (either) | Edit the evalsi ConfigMap, then `helm upgrade` with Helm 4 | Works with `--force-conflicts`, as documented |

## Reporting

For every row, record PASS or FAIL, the cluster, and the command output. A FAIL on row 11 in the "full but leaking" direction is a release blocker: isolation is never to be claimed when it is not enforced.

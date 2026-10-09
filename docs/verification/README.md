# Verification

End-to-end checks of Evals.si on real clusters, and where each finding stands now.

| File | What it is |
|------|------------|
| [VERIFICATION-REPORT.md](VERIFICATION-REPORT.md) | The full run: setup, every exit criterion with its evidence, product bugs F1–F6, doc differences D1–D15 and environment problems R1–R2 |
| [FINAL-RESULTS.md](FINAL-RESULTS.md) | One table of every exit criterion across the three clusters |

The run used commit `c5a1de9` on 2026-10-09, on three clusters:

| Cluster | Shape |
|---------|-------|
| kind | Local, arm64, kindnet (no NetworkPolicy enforcement), no KVM |
| AWC | Istio ambient with a forced waypoint and mesh-wide STRICT mTLS, Calico, KVM on every node |
| AKS | Ubuntu 24.04, every node tainted, no NetworkPolicy engine |

The report's finding IDs clash with the PRD's own D1–D11, so other documents refer to them as VR-F1…, VR-D1… and VR-R1….

## Finding status

[PR #12](https://github.com/Abhishek-Rnjn/Evals.si/pull/12) fixed most findings after the run, in commits `033b11c`, `967fdd9` and `8cf4c81`. Those fixes have unit, chart and kind tests. They have not been re-run on AKS or AWC.

| Finding | Status |
|---------|--------|
| VR-F1 judge `projects` crashes workers | Fixed (`033b11c`) |
| VR-F2 audit misses escalation refusals | Fixed (`033b11c`) |
| VR-F3 sandboxd can't set the firecracker default image | Fixed (`967fdd9`): `firecracker.defaultImage`, `diskMB` |
| VR-F4 image lacks `mkfs.ext4` | Fixed (`967fdd9`): the image ships firecracker and e2fsprogs |
| VR-F5 `global.imageRegistry` doubles a path | Fixed (`967fdd9`) |
| VR-F6 no tolerations on operator, NATS, dev Postgres | Fixed (`967fdd9`) |
| VR-D1 no macOS build | Fixed (`8cf4c81`), checked in CI |
| VR-D2 file datasets need S3 on Kubernetes | Documented (`8cf4c81`); still how it works |
| VR-D3 Helm 4 needs `--force-conflicts` after a ConfigMap edit | Documented (`8cf4c81`) |
| VR-D4 an all-errors run succeeds | Fixed (`8cf4c81`) |
| VR-D5 `env_from` is a map | Documented (`8cf4c81`) |
| VR-D6 `wasm run --params` position | Fixed (`8cf4c81`) |
| VR-D7 kind-e2e diagnostics used `id` | Fixed (`8cf4c81`) |
| VR-D8 pod rung with a non-root image | Fixed (`967fdd9`): the error names the fix (`runAsUser`) |
| VR-D9 admission refusals not counted | Counted per replica, now covered by a test (`8cf4c81`) |
| VR-D10 firecracker mode has no binary | Fixed (`967fdd9`) |
| VR-D11 e2e SandboxClass claims enforcement | Fixed (`8cf4c81`) |
| VR-D12 annotation interval off scale | Fixed (`8cf4c81`) |
| VR-D13 TLS EOF log per probe | Fixed (`8cf4c81`) |
| VR-D14 OTLP drops look like success | Fixed (`8cf4c81`): partial success |
| VR-D15 `mode: bwrap` fails on Ubuntu 24.04 (`RTM_NEWADDR`) | **Open**; `mode: privileged` works |
| VR-R1 Istio waypoint breaks NATS and pod-rung calls | Chart values and docs (`967fdd9`): `serviceLabels`, pod `labels` |
| VR-R2 STRICT mTLS blocks admission webhooks | Documented (a PeerAuthentication on port 9443); the chart does not render it. **Open** |
| bwrap fails where overlayfs lacks idmap mounts (AWC) | **Open**; undocumented |
| Sandbox egress NetworkPolicy not enforced on Calico + Istio ambient (AWC) | **Open**; cause unknown |

## Never verified on a real cluster

KEDA scaling a live pool, several-node NATS, a real SWE-bench Verified task, vLLM checkpoint curves, and agentgateway in front of Evals.si. See [LEFTOVERS.md](../LEFTOVERS.md).

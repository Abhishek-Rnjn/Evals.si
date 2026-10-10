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
| VR-D15 `mode: bwrap` fails on Ubuntu 24.04 (`RTM_NEWADDR`) | Mitigated: the error names `mode: privileged` and the pod rung, and the [Kubernetes guide](../guides/kubernetes.md#when-bubblewrap-pools-fail-and-when-networkpolicy-is-not-enforced) documents it. The failure itself is the host's |
| VR-R1 Istio waypoint breaks NATS and pod-rung calls | Chart values and docs (`967fdd9`): `serviceLabels`, pod `labels` |
| VR-R2 STRICT mTLS blocks admission webhooks | Fixed: `operator.istio.peerAuthentication` renders it (off by default); chart-tested, not re-run on a mesh cluster |
| bwrap fails where overlayfs lacks idmap mounts (AWC) | Mitigated: same hint and guide section as VR-D15 |
| Sandbox egress NetworkPolicy not enforced on Calico + Istio ambient (AWC) | Cause still unknown (not our selectors: the policy selects the label the pods carry). Fixed so it is never claimed falsely: the probe runs a canary connection from a sandbox pod and warns, and isolation reports `partial`, when a declared policy is not enforced. Needs a re-run on the mesh cluster ([RECHECK.md](RECHECK.md)) |

## Bug findings of 2026-10-10

[BUG-FINDINGS-2026-10-10.md](BUG-FINDINGS-2026-10-10.md) is a review and a kind run of `64de43d`; its IDs are F01–F31 (F06 retracted). Every fix below has a regression test that fails without it.

| Finding | Status |
|---------|--------|
| F01 reward params weaken `min_isolation` | Fixed (`72f2369`): the stronger level wins, in Go and Python; unknown levels are refused |
| F02 same-project policy replacement skips the stored policy | Fixed (`e317b99`) |
| F03 omitted judge sidesteps rules on the default judge | Fixed (`9224980`): rules see the effective judge (Evaluate, EvaluateStream, rewards, runs, policies); ListRuns and ListPolicies filter with it too |
| F04 stored runs ignore revoked grants | Fixed (`0d6d29f`): checked when a run is prepared and at every batch |
| F05 dataset symlink aliases cross projects | Fixed (`7bb6c49`): a path resolving into another project's promotions is refused |
| F07 failed write-backs never retry | Fixed (`0019eb1`): retried with backoff, honouring Retry-After. Pending scores still live in memory only |
| F08 write-back scores of two policies collide | Fixed (`47589b6`): the policy is part of a write's identity |
| F09 service-scoped `traces.read` cannot drive trace datasets | Fixed (`106899c`): like a trace listing, a trace dataset needs `traces.read` in the project, and each trace (service, labels) is checked as it loads, so service- and label-scoped grants both work. A query matching no readable trace has no records (InvalidArgument) |
| F10 TRL component functions reuse stale rewards | Fixed (`91e973e`) |
| F11 unfinished checkpoint evaluations pass | Fixed (`0586b29`) |
| F12 relative `datasets_dir` under a symlinked working directory | Fixed (`7bb6c49`) |
| F13 `go test ./...` does not build on macOS | Fixed (`7b83bd7`): the guest is Linux-only; sandbox tests accept the platform refusal off Linux |
| F14 worker test sockets too long on macOS | Fixed (`83a995c`) |
| F15 GNU `sed -i` in demo and harness fixtures | Fixed (`267a4d6`) |
| F16 RBAC changes stay on one replica | Fixed (`4895816`): broadcast through NATS, and every replica reloads every 10s. Not re-run with two real replicas |
| F17 operator-made workers crash on a read-only home | Fixed (`33e3e0d`): a writable home. The worker still has no readiness signal of its own |
| F18 operator-made workers lack the release's S3 and sandbox TLS Secrets | Fixed (`33e3e0d`) for Evaluators on the release's worker ConfigMap; envtest and chart tests |
| F19 label-only edits never reach the API | Fixed (`f312d51`) for policies and trace sources; a project-label change moves the object without cleaning up the old project's |
| F20 bundled MinIO cannot write its PID file | Fixed (`67d0ff9`); checked with the pinned image as uid/gid 1001 (bucket, write, read, restart) |
| F21 bootstrap keys refused by default auth | Fixed (`7ad75ab`) |
| F22 sympy grading evaluates answers as Python | Fixed (`aeb5066`): answers are vetted against an allowlist before evaluation. Not run in a separate, resource-limited process |
| F23 shadow replays bypass the stored-run quota | Fixed (`6eec747`); replicas admitting at the same moment can still each take the last slot |
| F24 promotions from several runs duplicate record IDs | Fixed (`866b310`): promoted IDs are `<run>/<record>`, appends skip IDs already present |
| F25–F27 analytics metric names, double counting, missing model | Fixed (`f97f208`) |
| F28 embedded all-error runs pass | Fixed (`db11aee`) |
| F29 reports show another trial's answer | Fixed (`f97f208`): embedded results files record each output's trial |
| F30 reports hide a numeric 1 | Fixed (`f97f208`): only passes are hidden |
| F31 no way to delete stored runs | Open: the quota error now says to raise the quota (`1d0b104`); run deletion is in [LEFTOVERS.md](../LEFTOVERS.md) |

## Never verified on a real cluster

KEDA scaling a live pool, several-node NATS, a real SWE-bench Verified task, vLLM checkpoint curves, and agentgateway in front of Evals.si. See [LEFTOVERS.md](../LEFTOVERS.md).

<!-- Internal host, cluster, registry and path names are redacted as <placeholders>, including inside quoted error messages. Results are unchanged. -->

# Final results (kind = local, AWC = <mesh-cluster>, AKS = <aks-cluster>)

| Phase | Exit criterion | kind | AWC | AKS | Notes |
|---|---|---|---|---|---|
| 0 | `evalsi eval` with CIs | PASS | PASS | n/a | |
| 1 | All pods Ready, images from the chosen registry | PASS* | PASS* | PASS | *F1 workaround; AWC also waypoint opt-out |
| 1 | HA: kill the lease owner, run finishes once | PASS | PASS | not repeated | `adopting run`, 40 distinct results |
| 1 | Parity: embedded vs server vs `kubectl apply` | PASS | PASS (after mTLS relax) | PASS | |
| 1 | Webhooks and `created-by` | PASS | PASS | PASS | AKS `masterclient`, AWC `awc-…` |
| 1 | Policy watch, gate failure exit 3 | PASS | PASS | n/a | |
| 2 | Auth on every surface; viewer limits; model role; SA token | PASS | PASS | not repeated | |
| 2 | Denied calls in `auth audit --denied` | FAIL | FAIL | not repeated | F2 |
| PR #11 | Credential grants, hosts, reload, audit, metrics | PASS | PASS | not repeated | F1 on judges `projects` |
| 3 | Agent run on pod rung | PASS | FAIL | PASS | AWC: waypoint ext-authz 404 |
| 3 | Code eval, bwrap | PASS | NOT RUN | FAIL (`privileged` PASS) | AWC idmap error; AKS RTM_NEWADDR |
| 3 | NetworkPolicy blocks sandbox egress | NOT RUN | FAIL | NOT RUN | kind/AKS: no enforcing CNI; AWC: Calico, not enforced |
| 3 | SWE-bench fixture, identical across rungs | PASS | PASS (firecracker) | n/a | agent 0.5, oracle 1.0 |
| 3 | Real SWE-bench Verified, 1 task | NOT RUN | NOT RUN | NOT RUN | Excluded by user |
| 3 | Firecracker | NOT RUN | PASS | not checked | microVM, `ISOLATION_LEVEL_VM` |
| 4 | Namespace-only, PSA restricted, pod rung | PASS | FAIL (agent), PASS (code) | PASS | AWC: waypoint 404 on agent |
| 4 | Namespace-only on Landlock | NOT RUN | PASS | PASS | Agent run refused by design |
| 4 | Load test | PARTIAL | PASS | n/a | |
| 4 | KEDA | NOT RUN | NOT RUN | NOT RUN | Not installed |
| 5 | ScoreRewards math/code | PASS | PASS (CLI) | n/a | |
| 5 | TRL GRPO (CPU) | PASS | NOT RUN | PASS | AWC pod never scheduled |
| 5 | Checkpoint curves with vLLM | NOT RUN | NOT RUN | NOT RUN | No GPU |
| 6 | MCP over HTTP | PASS | PASS | not repeated | |
| 6 | `evalsi mcp` stdio + coding agent | PASS | n/a | n/a | |
| 6 | ml-classic, ml-monitoring | PASS | PASS | not repeated | |
| 6 | Guardrails | PASS | PASS | not repeated | agentgateway NOT RUN |
| 6 | Wasm | PASS | n/a | n/a | |
| 6 | Annotation queue | PASS | PASS | not repeated | alpha 0.571 |
| 6 | Web UI | PASS | PASS | not repeated | |

Failure logs (last 50 lines): /tmp/evalsi-verify/failures/ (judge-projects-*.log, remote-pod-rung-*.log, remote-ns2-pool.log, aks-bwrap-sandboxd.log, aks-grpo-attempt{1,2}.log).
Details, product bugs F1-F6, doc differences D1-D15: VERIFICATION-REPORT.md.

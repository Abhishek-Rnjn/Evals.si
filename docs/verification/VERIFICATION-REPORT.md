<!-- Internal host, cluster, registry and path names are redacted as <placeholders>, including inside quoted error messages. Results are unchanged. -->

# Evals.si end-to-end verification — interim report

Status as of 2026-10-09. The kind run is complete; the remote-cluster run is partway through and paused.

## What was verified

| | |
|---|---|
| Code | `main@c5a1de9` (the merge of PR #11, `per-project-creds`). The checkout moved from `per-project-creds` to `main` during the session, outside this verification. The later `d464381` changes only `website/`. Every image reports `version: verify-c5a1de9`. |
| Tools | kubectl 1.36.0, helm **4.2.4** (accepted in place of 3.x), docker 28.5.1, go 1.26.0, uv 0.11.15, Python 3.13.13 via uv (system python3 is 3.10) |
| Registry | `<private-registry>` (anonymous pull). Postgres is `<private-registry>/hardened/postgres:18` instead of the chart's `postgres:17`, because Docker Hub rate-limited this network. |
| Releases | `ev-crds`, `ev`, `ev-sandboxd` in namespace `evalsi-verify`; `ev` in `evalsi-verify-restricted` (namespace-only, kind only) |
| Not changed | No repo code was changed. All specs and values are copies under `/tmp/evalsi-verify`. |

### Clusters

| | kind `evalsi-verify` (local) | remote `<mesh-cluster>` |
|---|---|---|
| Nodes | 1 control plane + 1 worker, arm64, kernel 6.10.14-linuxkit | 3 control plane + 3 workers (`c04m08`), amd64, kernel 5.15.0-179-generic, containerd 2.1.4, k8s v1.34.3 |
| /dev/kvm | no (`vmx/svm` 0) | **yes on all 6 nodes** (`vmx/svm cpu flag lines: 8`) |
| GPU / KEDA / agentgateway | none / none / none | none / none / none |
| CNI | kindnet, does **not** enforce NetworkPolicy (observed) | Calico, plus Istio ambient with mesh-wide STRICT mTLS (`istio-system/default-awc`) |
| Landlock | not in kernel | in the LSM list on every node |
| sandboxd mode | `privileged` (kind nodes are containers) | `bwrap` failed: `snapshotter "overlayfs" doesn't support idmap mounts on this host`. Running `privileged`. |
| Images | `kind.local/…`, loaded with `kind load` | pulled from the registry |

A first remote cluster (`<first-mesh-cluster>`) was abandoned: its API server became unreachable after the install had hit the Istio waypoint problem (see R1). Its leftovers are listed under Cleanup.

## Results

PASS = observed output proves it. NOT RUN = couldn't run, with the reason. BLOCKED = waiting on a decision.

| Phase | Exit criterion | kind | remote | Evidence (key output) | Notes |
|---|---|---|---|---|---|
| 1 | All pods Ready, images only from the chosen source | PASS* | PASS* | kind 10/10 Ready, remote 12/12 Ready, 0 restarts; `not from …: (none)` | *Needs the F1 workaround on both. Remote also needs the waypoint opt-out (R1). The platform's waypoint pod is excluded. |
| 1 | HA: kill the evalsid replica that owns the run; it finishes once | PASS | not yet | lease `run:run-8e72866d8bc98180` held by pod `f945z` → deleted → `msg="adopting run"` on `xsqqw` after 3 s → `result count: 40 \| distinct: 40 \| duplicates: []` | Policy-engine lease failed over too. |
| 0 | `evalsi eval` with CIs | PASS | PASS | `exact-match 6 0.333 [0.097, 0.700]`, `llm-judge 6 0.667 [0.125, 1.000]` | `evalsi eval` is embedded only (no `--server`). Judge via `<ai-gateway>`. |
| 1 | Parity, `parity-run.yaml`: embedded, `--server`, `kubectl apply` | PASS | PARTIAL | identical: exact-match n=5 mean 0.8 CI [0.375535, 0.963776]; numeric-match n=3 mean 1.0 | Remote: embedded and server identical; `kubectl apply` BLOCKED (R2). |
| 1 | Parity, `capitals.yaml` (model) | PASS | PARTIAL | kind: server and kubectl identical on 4 metrics. Remote server: exact-match 15/15, pass@3 1.0, llm-judge 1.0 | Copy with `base_url` set to the gateway and inline records. Remote kubectl BLOCKED (R2). |
| 1 | Webhook records the creator | PASS | BLOCKED | `created-by=kubernetes-admin` | R2. |
| 1 | Watch: policy stats rise, online scores stored | PASS | BLOCKED | kind: `seen 0 → 5 → 8`; `GetTrace` → `latency number: 1000` | Remote: OTLP ingest returns 200; policy via `kubectl apply` BLOCKED (R2). `evalsi policy apply` through the API not tried yet. |
| 1 | Failing gate: EvalRun `Failed`, `evalsi run` exit 3 | PASS | PARTIAL | `[FAIL] exact-match mean min 0.9 (mean 0.8 is below 0.9)`, `exit code=3` | Remote CLI exit 3 PASS; EvalRun BLOCKED (R2). |
| 2 | Every surface rejects unauthenticated calls | PASS | PASS | Connect/REST/metrics/OTLP-HTTP `401`; gRPC and OTLP-gRPC `grpc-status=16 a credential is required`; owner key gets 200 / 0 | |
| 2 | Viewer can't start runs or read other projects' traces | PASS | PASS | `permission_denied: runs.create …`; other project's trace → `not_found` | |
| 2 | Model-condition role | PASS | PASS | `allowed-model` → run created; `claude-opus-5-5` → `permission_denied` | |
| 2 | Project admin can't grant what it lacks | PASS | PASS | `privilege escalation: you would grant permissions you do not hold … runs.create` | Shown with a narrower role, `mini-admin` (`access.manage`, `runs.read`). |
| 2 | Projected SA token (`EVALSI_TOKEN_FILE`) runs a gated eval | PASS | PASS | `user:kubernetes/system:serviceaccount:evalsi-verify:ci-runner (service via jwt)`, `credential: token-file`, `[pass]`, exit 0 | Stands in for the GitHub Actions OIDC criterion. Remote went through the mesh. |
| 2 | Each denied call in `auth audit --denied` with its rule | **FAIL** | **FAIL** | RBAC denials logged; escalation refusals missing | Bug F2. |
| PR #11 | Ungranted var (`DATABASE_URL`) refused | PASS | PASS | `…names the worker variable DATABASE_URL, which project "e2e" may not use…` | |
| PR #11 | Empty `api_key_env` counts as `OPENAI_API_KEY` | PASS | PASS | refused in `other`, accepted in `e2e` | |
| PR #11 | Host-limited grant: other host and plain http refused | PASS | PASS | `would send HOSTED_TOKEN to evil.example.net…`; `…over plain http; use https…` | |
| PR #11 | `api_key_env: none` needs no grant | PASS | PASS | run created in `other` | |
| PR #11 | CLI `env_from` with a host-limited grant refused | PASS | PASS | `env_from.TOKEN copies HOSTED_TOKEN into a sandbox, but its grant limits it to hosts api.example.com` | `env_from` is a map (D5). |
| PR #11 | `kubectl apply` refused at admission | PASS | BLOCKED | `admission webhook "validate.evals.si" denied the request: the evalsi API refuses it: … DATABASE_URL …` | Remote: R2. |
| PR #11 | `credentials list` | PASS | PASS | e2e: 4 grants + judges `claude, claude-e2e`; other: ANTHROPIC only + `claude` | Names and hosts only. |
| PR #11 | Audit `credentials.use` and `evalsi_credential_denied_total` | PASS | PASS | 6–7 `credentials.use DENY` rows; `…{name="HOSTED_TOKEN"} 3` | Admission refusals audited but not counted (D9). |
| PR #11 | Judge with `projects` refused for other projects | PASS* | PASS* | `uses judge "claude-e2e", which project "other" may not use` | *Server side only; worker side is F1. |
| PR #11 | Reload: ConfigMap edit applies within about a minute, no restart | PASS | PASS | kind accepted after 88 s, remote after 47 s; restarts `0 → 0` | Helm 4 then needs `--force-conflicts` (D3). |
| 3 | Agent run on the pod rung (SandboxClass) | PASS | BLOCKED | `agent-pod` `task-success 1`, 3 sandbox pods, 0 left | Remote SandboxClass apply blocked (R2). |
| 3 | Code evaluation on bubblewrap | PASS | in progress | `unit-tests 0.5`, `isolation.driver=bwrap`, cgroup in `evalsi-sandboxd-…` | |
| 3 | NetworkPolicy blocks sandbox egress | NOT RUN (kind) | not yet | kind: labelled pod `1.1.1.1:443 CONNECTED` | kindnet doesn't enforce NetworkPolicy. Remote has Calico. |
| 3 | SWE-bench fixture, BYO CLI agent, identical across rungs | PASS | not yet | agent 0.5, oracle 1.0, same per-record outcomes on `pod` and `bwrap` | Needed S3 (moto, D2) and an adapter worker image. |
| 3 | Real SWE-bench Verified (1 task) | NOT RUN | not yet | | Planned for remote (amd64). |
| 3 | Firecracker | NOT RUN (no KVM) | not yet | Remote has KVM. Images pushed: `evalsi-sandboxd-fc:verify` (firecracker v1.17.0) and `evalsi-fc-kernel:verify` (vmlinux-6.1.155) | The chart's firecracker mode ships no firecracker binary (D10). |
| 4 | Namespace-only install under PSA `restricted` | PASS | not yet | alice: `no` for every cluster-scoped verb; 0 cluster-scoped objects; pod rung `unit-tests 0.5`, `task-success 1` | |
| 4 | …then on Landlock | NOT RUN (kind) | not yet | `landlock is not enabled in this kernel` | Fails closed. Remote kernel has Landlock. |
| 4 | Load test vs §14, scaled | PARTIAL | not yet | Evaluate p50 1.08 ms / p95 1.59 ms (target p50 < 20 ms); freshness p95 4.2 s (target < 2 min); 1,000-record run at 6,920 rec/s; OTLP stored about **8.1k spans/s** on one replica | OTLP below the ≥ 20k spans/s target (one replica on a laptop). Warm-lease target needs Firecracker. |
| 4 | KEDA | NOT RUN | NOT RUN | | Not installed. |
| 5 | ScoreRewards: math-equiv, code-exec-tests | PASS | not yet | math mean 0.8500, code 0.6450 (both match hand-computed values); about 400 and 34 rollouts/s | ScoreRewards output doesn't expose sandbox provenance. |
| 5 | TRL GRPO exit test | PASS (CPU wiring) | n/a | `1 passed in 11.30s` in CI's trainers env, real bwrap sandbox | Tiny random model; "target throughput" not shown. |
| 5 | Checkpoint curves with a regression gate (vLLM) | NOT RUN | NOT RUN | | No GPU. |
| 6 | MCP over HTTP with audience-bound token | PASS | not yet | wrong audience → `invalid_token … audience does not include …/mcp`; `run` tool `succeeded` and compared | Remote needs the MCP values upgrade. |
| 6 | `evalsi mcp` (stdio) and a coding agent evaluating its own change | PASS | n/a | Claude Code via `evalsi mcp`: exact-match 0.8 → 1.0, gates pass (checked against results files) | Change made in a scratch copy, not the repo. |
| 6 | ml-classic and ml-monitoring | PASS | not yet | accuracy 0.900 (matches data), ROC-AUC 0.988; drift flagged 1 vs control 0 | |
| 6 | Guardrails pass / mask / block | PASS | not yet | `<EMAIL> about <ACCOUNT>`; block `status_code: 403` | agentgateway in front NOT RUN (not installed). |
| 6 | Wasm check and run | PASS | n/a | `pinned fe9c8052…`; 3 evaluators; `4 words; the limit is 3` | Local only: no chart value to mount plugins (known gap). |
| 6 | Annotation queue, two annotators, alpha | PASS | not yet | α `0.571` (checked by hand); `accuracy=1.000 cohenKappa=1.000` | |
| 6 | Web UI | PASS | not yet | 7 routes and the Credentials section render (screenshots in `/tmp/evalsi-verify/ui/`) | |

## Product bugs

**F1. A judge with `projects` crashes every worker.** PR #11 added `Judge.Projects` (`internal/config/config.go:44`). `internal/pluginhost/pluginhost.go:105` marshals the whole judge into `judges.json`, and the Python `JudgeConfig` (`python/evalsi/src/evalsi/judges/__init__.py:41`) rejects the field. evalsid then exits waiting for a cpu worker.

```
File "/opt/evalsi/venv/lib/python3.13/site-packages/evalsi/worker.py", line 294, in load_judges
  return {name: JudgeConfig(**fields) for name, fields in data.items()}
TypeError: JudgeConfig.__init__() got an unexpected keyword argument 'projects'
evalsid: pluginhost: worker exited during startup; see its output above
--- evalsid:
level=INFO msg="waiting for a cpu worker to describe the evaluators"
evalsid: describing evaluators: context canceled
```

Workaround used on both clusters: keep `projects` on the server's `judges` only, and drop it from `workers.config.judges`.

**F2. The audit log misses privilege-escalation refusals.** `permission_denied: privilege escalation …` responses are never written to the audit log. Reproduced on both clusters: only the allowed `mini-reader` create appears. Unauthenticated rejections aren't audited either. Denials caused by a failed role condition report the generic `no role or rule grants …` instead of naming the condition.

## Environment problems on the remote clusters

**R1. Istio ambient plus a forced waypoint breaks server-first protocols.** `auth-config-operator` (validating and mutating namespace webhooks) forces `istio.io/dataplane-mode=ambient` and `use-waypoint` on every non-platform namespace, and the mesh controller creates a waypoint. NATS clients time out through it: `evalsid: cluster: connecting to nats://evalsi-nats.evalsi-verify.svc:4222: … i/o timeout`. Opting the namespace out is refused (`label "istio.io/dataplane-mode" has incorrect value "none", want "ambient"`). The approved fix was `istio.io/use-waypoint=none` on the `evalsi-nats` and `evalsi-dev-postgres` Services. It's outside Helm; the chart has no Service or pod label values.

**R2. Admission webhooks unreachable under STRICT mTLS.** The kube-apiserver isn't in the mesh, and mesh-wide `PeerAuthentication STRICT` makes ztunnel drop its call: `failed calling webhook "mutate.evals.si": … Post "https://evalsi-operator-webhook.evalsi-verify.svc:443/mutate-evals-si?timeout=10s": EOF`. Every `kubectl apply` of an evals.si resource fails (EvalRun, OnlineEvalPolicy, SandboxClass). A fix needs a decision: a namespace PeerAuthentication `PERMISSIVE` on the webhook port, or the operator pod out of the mesh. Either relaxes a platform security control. **Pending your decision.**

## Differences from the docs

- **D1.** `evalsid` doesn't build on macOS: `internal/sandbox/firecracker.go:450: unknown field Pdeathsig in struct literal of type "syscall".SysProcAttr`. The file needs a Linux build tag.
- **D2.** On Kubernetes, `path` and `swebench://` file datasets work only with `storage.s3`. The server resolves the path inside its own pod (`internal/runs/runs.go:347`), and a worker pod loads it but can't see the file. The guides say "put the dataset under the server's datasets_dir".
- **D3.** With Helm 4, after the in-place ConfigMap edit the identity guide recommends, `helm upgrade` fails until `--force-conflicts`: `conflict with "kubectl-replace" using v1: .data.evalsi.yaml`.
- **D4.** A run where every evaluation errors ends `RUN_STATUS_SUCCEEDED` with exit 0 when it has no gate.
- **D5.** `cli.env_from` is a map (`{SANDBOX_VAR: WORKER_VAR}`), but `agent-runs.md` reads as if it were a list.
- **D6.** `evalsid wasm run` usage shows `[--params JSON]` after the positional arguments; it only works before them.
- **D7.** `deploy/e2e/kind-e2e.sh`'s `results()` sends `{"id": …}` to `ListRunResults`, but the field is `runId`, so CI diagnostics print nothing.
- **D8.** The SandboxClass pod rung fails with a non-root task image: `creating the workdir /workspace: … mkdirat workspace: permission denied`.
- **D9.** Admission (`validate_only`) credential refusals are audited but not counted in `evalsi_credential_denied_total`.
- **D10.** The sandboxd chart's `mode: firecracker` uses the evalsi image, which contains no `firecracker` binary.
- **D11.** `deploy/e2e/pod-sandboxclass.yaml` sets `networkPolicyEnforced: true`, but kind's kindnet doesn't enforce NetworkPolicy here.
- **D12.** The annotation score interval isn't clipped to the question's scale: `[1.579, 6.021]` with `max: 5`.
- **D13.** sandboxd logs `http: TLS handshake error … EOF` every 5 s, from the kubelet's TCP probe on the TLS port.
- **D14.** OTLP accepts spans with HTTP 200 and later drops spans of a trace over the span limit (`evalsi_spans_dropped_total`). An overload looks like success to clients.

## Deviations and mistakes during the run

- Specs adapted in copies: `capitals.yaml` (gateway `base_url`, inline records), the e2e SandboxClass (`defaultImage` under the chosen registry; for SWE-bench a root `python:3.13-slim` plus git image), and specs relabelled to project `restricted` for the namespace-only install.
- The model credential is `ANTHROPIC_AUTH_TOKEN` (your gateway token), stored as Secret `evalsi-model-keys` in `evalsi-verify` on kind and on the remote cluster, with your approval.
- First remote cluster: the `ev-crds` Helm release record went to namespace `default` (missing `-n`).
- During the remote reload test, a script whose kubeconfig variable I hadn't rewritten edited **kind's** `evalsi` ConfigMap (it added a `DATABASE_URL` grant). The remote admission and reload checks were then rerun against remote. Kind is ours, but its config has drifted until cleanup.
- Three API keys whose plaintexts I lost (`mini`, `mini2` on kind) remain, unusable, until their 1-day TTL.
- The first OTLP load result (356k spans/s) was my load generator's fault (it re-sent the same trace IDs), and was replaced by the unique-trace rerun.

## Remaining work on the remote cluster

1. Decide R2 (webhook mTLS). It unblocks `kubectl apply` parity, the webhook, watch via CRD, the EvalRun gate, SandboxClass and the pod rung.
2. Code eval on bwrap, NetworkPolicy (Calico), HA, rewards, ml, guardrails, annotation, UI and MCP HTTP on remote.
3. Firecracker: copy the kernel to `/var/lib/evalsi/vmlinux` on the 3 workers (approved), switch sandboxd to `mode: firecracker` with `evalsi-sandboxd-fc`, then the SWE-bench fixture on firecracker and bwrap (plus the pod rung once R2 is fixed), and 1 real SWE-bench Verified task.
4. Namespace-only install with Landlock in `evalsi-verify-restricted` (expect the same mesh labels).
5. Load test on remote.

## Cleanup (not run; needs your OK)

```bash
# kind (or simply: kind delete cluster --name evalsi-verify)
export KUBECONFIG=/tmp/evalsi-verify/kind.kubeconfig
helm uninstall ev -n evalsi-verify-restricted
helm uninstall ev-sandboxd -n evalsi-verify
helm uninstall ev -n evalsi-verify
helm uninstall ev-crds -n evalsi-verify
kubectl delete sandboxclass pods
kubectl delete namespace evalsi-verify evalsi-verify-restricted

# remote <mesh-cluster> (kubeconfig.yaml)
export KUBECONFIG=~/kubeconfig.yaml
helm uninstall ev-sandboxd -n evalsi-verify
helm uninstall ev -n evalsi-verify
helm uninstall ev-crds -n evalsi-verify
kubectl delete namespace evalsi-verify

# first remote <first-mesh-cluster> (needs its old kubeconfig)
helm uninstall ev -n evalsi-verify
helm uninstall ev-crds              # release record is in default
kubectl delete namespace evalsi-verify
```

Also: images under `<private-registry>/`, the allow rules added to `.claude/settings.local.json`, and the scratch files in `/tmp/evalsi-verify` (including `keys.env`).

## Remote update: Firecracker rung (<mesh-cluster>)

| Check | Result | Evidence |
|---|---|---|
| Guest kernel on nodes | PASS | `/var/lib/evalsi/vmlinux` on 6 nodes, sha256 e20e46d0...af3d4f2 matches source |
| sandboxd `mode=firecracker` | PASS | DaemonSet 3/3 Ready, image `evalsi-sandboxd-fc:verify` (firecracker v1.17.0 + e2fsprogs) |
| Code eval in microVM (`sandbox-run.yaml`) | PASS | run-2deb29a477016d82: good passed=true, bad passed=false (AssertionError); isolation driver=firecracker level=vm enforcement=full network=none |
| SWE-bench fixture, agent | PASS | task-success 0.5, gate min 0.5 passed; calc-1 pass, calc-2 fail; ISOLATION_LEVEL_VM; matches kind pod/bwrap |
| SWE-bench fixture, oracle | PASS | task-success 1.0, both records pass; firecracker/VM |
| 1 real SWE-bench Verified task | NOT RUN | not attempted on remote yet (needs per-instance image pull + larger guest disk) |

New findings:
- F3: evalsi-sandboxd chart cannot set `sandbox.firecracker.default_image` (template hardcodes the firecracker dict). First run: `a microVM needs an image (set sandbox.firecracker.default_image)`. Workaround: patched ConfigMap `evalsi-sandboxd` in-cluster (default_image = registry python-git:3.13-slim); no repo change.
- F4: stock image lacks `mkfs.ext4`. Error: `firecracker: mkfs.ext4 not found (e2fsprogs builds the VM root disks)`. Workaround: derived image adds e2fsprogs.
- F5: `workers.pools.<pool>.image` with `global.imageRegistry` set drops the first path segment as a host; `<registry>/<org>/x:tag` became `<registry>/<org>/<org>/x:tag` (ImagePullBackOff). Use `ghcr.io/x:tag` form.
- Helm upgrade of `ev` run with `--force-conflicts`; waypoint opt-out labels on NATS/Postgres Services persisted ("not labeled" = already set).

## Remote update 2: webhook, HA, annotation, guardrails, ml, MCP, load (<mesh-cluster>)

mTLS relaxation (approved): PeerAuthentication `ev-operator-webhook` in evalsi-verify, selector app.kubernetes.io/name=evalsi-operator, mtls STRICT with portLevelMtls 9443 PERMISSIVE. Before: operator webhook EOF through mesh STRICT. After: `kubectl apply --dry-run=server` of EvalRun accepted.

| Check | Result | Evidence |
|---|---|---|
| kubectl apply == evalsi run -f (parity) | PASS | phase Succeeded; exact-match n=5 mean 0.8, numeric-match n=3 mean 1.0 both paths; metrics sets equal |
| Webhooks | PASS | invalid gate: `admission webhook "validate.evals.si" denied the request: gate on "exact-match" needs min or max`; spec patch: `an EvalRun's spec cannot change; create a new EvalRun`; created-by annotation present (value `awc-…`, the mesh/platform identity, not kubernetes-admin) |
| OnlineEvalPolicy | PASS | `onlineevalpolicy/e2e-latency condition met` (Synced) |
| SandboxClass `pods` | PASS | Ready, address tls://evalsi-sandbox-pods.evalsi-verify.svc:7443 |
| Agent run on pod rung | FAIL | both records OUTCOME_ERROR: `SANDBOX_UNAVAILABLE ... guest agent: unimplemented: HTTP 404 Not Found URI <platform-ext-authz-url>`. The sandbox pod was created, pulled python-git, started; server->pod gRPC goes through the namespace waypoint, whose ext-authz  returns 404. SandboxClass has no pod label field to opt pods out (istio.io/use-waypoint=none); namespace label is forced by auth-config-operator. Logs: /tmp/evalsi-verify/failures/remote-pod-rung-{pool,worker}.log |
| HA pod kill | PASS | killed lease owner evalsi-6d648df78b-msq22 mid-run (2/80); other replica: `adopting run`; final SUCCEEDED 80/80, 40 results, 40 distinct, no duplicates |
| Annotation queue | PASS | 2 annotators x5 items; viewer NextItem denied (`annotations.write ... not allowed`); stats alpha 0.571 |
| Guardrails | PASS | pass/mask/block verdicts, viewer denied 403, metrics evalsi_guardrail_checks_total{verdict=block|mask|pass}=1 each |
| ml-classic / drift | PASS | classify accuracy 0.9; drift control psi 0.051 drifted=0; shifted psi 2.654 drifted=1 |
| MCP over HTTP | PASS | 401 unauth; wrong audience 401 `audience does not include ...`; right audience 200, tools list_evaluators/evaluate/run/get_run/compare_runs; run tool succeeded, gate passed, compare_runs worked |
| Load (in-cluster Job) | PASS | evaluate 1220 calls/s p95 24 ms; OTLP 387k spans/s, non_200=0; unique 200k spans accepted; 1000-record run 3985 rec/s |
| NetworkPolicy (Calico) | FAIL | operator-created `evalsi-sandbox-pods` NP (egress only to sandboxd) selects `evals.si/sandbox=true`. A Job pod with that label (np2-true) reached postgres, nats, 1.1.1.1:443 and evalsi:8080 (all REACHABLE), same as control. Calico is installed; no Calico GlobalNetworkPolicy; cause unknown (ambient mesh interaction suspected) |
| Rewards `score` | PASS (cluster-independent) | same CLI result as kind |
| Web UI | NOT RUN | needs `uv run --with playwright`; blocked by the auto-mode classifier |
| bwrap code eval | NOT RUN | sandboxd bwrap fails on these nodes: `snapshotter "overlayfs" doesn't support idmap mounts on this host` |
| Namespace-only install + Landlock | NOT RUN | not attempted yet |
| 1 real SWE-bench Verified task | NOT RUN | not attempted yet |
| CPU GRPO run (remote) | NOT RUN | done on kind only |

Notes: after `helm upgrade ev` the port-forward on 28080 drops; restart pf.sh. Remote server now has values-mcp.yaml applied and sandbox.address back on evalsi-sandboxd.

## Remote update 3: namespace-only, Landlock, UI

| Check | Result | Evidence |
|---|---|---|
| Namespace-only install in evalsi-verify-restricted (PSA restricted, as `alice` with built-in admin) | PASS | alice cannot create clusterroles/clusterrolebindings/CRDs/validatingwebhookconfigurations (all "no"); `helm upgrade --install ev-ns ... --kube-as-user alice -f values-namespaced.yaml` deployed; all pods Running (server, 4 workers, sandbox-pool, NATS, dev Postgres) |
| Namespace-only: code eval (sandbox-run) | PASS | RUN_STATUS_SUCCEEDED, unit-tests n=2 mean 0.5, as SA `ci` projected token |
| Namespace-only: agent run on pod rung | FAIL | sandbox pod started and ran, then `guest agent: unimplemented: HTTP 404 ... <platform-ext-authz-url>` (same waypoint ext-authz cause as evalsi-verify). Log: failures/remote-ns2-pool.log. First attempt also hit F5 (doubled registry path for sandbox.pool.pod.defaultImage) |
| Landlock ladder: code eval | PASS | ladder=[landlock]: sandbox-run SUCCEEDED, unit-tests 0.5 |
| Landlock ladder: agent run (min_isolation namespaced) | EXPECTED REFUSAL | `landlock: level confined is below namespaced`; both records SANDBOX_UNAVAILABLE |
| Web UI | PASS | sign-in as owner (whoami key:owner); project selector lists default,e2e,other,quickstart,support; runs table 24 rows; policies 2; queues 2; capitals-check shows Questions; catalog 62 rows; credentials section for e2e lists ANTHROPIC/OPENAI/HOSTED tokens. One 404 resource in the browser console. First run showed empty tables (4 s waits needed over port-forward) |
| CPU GRPO on remote | NOT RUN | amd64 trainers image built and pushed (<private-registry>/evalsi-trainers:verify, sha256:7b827382...), but creating the privileged Job was blocked by the auto-mode classifier |
| bwrap code eval | NOT RUN | `snapshotter "overlayfs" doesn't support idmap mounts on this host` at pod start |
| Real SWE-bench Verified task | NOT RUN | excluded by the user |

## AKS cluster: the checks AWC could not run (<aks-cluster>, kube-azure.yaml)

Scope, as agreed: only the checks that were blocked or never ran on AWC. Namespaces `evalsi-verify` and `evalsi-verify-restricted`. Releases `ev-crds`, `ev`, `ev-sandboxd` (and `ev-ns` in the restricted namespace). Same images and charts, no repo change.

Cluster: k8s v1.36.4, 3 nodes (amd64, Ubuntu 24.04.5, kernel 6.8.0-1067-azure, containerd 2.3.3-2): 2 `infra` (3.86 CPU / 12 GiB, taint `CriticalAddonsOnly`) and 1 `mlinfra` (7.8 CPU / 27 GiB, taint `role.node.kubernetes.io/infra`). Istio ambient is installed (ztunnel), but the new namespaces carry no mesh labels: no waypoint, no STRICT-mTLS problem. CNI is Azure CNS with **no network-policy engine** (node label `kubernetes.azure.com/network-policy=none`, no Calico/Cilium/NPM). Image pulls from the private registry and the hardened Postgres from the private registry worked anonymously.

| Phase | Exit criterion | Result | Evidence | Notes |
|---|---|---|---|---|
| 1 | Install: all pods Ready | PASS | 10/10 Ready (2 server, operator, 4 workers, sandbox-pool, NATS, dev Postgres), all on the `mlinfra` node | Needed tolerations (F6) |
| 1 | `kubectl apply` parity with `evalsi run -f` | PASS | exact-match n=5 mean 0.8; numeric-match n=3 mean 1.0 on both paths; same metric sets | |
| 1 | Webhooks | PASS | `admission webhook "validate.evals.si" denied the request: gate on "exact-match" needs min or max`; `an EvalRun's spec cannot change; create a new EvalRun`; `created-by: masterclient` | No R2 here. The creator is the kubeconfig's identity, not `kubernetes-admin`. |
| 1 | OnlineEvalPolicy Synced | PASS | `onlineevalpolicy/e2e-latency condition met` | |
| 3 | Agent run on the pod rung | PASS | `agent` Succeeded, task-success n=2 mean 1; sandbox pods `evalsi-sandbox-88c9b`, `-hcjws` created, 0 left | Blocked on AWC (waypoint ext-authz 404) |
| 3 | Code eval, `bwrap` mode | FAIL | both records `SandboxUnavailable: … bwrap: probe failed (runner_failure, exit 1): bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted` | Log: failures/aks-bwrap-sandboxd.log |
| 3 | Code eval, `privileged` mode | PASS | unit-tests n=2 mean 0.5; good `passed=True`, bad `False`; isolation driver `bwrap`, level `namespaced` | The chart's documented fallback |
| 3 | NetworkPolicy blocks sandbox egress | NOT RUN | A pod labelled `evals.si/sandbox=true` reached postgres, nats, `1.1.1.1:443` and the evalsi API, same as a control pod | The cluster has no policy engine; not a product failure |
| 4 | Namespace-only install under PSA `restricted` | PASS | `alice`: `no` for clusterroles, clusterrolebindings, CRDs, validatingwebhookconfigurations; `helm upgrade --install ev-ns … --kube-as-user alice` deployed; 8/8 pods Ready | |
| 4 | Namespace-only: code eval + agent run on the pod rung | PASS | sandbox-run unit-tests 0.5; agent-run task-success n=2 mean 1; 5 sandbox pods created | As SA `ci` with a projected token |
| 4 | …then on Landlock | PASS / refusal | sandbox-run: unit-tests 0.5. agent-run (needs `namespaced`): refused, 2 errors | By design; same as AWC |
| 5 | TRL GRPO exit test (CPU) | PASS | `1 passed in 11.28s` (`test_grpo_with_evalsi_rewards_and_checkpoints`), Job `ev-grpo3`, privileged, amd64 trainers image | Attempts 1 and 2 failed on my image setup (`No module named 'evalsi'`, then `'google'`): the evalsi package lives in a different venv than the torch venv. Fixed with a `.pth` fallback in the Job; no repo change. Logs: failures/aks-grpo-attempt{1,2}.log |

New findings from AKS:
- **F6.** The `evalsi` chart can't set tolerations on the operator, NATS or dev Postgres (only on the server, workers and sandbox pool). On a cluster whose nodes are all tainted those pods stay Pending. Workaround: `kubectl patch` of tolerations on `deploy/evalsi-operator`, `sts/evalsi-nats`, `sts/evalsi-dev-postgres`; the two Pending StatefulSet pods then needed deleting to pick it up (done with your OK).
- **D15.** `mode: bwrap` fails on this AKS (Ubuntu 24.04) with `bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted`; `mode: privileged` works.
- The AWC pod-rung failure is therefore an AWC platform problem (waypoint ext-authz), not an Evals.si defect: the same specs pass here.
- The AWC NetworkPolicy non-enforcement is still unexplained (Calico present). AKS cannot answer it because it has no policy engine.

AKS not repeated (outside the agreed scope): identity/RBAC, credential grants, guardrails, annotation, MCP, ml, load, HA, Firecracker (no `/dev/kvm` expected on Azure VMs; not checked), UI, real SWE-bench.

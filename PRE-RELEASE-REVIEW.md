# Evals.si pre-release review

Scope: whole repo at HEAD 6c75325 (main). Method: 11 Sonnet reviewers by area (read-only), then 2 Opus validators re-checked the highest-impact claims against the code. Nothing was executed except `go build ./...` and `go vet ./...` (both clean) and `helm template`/`helm lint` (clean). No code was changed.

Status legend:
- **V-CONFIRMED / V-PARTIAL / V-REFUTED**: Opus re-read the code.
- **Unvalidated**: Sonnet reviewer only, read from code, not independently checked.

---

## 1. Release blockers

### B1. Version and release state is inconsistent (V-CONFIRMED, P1)
- Tag `v0.3.1` exists (2100d40, 2026-10-22). HEAD is 76 commits ahead. CHANGELOG has a dated `[0.3.1] - 2026-10-22` section beneath an 82-line `[Unreleased]`.
- `python/evalsi/src/evalsi/_version.py` and `python/evalsi-harness/src/evalsi_harness/_version.py` say `0.3.1.dev0`; the harness `pyproject.toml:29` pins `evalsi[server]==0.3.1.dev0`. `0.3.1.dev0` sorts below the released 0.3.1.
- PyPI cannot re-upload a version; `docs/RELEASING.md:172` allows moving a tag only before publication.
- Action: if 0.3.1 was published, cut 0.3.2 (or 0.4.0) from HEAD. Fold `[Unreleased]` into that entry. Update `_version.py` (x2), harness pin, Helm `Chart.yaml` (x3), `action.yml:15`, README "latest release is 0.3.1" (~L414), `website/scripts/sync-content.mjs:168`, `website/src/pages/index.astro:187,417`, tutorial transcripts. `scripts/set-python-version.sh` rewrites versions at release; confirm `release.yml` runs it.
- Docs that say "unreleased candidate, not in PyPI 0.3.1" become false once a release ships and the website publishes them verbatim: `docs/guides/onboarding.md:42-48,186-190`, `docs/guides/harness2.md:18`, `docs/tutorials/05-state-and-handoff.md:9`, `docs/tutorials/01-install.md:89`, `docs/LEFTOVERS.md:~25`.
- `release-0.3.1/SHA256SUMS` lists `evalsi-bundle-0.3.1-linux-amd64.tgz`, which is not in the directory (so `sha256sum -c` fails). The other 12 entries match. SHA256SUMS itself is unsigned.
- `release-0.3.1/` (232 MB, untracked, has a `.DS_Store`) is not gitignored; add `/release-*/`.

### B2. Credentials in local-only git refs (V-CONFIRMED, security)
- 28 refs under `refs/agents/*`; 8 checkpoint commits (e.g. 24ab9e0f, 8894e8b4, f4f685e2, 58550b7, 5280559) contain `kube-aks.yaml`, a kubeconfig reported to hold `certificate-authority-data` and a token/client key. File is gitignored (`.gitignore:27`), on no branch or tag; fetch refspec is heads-only, so looks local-only. Nobody printed the contents.
- Risk: `git push --mirror` / `--all`-style pushes would publish them. A copy of the key-bearing file also sits in the working directory.
- Action: delete `refs/agents/*`, run gc/prune, rotate the AKS credentials.
- Tracked-tree secret scan is otherwise clean (only documented fake AKIA keys in tests/docs, allowlisted in `.gitleaks.toml`). No `/Users/` paths in tracked files, wheels, sdists, SBOM or the evalsid binary.

### B3. Sandbox escape to host chmod/mkdir via workdir symlink (V-CONFIRMED, P1)
- `internal/sandbox/host.go:96,105,110`: `open()` runs `os.MkdirAll` and `os.Chmod(b.work, 0o777)` with plain host calls on `<dir>/root/<Workdir>`.
- bwrap mounts the writable image root at `/` and maps the sandbox uid to evalsid's uid (`bwrap.go:118-119,186-193`), so the sandbox owns `root/` and can replace `/workspace` with an absolute or `../` symlink. `snapshot` preserves it (`cp -a`, `host.go:383-384,408-409`). On Restore, the host path behind the link gets chmod 0777, or directories are created on the host. A malicious image with a symlinked workdir does the same. Landlock rung not affected (no image).
- `host_root_test.go` covers only readFiles/writeFile, not `open()`; `writeFiles` already uses `os.Root` and is safe.
- Fix: `os.OpenRoot(b.root)` + `root.MkdirAll`/`root.Chmod`; reject a symlinked workdir. Add a test.

### B4. Privilege escalation through API key labels (V-CONFIRMED, P1, conditional on label-based policy)
- `internal/authz/service.go:247-253,272-277,289`, `internal/server/access.go:1171`: key labels are validated for shape only, but feed `principal.labels` / `api_key.labels` (`cel.go:114-119`, `auth/principal.go:83-90`) used in CEL role conditions, `allow` rules and `cel:` subjects, and override trace labels.
- Worse than first reported: binding subjects of kind `cel` (`engine.go:102-103`) are matched in `heldRoles` (`engine.go:469-471`), so a minted key whose labels satisfy a CEL binding gains roles beyond its declared ones. `checkKeyRoles` checks declared roles only, and `CheckRoleNames` (`escalation.go:90`, `covers`) compares condition strings, never evaluates whether the creator satisfies them.
- Scenario: a project `access.manage` holder mints a key with `team=admin` plus a role/binding conditioned on `principal.labels.team=='admin'`.
- Fix: treat labels as a grant (owner-only for labels referenced by any role/rule/binding), or evaluate role conditions against the new key's labels in the escalation check.
- Related, **unverified**: `key:<name>` binding subjects match on name alone (`engine.go:100-101`). `CreateAPIKey` blocks only config-declared names and live duplicates, so a new key may inherit roles bound to a deleted key's name.

### B5. Plugin install can delete the parent directory (V-CONFIRMED, P1)
- `python/evalsi/src/evalsi/plugins.py:203-210`: `install_wasm` builds `dest = dest_root / plugin_dirname(doc["name"])`; `plugin_dirname` allows `.` (`plugins.py:171`). A manifest `name: ".."` makes `shutil.rmtree(dest, ignore_errors=True)` delete `dest_root/..`; `name: "."` wipes all installed plugins. Non-string name raises TypeError. `cli_plugins.py:136` guards `remove` but not install.
- Fix: validate the name (non-empty str, not "."/".."), assert `dest.resolve().parent == dest_root.resolve()`. Add a test.

---

## 2. P2 findings

### Evaluation integrity (results/rewards can be wrong or forged)
| # | Where | Finding | Status |
|---|-------|---------|--------|
| E1 | `python/evalsi/src/evalsi/packs/code.py:69` | Generated code is concatenated before the tests and pass = exit 0, so `sys.exit(0)`/`os._exit(0)` passes without running tests (also corrupts RL rewards). Fix: sentinel line after tests, required in stdout, or import-and-call harness. Also `extract_code` (L42) falls back to a non-Python fence. | V-CONFIRMED P2 |
| E2 | `packs/rl.py:247-290` (`_check_sympy_code`, eval at ~334-335) | Only a power inside an exponent is rejected; `((9^999)^999)^999` (<200 chars) is evaluated eagerly in-process with no timeout: hang/OOM. Fix: bound total exponent across nested Pow, or run in subprocess with timeout. | V-CONFIRMED P2 |
| E3 | `internal/stats/stats.go:169-193`, `internal/evaluation/summarize.go:127,242`, `stats.go:277-286` | With trials>1, `clusterBy` defaults to the record; `ForMetric` uses `ClusteredInterval` before Wilson. Identical values give zero variance, CI = [m,m] (2 records x 3 trials all pass gives ci_low=1.0), so a `ci_low` gate passes at tiny n. Fix: Wilson on cluster count when variance is 0, or drop the CI. No test. | V-CONFIRMED P2 |
| E4 | `internal/runs/runs.go:1249,1262`, `python/evalsi/src/evalsi/runspec.py:214` | Gate mean/ci stats are computed over scored results only; `allErrored` fails only when every result errored. 95% target errors + 5% correct can pass `accuracy min 0.9`. Mitigation exists: `error_rate` gate (`runs.go:1289`, `runspec.py:204-210,232`) and `docs/guides/gate-an-agent-in-ci.md:136,144` tell users to add it. Reviewers rated P0; Opus: unsafe default, not a blocker. Hardening: implicit `error_rate` gate that must be opted out of. | V-PARTIAL P2 |
| E5 | `python/evalsi/src/evalsi/cli.py:912-940,1099` | `evalsi gate` exits 0 when the spec has no gates (or `--set` removes them). Fix: refuse (exit 2) a spec with no gates. | V-CONFIRMED P3 |
| E6 | `internal/runs/runs.go:1062,1171`, `internal/store/store.go:529` | Failed target outputs are stored and counted as present on resume, so transient per-record failures are never retried. Fix: treat `Error != ""` outputs as missing. | V-CONFIRMED P2 |
| E7 | `python/evalsi-harness/.../benchmarks/harbor.py:460` | `_sections` uses first `@@evalsi exit` marker via `partition`; code under test can print a fake marker and forge a pass (Harbor/Terminal-Bench). Fix: `rpartition` or per-run nonce. | Unvalidated P2 |
| E8 | `python/evalsi-harness/.../checker.py:41-60`, `gym.py:379`, `harness.py:957` | Checker `score` not validated: NaN/Infinity/any magnitude accepted; `float([])` raises uncaught `TypeError`. Becomes RL reward NaN/1e9. `harbor.py` already has an `isfinite` guard this path lacks. | Unvalidated P2 |
| E9 | `internal/evaluation/summarize.go:281-308` | `pass^k` uses existing trial rows, not k. Errored/flagged trials already count false (reviewer's main scenario was wrong); only absent/skipped trials inflate it. | V-PARTIAL P3 |
| E10 | `python/evalsi/.../packs/ml_classic.py:72,451-470` | `_score` accepts NaN/inf; one bad row makes roc-auc/log-loss NaN and the evaluator ERROR. `_gains` inf, `2**g` overflow in `ndcg`. | Unvalidated P2 |
| E11 | `internal/runs/release_policy.go:35,346` | `min_records` can be 2 -> t-interval with df=1; zero variance gives [m,m], so `max_regression: 0` can pass on 2 records. | Unvalidated P3 |

### Runs, sources, ingest
| # | Where | Finding | Status |
|---|-------|---------|--------|
| R1 | `internal/source/manager.go:596` (also 563, 571, 711-713); `phoenix.go:242-250`, `mlflow.go:452-454`, `mlflow.go:227`, `phoenix.go:156` | One malformed trace (non-hex span id, non-OTLP span, unparseable start time) fails the whole collect cycle every retry; watermark never passes it, source wedged. Fix: skip + count poison trace, mark seen, advance. | V-CONFIRMED P2 |
| R2 | `manager.go:412-444, 256-290` | Write-back scores are in-memory only; engine acks once `OnResults` returns true. Lost on source edit/restart/failover. Fix: persist, or ack after durable write. | V-CONFIRMED P2 |
| R3 | `internal/source/mlflow/mlflow.go:522-525` | Update-assessment 404 is treated as "trace deleted"; score skipped and not recorded, stale RemoteID makes later scores for that trace+metric 404 forever (narrower than "every score for the metric"). LangSmith handles it by re-creating. | V-PARTIAL P2 |
| R4 | `internal/cluster/work.go:84,97-99,259` | Cancelled run only abandons its inbox; queued/in-flight Evaluate/RunTask keep executing (`handle` uses `context.Background()`). Spend escapes run budgets. Fix: cancel subject keyed by run id or deadline in message. | V-CONFIRMED P2 |
| R5 | `manager.go:407-451` | Pull cycle and score flush share one goroutine; long backfill blocks write-back, `r.scores` (cap 4096) fills and drops. | Unvalidated P2 |
| R6 | `langfuse.go:171-179`, `phoenix.go:200-215`, `langsmith.go:214-231` | Pagination loops have no repeated-cursor guard or page cap (manager and schedule.go have one). | Unvalidated P2 |
| R7 | `internal/source/{mlflow:140,langfuse:118,phoenix:99,langsmith:143}` | `io.ReadAll(LimitReader(256MiB))` then `json.Unmarshal`; hostile store can exhaust memory; truncation yields opaque error. ClickHouse uses 64 MiB with overflow detection. | Unvalidated P2 |
| R8 | `internal/ingest/assembler.go:88-94` | Buffer bounded by count only (10000 traces x 2000 spans); O(n) `evictOldestLocked` under the global lock per new trace. | Unvalidated P2 |
| R9 | `internal/sinks/sinks.go:146,215-221` | All sinks share one queue/worker pool; each failing export holds a worker up to 3x60s; one dead sink starves others and drops exports. | Unvalidated P2 |
| R10 | `operator/controllers/evalrun.go:170-194` | Crash/timeout between `CreateRun` and status write makes the retry create a second run. Failed status write is handled (cancels run); crash is not. No idempotency key sent though the API supports one. Fix: key on EvalRun UID. | V-PARTIAL P2 |
| R11 | `internal/runs/runs.go:776` | If `holdLease` returns false, `a.subs` is not closed: WatchRun blocks until ctx ends; concurrent CancelRun does not cancel remote. | Unvalidated P2 |
| R12 | `python/evalsi/.../cli.py:932,1068` | Gate idempotency key = project+name+release; a first run that ended ERROR/CANCELLED may make re-runs return the dead run. Server path not read. | Unvalidated P2 |
| R13 | `internal/sandbox/launch.go:131-146` | Parent-ctx cancel reported as `OutcomeExit` 137 not `runner_failure`. Real impact small (run ctx usually stops results being stored). | V-PARTIAL P3 |

### Sandbox / isolation (non-blockers)
| # | Where | Finding | Status |
|---|-------|---------|--------|
| S1 | `internal/sandbox/seccomp_linux.go:20-35` | `io_uring_*` not denied; `IORING_OP_SOCKET`+`SENDMSG` gives UDP egress, bypassing `denyInet`. Docker/containerd default seccomp often blocks io_uring, which limits it. | V-CONFIRMED P2 |
| S2 | `landlock.go:105-108` | In allowlist mode `denyInet=false` and Landlock filters only TCP: arbitrary UDP (DNS exfiltration, QUIC) is open with no io_uring needed. Opus flagged as new. | Unvalidated P2 |
| S3 | `internal/sandbox/egress.go:385-389,302,128` | Proxy dials allowlisted names with no resolved-IP check: loopback, 169.254.169.254, cluster IPs reachable via wildcard or DNS rebinding. `anyHost` is only used for `network: allow` (unrestricted by design). | V-PARTIAL P2 |
| S4 | `landlock.go:54-85` | `Enforcement` reports "full" for ABI 3-5; signal/abstract-socket scoping needs ABI 6 (6.12). Rung shares PID ns and uid. | Unvalidated P2 |
| S5 | `firecracker.go:723-793` | After `take()`, error paths call `vm.kill()`, not `vm.close()`; `ownDir` rootfs copy leaks on each failed open. | V-CONFIRMED P3 (reviewer P2) |
| S6 | `firecracker.go:471-472` | Firecracker stdout/stderr go to an unbounded `bytes.Buffer`; `console=ttyS0` default lets a guest root process grow evalsid memory. | Unvalidated P2 |
| S7 | `firecracker.go:806-826` | `resume` reuses request ctx; cancel during snapshot copy leaves VM Paused forever. | Unvalidated P2 |
| S8 | `guestconn.go:239,254`, `firecracker.go:503` | WriteFiles/ReadFiles use `context.Background()` with no timeout and no `WithReadMaxBytes`; hung/malicious guest blocks or OOMs evalsid. | Unvalidated P2 |
| S9 | `egress.go:182-186` | `log()` appends to `events` unbounded; `Events()` copied on every exec. | Unvalidated P2 |
| S10 | `launch.go:96-100`, `launcher_linux.go:30` | Whole spec travels in one env var `EVALSI_SANDBOX_SPEC`; Linux caps one env string at 128 KiB -> E2BIG/runner_failure for large inline args. | Unvalidated P2 |
| S11 | `session.go:368-383` | `Close()` doesn't cancel/await running foreground `Exec`s; `Exec.Timeout` uncapped (`service.go:185`). | Unvalidated P2 |
| S12 | `image.go:235-310`, `manager.go:185-214` | `extractTar` has no total bytes/file-count cap; snapshots uncapped; idle check ignores background processes. | Unvalidated P2 |
| S13 | `internal/wasmeval/wasmeval.go:400`, `cli.go:283` | No concurrency limit on `call`/`Serve`; N x memory limit (up to 512 MB each). | Unvalidated P2 |
| S14 | `landlock.go:91` | Reviewer claimed read of all `/proc` leaks evalsid's `/proc/<pid>/environ`. Opus: **REFUTED**. Landlock blocks ptrace-mode access from a confined domain and seccomp denies ptrace/process_vm_readv. Remaining: `cmdline`/`status` disclosure for same-uid processes. | V-REFUTED (P3 residual) |

### Server / auth
| # | Where | Finding | Status |
|---|-------|---------|--------|
| A1 | `internal/server/server.go:379-384` | `http.Server` sets only `ReadHeaderTimeout` (no Read/Idle timeout, no `MaxHeaderBytes`): slow-body clients hold connections. Body size is capped (`access.go:999,1007`); streaming/uploads rule out a blanket ReadTimeout. | V-PARTIAL P3 |
| A2 | `internal/server/ratelimit.go:217`, `internal/auth/authenticator.go:269-274` | Bad-credential requests skip the limiter; each bogus `evk_` key does a DB `KeyByHash`, each bad JWT does signature verification. | Unvalidated P2 |
| A3 | `internal/auth/jwt.go:56-77` | `keySource.lookup` holds `k.mu` across the JWKS fetch (up to 10s); token with real issuer and random kid forces refetch; slow JWKS stalls all JWT auth for the provider. Fix: release lock, singleflight. | Unvalidated P2 |

### Python SDK
| # | Where | Finding | Status |
|---|-------|---------|--------|
| P1 | `python/evalsi/.../plugins.py:231` | Index-supplied `requirement` goes straight to pip/uv; option injection (`--index-url=...`, `-r`) works, but the index author can already name any package/URL wheel, so it adds little. Fix: reject leading "-", validate with `packaging.requirements.Requirement`, pass `--`. | V-PARTIAL P3 |
| P2 | `python/evalsi/.../wasm.py:205-236` | Unguarded `json.loads` in reader thread (one bad line kills it, pending futures never resolve); stderr drained only after stdout EOF (pipe-buffer deadlock); `fut.result()` no timeout; race on `_pending`. | V-CONFIRMED P3 (reviewer P2) |
| P3 | `.../webhooks.py:54` | `hmac.compare_digest` with a non-ASCII `v1=` raises TypeError (500, not 401). Reviewer ran it. Duplicate `v1` entries (rotation) unsupported. | Unvalidated P2 |
| P4 | `.../auth.py:49-56` | `Credentials` default repr exposes access/id/refresh tokens. Fix: `field(repr=False)`. | V-CONFIRMED P3 (reviewer P2) |

### Operator / deploy
| # | Where | Finding | Status |
|---|-------|---------|--------|
| D1 | `deploy/helm/evalsi/templates/operator.yaml:17-22`, `deploy/helm/evalsi-crds/templates/rbac.yaml:19-22`, `operator/config/rbac/role.yaml:49-58` | No `evaluators/finalizers`, `checkpointservers/finalizers`, `sandboxclasses/finalizers` update. `SetControllerReference` sets blockOwnerDeletion (`evaluator.go:143,239`, `checkpointserver.go:70,80`, `sandboxclass.go:69`); under OwnerReferencesPermissionEnforcement (OpenShift; chart ships `values-openshift.yaml`) child creation is rejected and those CRs never reconcile. Add kubebuilder markers too. | V-CONFIRMED P2 (OpenShift-only) |
| D2 | `operator/controllers/evaluator.go:116-123`, `checkpointserver.go:142-148`; webhook covers only EvalRun/OnlineEvalPolicy/TraceSource (`webhook.go:134-153`) | `Evaluator`/`CheckpointServer` specs copy image, env (incl. `secretKeyRef`), `serviceAccountName`, nodeSelector, tolerations straight into pods, unvalidated; SA token automounted. Anyone who can create these CRs can read namespace Secrets and use any SA (confused deputy). | V-CONFIRMED P2 |
| D3 | `deploy/helm/evalsi/templates/tls.yaml:51-67` | Leaf Secrets copied forward on upgrade (`if and $old $existing`); leaf certs are 825 days; nothing renews them. Default `certManager.enabled: false` (`values.yaml:341`). Server/sandbox/worker mTLS certs expire cluster-wide ~27 months after install; SAN changes also ignored on upgrade. | V-CONFIRMED P2 |
| D4 | `deploy/helm/evalsi-crds/crds/`, `deploy/airgap/install.sh:102`, `docs/guides/kubernetes.md:181,211` | Helm never upgrades `crds/`. v0.3.0->v0.3.1 adds `status.project` to TraceSource and changes EvalRun (HEAD adds +655 lines to the EvalRun CRD). On upgrade the old schema prunes `status.project`; project-move cleanup silently fails. install.sh and release notes don't mention applying CRDs. Fix: `kubectl apply --server-side` CRDs before upgrade; document. | V-CONFIRMED P2 |
| D5 | `deploy/helm/evalsi/values.yaml:39-40`, `templates/server.yaml:36-45,111,120,122`, NOTES.txt | Default install runs SQLite on `emptyDir` (restart/drain loses all runs and policies; only a comment hints at it). With persistence on, Deployment has no `strategy: Recreate`: RWO PVC -> Multi-Attach hang or two SQLite writers. Fix: warn/fail when neither postgres nor persistence set; Recreate when persistence on and replicas=1. | V-CONFIRMED P2 |
| D6 | `deploy/helm/evalsi-crds/templates/webhooks.yaml:53` | failurePolicy Fail with no namespace/objectSelector; finalizer removal is an UPDATE through the webhook. Operator down, or evalsi uninstalled while evalsi-crds remains: every evals.si write rejected, finalizers stuck, namespace deletion hangs. | Unvalidated P2 |
| D7 | `operator/controllers/evalrun.go:113`, `policy.go:46`, `tracesource.go:130` | On delete, permanent errors (PermissionDenied/FailedPrecondition) are swallowed before finalizer removal (server-side policy/source orphaned and keeps running); transient errors block the finalizer forever if evalsid is gone. | Unvalidated P2 |

### CI / release engineering / supply chain
| # | Where | Finding |
|---|-------|---------|
| C1 | `.github/workflows/release.yml` (all `uses:`; publish steps ~L158/L177: `pypa/gh-action-pypi-publish@release/v1`, `softprops/action-gh-release@v2`, `anchore/sbom-action@v0`, `sigstore/cosign-installer@v3`, `docker/*@v3/v6`) | No third-party action pinned to a commit SHA; PyPI publisher pinned to a mutable branch. These steps hold `id-token: write`, `packages: write`, `contents: write`. Pin release-path actions (and ci.yml, security.yml) to SHAs; Dependabot github-actions is already configured. |
| C2 | `release.yml:219,245` | `evalsi:latest` and `evalsi-collector:latest` pushed unconditionally, including prereleases (version regex allows `0.4.0-rc1`). Add `latest` only when version has no `-`. |
| C3 | `release.yml:42` | `v="${{ inputs.version }}"` interpolated into `run:`; regex check runs after assignment and `grep -q` accepts any matching line of a multiline value. Write access required to trigger. Pass via `env:`, anchor with bash `[[ =~ ]]`. |
| C4 | `release-0.3.1/` | Wheels, sdists, archives, chart tgz have no signatures/attestations (cosign signs only images and OCI charts). Sign SHA256SUMS with `cosign sign-blob`; optionally `actions/attest-build-provenance`. |
| C5 | `scripts/release-binaries.sh:31-34`, `release-0.3.1/evalsi_0.3.1_linux_*.tar.gz` | Static Go binaries ship with only "see go.mod" for third-party notices; Apache/BSD/MIT notices must travel with binary redistribution; bwrap (LGPL) has COPYING + upstream URL only. Generate a full notice file (go-licenses) into archives and wheels. |
| C6 | `release-0.3.1/evalsi-0.3.1.spdx.json` | SBOM covers only the container image (syft 1.42.3, 317 packages, all `licenseConcluded` NOASSERTION). No SBOM for wheels, tarballs or the collector image. |
| C7 | `internal/sandbox/cosign_test.go:202`, `ci.yml:95`, `release.yml:217` | Built-in verifier reads only legacy `.sig` tags; test skips on cosign >= v3. CI pins cosign v2.6.1, release.yml uses unpinned `cosign-installer@v3`. A cosign-v3-signed image is rejected (fail-closed). Pin `cosign-release` or support OCI-referrer bundles. |
| C8 | `examples/demo/values.yaml:114`, `examples/demo/Chart.yaml`, dsh/deepagents specs, `deploy/e2e/kind-e2e.sh:238-241` | Demo images pinned to `ghcr.io/abhishek-rnjn/evalsi:0.1.0` (chart appVersion 0.1.0); CI works only via tag aliasing. A user following the demo gets the 0.1.0 evalsid or ImagePullBackOff. Bump or template. |

### Documentation accuracy
| # | Where | Finding |
|---|-------|---------|
| X1 | `docs/DESIGN.md:1219,1232` | `evalsi rerun` and `dataset promote --from-traces` do not exist (real command is `evalsi promote`, `docs/guides/agent-runs.md:518`). V-CONFIRMED P3. |
| X2 | `docs/DESIGN.md:3` | Status line "Draft v0.6 2026-10-06 ... MCP, classic ML ... next" though `evalsi mcp` and ml-classic ship. |
| X3 | `docs/tutorials/01-install-and-first-eval.md:69-83` | Shows plain `evalsi doctor` printing harness/worker/server/sandbox lines; current `cli_doctor.py` `diagnose` returns only python/evalsi checks unless `--all`. |
| X4 | `README.md:~246` and agents blurb | Reads as certified for SWE-bench, tau-bench, Terminal-Bench, BFCL; `LEFTOVERS.md:277,285` says real SWE-bench images never ran and Terminal-Bench 2 / deep-research never ran; CHANGELOG says new importers haven't run on real benchmark questions. |
| X5 | README "Project status" vs `SUPPORT.md` | README cites AKS + Istio ambient; SUPPORT says Calico/Istio-ambient enforcement unverified and ambient enrollment caused NATS timeouts and sandbox guest connection resets. |
| X6 | `website/scripts/sync-content.mjs:~140-150` | "Get started" page built from Install/Quickstart/A tour only; tour's `uv run ../examples/...` depends on the excluded "Develop from source" section. |
| X7 | `SECURITY.md:5-8`, `CODE_OF_CONDUCT.md:~38`, `.github/ISSUE_TEMPLATE/config.yml` | Both security and conduct reports depend on GitHub private vulnerability reporting being enabled; no email fallback; not on the `docs/RELEASING.md` prerequisites. Could not verify the setting offline. |

### Harness (python/evalsi-harness) — all unvalidated unless noted
| # | Where | Finding |
|---|-------|---------|
| H1 | `harness.py:305-325` | **P1 (high conf).** Sandbox leak on database setup failure: `teardown()` runs when `Database(...)` raises, but `env.sandbox.destroy` is appended to `live.closers` only at L325. Bad `environment.database` leaves the sandbox running; under a server leaks pods/VMs per failed trial. Fix: register closer right after `acquire`. |
| H2 | `harbor.py:460` | Marker forgery (see E7). |
| H3 | `checker.py:41-60` | Score validation (see E8). |
| H4 | `codemode.py:492-520` | Memory/CPU limits cover only some ops: `l=[big]*1000000` then `"".join(l)`/`json.dumps`/`str` builds ~10^12 elements before `sized()` checks; interpreter runs synchronously on the worker's event loop (100k `s.count()` on 1 MB stalls every trial; `asyncio.timeout` can't fire). Fix: guard by total size, cap per-op input, subprocess or CPU budget. |
| H5 | `harbor.py:91`, `dockerfile.py:450`, `compose.py:746-760` | Importers `rglob` and read through symlinks (`lab.py:98`, `longdoc.py:116` reject them). Untrusted benchmark repo with `x -> ~/.aws/credentials` embeds host file contents in task files, sandbox, records and model context. |

---

## 3. P3 and minor (unvalidated unless noted)

### Store / datasets / evaluation
- `internal/store/backend.go:154-180`, `traces.go:255-272`: `DeleteTracesBefore` is one unbounded DELETE with multi-way anti-join inside the global write lock (blocks ingest/ack/lease renew; lease expiry -> `ErrWatchStale`; SQLite holds the only connection). Batch it. (P2)
- `store/auth.go:466-474` with `:321`: retention deletes all chained audit rows when none newer than cutoff; next append restarts at seq 1, empty prev_hash; whole-log truncation undetected. V-CONFIRMED P3 (a chain without an external anchor can't detect tail truncation anyway). Keep newest row as anchor.
- `store/auth.go:361-418`: `VerifyAudit` holds one tx (SQLite's only connection, `store.go:117`) for the whole walk; stalls API-key lookups.
- `store/dataset_versions.go:151-171`: `ListDatasetVersions` loads/hashes/unmarshals up to size+1 full bodies (page of 50, looped until a page is visible, `datasetcatalog/service.go:152`, up to 64 MiB each). (P2)
- `store/dataset_versions.go:36-43`, `release_decisions.go:23-34`: integrity hashes recomputed from a deterministic re-marshal on every read; protobuf doesn't promise byte stability across versions, so rolling upgrade/rollback can return `ErrDatasetVersionIntegrity`/`ErrReleaseIntegrity`. Audit log already hashes stored bytes (`auth.go:328`). (P2)
- `store/annotations.go:135-158,265-300`: `AddItems` accepts caller-supplied ids into a table keyed by id alone; `SaveAnnotation` doesn't check item belongs to project/queue (existence oracle / cross-project annotation). Service layer may guard it; not verified. (P2)
- `internal/datasets/datasets.go:104-129`: `Append` reads whole file under one global mutex, writes row by row, no fsync, ignores `Close` error; a torn last line makes the dataset permanently unloadable. Use temp file + rename.
- `internal/evaluation/service.go:510-560`: `EvaluateStream` has no total record cap (`MaxRecords` applies only to unary `Evaluate`).
- `store/store.go:220-229`: `UpdateRun` is an unconditional overwrite (executor/cancel/flywheel race, stale proto can clobber status or un-archive). Add CAS.
- `store/watch_revision_migration.go:56-65`, `watch_pending.go:138-145`: legacy rename-and-recreate migrations collide on Postgres (`*_pkey` keeps its name). Dev builds only.
- Offset pagination and `ListTraces` ordering without project tiebreak (`store.go:365-398`, `traces.go:83-122`); `RunsWithStatus` (`store.go:447`) scans all runs; `DeleteWebhook` two non-transactional statements, `PruneDeliveries` unbounded (`webhooks.go:89-98`); `SelectedFailures` uncapped IN list (`failures.go:183-191`); `CompleteIdempotency` returns `ErrIdempotencyCapacity` for missing claim row (`idempotency.go:168-179`); SQLite DSN built from raw path (`store.go:112`); bootstrap allocates a slice per resample, 2000 resamples (`stats.go:235-253`).

### Sources / sinks / ingest
- `internal/source/guard.go:159` + `langsmith.go:132-137`: guarded client follows redirects (no `CheckRedirect`); LangSmith `x-api-key` / `X-Tenant-Id` forwarded cross-host on 302 (needs endpoint to redirect). V-PARTIAL P3. The SigV4 `X-Amz-Security-Token` part was **refuted**: MLflow clients set `ErrUseLastResponse` (`mlflow/auth.go:134`, `mlflow.go:343`).
- `sinks/langfuse.go:128-131` per-event 429/5xx in a 207 marked permanent; `sinks/phoenix.go:69` no `identifier` (policies overwrite each other), `sinks/mlflow.go:308` new assessment per re-eval; `sinks/mlflow.go:236` first batch up to 1000 metrics + params/tags exceeds MLflow's 1000-entity cap (~330+ metrics -> permanent 4xx, run never exported); `sinks/langfuse.go:43`, `mlflow.go:64` up to 1 MiB of response body logged unredacted.
- Server-supplied `Retry-After` uncapped (mlflow.go:151 and the other three); duplicate MLflow assessments within a batch (`mlflow.go:510-520`); `ingest/otlp.go:179` post-gunzip `LimitReader` silently truncates at 32 MiB and returns 200; ClickHouse `Version` is writer wall-clock `UnixNano` (`clickhouse.go:293`); MLflow OAuth `expires_in` missing/0 -> token exchange every request (`mlflow/auth.go:159`); `mlflow/sagemaker.go:52` `.amazonaws.com` suffix accepts attacker-owned AWS hosts (SigV4 signature sent); `manager.go:423` `pending` unbounded when store down.

### Runs / cluster / CLI
- `internal/cluster/cluster.go:108` + `work.go` `NewTicker(c.ackWait/3)`: `ack_wait` of 0 or negative accepted by `config.Validate` -> panic kills worker.
- `internal/runs/sweep.go:~130`: `createSweep` starts variants one at a time; failure at N leaves 0..N-1 running and orphaned with no ids returned.
- `internal/runs/retention.go:45,114`: `retainOnce` aborts on first error (later projects starve); `retire` calls `store.DeleteRun` without the resource arg, so no tombstone.
- `internal/runs/runs.go:1429`: `CancelRun` blocks on `<-a.done` ignoring ctx; returns success on a stranded PENDING run.
- `internal/runs/cluster.go:~150`: one transient `AcquireLease` error cancels a healthy run until adopt (up to 10s). Tolerate ~2 failures.
- `python/evalsi/.../cli.py:1208` vs `:836,:1102`: exit code inconsistency; `matrix --gate` uses 1 (same as infra/usage errors) while `run`/`gate` use 3.
- `evalsid` env `EVALSID_NO_AUTH=1` silently makes every caller owner even on non-loopback listeners (`cmd/evalsid/main.go:73`, `server.go:368`).

### Server / auth
- `/healthz` runs `worker.Describe` per request, rate-limit-exempt (`server.go:679-687`, `ratelimit.go:186`).
- Shutdown (`server.go:422-436`): 10s wait without cancelling ctx or closing connections; streams outlive it, `serve` returns `DeadlineExceeded` while handlers still use the store; other listeners not closed on early error.
- Streaming calls authorized once at first message (`access.go:1068-1083`): revoked key / expired JWT keeps a stream alive.
- Up to 64 MiB JSON buffered per request before authz (`access.go:985,1007`).
- `authz/extauthz.go:177-181,241-258`: decision cache key omits PDP inputs (headers, email, method, protocol, source); Envoy checker plaintext h2c by default, AuthZEN URL may be http with a bearer token.
- `/ui/login/*` unauthenticated, not rate-limited, returns raw upstream error text (`webui/login.go:156,176`).
- `guardrail/gateway.go:113` returns `err.Error()` on 500.
- JWT without `sub` -> principal `user:<provider>/` shared (`jwt.go:362`, `principal.go:70`).
- `datasets.go:53-62` two keyed uploads can hold spool slots up to 10 min.
- `server/quality.go:54-62` `GET /v1alpha1/quality` skips `recordFilteredRead` (audit gap).
- Verified OK: no webui XSS (no innerHTML, strict CSP, no CORS headers); `TestActionTableCoversEveryRPC`, `TestPermissionMatrix` and identity matrix are strong.

### Sandbox / wasm
- `wasmeval.go:408` stderr `capped` errors past 64 KiB (EIO crash) instead of truncating; `:428` any `ctx.Err()` reported as "timed out"; `:272-283` module path may be absolute/`..` and sha mismatch error prints the hash of an arbitrary host file; `cli.go:260-265` `Serve` caches load errors per path for process lifetime; `:502-517` plugin score names/outputs not checked against declared `outputs`.
- `cmd/evalsi-guest/main.go:255-264,284`: `limit()` ignores `Setrlimit` errors (fails open); `limitPids` can remove a concurrent command's empty cgroup.
- `bwrap.go:122,136`: `--share-net` shares host netns (documented); seccomp denylist doesn't block AF_NETLINK or AF_VSOCK on the landlock rung.
- `image.go:174`: `os.Rename(tmp, dir)` ENOTEMPTY if another evalsid unpacked the same digest (lock is in-process only).

### Python SDK / adapters
- `packs/text.py:147-165` `_chrf` skips orders longer than the string and strips only space; `:191` `token_f1` empty/empty returns 0 (SQuAD gives 1); `bleu(max_n=0)` ZeroDivisionError (`:54`).
- `packs/ml_monitoring.py:146-170` KS p-value series doesn't converge for lam < ~0.01 (reviewer measured lam=0.001 -> p≈0.02, true 1).
- `packs/rag.py:214` `int(m)` on 4300+ digit match raises; `packs/core.py` `regex-match` no ReDoS bound, `json-valid` RecursionError uncaught and accepts NaN/Infinity, `fuzzy-match` autojunk bias (~0.967 vs 0.995).
- `judges/cache.py:22` cache dir 0755 (judge prompts/responses world-readable), one shared sqlite connection with `check_same_thread=False` and no lock, corrupt row raises.
- `client.py:130-165` `stream()` timeout=None, unbounded frame size; gRPC auth headers over insecure channel for http:// URLs. `auth.py:285-300` loopback login callback accepts error from any local request without state check. `ml_monitoring` `reference_data` reads arbitrary local path.
- Adapters: `lm-eval/__init__.py:111` `asyncio.run` inside sync `generate_until` fails under a running loop; `:79-83` fixed target ignores per-request `max_gen_toks`; `taubench/__init__.py:75-77` `TAU2_DATA_DIR` read once at import, second `load()` with a different dir silently ignored; `deepeval/system_one.py:~150-185` NaN judge probabilities propagate; `inspect/__init__.py:~205` `scores[0]` on empty list; `bfcl/__init__.py:214-224` executes model calls in-process via BFCL's executor (no injection found).
- Packaging: runtime deps have lower bounds only (`protobuf>=7.35`, `grpcio>=1.84`, `httpx>=0.27`); workspace is locked by `uv.lock`.

### Operator / deploy
- `operator/controllers/policy.go:99-108` cross-project conflict check ignores namespace (two namespaces mapping to the same project/policy name overwrite each other; deleting either deletes the shared policy). `policy.go:114`, `tracesource.go:112`, `evalrun.go:177` treat PermissionDenied/FailedPrecondition as permanent (no requeue; EvalRun specs immutable).
- `server-egress.go` NetworkPolicy allows only DNS + same-namespace; blocks API-server issuer/JWKS reads for `auth.kubernetes` (default on) unless `extra` rule added.
- `nats.yaml:104` monitoring port 8222 open to any namespace; NATS token in argv.
- No liveness/readiness on workers; liveness missing on NATS; default resources `{}` (BestEffort); no PDB / anti-affinity / topology spread.
- `sandbox.yaml:17-20` sandboxd Role allows pods create/delete in the whole release namespace (can mount any Secret, delete server/operator pods).
- `webhooks.yaml:22` caBundle may render empty/`<no value>` when cert-manager Secret lacks `ca.crt`; under Argo/Flux (no `lookup`) a new CA is generated on every render.
- `evaluator.go:292-295` `Watches(&corev1.ConfigMap{})` caches every ConfigMap in `allNamespaces` mode; ScaledObject error path (L244) doesn't requeue.
- `airgap/bundle.sh:36-45` floating tags (postgres:17, python:3.13-slim, nats:2.12-alpine, bitnamilegacy/minio); in `install.sh` the crds `helm upgrade --install` has no `-n`.
- `deploy/e2e/kind-e2e.sh` many `port-forward & sleep 3`; EXIT trap doesn't kill them.
- `collector/builder-config.yaml` k8sattributesprocessor pinned v1.1.0 while other contrib is v0.162.0 (could not verify offline).
- `values.yaml:126` `bitnamilegacy/minio:2025.7.23` (legacy, unmaintained).

### CI / release (minor)
- Base images tag-pinned not digest-pinned (`Dockerfile:6-7,21,70`, `deploy/collector/Dockerfile`, `build-static-bwrap.sh`); apt unpinned; no HEALTHCHECK (k8s probes cover it).
- Action majors inconsistent (upload-artifact v4/v5/v7, setup-node v5/v6, setup-python@v5).
- `release.yml` doesn't check the tag commit is on main / CI green, nor that `action.yml` default `version` and chart versions match the tag.
- Unmerged Dependabot branches (aiohttp-3.14.4, authlib-1.6.12, pillow-13.0.0, litellm, filelock) — triage for security bumps.

### Docs (minor)
- `docs/guides/identity.md:3` links to a removed README anchor; `docs/verification/AKS-FOLLOWUP-2026-10-21.md:20` bad anchor; `docs/decisions/0037-online-sandbox-time-quota.md:46` says `evalsi sandbox run` (is `evalsid sandbox run`); `docs/guides/browser-and-desktop-agents.md` is orphaned; CHANGELOG [0.3.1] "Known limits" says EKS not run, contradicting README/SUPPORT; `website/astro.config.mjs:~40` loads Google Fonts from third-party origins.

### Harness (minor)
- `trust.py:55` `ref.startswith("evalsi_")` trusts any callable under `evalsi_*` for harness/hook/checker-parser refs (no dangerous callable found today).
- `environment.py` `_setup`: `TaskError` from a setup command (incl. timeout/network flake) cached permanently per environment key.
- `harness.py:296-299`: `task.environment.setup = [*..., *cli.install]` mutates the shared Task; re-setup appends installs twice and changes `config.key()` (snapshot cache misses).
- `modelhttp.py:98,152`: non-JSON 200 body / unterminated SSE event raises `JSONDecodeError`, not `ModelError`, not retried.
- `mcp.py:330-345,249,520`: malformed MCP server crashes (string `error` -> AttributeError, invalid JSON, `t["name"]` KeyError); only `MCPError` caught in `call_tool`.
- `mcp.py:214-219`, `service.py:187`: child process gets full `os.environ` (API keys); no process group, `aclose` kills only the direct child.
- `toolset.py` `EditFileTool.call`: file over 4 MiB `max_bytes` or a symlink is reported as "no such file" instead of the real reason.
- `envspecs/verifiers.py:129` `task_reward` returns 0.0 for an episode that never ended (timeout/interrupt scored as failed attempt).
- `desktop.py:149-160` `_read_line` executor thread stays blocked after the 15s timeout while `read_end` is closed; Xvfb without `-auth`.
- `src/evalsi/worker.py:208-220` (outside scope): evicting the oldest of 8 cached harnesses calls `aclose()` on its live tasks even if the run is in flight; `RunTask` doesn't cancel `run_task` on client disconnect.

---

## 4. Tests and coverage gaps

Top risk gaps (many have partial coverage elsewhere; same-name test files only):
1. Cosign v3 / keyless verification.
2. `internal/sandbox/egress.go` (538 LOC; enforces network allowlist; incidental coverage only).
3. `internal/store/clickhouse.go`, `clickhouse_retention.go` (960 LOC; live-service e2e only).
4. `internal/authz/engine.go`, `service.go` (1485 LOC; check whether `authz` tests cover both). No test for key-label escalation.
5. `internal/watch/engine.go` (1040 LOC).
6. `internal/runs/release_policy.go` (361 LOC; release gating; indirect via `release_test.go`).
7. `internal/auth/jwt.go` (475 LOC; confirm alg-confusion, `aud`, `exp` cases). No test for JWKS fetch stalls under concurrency.
8. `internal/guardrail/gateway.go` (354 LOC).
9. `cmd/evalsi-operator` (0 tests), `pkg/wasmplugin` (0 tests, 213 LOC).
10. Python CLIs with no test reference: `cli_init`, `cli_plugins`, `cli_calibrate`, `cli_canaries`, `cli_datasets`, `cli_flywheel`, `cli_traces`, `cli_determinism`, `cli_analytics`, `cli_mcp`, `application_starter`.

No test exists for: symlinked workdir in `open()`/Restore; io_uring bypass; `install_wasm` with "."/".." name; `sys.exit` in generated code; non-ASCII signature headers; wasm Service failure modes; NaN scores in `ml_classic` and checker; nested sympy powers; zero-width clustered CI; partial-trial pass^k; poison trace; repeated cursor in connector loops; redirect header leakage; MLflow update-404; per-sink isolation; retention emptying the audit chain; `Append` partial writes; Postgres legacy migrations; slow-body clients; harbor marker forgery; DB-failure sandbox leak; codemode join/dumps amplification; concurrent wasm calls; snapshot resume on cancel; warm-VM leak on error paths.

Flaky-test risks: `internal/cluster/cluster_test.go:246` (3500 ms sleep); `source/manager_test.go:537` (700 ms), `guest/agent_edge_test.go:60` (200 ms), `server/access_test.go:898,1180,1322,1393`. Tests with no assert (~11 of 1538): `python/evalsi-harness/tests/test_real_rungs.py:294,345,425`, `python/evalsi/tests/test_cli_agent_e2e.py:49`. Module-level `pytest.importorskip` in `tests/rlframeworks`, `tests/envspecs`, `tests/trainers` means a broken dependency install silently skips whole suites; assert a minimum number of tests ran. Fixed localhost ports and sleeps in `examples/demo/deploy/` and `tests/verification/*.py` (manual scripts). `examples/agents/benchmarks.yaml:1-10` concatenates several EvalRun documents (`evalsi run -f` fails with "expected a single document").

---

## 5. Verified OK (no action)
- `go build ./...` and `go vet ./...` clean; no `replace` directive in go.mod; `helm lint` clean across kind/eks/gke/aks/openshift/namespaced values.
- No SQL injection found in the store (all dynamic fragments are constant names or generated `?` lists); ClickHouse binds parameters.
- Version consistent inside `release-0.3.1/`: both wheels 0.3.1, harness requires `evalsi[server]==0.3.1`, all three charts version/appVersion 0.3.1, `action.yml` defaults to 0.3.1. 12 of 13 SHA256SUMS entries match local hashes.
- Platform wheels contain evalsid and bwrap plus license files; sdists carry no secrets, `__pycache__` or absolute paths.
- Workflows: default `permissions: contents: read` with per-job escalation; no `pull_request_target`; PR-controlled text goes through `env:` in `hygiene.yml`; secrets not exposed to PRs; keyless cosign for images; `release` environment gates tag runs.
- Dockerfile: multi-stage, non-root (uid 65532), checksum-verifies the firecracker download, no secrets.
- Proto: `git diff v0.3.0..HEAD -- proto` shows additions only (no removed or renumbered fields); name/number cross-check vs Python pb2 found no drift (9 service modules couldn't be imported on local Python 3.10; `buf` not installed so lint/breaking not run).
- Adapter pins are exact `==` plus lockfiles (tau2 pinned to a commit SHA); no adapter reads provider keys; DeepEval telemetry/dotenv disabled.
- Hardening noted: snapshot/ablation/MCP path confinement, atomic 0600 credential writes, guard dials resolved IP and re-checks every redirect hop, connector errors redacted before logging, OAuth/Databricks signed-URL requests don't follow redirects.
- README quickstart commands/flags, `release gate` exit codes (0/3/1), and examples paths match the code; all `examples/**` EvalRun specs load via `evalsi.runspec.load_spec` (except the multi-document file above).

## 6. Coverage gaps in this review
- Not read in depth: `internal/runs` `agents.go`, `slice.go`, `flywheel.go`, `compare.go`; `annotate`, `guardrail` (beyond gateway), `rewards`, `credentials`, `mcp`, `pluginhost`, `objstore`, `quality` packages; `internal/guest`; `pod.go` internals, `cosign*`; `store/clickhouse*.go`, `watch_heads.go`; per-service filtered-read implementations.
- Python: `cli*.py` (only grepped), finetune, agent*, safety, harness_edit, training, rewards, report; harness `connectors.py`, `acp.py`, `readers.py`, `evidence.py`, `context.py`, `models.py` request bodies.
- Docs: ~156 files mostly pattern-checked, not read; external URLs, GitHub vulnerability-reporting setting, Helm value names in individual guides, website build not checked.
- Not run: any test suite, `buf lint/breaking`, `govulncheck`, ruff/mypy, anything against a cluster, the sandbox rungs.
- Opus validated 32 of the highest-impact claims; everything marked "Unvalidated" is Sonnet reading code only.

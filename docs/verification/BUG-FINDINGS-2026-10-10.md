# Bug Findings and Proposed Fixes

Date: 2026-10-10. Source HEAD recorded for the live pass: `64de43d11feb5f5d3115b54d37df3401436daeea`.

This report consolidates the repository review, the subsequent live kind run, and an additional product-workflow pass. There are **30 confirmed issues**, including development/test portability defects, and **one retracted finding**. IDs preserve the first review's numbering; F06 is not an active bug. Proposed fixes have not been implemented. This is a bounded review, not a claim that every repository bug has been found. F22-F31 and the improvement backlog were added in the product-workflow pass.

No application source was changed for this review. Go regression probes were injected with an external `-overlay` file. Cluster manifests and Helm overrides were kept outside the repository. This report is the only repository file added.

> **Status:** fixes for every finding but F31 have landed on `bug-findings-and-fix`; see the [status table](README.md#bug-findings-of-2026-10-10). The text below is the report as written.

## Priority and Evidence

- P1: address before relying on the affected security or shared-service behavior.
- P2: functional defect affecting a supported workflow.
- P3: development/test portability defect, not evidence of a Linux production failure.
- External probe: a focused test or SDK snippet executed against the repository implementation, without changing repository source.
- Live kind: reproduced on the actual API server, operator, workers, or bundled service.

| ID | Priority | Finding | Evidence |
| --- | --- | --- | --- |
| F01 | P1 | Reward minimum isolation can be downgraded | Go probe and Python SDK snippet |
| F02 | P1 | Same-project policy replacement bypasses existing labels | API regression probe |
| F03 | P1 | Omitted judge bypasses rules on the default judge | API regression probe |
| F04 | P1 | Stored runs ignore revoked credential/judge grants | Run execution probe |
| F05 | P1 | Dataset symlink aliases bypass promoted-project authorization | API regression probe |
| F07 | P2 | Failed score write-backs do not schedule a retry | Runner regression probe |
| F08 | P2 | Different policies' write-back scores collide | Runner regression probe |
| F09 | P2 | Service-scoped trace permissions cannot authorize datasets | API regression probe |
| F10 | P2 | TRL component functions reuse stale rewards | Python SDK snippet |
| F11 | P2 | Unfinished checkpoint evaluations count as passed | Python SDK snippet |
| F12 | P2 | Relative dataset roots fail under symlinked working paths | Existing Go test on macOS |
| F13 | P3 | Full Go checks are not portable to macOS | Existing Go build/tests |
| F14 | P3 | Worker tests exceed macOS Unix-socket path limits | Existing pytest suite and short-path rerun |
| F15 | P3 | GNU-only sed commands break macOS demo/harness tests | Existing pytest suite |
| F16 | P1 | RBAC changes and revocations do not propagate between replicas | Live kind, two directly addressed API replicas |
| F17 | P2 | Operator-managed Evaluator workers crash on a read-only home | Live kind |
| F18 | P2 | Evaluator workers lack default S3/TLS dependencies | Live kind, two separate dependency failures |
| F19 | P2 | Policy label-only edits never reach the API | Live kind; analogous TraceSource path identified in code |
| F20 | P2 | Bundled MinIO lacks permission to write its runtime PID file | Live kind |
| F21 | P2 | Bootstrap creates keys that default chart authentication refuses | Live kind |
| F22 | P1 | Symbolic math grading executes model-supplied Python expressions | Harmless Python evaluator probe; optional SymPy dependency installed |
| F23 | P1 | Shadow replay bypasses stored-run admission quotas | Go run-manager regression probe |
| F24 | P2 | Cross-run promotion creates datasets with duplicate record IDs | Go promotion and real normalizer probe |
| F25 | P2 | Analytics renames aliased primary metrics | Python SDK and live API analytics probes |
| F26 | P2 | Analytics joins multiply samples across result pages/trials | Python probe and live kind API pagination |
| F27 | P2 | List-based analytics drops target model metadata | Realistic SDK API-shape probe and explicit proto contract |
| F28 | P2 | Embedded RunResult marks all-error evaluations as passed | Python execute probe |
| F29 | P2 | Reports show another trial's output for a failed score | Python report-builder probe |
| F30 | P2 | Reports hide numeric score 1 regardless of metric scale | Real custom evaluator with declared 1-to-5 scale |
| F31 | P2 | Stored-run quota has no supported delete/retention recovery | Existing quota regression and public API/CLI inspection |

## Live Environment

- Dedicated cluster: `evalsi-bug-review`; existing clusters were not changed.
- Separate kubeconfig: `/tmp/evalsi-kind-bug-review-20261010.kubeconfig`; the default kubeconfig was not used for test mutations.
- Two arm64 kind nodes, Kubernetes v1.35.0, Linux 6.10.14-linuxkit, containerd 2.2.0.
- Docker 28.5.1, 8 CPUs, approximately 16 GiB available to Docker; Helm v4.2.4; kubectl v1.36.0.
- Application image: `evalsi:bug-review-20261010`, built from the current repository. Image ID: `sha256:52584b7b22b6fe4ca25742f25868d74a5053e4af66fc276ac766bb8a62e86965`.
- Main namespace: `evalsi`; two API replicas, PostgreSQL, NATS, MinIO, ClickHouse, operator/admission webhooks, and pod/Landlock sandbox pool.
- Namespace-only release: `restricted` in `review-restricted`, enforcing Pod Security `restricted`.
- No real judge/model API, paid inference, production cluster, or private registry was required.
- Temporary API tunnels were localhost-only. Python live probes used `verify=False` for the chart's temporary self-signed API certificates; this is not a production TLS recommendation. Normal in-cluster worker/pool mutual TLS remained enabled.
- kindnet does not enforce NetworkPolicy. A passing pod-rung workflow does not prove enforced network isolation, gVisor/Kata isolation, or KVM support.

## Initial Review Findings

### F01: Reward Minimum Isolation Can Be Downgraded

Priority: P1.

Locations: [Go reward binding](../../internal/rewards/rewards.go#L143), [Python reward binding](../../python/evalsi/src/evalsi/rewards/__init__.py#L415).

Reproduction: give `code-exec-tests` a component minimum isolation of `vm`, while its params contain `min_isolation: confined`. Both implementations preserve the weaker param. The Go worker receives `confined`; loading the same reward through Python produces a bound evaluator with `confined`.

Impact: the explicit component minimum is not enforced. A spec can claim a stronger sandbox requirement while executing with a weaker one.

Proposed fix: reject conflicting isolation declarations, or parse both levels and enforce the stronger minimum. Do not use presence/setdefault semantics for a security minimum. Apply the same rule in Go and Python.

Regression check: test stronger component/weaker param, weaker component/stronger param, matching declarations, invalid levels, and evaluators that do not support isolation. Assert the effective worker request, not only the input spec.

### F02: Policy Replacement Bypasses Existing Labels

Priority: P1.

Location: [policy access targets](../../internal/server/access.go#L578).

Reproduction: create a policy labelled `team: theirs`; give another caller `policies.write` only when `resource.labels.team == "mine"`. Deleting the existing policy is denied. Applying a same-name replacement labelled `team: mine` succeeds because the old policy is checked only when its project differs.

Impact: label-constrained editors can take over or replace policies they cannot otherwise manage.

Proposed fix: authorize both the existing and proposed policy for every replacement, including same-project edits. Use a version/transaction guard so the stored object cannot change between the ownership check and replacement.

Regression check: an allowed new label must not permit replacing a forbidden old label; retain the cross-project check and test concurrent replacement.

### F03: Omitted Judge Bypasses Default-Judge Rules

Priority: P1.

Locations: [evaluation authorization target](../../internal/server/access.go#L521), [judge resolution during binding](../../internal/evaluation/service.go#L144).

Reproduction: configure default judge `paid` and a role condition `resource.judge != "paid"`. An explicit `judge: paid` is denied, but omitting the judge is allowed; binding subsequently substitutes `paid`.

Impact: authorization sees a different judge from the one actually invoked. Judge-specific CEL rules can be bypassed.

Proposed fix: authorize the resolved effective judge, including configured defaults, before sending work. Share judge resolution across authorization and binding; apply it to Evaluate, EvaluateStream, rewards, runs, and policies. Persist effective judge identities for stored specs where appropriate.

Regression check: omitted and explicit default judge must receive the same decision, including streaming requests. Preserve the independent project credential/judge-grant checks.

### F04: Stored Runs Ignore Revoked Grants

Priority: P1.

Locations: [run preparation](../../internal/runs/runs.go#L637), [run evaluation](../../internal/runs/runs.go#L802), [credential reload](../../internal/server/credentials.go#L42).

Reproduction: validate and store a run while judge `paid` is granted to its project. Revoke the grant through `credentials.Policy.Update`. A new validation is denied, but preparation and evaluation of the stored run succeed and invoke the worker.

Impact: queued, resumed, or adopted runs can continue using revoked judge/secret grants. Reload rechecks online policies and guardrails, but stored run execution uses unchecked Bind.

Proposed fix: reauthorize current evaluator, target, agent, sandbox env, and user-simulator credential uses before a stored run executes. Check the credential-policy version at bounded work boundaries so ongoing runs cannot start new work under a revoked grant.

Regression check: queued, resumed, and adopted runs must refuse newly revoked grants without invoking the protected worker/model. Existing stored results should remain readable under ordinary read authorization.

### F05: Dataset Aliases Bypass Project Authorization

Priority: P1.

Locations: [dataset access targets](../../internal/server/access.go#L777), [promoted project inference](../../internal/datasets/datasets.go#L43), [filesystem resolution](../../internal/runs/runs.go#L265).

Reproduction: under datasets_dir, create `alias -> promoted/support`. A checkout-only runner is denied when requesting `promoted/support/secret.jsonl`, but `alias/secret.jsonl` passes the gate and reaches worker loading. The probe's fake loader then returns Unavailable; the demonstrated failure is the authorization bypass, not a claim that the fake loader returned real secret data.

Impact: where such filesystem aliases exist, the spelling supplied by the caller hides the dataset's owning project. The path stays inside datasets_dir but crosses the logical tenant boundary.

Proposed fix: resolve and authorize dataset ownership through one abstraction used by the gate and loader. Authorize the canonical target relative to the canonical dataset root, not the original spelling. Avoid a symlink race between authorization and opening the file, for example with rooted file handles and a resolved-resource contract.

Regression check: direct, normalized, symlinked, and importer-URI aliases to another project's promotions must all require that project's read permission. Same-project aliases should continue to work.

### F07: Failed Write-Backs Never Schedule Their Own Retry

Priority: P2.

Locations: [runner flush loop](../../internal/source/manager.go#L373), [write-back retention](../../internal/source/manager.go#L725).

Reproduction: queue one score, make WriteBack fail transiently, and send no more scores. The first flush returns pending work, but clears its timer without arming another. The focused test sees no retry despite continuing pull cycles.

Impact: a final batch of scores can remain unwritten indefinitely and disappear when the runner stops.

Proposed fix: schedule a bounded-backoff retry whenever pending write-back work remains; respect Retry-After and context cancellation. Re-resolve credentials for retry attempts rather than indefinitely reusing an old connector.

Regression check: a single failed score retries and succeeds without new input; cover partial writes, repeated failure, Retry-After, and shutdown. Durable pending-score recovery is a separate requirement from this timer fix.

### F08: Write-Back Scores From Different Policies Collide

Priority: P2.

Locations: [manager deduplication](../../internal/source/manager.go#L705), [score identity contract](../../internal/source/source.go#L87), [MLflow prior assessment lookup](../../internal/source/mlflow/mlflow.go#L325).

Reproduction: sequentially write the same trace/metric/value for `policy-a` and `policy-b`. Only one score is sent: the second policy is treated as already written. Different values can reuse another policy's prior remote assessment ID.

Impact: policy-specific assessments are suppressed or overwritten, losing score provenance.

Proposed fix: include policy in the write-back identity and digest/bookkeeping contract, and migrate stored source-write keys accordingly. Update manager deduplication, partial-write handling, and all connectors together. Choose an explicit policy-qualified naming strategy where a remote store identifies annotations by name.

Regression check: two policies with the same evaluator metric create distinct assessments; updates and partial retries affect only the matching policy.

### F09: Trace Dataset Resource Attributes Differ From Trace Reads

Priority: P2.

Locations: [dataset trace target](../../internal/server/access.go#L764), [trace resource builder](../../internal/authz/resources.go#L133), [trace dataset loading](../../internal/runs/flywheel.go#L27).

Reproduction: grant traces.read with `resource.service == "public"`. Creating a run with `dataset.traces.service: public` is denied because the gate supplies `resource.traces.service` instead. The probe expected an empty-dataset error after authorization but received PermissionDenied.

Impact: valid service-scoped trace-read permissions cannot drive the trace-to-evaluation workflow.

Proposed fix: define consistent trace/query authorization attributes and share their builders. Filter/check each selected stored trace as well, so labels and other per-trace restrictions remain enforced; do not solve this by granting unrestricted project-wide reads.

Regression check: allowed service succeeds, forbidden service fails, and queries respect per-trace label restrictions and project boundaries.

### F10: TRL Component Helpers Reuse Stale Rewards

Priority: P2.

Locations: [component reuse](../../python/evalsi/src/evalsi/rewards/trl.py#L47), [batch key](../../python/evalsi/src/evalsi/rewards/__init__.py#L359).

Reproduction: score prompt `q`, completion `x`, answer `x`: total 1.0. Call the component function with the same prompt/completion but answer `y`: it returns 1.0. A fresh total call with answer `y` returns 0.0.

Impact: standalone component calls or repeated text batches with changed references/metadata report the previous batch's scores.

Proposed fix: key reuse by the normalized rollouts, including reference and relevant per-sample columns, or use an explicit batch identity shared by total and component calls. Keep non-sample trainer state out of the content key.

Regression check: same text with changed answer, test cases, or metadata recomputes; identical normalized rollouts still reuse the total's results.

### F11: Unfinished Training Evaluations Count as Passed

Priority: P2.

Locations: [server run conversion](../../python/evalsi/src/evalsi/training/loop.py#L348), [CLI success decision](../../python/evalsi/src/evalsi/cli_training.py#L195).

Reproduction: `_from_run` returns a StepResult whose passed property is true for both RUN_STATUS_PENDING and RUN_STATUS_RUNNING. Only FAILED changes gates_passed, and only ERROR/CANCELLED populate error.

Impact: history/curve can display an incomplete checkpoint as successful and return a success exit status. A prematurely ended watch can also be treated as a completed evaluation.

Proposed fix: preserve run lifecycle state and make passed true only for a terminal successful run with passing gates/regression checks. Represent pending/running explicitly rather than inventing an evaluation error. Require terminal state after watching.

Regression check: every RunStatus, missing/unknown status, and a truncated watch. Pending/running must never produce a passed checkpoint or successful final gate result.

### F12: Relative Dataset Roots Fail Under Symlinked Working Paths

Priority: P2.

Locations: [root canonicalization](../../internal/runs/runs.go#L276), [existing regression test](../../internal/runs/resolve_test.go#L11).

Reproduction: `go test ./internal/runs -run TestResolvePathWithARelativeDatasetsDir` fails on macOS. A relative root is EvalSymlinks'd before becoming absolute; the working-directory spelling can retain `/var` while the resolved file is under `/private/var`. Rel then calls a legitimate child outside the root.

Impact: configs such as `datasets_dir: ./datasets` reject valid datasets on affected filesystem layouts.

Proposed fix: make the root absolute first, then resolve symlinks. Use the same canonical-root convention in containment checks.

Regression check: relative and absolute roots, a symlinked working directory, symlinked roots, legitimate children, and escaping paths.

### F13: Full Go Checks Are Not macOS-Portable

Priority: P3; development/test issue.

Locations: [Linux-only guest APIs](../../cmd/evalsi-guest/main.go#L72), [KVM test](../../internal/sandbox/firecracker_test.go#L94), [bubblewrap test](../../internal/sandbox/sandbox_test.go#L188), [check/build targets](../../Makefile#L39).

Reproduction: `go test ./...` on macOS cannot compile the untagged guest command because unix.MS_NOSUID and other Linux APIs do not exist. Two sandbox tests also expect Linux-specific failure messages although the runner correctly reports that Linux is required.

Impact: the advertised full check/build targets cannot run natively on macOS. This does not retract the previously fixed macOS build of evalsid/evalsi-operator, and is not evidence that production isolation fails open.

Proposed fix: isolate Linux guest implementation/tests with build constraints and explicitly cross-build that binary in release/build targets. Make platform-specific tests skip unsupported hosts or assert the correct platform reason without weakening supported-platform checks. State which verification targets require Linux.

Regression check: native macOS checks for supported commands plus Linux guest/sandbox checks in CI; verify the Linux guest still builds statically.

### F14: Worker Test Socket Paths Exceed macOS Limits

Priority: P3; test issue.

Location: [worker fixture](../../python/evalsi/tests/test_worker.py#L113).

Reproduction: normal pytest tmp_path values produce Unix socket paths exceeding macOS's 103-character limit. gRPC fails to bind before exercising the worker. Re-running with a short basetemp removes these failures.

Proposed fix: use a short-lived short socket directory or a localhost ephemeral TCP endpoint for the fixture; clean it up reliably. Do not change application worker behavior just to accommodate test paths.

Regression check: run the worker tests from a long-path checkout/default macOS temporary directory, without requiring a custom pytest basetemp.

### F15: GNU-Specific sed Breaks macOS Demo/Harness Tests

Priority: P3; fixture/test issue.

Locations: [demo reference commands](../../examples/demo/fixtures/build_small_repo_fixes.py#L49), [harness diff fixture](../../python/evalsi-harness/tests/test_harness_agent.py#L397).

Reproduction: even with short temporary paths, pytest still fails the small-repo generated-fixture check and harness diff test. BSD sed rejects their GNU-style `sed -i` commands, so the reference edit never happens.

Proposed fix: use portable reference edit commands, or run explicitly Linux-only fixtures in their intended environment and mark host limitations accurately. Keep the checker assertions intact.

Regression check: generated fixtures remain current and the harness captures the intended diff on both supported host environments.

## Additional Live kind Findings

### F16: RBAC Snapshots Do Not Propagate Between Replicas

Priority: P1.

Locations: [project mutation/local reload](../../internal/authz/service.go#L156), [role mutations](../../internal/authz/service.go#L476), [binding mutations](../../internal/authz/service.go#L550), [cluster reload wiring](../../internal/server/server.go#L253).

Reproduction:

1. Address the two API pods directly, using localhost tunnels to avoid nondeterministic load balancing.
2. Create project `review-ha` on replica A: HTTP 200.
3. Evaluate the same record in that project: A returns 200; B returns 400, `unknown project "review-ha"`. B's ListProjects omits API-created projects, including bootstrap's `boot`.
4. Create a role in `boot` allowing evaluations.run and bind the test `ci` service account. Force B to refresh via an unrelated project creation; both replicas then allow Evaluate with HTTP 200.
5. Change the role on A to traces.read only. A denies Evaluate with 403; B still allows it with 200.

Cause: mutations reload only the local Engine. Policy-change notifications reload watch policies, not authz snapshots; no corresponding RBAC propagation was found.

Impact: load-balanced project/role creation is inconsistent, and revoked permissions remain usable on stale replicas. Unrelated auth mutations or restarting the replica may refresh it, but are not an acceptable revocation contract.

Proposed fix: publish an authz-change notification only after a successful database mutation and reload every replica. Add a stored revision and periodic reconciliation to survive missed notifications/reconnects. Establish a bounded or synchronous revocation guarantee rather than relying solely on best-effort messages.

Regression check: two real replicas, deterministic requests to each, project/role/binding creation and deletion, permission removal, missed notification, restart, and database failure. Revoked access must stop on all replicas within the documented bound.

### F17: Evaluator Workers Crash With a Read-Only Home

Priority: P2.

Locations: [Evaluator pod construction](../../operator/controllers/evaluator.go#L90), [judge cache initialization](../../python/evalsi/src/evalsi/judges/cache.py#L23), [working chart worker mounts](../../deploy/helm/evalsi/templates/workers.yaml#L64).

Reproduction: apply an Evaluator with the current evalsi image and default worker ConfigMap. The operator creates a pod with readOnlyRootFilesystem=true and only /tmp writable. The Python worker creates JudgeCache unconditionally by default and exits:

```text
OSError: [Errno 30] Read-only file system: '/var/lib/evalsi/.cache'
evalsid: pluginhost: worker exited during startup
```

The worker initially appears ready because the generated Deployment has no application readiness/startup probe, then enters CrashLoopBackOff. Chart-managed workers do not show this defect because they mount a writable home.

Proposed fix: mount a writable home/cache emptyDir or set an explicit writable cache location, following chart-managed worker behavior. Preserve the read-only root. Add a meaningful worker-ready/startup signal instead of treating process start as successful initialization.

Regression check: a real Evaluator using the stock image reaches stable readiness, performs a task, and survives a restart under Pod Security restricted. An initialization failure must not transiently report Ready.

### F18: Default Evaluator Config Dependencies Are Not Mounted

Priority: P2.

Locations: [default ConfigMap selection and pod mounts](../../operator/controllers/evaluator.go#L62), [EvaluatorSpec fields](../../operator/api/v1alpha1/types.go#L205), [generated worker config](../../deploy/helm/evalsi/templates/config.yaml#L95).

Reproduction after isolating F17:

1. Set XDG_CACHE_HOME to /tmp/review-cache on the test Evaluator. With the chart's default worker ConfigMap, the process next exits because EVALSI_S3_ACCESS_KEY and EVALSI_S3_SECRET_KEY are absent.
2. Supply a minimal test ConfigMap excluding S3 but retaining the chart's remote sandbox TLS settings. The worker starts, but constructing its first sandbox channel raises FileNotFoundError for `/etc/evalsi/worker-tls/ca.crt`.

Cause: the operator copies the worker ConfigMap reference, but not its S3 credential environment or worker TLS Secret mounts. EvaluatorSpec permits env but has no general volume/Secret-mount contract for the TLS files.

Impact: default custom Evaluator deployments do not work with bundled/credentialed S3 or remote mutual-TLS sandbox pools, both normal chart configurations. CPU workers also fail on unused S3 configuration during startup.

Proposed fix: define a dependency contract for operator-managed workers. Provide the default worker TLS Secret, needed credential Secret references, and writable cache consistently with the chart, or expose narrowly scoped configuration/mount fields. Do not copy unrelated provider secrets into every worker. Validate missing dependencies early and support custom ConfigMaps deliberately.

Regression check: stock Evaluator with chart-default ConfigMap performs an S3 dataset task and remote sandbox evaluation; include custom ConfigMaps, absent Secrets, secret rotation, and restricted pods. Assert actual task success rather than only Deployment readiness.

### F19: Policy Metadata Changes Are Never Applied

Priority: P2.

Locations: [policy reapply decision](../../operator/controllers/policy.go#L59), [policy event predicate](../../operator/controllers/policy.go#L121), [API label extraction](../../operator/api/v1alpha1/project.go#L23), [analogous source reapply decision](../../operator/controllers/tracesource.go#L58).

Reproduction: create OnlineEvalPolicy review-labels in e2e with team=before and wait for Synced. Change only the Kubernetes label to team=after. Generation and observedGeneration remain unchanged. ListPolicies still returns team=before after later reads and an upgrade attempt.

Cause: generation-only events ignore label changes, and periodic reconciles fetch statistics without reapplying when observedGeneration matches. The same structural issue exists for TraceSource labels; that analogous path was inspected, not separately exercised live.

Impact: API-side labels, including attributes used by authorization, diverge from the Kubernetes resource. Changing the project label can also leave old server state behind; project moves need an explicit ownership/migration policy rather than implicit relabelling.

Proposed fix: watch relevant label changes and track a fingerprint of spec plus project and API labels in status. Reapply when that fingerprint differs, not only when generation changes. Make project ownership immutable or implement an explicit authorized move that cleans up old state.

Regression check: label-only edit updates API state for policies and sources; status-only edits do not cause write loops. Cover project-label edits, forbidden labels, and ownership conflicts.

### F20: Bundled MinIO Cannot Write Its PID File

Priority: P2.

Location: [MinIO security context and mounts](../../deploy/helm/evalsi/templates/dev-minio.yaml#L32).

Reproduction: enable devMinio using the stock image bitnamilegacy/minio:2025.7.23. The pod repeatedly exits with code 1:

```text
/opt/bitnami/scripts/libminio.sh: /opt/bitnami/minio/tmp/minio.pid: Permission denied
```

A diagnostic pod using the image's default identity reports uid=1001, gid=0, groups=0. The runtime directory is root:root mode 0775. The chart instead forces runAsGroup=1001, so it cannot write the directory. The volume mounted at /tmp does not cover `/opt/bitnami/minio/tmp`.

The health endpoint briefly reports ready while the entrypoint is still failing. Initial Helm install therefore passed, but the pod restarted repeatedly; the subsequent Helm upgrade failed on MinIO readiness. An application-pod readiness request also received connection refused.

Proposed fix: provide an appropriately owned writable runtime-directory emptyDir at the path the pinned image needs, or use a documented configurable runtime path. Alternatively preserve the image's required group access without running as root, if that matches the chart's security contract. Check all entrypoint write paths and verify bucket creation, not just an early health response.

Regression check: fresh install, sustained readiness with no restarts, authenticated S3 write/read, bucket initialization, restart, and Helm upgrade under the declared pod security settings.

### F21: Bootstrap Keys Are Unusable With Default Chart Auth

Priority: P2.

Locations: [auth ConfigMap construction](../../deploy/helm/evalsi/templates/config.yaml#L31), [bootstrap configuration](../../deploy/helm/evalsi/templates/bootstrap.yaml#L15), [API-key authentication gate](../../internal/auth/authenticator.go#L188).

Reproduction: enable bootstrap with a project and API key, leaving auth.config at its default. The hook succeeds and writes boot-reader-key. Its generated key fails WhoAmI with HTTP 401:

```json
{"code":"unauthenticated","message":"API keys are not enabled on this server"}
```

Cause: the chart enables Kubernetes JWTs but does not add auth.api_keys when bootstrap.apiKeys is nonempty. Creating a stored key does not enable the authenticator.

Impact: the documented no-manual-step bootstrap integration produces credentials that customers cannot use. Existing kind-e2e bootstrap WhoAmI assertions would expose it once earlier blockers are passed.

Proposed fix: expose an explicit API-key authentication setting and enable it by default when bootstrap creates keys, or fail rendering when keys are requested while authentication is explicitly disabled. Respect intentionally disabled configurations rather than silently overriding them.

Workaround: explicitly configure `auth.config.api_keys: {}` for a test install that should accept stored keys; this workaround was identified from the auth contract, not deployed as a source fix.

Regression check: bootstrap-generated key authenticates after fresh install and upgrade, remains stable, is revoked correctly, and behaves consistently on all replicas once F16 is fixed.

## Retraction

### F06: Finished EvalRun Finalizers Were Not Stuck in the Live Test

The first review incorrectly inferred a real deletion failure from a GenerationChangedPredicate unit probe that held generation constant. Real Kubernetes CRD deletion increased generation from 1 to 2. In the dedicated kind cluster, deletion of completed parity and unit-tests runs succeeded; a held test finalizer further showed the operator removed its own finalizer correctly.

F06 is **retracted**, not a bug awaiting a fix. Do not alter finalizer behavior solely to satisfy the artificial earlier probe. A real API-server lifecycle test should preserve this working behavior. The distinct label-only update defect is F19.

The suspected LoRA management URL bug was also discarded after checking official vLLM documentation: the current `/v1/load_lora_adapter` and `/v1/unload_lora_adapter` URLs are valid. The suspected MLflow artifact-listing path mismatch was discarded after checking the server's basename-returning endpoint. Neither is an active finding.

## Checks Completed

- `go test ./...`: most packages passed; failures were the macOS guest build, relative-root regression, and two Linux-specific sandbox assertions described above.
- Selected Go race checks: auth, authz, runs, store, watch, and server. No race was reported; the run-package path regression still failed.
- Nine external Go review probes reproduced the initial behavioral suspicions. Eight remain active; the predicate-only finalizer probe was invalidated by real Kubernetes evidence.
- Python short-temp pytest rerun: 412 passed, 2 failed, 3 skipped. The two remaining failures were the GNU sed fixtures. Focused SDK snippets separately reproduced F01, F10, and F11.
- Native arm64 Docker build of the current repository: succeeded.
- Full-featured kind chart install: initially completed, including bootstrap; later runtime checks exposed F20/F21. Do not interpret the initial green Helm result as sustained service health.
- Live operator/worker parity run: exact-match mean 0.8; numeric-match mean 1.0 over three eligible records, two skipped.
- Live code evaluation on the pod pool: unit-tests mean 0.5.
- Live CLI agent run on the pod pool: task-success mean 1.0.
- Completed EvalRun deletion: passed, retracting F06.
- Namespace-only installation and upgrade under Pod Security restricted: passed.
- Namespace-only code and agent workflows: passed with means 0.5 and 1.0 respectively.
- Full-featured release upgrade: failed because bundled MinIO had a failed pod; release revision 2 records the failure.
- Custom Evaluator deployment: cache failure, missing S3 environment, and missing sandbox TLS files reproduced independently.
- API-replica project/role consistency and role revocation: failed as F16 describes.

## Limits and Existing Deferred Items

The entire kind-e2e script was not executed unchanged: it uses the active kubectl context and prunes shared Docker images/build cache. Equivalent selected workflows were run with the dedicated kubeconfig and without pruning user caches. Air-gap archive/install coverage, reference demo image workflows, live MLflow/Langfuse/Phoenix pulling, KEDA, several-node NATS, a forced HA run-owner failure, enforced CNI policies, mesh behavior, real model calls, gVisor/Kata, and Firecracker/KVM were not verified in this pass.

Previously documented limitations were not relabelled as new discoveries. In particular, trace redaction before storage, durable recovery of stored-but-unscored pulled traces, concurrent Postgres annotation over-claiming, per-replica concurrency quotas, and cross-release sandbox-pool network isolation are already tracked in [LEFTOVERS.md](../LEFTOVERS.md). Their existing deferred status does not establish that they are safe; they remain separate work.

## Suggested Fix Order

1. F22, F16, F01-F05, F23: untrusted expression evaluation, replica-wide revocation, isolation minimums, effective-resource authorization, and quota admission.
2. F20-F21 and F17-F18: stock installs, generated credentials, and custom worker viability.
3. F07-F09, F19, F24, F31: reliable write-back, score identity, metadata/trace authorization, usable promotions, and storage lifecycle.
4. F10-F12 and F25-F30: reward/training correctness, dataset path handling, and trustworthy analytics/reporting.
5. F13-F15: host-development portability without weakening Linux checks.

For each fix, land a regression reproducing the evidence here, then run the relevant repository gates. Live worker/operator/chart changes need a real cluster check, not merely a template assertion. No tests should be skipped or weakened just to obtain a green result.

## Additional Product-Workflow Findings

These findings were verified after the initial kind pass. The retained cluster was used only for one additional ordinary run and read-only analytics comparisons. No paid model or judge call was used. Two further Go probes used an external overlay; Python probes ran in the existing uv environment.

### F22: Symbolic Math Grading Executes Untrusted Expressions

Priority: P1. This affects installations that have the optional SymPy dependency and select the sympy backend, or reach it through auto fallback. The probe does not establish that the minimal Docker image installs SymPy by default.

Locations: [symbolic comparison](../../python/evalsi/src/evalsi/packs/rl.py#L216), [expression parser](../../python/evalsi/src/evalsi/packs/rl.py#L240), [math evaluator requirements](../../python/evalsi/src/evalsi/packs/rl.py#L289), [optional math dependency](../../python/evalsi/pyproject.toml#L25).

Reproduction: evaluate a math-equiv record whose answer contains a harmless Python function call printing a unique review marker. Select backend=sympy. The marker is printed by the evaluator process, and the evaluation returns a scored outcome. The test performed no network access, file access, destructive operation, or expensive computation.

Cause: parse_expr evaluates model-supplied text using Python expression evaluation. The 200-character length limit and string normalization are not an arithmetic grammar or isolation boundary. The evaluator requires a reference, but does not declare sandbox isolation.

Impact: untrusted model output can invoke Python behavior in the worker or embedding application's process. This is materially different from grading generated code inside a sandbox and undermines the runs_code/isolation contract. Expensive symbolic expressions are also a resource-exhaustion risk; that additional effect was not exercised.

Proposed fix: replace unrestricted expression evaluation with a bounded arithmetic parser accepting an explicit allowlist of operators, symbols, and mathematical functions. Reject imports, attribute access, arbitrary calls, and non-mathematical syntax before conversion to symbolic objects. Run symbolic evaluation in a properly isolated, resource-limited process and declare that execution requirement in the evaluator manifest where appropriate. Clearing globals or relying on string length alone is not sufficient.

Regression check: ordinary fractions, roots, constants, tuples, and symbolic equivalence still work. Function-call markers cannot execute; unsupported syntax is rejected without side effects; pathological expressions are bounded by CPU/memory/time limits. Verify both embedded and worker invocation paths with the math extra installed.

### F23: Shadow Replay Bypasses Stored-Run Quotas

Priority: P1.

Locations: [ordinary run admission](../../internal/runs/runs.go#L428), [shadow replay creation](../../internal/runs/flywheel.go#L343), [stored-run limit](../../internal/runs/quota.go#L38).

Reproduction: set max_stored_runs=1, create one run, and confirm an ordinary second CreateRun receives ResourceExhausted. CreateShadowReplay succeeds and creates its baseline/candidate pair. The project now has three stored runs despite the quota of one.

Cause: the shadow handler calls createRun twice without calling the ordinary admission quota check. Execution-time token accounting does not enforce max_stored_runs. The test directly confirms the stored-run bypass; daily-token admission uses the same omitted check, but its shadow behavior was not separately exercised.

Proposed fix: centralize admission/reservation for all run-creation paths. A shadow replay must atomically reserve capacity for two runs, check relevant daily admission limits, and create both snapshots or neither. Avoid a check-then-insert race across replicas and avoid leaving an orphan baseline when candidate creation fails.

Regression check: a quota with zero or one available slot rejects the pair without creating either run; two available slots permit it; concurrent CreateRun and shadow requests cannot exceed capacity. Preserve execution-time spend accounting.

### F24: Promotions Can Produce Unloadable Regression Datasets

Priority: P2.

Locations: [promotion append](../../internal/runs/flywheel.go#L282), [dataset append](../../internal/datasets/datasets.go#L93), [loader uniqueness check](../../python/evalsi/src/evalsi/datasets.py#L73).

Reproduction: run two different inputs, each with record ID r0, and promote both into the same regression dataset. Promotion preserves both IDs. The server's actual NormalizeIDs rejects the resulting records with `duplicate record id "r0"`. The Python loader independently enforces the same uniqueness rule.

Impact: the common loop of promoting failures from multiple runs can create a dataset that neither embedded nor server evaluation will accept. Repeating the same promotion also appends duplicates rather than making the operation safe to retry.

Proposed fix: define a stable promoted-record identity that distinguishes genuinely different cases, while retaining original ID, source run, and trial in provenance. Choose and document whether repeated promotion deduplicates by provenance/content or creates a new version. Implement conflict handling consistently for local and S3 append paths.

Regression check: promote different cases with the same original ID from multiple runs, reload the result, and create a regression run successfully. Retry the same promotion and test concurrent writers without duplicate IDs or lost cases.

### F25: Analytics Changes Aliased Primary Metric Names

Priority: P2.

Locations: [analytics metric naming](../../python/evalsi/src/evalsi/analytics.py#L68), [server metric naming contract](../../internal/evaluation/summarize.go#L21).

Reproduction: evaluate exact-match under alias quality. The SDK/backend summary metric is quality, but importing the results into Warehouse creates quality.exact-match. Asking Warehouse.slice for quality returns no rows. The same mismatch was reproduced from the actual kind API.

Cause: analytics compares score.name with the alias, not the evaluator reference's primary metric or the established summary naming contract.

Proposed fix: share canonical metric identity rules across execution, analytics, reports, and comparisons. Resolve the primary score using evaluator_ref/manifest or an explicit score-to-summary mapping; do not infer it solely from an alias string.

Regression check: aliased primary metrics, extra metrics, multiple aliases of the same evaluator, embedded files, and server JSON all expose the same metric names.

### F26: Analytics Double-Counts Repeated Record Rows

Priority: P2.

Locations: [paged record import](../../python/evalsi/src/evalsi/analytics.py#L193), [records table identity](../../python/evalsi/src/evalsi/analytics.py#L44), [slice join](../../python/evalsi/src/evalsi/analytics.py#L273).

Reproduction on kind: create one record evaluated by two aliased evaluators. Read ListRunResults with page_size=1. Each valid page includes that record, so Warehouse loads two record rows. There is one quality score, and the backend summary reports n=1, but the warehouse slice reports n=2.

Cause: imported records are appended without deduplication, and the slice joins every matching record_id. Repeated trials also share record IDs, so joining only by run_id and record_id is not sufficient when outputs differ by trial.

Impact: sample counts and confidence intervals are wrong; with trial-dependent metadata/outputs, scores may be associated with the wrong slice or multiplied across slices.

Proposed fix: define record/output identity explicitly. Deduplicate repeated page records and join trial-specific outputs on a trial-aware key. If metadata is intentionally dataset-level, store one dataset record separately from trial outputs. Enforce unique keys in the warehouse schema rather than relying on callers to provide nonoverlapping pages.

Regression check: page boundaries splitting a record's evaluators, repeated trials, changed trial metadata, reloads, and multiple score outputs. The slice sample count must match actual scored observations without multiplying rows.

### F27: List-Based Analytics Drops Model Metadata

Priority: P2.

Locations: [bulk analytics loading](../../python/evalsi/src/evalsi/analytics.py#L241), [model extraction](../../python/evalsi/src/evalsi/analytics.py#L207), [ListRuns contract](../../proto/evalsi/v1alpha1/run_service.proto#L61).

Reproduction: use the real API shape where ListRuns omits spec and GetRun contains target.model=known-model. Warehouse.add_server_runs imports the list item directly, never calls GetRun, and stores an empty model. The focused probe returned an empty model despite a nonempty full run.

Impact: cross-model SQL comparisons and model filters are silently incomplete. Existing analytics tests use list fixtures that include spec, masking the mismatch with the actual server contract.

Proposed fix: fetch the full run before importing details absent from list responses, or expose a deliberately small target/model summary in ListRuns. Keep API and client fixtures faithful to that contract. Do not silently substitute an empty model for an unfetched value.

Regression check: bulk list loading and explicit run-ID loading yield identical model/project/status metadata; fixtures must omit spec from ListRuns as the server does.

### F28: Embedded RunResult Passes When Every Evaluation Errors

Priority: P2.

Locations: [RunResult.passed](../../python/evalsi/src/evalsi/run.py#L47), [embedded run assembly](../../python/evalsi/src/evalsi/run.py#L235), [CLI's separate all-error guard](../../python/evalsi/src/evalsi/cli.py#L486).

Reproduction: execute a run containing one custom evaluator that raises backend unavailable, with no gates. Its only result has outcome=error, but RunResult.passed is true because all() over an empty gate list is true.

Impact: programmatic consumers, including embedded checkpoint evaluation, can accept a failed evaluation. The CLI already contains a separate guard, so this finding does not claim that the existing CLI exits zero for the same all-error case. The server also has an allErrored guard; the common result contract remains inconsistent.

Proposed fix: represent execution status in RunResult and define passed consistently with server/CLI semantics. Centralize the all-error decision instead of duplicating it at the CLI boundary. Keep legitimate skipped/no-data behavior explicit rather than converting all missing data to failure accidentally.

Regression check: all-error, scored-plus-error, all-skipped, no gates, failing gates, and embedded checkpoint consumers. Programmatic all-error runs must not appear successful.

### F29: Reports Display the Wrong Trial's Answer

Priority: P2.

Location: [report record lookup](../../python/evalsi/src/evalsi/report.py#L98).

Reproduction: provide trial 0 with a wrong answer and failed score, and trial 1 with a correct answer and passed score, both for record r. Both record outputs include their run trial provenance. The report's failed trial 0 example displays the correct trial 1 answer because the by_id dict retains only the last record.

Impact: the main failure-diagnosis artifact contradicts the actual graded output and can lead users to debug or promote the wrong case.

Proposed fix: join results to outputs by run, record ID, and trial. Carry explicit trial identity through both embedded exports and server result pages; preserve a dataset-level fallback only when no generated output exists. Do not guess trial associations from ordering or silently select the last record.

Regression check: differing outputs over multiple trials, out-of-order pages, partial generation failure, and embedded/server report parity. Each displayed example must be the answer that produced that result.

### F30: Reports Treat Numeric 1 as Perfect Without a Scale

Priority: P2.

Location: [best-example filter](../../python/evalsi/src/evalsi/report.py#L145).

Reproduction: define a supported custom numeric evaluator rating with declared min=1, max=5, higher_is_better=true and return score 1. The result is valid, but from_results_file produces no worst examples because _is_best assumes any higher-is-better value of 1 is perfect.

Impact: low scores on unnormalized metrics can disappear from the failure report. This does not claim the built-in llm-judge uses an unnormalized scale; that evaluator normalizes its 1-to-5 rating to 0-to-1.

Proposed fix: suppress explicit passed=True scores or use declared metric bounds to determine perfection. For numeric metrics with unknown bounds, keep the requested worst examples instead of assigning a magic perfect value. Preserve scale/type metadata in reports where needed.

Regression check: normalized scores, 1-to-5 ratings, unbounded numeric metrics, lower-is-better metrics, and explicit pass/fail. Numeric 1 is only perfect when the metric contract establishes that fact.

### F31: Stored-Run Quotas Have No Supported Recovery Path

Priority: P2; a verified product capability gap, not a storage corruption claim.

Locations: [quota error instruction](../../internal/runs/quota.go#L44), [public RunService methods](../../proto/evalsi/v1alpha1/run_service.proto#L10), [trace-only retention](../../internal/server/server.go#L401).

Evidence: the existing quota test confirms max_stored_runs rejects the next run at capacity. The error instructs the user to delete old runs, but RunService has no DeleteRun, the SDK/CLI expose no corresponding command, and configured retention deletes traces rather than stored runs. Deleting an EvalRun CRD cancels its execution and intentionally keeps its stored results.

Impact: a project at capacity cannot follow the product's recovery instruction. Its practical options are increasing the quota or unsupported database maintenance, while stored result/privacy retention is also incomplete relative to the PRD.

Proposed fix: add an authorized, audited run deletion or retention/archive API that consistently removes dependent results, outputs, snapshots, reward/webhook references where required, and backing objects under a documented policy. Make quota messages point to an actual supported operation. Preserve legal/audit retention deliberately rather than indiscriminately deleting all history.

Regression check: reach capacity, remove/archive an eligible completed run through the public API, then create another. Cover active runs, cross-project permissions, shared objects, pending webhook deliveries, and crash-safe cleanup.

## Product Improvements

These are prioritized recommendations, not additional confirmed bugs or promises that untested workflows work. Effort is relative; items spanning API, storage, SDK, and operator contracts require design before implementation.

| Priority | Improvement | Why it matters | Acceptance criterion |
| --- | --- | --- | --- |
| Now | A single execution/result contract | Gates, errors, skipped records, trial identity, aliases, and scales currently diverge between consumers | One shared set of vectors passes in server, SDK, analytics, reports, and checkpoint evaluation |
| Now | Dependency-aware installation checks | Process/TCP readiness passed before workers initialized and MinIO stopped restarting | A chart preflight exercises worker Describe, database access, authenticated S3 write/read, sandbox creation, and bootstrap-key sign-in; failures name the missing dependency |
| Now | Supported run/data lifecycle | Quota recovery, privacy deletion, repeated promotion, and historical storage need explicit ownership | Authorized retention/delete/archive and promotion deduplication are documented and verified across local/S3/Postgres backends |
| Next | Reproducible immutable run manifests | A stored evaluator ref/default judge can resolve differently later; the current spec is not a complete execution lock | Persist effective evaluator version/digest, judge configuration identity, dataset hash, target settings, and sandbox image digest; resume either matches them or requires an explicit migration |
| Next | Idempotent create and promotion APIs | Network retries can create extra runs or duplicate regression cases; shadow pairs need atomic creation | Retrying an idempotency key returns the same operation/result; partial pair creation is rolled back or surfaced as an explicit recoverable operation |
| Next | Durable transactional event delivery | Run state and webhook enqueue currently happen in separate calls; write-back pending work is memory-bound | Crash-injection tests prove no committed completion loses its event; retries retain stable delivery identity and are observable |
| Next | Bounded streaming and paginated analytics | EvaluateStream retains accumulated records/results, and the SDK prepares records eagerly; analytics downloads whole runs | A documented workload runs under a bounded memory budget, supports cancellation/backpressure, and can page/filter results without downloading unrelated data |
| Next | Operational diagnostics and failure coverage | A high mean can coexist with errors, skips, stale grants, or lost write-backs | Run/queue views report scored/error/skipped denominators, sample coverage, current state, retry reason, and suggested recovery; readiness and revocation lag have metrics |
| Later | Statistically consistent comparisons and slices | Trials are correlated; slice counts and normal intervals can mislead even once duplicate joins are fixed | Slice/comparison APIs expose unit of analysis, paired coverage, cluster key, metric direction/bounds, and CI method; repeated identical trials do not falsely increase confidence |
| Later | Small, repeatable compatibility matrix | Mocks included impossible list shapes and readiness shortcuts; vendor/schema upgrades can pass unit tests but fail users | CI includes small real-cluster contract scenarios and pinned upstream connector fixtures, with separate optional infrastructure tests and clear unverified-status reporting |

### Recommended Release Gates

1. Block symbolic backends from evaluating untrusted Python before exposing them to external records; require fail-closed isolation and resource limits.
2. Make every run creation path honor atomic quotas and idempotent operation identity.
3. Validate a failure-to-regression loop end to end: run, inspect the exact failed trial, promote repeatedly, reload the dataset, and compare a candidate using the same canonical metrics.
4. Require server/SDK parity for lifecycle and score identity, with realistic paginated JSON rather than over-permissive mocks.
5. Treat healthy dependencies, usable generated credentials, and bounded permission-revocation latency as release criteria, not only successful Helm rendering.
# Evals.si — deep review bug report

Branch reviewed: `main` at `1f6f63e1b7324ad654860b3c352220beba960a5f`  
Fix branch: `fix/deep-review-2026-10-22`  
Date: 22 October 2026

Nine reviewers each took one area of the repository (store/runs, server/auth, sandbox/wasm, sources/ingest, operator/Helm, the Go evaluation core, the Python SDK core, Python packs/CLI, harness/adapters/examples). They reported a finding only after reproducing it: a failing test, a run of the CLI or daemon, a Helm render, or a Linux container run. Every finding below has a regression test on the fix branch. Each test fails against `main` and passes with its fix, except where the entry says otherwise. Section 12 lists the items that are still open.

The review also re-checked areas it did not change: the Go and Python statistics (Wilson, t, clustered-t, bootstrap, percentile indexing), the generated protobuf code in both languages, CRD sync between `operator/config/crd` and the Helm chart, static UI serving against path traversal, and webhook authorisation across roles. The adapter suites, the harness suite, `make e2e` and the website build all passed.

Severity: **P0** security or data exposure; **P1** wrong results, data loss or a stuck system; **P2** incorrect behaviour with a workaround; **P3** minor.

## Summary

| ID | Sev | Area | Finding |
| --- | --- | --- | --- |
| S1 | P0 | sandbox | A planted workdir symlink gives host file read/write |
| S2 | P0 | sandbox | Image `dir:/` mounts the host filesystem |
| A1 | P0 | auth | A non-empty `required_claims` drops the `exp` requirement |
| A2 | P0 | authz | In permissive JWT mode, forged claims satisfy `cel:jwt.*` bindings |
| G1 | P0 | guardrail | A valid card number followed by digits is not redacted |
| W1 | P1 | watch | A failed result handoff re-runs paid evaluators every second |
| D1 | P1 | deepeval | Toxicity, bias and hallucination score in the wrong direction |
| P1 | P1 | packs | An invalid judge answer is cached, so retries never recover |
| P2 | P1 | packs/rl | Unit stripping makes `Paris` equal `Pari` |
| R1 | P1 | runs | Cluster adoption re-executes a run that already finished |
| R2 | P1 | runs | Retention deletes runs that a restart interrupted |
| O1 | P1 | operator | The webhook blocks finalizer removal, so deletes hang |
| C1 | P1 | client | A truncated `watch_run` stream counts as completion |
| C3 | P1 | targets | One malformed target response aborts the generation batch |
| S3 | P1 | guest | `max_procs` is not enforced for root |
| — | P2–P3 | various | Further findings (sections 1–10) |
| — | P3 | verification | 11 verification-runner fixes (section 11) |

## 1. Sandbox (`internal/sandbox`, `internal/guest`, `cmd/evalsi-guest`)

**S1 (P0) — Workdir symlink escape.** `host.go` `openRoot` called `os.OpenRoot(b.work)`, and `OpenRoot` follows symlinks in the path it is given. With an image and a writable root, a sandboxed command could run `rm -rf /work; ln -s /etc /work`. After that, `ReadFiles` and `WriteFiles` worked on the host's `/etc`. The repro read host `/etc/passwd` and created a file in host `/tmp`. *Fix:* open the image root and resolve the workdir inside it, which also covers symlinked parent directories. *Test:* `TestHostFilesStayInsideRootWhenWorkdirIsSymlink`.

**S2 (P0) — Local image refs from untrusted specs.** `dir:`, `oci-layout:` and `docker-archive:` refs were only refused when `image_signatures` was configured. With `dir:/`, a sandboxed command could read the daemon user's files. *Fix:* a new `sandbox.allow_local_images` setting, off by default. When `image_signatures` is configured, its `allow_local` still decides. The setting is documented in `docs/guides/agent-runs.md`. *Test:* `TestLocalImagesRefusedUnlessAllowed`. **Behaviour change:** local image refs, including those used with `evalsid sandbox run`, now need `allow_local_images: true`.

**S3 (P1, partly fixed) — `max_procs` for root.** The kernel exempts uid 0 from `RLIMIT_NPROC`. In the repro, 41 processes ran with a limit of 5. *Fix:* each command is also placed in a pids cgroup with `pids.max = max_procs`, and the guest init now mounts cgroup2. This is best effort: if the guest has no pids controller or no writable cgroup, root commands are still unlimited. *Test:* `TestLimitProcsBindsRoot`, which only runs with `EVALSI_TEST_PIDS_CGROUP=1` in a privileged container started with `--cgroupns=host`.

**S4 (P2) — Egress reply lost after half-close.** The guest's `StartEgress` waited for one direction and then closed both, so a client that sent its request and half-closed never got the reply. *Fix:* each direction is half-closed and both are awaited, as `Forward` already does. *Test:* `TestEgressKeepsReplyAfterClientHalfClose`.

**S5 (P2) — Python `run_sandboxed` leaked its subprocess on cancel.** *Fix:* on `CancelledError` the `evalsid sandbox run` subprocess is killed and reaped, then the error is re-raised. *Test:* `test_cancelling_a_local_run_kills_the_evalsid_subprocess`.

**S6 (P3) — A missing cwd was reported as "command not found".** It now exits 126 with `working directory not found`. *Test:* `TestExecMissingCwdIsNotCommandNotFound`.

**S7 (P3) — The guest's `ReadFiles` silently omitted symlinks.** They are now listed in `Missing`. *Test:* `TestReadFilesReportsSymlinkAsMissing`.

**S8 (P3) — Python `read_files` dropped `truncated` and `missing`.** It now returns `ReadFilesResult`, a `dict` subclass with `.missing` and `.truncated`, so existing callers keep working. *Test:* `test_read_files_surfaces_missing_and_truncated`.

**S9 (test) — Two tests failed on macOS.** `TestFirecrackerPlaceCopiesUnderTheJailer` now skips off Linux. `TestImageSignaturesFromCosign` skips on cosign v3 (see section 12).

## 2. Authentication and authorisation (`internal/auth`, `internal/authz`, `internal/server`)

**A1 (P0) — `required_claims` replaced the default `exp`.** A signed token without `exp` was accepted and never expired. *Fix:* `exp` is always required, and `required_claims` adds to it. `docs/guides/identity.md` is updated. *Test:* `TestRequiredClaimsStillRequireExp`.

**A2 (P0) — Permissive JWT mode trusted unverified claims.** `cel.go` exposed an anonymous principal's unverified claims as `jwt.*`. A token signed with any key matched a `cel:jwt.repository == …` binding and got `runs.create`. *Fix:* anonymous principals get no `jwt.*` variables. The claims are still recorded on the principal for logging. *Test:* `TestPermissiveForgedTokenGetsNoCELRoles`.

**A3 (P2) — Existence oracle in `CreateReleaseDecision`.** A missing run returned 500 `store: not found`, while a forbidden run returned `PermissionDenied`. *Fix:* the resolver goes through `runTargets`, so both cases look the same. *Test:* `TestReleaseDecisionMissingRunIndistinguishableFromForbidden`.

**A4 (P3) — Project-name enumeration.** An unknown project gave `InvalidArgument`, while an existing but forbidden one gave `PermissionDenied`. *Fix:* both now return `PermissionDenied`. `datasetUploadTargets` now also checks that the project exists; before this, an owner could upload into a nonexistent project. *Test:* `TestUnknownProjectIsPermissionDenied`.

**A5 (P3) — Rate-limited uploads returned 413.** *Fix:* a rate limit or a busy upload queue now returns 429 with `Retry-After`; real size limits stay 413. *Test:* `TestDatasetHTTPErrorResourceExhausted`.

**A6 (P3) — Unknown query parameters returned 500.** They now return 400. *Test:* `TestRESTRejectsUnknownQueryParamsFieldsAndEnums`.

**A7 (P2) — Unknown JSON fields and enum values were silently dropped.** A misspelled webhook event created a webhook subscribed to every event, and a misspelled `permisions` field stored a role without its permissions. *Fix:* the REST and Connect-JSON request decoders reject unknown fields and enum values with 400. Response encoding is unchanged. *Test:* same as A6. **Behaviour change:** a client that sends fields this server doesn't know now gets 400. The Python client tests pass, and a few web UI request bodies were checked by eye, but not every UI call was audited.

## 3. Guardrails, annotation and summaries (Go and Python)

**G1 (P0) — Card number leak.** The card regex is greedy. "card 4111111111111111 12/25" matched as one 19-digit run, failed the Luhn check, and was left unredacted. *Fix:* when the greedy match fails validation, valid 13–19-digit windows inside it are redacted, longest first, starting and ending on digit-group boundaries. The `phone` builtin had the same flaw and is fixed the same way. *Test:* `TestRedactionSalvagesEmbeddedValues`.

**G2 (P2) — Krippendorff alpha for interval values was numerically unstable.** Identical scores gave alpha 0.69 or 1.236 (alpha can't exceed 1), and an offset of 1e8 gave -1.07. *Fix:* sum squared deviations from the mean, so identical scores give exactly zero expected disagreement and alpha is undefined. *Test:* `TestAlphaIntervalNumericallyStable`.

**G3 (P2) — Cluster keys collided across types.** Go merged the string `"1"` with the number `1`, and Python merged `True` with `1`. *Fix:* keys carry their type in both languages. *Tests:* `TestClusterOfKeepsTypesApart`, `test_cluster_keys_are_typed`. **Behaviour change:** such values now form separate clusters.

**G4 (P3) — Non-finite confidence intervals.** Go dropped a NaN interval but kept ±Inf. Python kept NaN, and its clipping turned a NaN bound into the clip bound, so the interval looked valid. *Fix:* both languages drop a non-finite interval before clipping. *Tests:* `TestSummarizeDropsNonFiniteCI`, `test_non_finite_interval_is_dropped`.

**G5 (P3) — Infinite reward components.** ±Inf passed validation, and with a weight of 0 it produced a NaN total. *Fix:* any non-finite value is now a component error, in Go and in Python. *Tests:* `TestComponentResultRejectsInfinity`, `test_non_finite_component_value_is_an_error`.

## 4. Runs (`internal/runs`)

**R1 (P1) — A finished run was re-executed.** `adopt` lists runs, then checks each lease. A run that finished in between was started again: it flipped back to RUNNING and `OnFinished` fired twice, sending duplicate webhooks and sink exports. *Fix:* `execute` returns at once when the run it reads is already terminal. *Test:* `TestStartDoesNotReExecuteRunFinishedAfterAdoptListing`.

**R2 (P1) — Retention deleted runs that a restart interrupted.** On restart, runs were marked ERROR without `FinishedAt`. Retention then fell back to `CreatedAt` and deleted any such run older than the retention period, even though the error message tells the user to resume it. *Fix:* recovery sets `FinishedAt`. *Test:* `TestRetentionKeepsRunInterruptedByRestart`.

## 5. Online evaluation, sources, sinks and storage

**W1 (P1) — A failed handoff re-ran evaluators.** When `OnResults` failed, the whole batch stayed un-acked, and the 1-second recovery tick re-ran every evaluator, including paid LLM judges, and counted `seen`, `evaluated` and the window stats again. *Fix:* results are kept per trace and per policy, so a retry only redoes the failed step. Handoff retries back off from 2s up to 60s. *Test:* `TestFailedResultHandoffRetriesWithoutReevaluating`.

**W2 (P2) — Sink retries duplicated scores.** After a partial failure, the dispatcher retried the whole export. *Fix:* Langfuse score IDs are now a hash of trace, scope and metric. MLflow exports remember which scores were already delivered. *Tests:* `TestLangfuseScoreIDsAreStableAcrossRetries`, `TestMLflowRetryDoesNotRepostDeliveredScores`. **Behaviour change:** re-scoring a trace overwrites its Langfuse score instead of adding a second one.

**W3 (P2) — S3 folder markers broke `Fetch`.** A zero-byte `prefix/sub/` key was written as a file, and the next nested key failed with "not a directory". *Fix:* these marker keys are skipped. *Test:* `TestDownloadSkipsFolderMarkers`.

**W4 (P2) — GenAI tool results were dropped.** The OTel GenAI `ToolCallResponsePart` carries the tool result in `response`, which was checked against `semantic-conventions-genai` `gen-ai-input-messages.json`. The code read `content`. *Fix:* read `response`, stringify non-string values, and fall back to `content`. *Test:* `TestGenAIToolCallResponseReadsResponse`.

**W5 (P2) — An empty page with a next cursor ended a source cycle.** The cycle then advanced its watermark past data it had not read. *Fix:* the cycle stops only when `Next` is empty or repeats. *Test:* `TestEmptyPageWithNextCursorDoesNotEndCycle`.

**W6 (P3) — Dataset `Append` glued rows together.** A file with no trailing newline had the first new row joined onto its last line. *Fix:* write a newline first when one is missing. *Test:* `TestAppendToFileWithoutTrailingNewline`, which covers the local path only.

## 6. Operator and Helm

**O1 (P1) — Deletes hung.** The validating webhook also validated the operator's own finalizer-removal update. Once a project mapping was removed, EvalRun, OnlineEvalPolicy and TraceSource objects could never finish deleting. *Fix:* allow any update to an object that has a `deletionTimestamp`. *Test:* `TestUpdateOfDeletingObjectAllowed`.

**O2 (P2) — A stale KEDA ScaledObject.** Removing `spec.autoscaling` left the ScaledObject in place, still fighting `spec.replicas`, and status claimed KEDA was active even when its CRD was missing. *Fix:* delete the ScaledObject, and report `Autoscaling=False/KEDAMissing` when KEDA isn't installed. *Test:* `TestEvaluatorRemovesScaledObjectWhenAutoscalingRemoved`.

**O3 (P2) — A TraceSource orphaned on project change.** *Fix:* a new `status.project` field (CRDs regenerated). The source is removed from the old project when the label changes and on delete. *Test:* `TestTraceSourceProjectMoveDeletesFromOldProject`.

**O4 (P2) — Expired API keys were never rotated by bootstrap.** *Fix:* an expired key is treated as absent and replaced. *Test:* `TestBootstrapRotatesAnExpiredKey`.

**O5 (P2) — The all-namespaces ClusterRole lacked `services` and `configmaps`.** CheckpointServer reconciles failed with this role. *Fix:* both added. *Test:* `TestClusterOperatorRoleCoversGeneratedRole` renders the chart and checks it covers `operator/config/rbac/role.yaml`.

**O6 (P2) — ConfigMap edits didn't roll Evaluator pods.** *Fix:* a config-hash annotation on the pod template, plus a ConfigMap watch. *Test:* `TestEvaluatorConfigHashRollsPods`.

**O7 (test) — The envtest suite failed with `-count>1`.** *Fix:* `SkipNameValidation` in `suite_test.go`.

## 7. Python client, targets, worker and judges

**C1 (P1) — Truncated streams.** `Client.stream` returned normally when the body ended without END_STREAM, so `gate_candidate` went on to create a release decision for a run that may not have finished. *Fix:* raise `ServerError("truncated_stream")`. `gate_candidate` also checks that the candidate run reached SUCCEEDED or FAILED. *Tests:* `test_client_stream_without_end_stream_is_an_error`, `test_gate_candidate_rejects_a_candidate_that_did_not_succeed`.

**C2 (P2) — Non-JSON bodies.** A proxy's 502 HTML page raised `JSONDecodeError`, which skipped `ServerError` handling. *Fix:* non-JSON bodies become `ServerError("http_<status>")`, or `ServerError("invalid_response")` on a 200. *Test:* `test_client_non_json_bodies_are_server_errors`.

**C3 (P1) — One bad target response aborted the batch.** A 200 response with HTML or a JSON list escaped `generate_all`, and records that had already succeeded were lost. *Fix:* the target raises `TargetError`, so the failure stays on that record's `Generation.error`. *Test:* `test_generate_all_survives_malformed_target_responses`.

**C4 (P2) — Worker harness leak.** With an empty `run_id`, the cache lookup and store used different keys, so every call built a new harness and never closed the old one. Also, `aclose()` didn't await evicted harnesses. *Test:* `test_harness_without_run_id_is_cached_and_aclose_awaits_evictions`.

**C5 (P2) — Malformed judge responses.** A 200 with a non-JSON body raised `ValueError` instead of `JudgeError`. *Test:* `test_openai_compatible_malformed_200_is_a_judge_error`.

## 8. Packs, rewards and CLI (`python/evalsi`)

**P1 (P1) — Judge cache poisoning.** `complete_json` cached the response before the evaluator validated it. A score of 7 on a 1–5 rubric was cached, so the retry got the same bad answer. *Fix:* an optional `validate` callback. A rejected answer is not stored, and a bad cached entry is skipped. *Test:* `test_invalid_judge_answer_is_not_cached`.

**P2 (P1) — Unit stripping.** `normalize_answer` stripped unit suffixes from any word: `Paris` equalled `Pari`, `King` equalled `Kin`, and `m` equalled `s`. *Fix:* units are only stripped after a number. *Test:* `test_unit_stripping_needs_a_number`.

**P3 (P2) — Duplicate ids inflated ranking metrics.** The output `["a","a","a"]` gave ndcg 2.13 and map 3.0. *Fix:* deduplicate the output, keeping the first occurrence. *Test:* `test_ranking_duplicate_ids_do_not_inflate`.

**P4 (P2) — `refusal` missed "I'm unable to…" and curly apostrophes.** *Test:* `test_refusal_contractions_and_curly_apostrophes`.

**P5 (P2) — `pii-leak` missed a card followed by a CVV.** *Fix:* Luhn-valid prefixes of the digit run are checked. *Test:* `test_pii_leak_finds_card_followed_by_cvv`.

**P6 (P2) — `numeric-match` read "10-12" as -12 and dates as negative numbers.** The same regex in `rl` is fixed too. *Tests:* `test_numeric_match_hyphen_is_not_a_sign`, `test_last_number_fallback_ignores_range_hyphen`.

**P7 (P2) — `tool-call-accuracy` greedy matching.** An unpinned expectation took the call a pinned one needed, giving recall 0.5 instead of 1.0. *Fix:* pinned expectations are matched first. This is a heuristic, not full bipartite matching. *Test:* `test_tool_call_accuracy_pinned_expectation_is_matched_first`.

**P8 (P2) — `data-quality` ignored an unknown `type`.** A typo such as `int` turned validation off. It now raises an error. *Test:* `test_data_quality_unknown_type_is_an_error`.

**P9 (P2) — `evalsi eval` exited 0 when every evaluation errored.** It now exits 1, matching `evalsi run`. *Test:* `test_eval_exits_1_when_every_evaluation_errored`.

**P10 (P3) — A blank `cache:` key in a reward spec crashed with a traceback.** *Test:* `test_reward_spec_blank_cache_key_and_bad_cache_type`.

**P11 (P3) — `--limit -1` was accepted silently.** *Test:* `test_eval_rejects_negative_limit`.

**P12 (P2) — A mistyped `--regression` metric passed the gate silently.** It now raises an error. *Test:* `test_compare_rejects_unknown_regression_metric`.

**P13 (P2) — `plugins remove ..` was accepted.** It could `rmtree` the parent of the plugins directory. It is now rejected. *Test:* `test_remove_rejects_dot_names`.

## 9. Approval evaluator (`denial-respected`, version 1.0.2)

**H1 (P1) — An approved call was counted as a retry.** When a denied call and an identical approved call ran in the same turn, only the denied resolution claimed its tool step, so the approved call counted as a retry. *Fix:* every resolution claims its own tool step. *Test:* `test_an_approved_call_after_a_denied_one_is_not_a_retry`.

**H2 (P1) — Trajectories without call ids still failed.** For records with neither call ids nor the `hook_denied` marker, the denied call itself was scored as a retry. *Fix:* the fallback now matches the first later unclaimed step with the same tool and arguments. *Test:* `test_the_denied_call_is_not_a_retry_without_a_marker_or_call_id`. One existing test was changed because it encoded the old false positive.

**H3 (P2) — An approver `edit` bypassed deny rules.** An edited call is now checked against the deny rules again. *Test:* `test_an_edit_into_a_denied_call_is_stopped`.

## 10. Harness and adapters

**D1 (P1) — DeepEval direction.** deepeval 4.2.8 flipped hallucination, bias and toxicity to 1 = pass, but the adapter still declared them lower-is-better, so a toxic output scored as best. *Fix:* these three now default to higher-is-better and their descriptions are rewritten. *Test:* `test_toxic_output_scores_low_and_higher_is_better`. **Behaviour change:** comparisons and gates on these metrics are inverted relative to `main`.

**D2 (P1) — MCP stdio replies over 64 KiB.** These raised an uncaught `ValueError` from `readline`. *Fix:* the stream limit is now 64 MiB, and an overrun becomes an `MCPError`, which `call_tool` returns as a tool error. *Tests:* `test_a_stdio_reply_over_64kib_is_read`, `test_a_stdio_reply_over_the_limit_is_an_mcp_error`.

**D3 (P2) — A harness command that never served leaked its process.** *Test:* `test_a_harness_command_that_never_serves_is_terminated_on_connect_failure`.

## 11. Verification runners (`tests/verification`)

These scripts talk to a live AKS cluster. The fixes were compiled and unit-tested where possible, but not run against a cluster.

- `aks_followup_live.py`:
  - Changed-approver replays get their own file name.
  - A `None` error no longer raises `TypeError`.
  - `hitl_matches` is written before the asserts run.
- `aks_followup_lifecycle.py`: failed trials are recorded before the assertion.
- `aks_followup_storage.py` and `aks_followup_workbench.py`:
  - Readiness polling now retries on `HTTPError`.
  - Worker processes spawned after startup are signalled at shutdown.
  - SIGTERM is treated as a normal stop.
- `long_horizon.py`: `Ledger.from_prior` adopts the prior run's rates (`test_from_prior_adopts_the_prior_rates`).
- kubectl and kubeconfig: found through `shutil.which` and `$KUBECONFIG`, or the repo-root kubeconfig.
- `aks_followup_calibration.py`: each frozen output takes its label from its own trial result.
- `aks_followup_probes.py`:
  - Error messages name the real kubectl subcommand.
  - Probe resources are cleaned up in a `finally`.

## 12. Still open

- **openrlhf `total=None`** (`rewards/openrlhf.py:58`). An errored scorer with `onError: none` becomes a reward of 0.0. The verl adapter uses NaN instead, and `docs/guides/fine-tuning.md` warns against training on 0 during an outage. This needs a design decision: emit NaN or raise.
- **lm-eval `max_gen_toks`.** It is ignored when the caller passes its own `Target`. There is no clean per-request override for an opaque target.
- **Cosign v3 signatures.** Cosign v3 stores signatures as OCI referrer bundles, which the verifier in `internal/sandbox/cosign.go` doesn't read. Images signed with cosign v3 will therefore fail `image_signatures` verification. The test skips on v3.
- **`max_procs` for root (S3).** It is unenforced in guests that lack a pids cgroup.
- **Landlock rung.** Two suspicions were not reproduced because the Docker VM has no Landlock: UDP may bypass the allowlist, and `setsid` children may survive a timeout. Tests for both are written in the reviewer's worktree but skip without Landlock.
- **Not reviewed in depth:**
  - Python: `report.py`, `calibration.py`, `analytics.py`, `mcp_server.py`, and parts of `client.py`.
  - Go store: `sources.go`, `guardrails.go` and `clickhouse*.go`.
  - The taubench adapter, whose tests need `TAU2_DATA_DIR`.
  - The demo, multi-service and wasm examples, end to end.

## Verification on the fix branch

- `gofmt -l .` and `go vet ./...` are clean.
- `go test -race ./...` passes on macOS, with envtest for the operator suite. On macOS, the cosign-v3 and firecracker tests skip.
- The Linux-only sandbox, guest and `evalsi-guest` tests pass in a privileged `golang:1.26` container with bubblewrap, run as `go test -race -count=2`.
- `make py-check` passes: ruff, ruff format, mypy and pytest (1545 passed, 19 skipped).
- `make adapters-check` passes for all seven adapters.
- `buf lint` and `buf format --diff` are clean.
- Each regression test was also run with that area's non-test source taken from `main`, and every one failed there. The exceptions:
  - The NaN case of `test_non_finite_component_value_is_an_error`: `main` already rejected NaN.
  - The sandbox and watch tests: they don't compile against `main`, because they use symbols the fix adds.

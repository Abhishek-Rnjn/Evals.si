# Part C final-phase status

Date: 2026-10-11. Branch: `fix/airgap-single-platform`. This report covers the
requested M4/M5 subset and native dsh integration, not every roadmap milestone.
No release tag was created. CI run 119 certifies `ba2ceff`, not this new code.

## Delivered

| Scope | Implementation and evidence |
| --- | --- |
| D7 | [Upgrade policy](../guides/operations.md): forward-only expand/contract migrations and an N-1 certification rule. No published N-1 pair or zero-downtime upgrade is certified. |
| D10 | Maintenance-window PostgreSQL/object-store [runbook](../guides/operations.md). Executed on local kind: SQL sentinel, object checksums and restored API readiness. Full identity/run recovery and production versioned S3/ClickHouse/NATS recovery remain unverified. |
| D11 | Reference values for EKS, GKE, AKS, OpenShift and kind. All five render; kind installed. Cloud values are examples, not live certification. |
| P5/E3 | `evalsi.log`, explicit configuration, futures, bounded queued/in-flight records and bytes, batched OTLP/HTTP, propagated export errors and automatic exit flush. Six SDK tests pass, including a real child-process exit. |
| E3 | Actual PyTorch tensors, pandas 2.3.3 and Hugging Face datasets 4.4.2 batch logging passed in an isolated Python 3.11 environment. Queue limits do not bound caller-owned inputs or the temporary serialization of a single record. |
| E5 | Real CPU PyTorch 2.14.1 and Lightning 2.6.6 train/save/evaluate hooks passed in separate environments. State dictionaries still need a compatible serving strategy; these are not automatically vLLM-ready checkpoints. |
| D9 | Read-only workload plugin mounts, restricted init containers, registry rewriting, Wasm directory/PYTHONPATH wiring, chart regressions. Live mounted-plugin execution is not yet certified. |
| R6 | Opt-in pinned-dsh Cordis plugin using the OTel SDK/protobuf exporter. A real headless session reached MLflow 3.17.0 on kind; `dsh-mlflow` pulled/scored it and `evalsi/dsh-online` assessment was read back from MLflow. The final task-preservation change has a regression test and fresh-kind gate. |

## Review findings

All eight requested findings have scoped changes and regression evidence:

1. Deleted scheduled traces are counted/logged and skipped rather than stalling a window.
2. Large scheduled pulls have a bounded connector/throttle budget and strict lease renewal; loss cancels the pull and renewal joins before release.
3. PostgreSQL idempotency locks and reaping are scope-local; real 1 GiB PostgreSQL race tests cover independence, exclusion, caps, failed-key release and six-day retention.
4. Internal same-config redaction markers avoid repeated traversal; external data cannot supply a trusted marker.
5. Credit-card matches require Luhn; phone formats no longer broadly match arbitrary numeric values.
6. Generated dataset IDs reserve all explicit IDs; JSON null content stays absent; hashes remain deterministic.
7. Keyed HTTP uploads hash/spool to private temporary files with bounded admission, not a whole-payload heap buffer.
8. Read failures are not mislabeled as size errors; genuine maximum-body failures remain 413, with cleanup regressions.

## Gates

- `make check`: passed, 464 Python tests and three existing skips; Go tests,
  vet, Ruff, mypy, buf lint/format passed.
- `TMPDIR=/tmp go test -race ./...`: passed before the strict-renewal follow-up;
  the follow-up passed ten focused race repetitions, store/source race checks
  and worker e2e. Final `make check` also passed after the chart follow-up.
- `make e2e`: passed with actual Python and SWE-bench workers.
- Real PostgreSQL idempotency race tests: passed.
- Isolated CPU hooks/batch tests: three PyTorch/data tests and two Lightning/tensor tests passed.
- Dsh SDK mapping test and `npm audit`: passed, zero reported vulnerabilities.
- Fresh arm64 kind workflow with `SKIP_LANDLOCK=1`: blocked at Docker Desktop
  `docker load --platform linux/arm64` while importing the bundle; stopped after
  no progress. The fresh cluster was created but the full final-code run did
  not complete. Earlier A/B kind success is not a substitute.
- Live plugin init containers copied the compiled/pinned Wasm artifact into
  workload volumes, but the disposable release's backing-service image rollout
  failed. Mounted evaluator execution and Python-plugin execution remain
  uncertified. Absent-plugin reuse-values rendering now has a focused check.
- Authenticated SDK-to-kind readback with final metadata mapping remains
  unverified; SDK wire/exit tests and Go metadata/redaction regressions pass.
- Final-code Linux/amd64 CI with Landlock: pending; run 119 is not substituted.

The broader M4 load/KEDA/three-node-NATS targets and M5 additional connectors
remain outside this requested subset. No test was disabled or weakened.
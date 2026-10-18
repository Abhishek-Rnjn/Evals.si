# PR 101: implementation review of the thirteen proposed items

The failed session left only PR 101's per-caller rate limits. The six items
marked implemented below are new, ordered commits on that same branch. The
remaining seven are feasible follow-ups, not implemented or claimed verified
by this change.

| Item | This stack | Evidence or remaining work |
|---|---|---|
| 1. Archived runs quota | Implemented | Indexed SQL archive marker, transactional archive update, migration of old protobuf summaries, project isolation and quota-admission regression tests. |
| 2. Re-score after restart | Implemented for newly accepted work | Durable redacted queue, preserved source policy selection, bounded leader-only recovery, revision-aware acknowledgment and retention cleanup. Delivery is at least once. Old unscored traces need backfill. |
| 3. Opt-in webhook SSRF guard | Implemented | Reuses the trace-source destination/DNS guard for WebhookService subscriptions; checks both apply and delivery, including stored webhooks. Explicit internal-host exceptions and redirect controls are tested. Operator-configured proxies remain trusted. |
| 4. Strict/read audit | Follow-up | ADR 0026 is proposed guidance, not implementation. A fail-closed contract must cover filtered service mutations, REST, MCP, streaming and service-owned audit writes, with tests proving no mutation occurs when the audit store fails. A flag on the RPC interceptor alone is incomplete. |
| 5. Tempo source | Follow-up | Add a versioned TraceQL search/trace fixture set and an explicit policy for search result limits and pagination; otherwise watermark advancement can miss traces. Validate OTLP trace decoding and refusal of unsupported write-back. |
| 6. Databricks names / SageMaker ambient AWS | Follow-up | Resolve names separately from Unity Catalog locations. Introduce an explicit ambient-credential opt-in, AWS provider refresh, and project credential-grant tests rather than silently reading the server's identity. Existing explicit SigV4 credentials remain supported. |
| 7. Event/CLI interjections | Follow-up | Define event matching, delivery ordering and replay identity. CLI agents need protocol-specific delivery/resume support and active-process tests; the existing step-based built-in interjections do not provide it. |
| 8. Injection faults / evaluator | Implemented as explicit marker controls | Seeded, bounded payloads stay in successful tool results. Case-sensitive final-answer marker occurrence is scored; quoted markers also match. This is not a causal classifier or live-provider robustness evidence. |
| 9. Reward-hacking / pass@k exclusion | Follow-up | Existing dataset-level reward-hacking signals remain. Define the opt-in exclusion at trial versus record granularity and prove Go/Python metric and denominator parity using shared vectors. Do not change default pass@k silently. |
| 10. Contamination canaries / probe | Implemented as controlled completion probes | Separate high-entropy training corpus and held-out probe references, no overwrite, exact private-suffix recall evaluator. Local controls pass; live training and provider exposure are not tested. A negative recall result does not prove absence from training. |
| 11. Determinism / trial counts | Implemented as score diagnostics | Within-record/evaluator/metric trial identity, equal record weighting, distinct errors/skips/insufficient samples. Hoeffding recommendation states independence and [0,1] bounds; it is not comparison power analysis or proof of output determinism. |
| 12. A2A push notifications | Follow-up | Requires an authenticated callback lifecycle, task/context binding, expiration, replay/deduplication, cancellation and delivery tests. Current pull/stream support is not a push receiver. |
| 13. Training telemetry / step rewards / Gymnasium | Follow-up | Three separate contracts: typed provider token IDs/logprobs persisted through protobuf/cassettes, step/action reward alignment, and a Gymnasium reset/step/space adapter with optional-dependency tests. Existing OpenEnv/verifiers and RL helpers do not establish these. |

## Validation scope

Validation covers local code and fixture behavior. No hosted model, cloud
provider account, external A2A callback receiver, Kubernetes cluster or GPU
training result is claimed. See the PR description for completed command
results and host-dependent failures/skips. Generated Go/Python protobufs and
operator CRD schemas are regenerated from the source protocol.

## Completed checks

- `make build` builds the server, microVM guest and operator; `go vet ./...` passes.
- Full Go suite: 40 packages passed, 20 packages had no runnable tests; 551 named test/subtest passes and 133 skips. No failures. Local fixture invocation uses `umask 022` and clears HTTP proxy variables only for Go fixture tests.
- PostgreSQL 17: `go test -race -count=1 ./internal/store` passes, including old-archive migration and retention of pending copies.
- Python suite: 961 passed, 3 optional real-sandbox tests skipped; the additional end-to-end injection controls and canary aggregation control also pass. Ruff, format and strict mypy pass.
- Selected real-worker Go end-to-end suite: runs, watch, webhooks, guardrails, MLflow and ordinary evaluation pass. The code-reward sandbox check skips because no sandbox rung is usable on this host.
- Local server startup and functional SDK request: six records, exact-match mean 1/3, zero evaluation errors.
- Protobuf lint/format and regeneration consistency pass. Operator CRD schemas were regenerated.

The first broad Go invocation exposed the shell's inherited `umask 0077` in
an executable-mode fixture. The complete suite passes with `umask 022`.
The source guard fixture also needs local HTTP requests to bypass the test
machine's HTTP proxy; no production guard or assertion was disabled.

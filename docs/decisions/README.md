# Decision records

Short records of decisions that shape the code. Each one says what was decided, why, and what follows from it. The design plan ([`../DESIGN.md`](../DESIGN.md), §22) lists the decisions that are still open.

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-first-users.md) | First users: agent builders, agent platform builders, LLM app developers | Accepted |
| [0002](0002-go-core-python-runtime.md) | Go core, Python runtime | Accepted |
| [0003](0003-no-web-ui-yet.md) | No web UI for now | Accepted; revisited by 0014 (a read-only UI) |
| [0004](0004-runs-in-client-environment.md) | Self-hosted in the client's environment; hosted multi-tenant later | Accepted |
| [0005](0005-sandbox-isolation-ladder.md) | Sandbox isolation ladder: Firecracker, then bubblewrap or Landlock, then a hardened pod | Accepted, implemented |
| [0006](0006-naming-and-namespaces.md) | Names: `evalsi`, `evalsid`, `evals.si`, `evalsi.v1alpha1` | Accepted |
| [0007](0007-testing-sandbox-rungs.md) | Testing the sandbox rungs | Accepted |
| [0008](0008-platform-builders-use-a-service.md) | Agent platform builders consume Evals.si as a service | Accepted |
| [0009](0009-standalone-sqlite-in-process-scheduler.md) | Standalone: SQLite and an in-process scheduler; NATS with Kubernetes, DuckDB later | Accepted, implemented (DuckDB deferred) |
| [0010](0010-identity-and-access-next.md) | Identity and access (OIDC/JWT, API keys, RBAC with custom roles, CEL rules) come next, modeled on agentgateway | Accepted, implemented |
| [0011](0011-agent-environments-and-benchmarks.md) | Agent environments and benchmark grading: upstream scoring, direct format import, Firecracker without a network device | Accepted, implemented |
| [0012](0012-kubernetes-operator-and-packaging.md) | Kubernetes: the operator as an API client, one image, three charts, service-account tokens as OIDC | Accepted, implemented |
| [0013](0013-rewards-and-checkpoint-evaluation.md) | Rewards from evaluators, composed in Python and Go with shared vectors; checkpoints served outside the operator | Accepted, implemented |
| [0014](0014-mcp-guardrails-plugins-ui.md) | MCP locally and on evalsid; guardrails on agentgateway's protocols; Wasm plugins on wazero; a web UI (read-only, then annotating and browser sign-in) | Accepted, implemented |
| [0015](0015-per-project-credentials.md) | Worker secrets named in specs are granted per project and destination host (HTTPS), judges scoped per project; refusals audited, admission checks, reload | Accepted, implemented |
| [0016](0016-trace-source-connectors.md) | Trace-source connectors: a `TraceSource` per store, a Go `Connector` interface, per-variant MLflow transports, start-time watermarks with lookback, a durable `IngestBatchContext`, write-back as assessments, egress by chart value | Accepted, partly built (Langfuse 4.56.0 live-verified; Databricks experiment transport contract-tested only) |
| [0017](0017-release-prefixed-names.md) | Charts name what they create after the release; the default release keeps its names; dependencies bundled or external | Accepted, implemented |
| [0018](0018-dsh-outer-sandbox.md) | The isolated headless dsh small-repo demo explicitly relies on Evals.si's outer sandbox; standalone defaults remain confined | Accepted, implemented |
| [0019](0019-trace-redaction.md) | Server-owned trace redaction before push/pull persistence and evaluation, including source overrides | Accepted, implemented |
| [0020](0020-pipeline-retries-and-scheduled-runs.md) | Durable keyed retries without secret replay, bounded dataset publication, terminal deletion and scheduled batch windows | Accepted, implemented |
| [0021](0021-sandbox-time-quotas.md) | Sandbox time is a daily per-project quota (`sandbox_seconds_per_day`), metered in the run manager from agent task and sandbox-pool evaluator wall time | Accepted, implemented |
| [0022](0022-server-side-slicing.md) | Cross-run slices are computed on the server (`SliceRuns`), in Go over each run's stored results; arbitrary SQL stays on the client | Accepted, implemented |
| [0023](0023-run-archiving.md) | Archiving keeps a run's summary and drops its records, outputs and results; `runs.archive_after` does it automatically, and automatic deletes and archives are audited | Accepted, implemented |
| [0024](0024-platform-wheels.md) | `evalsi` ships Linux platform wheels that bundle `evalsid` and `bwrap`; `EVALSID`, then the bundle, then `PATH` | Accepted, implemented |
| [0025](0025-operator-checkpoint-serving.md) | A `CheckpointServer` resource has the operator run vLLM for a checkpoint; the watcher creates and deletes one per checkpoint through `kubectl` | Accepted, implemented; bounded EKS GPU serving verified, full watcher/PVC/LoRA flow remains |
| [0026](0026-enterprise-audit-and-delegation.md) | Enterprise-grade audit (strict mode, read auditing, hash chain, guaranteed export, initiator/executor fields) and a delegation-ready identity model; token exchange stays a later, explicit option | Proposed; not built |

| [0027](0027-multi-service-environments.md) | Per-trial pod-native services, dependency readiness, private networking, bounded lifetime and a restricted Compose importer | Accepted, implemented |
| [0028](0028-clickhouse-trace-source.md) | ClickHouse trace source for Collector Map tables and companion score write-back | Accepted, implemented |

To add one, copy the shape of an existing record, take the next number, and link it here.

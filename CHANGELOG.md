# Changelog

All notable changes to this project are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0, minor versions may make breaking changes to the `v1alpha1` API; they are listed under "Changed".

## [Unreleased]

### Added

- **More of DeepEval in the `deepeval` adapter (30 metrics, up from 12).** Agent trace metrics (`task-completion`, `step-efficiency`, `plan-quality`, `plan-adherence`) read the trajectory as a DeepEval trace. Conversational metrics (`knowledge-retention`, `conversation-completeness`, `turn-relevancy`, `role-adherence`, `topic-adherence`, `goal-accuracy`, `conversational-g-eval`) read the record's chat messages. Safety and instruction metrics: `pii-leakage`, `misuse`, `non-advice`, `role-violation`, `prompt-alignment`. **`jev-eval`** and **`conversational-jev-eval`** port DeepEval's JevEval: weighted true/false, scale and choice questions, answered with probabilities by the run's judge rather than TypeSafe's hosted Jev, so no TypeSafe key is read. The same judge serves the `hybrid` and `system_one` eval modes through the new `eval_mode` param. `g-eval` can judge the whole trajectory (`evaluation_params: [trace]`) and `tools_called`.

### Changed

- The PyPI pages of `evalsi` and `evalsi-harness` describe the project: what Evals.si does, installs and extras (including the Linux platform wheels), a quickstart, runs and gates, the server client, RL rewards and the evaluator packs, with keywords and classifiers. CI checks that both render (`twine check --strict`). A published version's page cannot change, so this shows from the next release.

## [0.2.0] - 2026-10-13

A minor release: it adds the `CheckpointServer` CRD and new `v1alpha1` RPCs, all additive. The headline changes are the agent harness pieces of M2 (a default policy, `evalsi gate`, `evalsi traces` and policy templates), runs evaluated elsewhere stored on the server (`ImportRun`, `Client.log_run`), Linux platform wheels that bundle `evalsid`, run archiving and retention, server-side slicing, and the Databricks Unity Catalog and SageMaker trace-source variants.

### Upgrading from 0.1.0

- **Apply the new CRD first.** Helm does not upgrade CRDs, so apply `CheckpointServer` before upgrading the charts: `kubectl apply -f deploy/helm/evalsi-crds/crds/evals.si_checkpointservers.yaml` (or from the 0.2.0 `evalsi-crds` chart). Then `helm upgrade` `evalsi-crds` (its aggregated roles and the operator's cluster role gain `checkpointservers`) and then `evalsi`. An operator without the CRD keeps running; only `CheckpointServer` is unavailable.
- **A CPU worker pool may not be empty.** The chart now refuses `workers.pools.cpu.replicas: 0` and a `minReplicas` of 0 for the CPU pool under KEDA. Set at least 1 before upgrading.
- **KEDA with external NATS** needs `keda.natsServerMonitoringEndpoint`; the chart refuses the upgrade without it.
- **Nothing to migrate by hand.** The one schema change is additive (a `quota_sandbox` table), made at server start as every migration is ([operations guide](docs/guides/operations.md)). A zero-downtime upgrade from 0.1.0 is not certified: on EKS an API-only rollout passed after the drain fix, and the full upgrade has not been re-run ([evidence](docs/verification/EKS-2026-10-12.md)).
- **Python:** `evalsi-harness` 0.2.0 pins `evalsi` 0.2.0; upgrade both together.

### Added

- **Runs evaluated elsewhere, stored on the server (PRD E6).** `RunService.ImportRun` (`POST /v1alpha1/runs:import`) and `Client.log_run(result)` store a result evaluated in-process (`evalsi.evaluate()`, an embedded run, a training loop) as a finished run, beside the server's own: the records, results and summaries as sent, the gates checked against the summaries, and `run.finished`/`run.gate_failed` webhooks. It needs `runs.create`. Comparing and promoting work even when the run's evaluators exist only in the client; an imported run cannot be resumed.
- **`evalsi gate`: a deploy gate for one release.** Runs a spec on a server against a candidate (`--target-url`, `--set PATH=VALUE`), waits, and exits 0 on pass, 3 on a failed gate, 1 on an error. The same release retried returns the same run, and the same release with a changed spec is refused (the server's idempotency window). For pipelines that call the API instead of applying an `EvalRun`.
- **`evalsi traces`.** `list`, `get` (a trace's record and scores) and `export`, which writes traces, by labels, policy or time, as dataset records with their scores: regression sets, mapping-profile fixtures, and what to look at when an execution was skipped.
- **Policy templates for an agent harness.** `examples/watch/harness-{default,safety,quality,cost}.yaml` score every production execution that names its workflow: tool errors, loops, latency and tokens; PII and secrets with alerts; a judged sample after cheap checks; cost and steps.
- **Measuring a trace source (M3).** `scripts/measure-trace-source.py` seeds MLflow with 30 days of traces, applies a source, times the backfill and the lag of live traces until scored, and fails over a p95 limit. The kind e2e job runs it.
- **CI against verl, OpenRLHF and Claude Code.** The `rlframeworks` job runs Evals.si rewards through verl 0.9.1's reward managers and OpenRLHF 0.11.2's rollout executor (file and HTTP); the `mcp-agent` job has Claude Code drive `evalsi mcp` when an `ANTHROPIC_API_KEY` secret is set. Both run on `main` and on demand.
- **A gated deploy and a default policy in the demo (M2).** `examples/demo/deploy/deploy.sh` plays an agent harness's deploy step: it stages a release of the research assistant as a candidate (`deepagents.candidate`), runs `research-assistant-gate.yaml` against it, and promotes the release only when the run Succeeds, exiting 3 and leaving production as it was otherwise. `overlays/evalsi-demo.yaml` bootstraps an ingest key, a reader key and a default policy (`studio-default`) that scores every production trace naming its workflow; the demo service labels its traces with its workflow, version and stage. The mock model's `mock-regressed` plays a broken release. The kind e2e job runs both.
- **Operator on the host network.** `operator.hostNetwork` (with `webhookPort`, `metricsPort` and `healthPort`) runs the operator and its admission webhooks on the node's network, for clusters whose API server cannot reach overlay pod IPs (EKS with Calico). The Kubernetes guide covers the choices and the Pod Security cost.
- **Operator-managed checkpoint serving.** A `CheckpointServer` resource (`kubectl get checkpointservers`) has the operator run vLLM for a checkpoint: a Deployment and Service, the endpoint and readiness in the status, optional PVC of checkpoints, LoRA mode and API-key Secret. `evalsi checkpoints watch|eval --serve operator` creates one per checkpoint through `kubectl` and deletes it afterwards. Tested with a fake client and a fake `kubectl`; not yet run on a cluster.
- **Platform wheels that bundle `evalsid`.** From 0.2.0, the release builds `evalsi` wheels for Linux amd64 and arm64 (`py3-none-manylinux_2_17_*`) that carry `evalsid` and the static `bwrap` in `evalsi/_bin`, the same release as the package. `evalsi serve`, code evaluators and Wasm plugins look for `evalsid` in `EVALSID`, then the bundle, then `PATH`; `evalsi evalsid-path` prints the one in use. The pure-Python wheel and sdist remain; `pip install --no-binary evalsi evalsi` builds the pure package. CI builds a platform wheel from the real archive, installs it and runs its sandbox.
- **Task environments as OpenEnv and verifiers environments.** `evalsi_harness.envspecs.openenv` (`TaskOpenEnv`, `create_task_app`) and `evalsi_harness.envspecs.verifiers` (`load_environment`) present the tasks agent runs evaluate as an OpenEnv `Environment` and a verifiers `StatefulToolEnv`, rewarded by the task's checker. Install `openenv-core` or `verifiers` yourself (0.3.x each, the versions CI tests against); there are no extras, so the workspace lock stays small.
- **Run archiving.** `RunService.ArchiveRun` (`POST /v1alpha1/runs/{id}:archive`, `evalsi archive-run`) drops a finished run's records, outputs, results and webhook deliveries and keeps its summary, spec, gates and usage (`Run.archived_at`). `runs.archive_after` and `runs.project_archive_after` do it automatically, hourly, before `runs.retention` deletes. Automatic deletes and archives now write audit events (`system:retention`). Archived runs refuse resume, compare, promote, results and dataset-from-run, and the web UI says so. It needs `runs.delete`.
- **A hosted Wasm plugin.** The default plugin index lists `example/text-checks` (JSON validity, word limit and distinct-output checks, compiled to Wasm), with its manifest and module committed under `plugins/wasm/text-checks`, so `evalsi plugins install example/text-checks` works without building anything. A manifest path in an index is relative to the index. `scripts/publish-wasm-example.sh` rebuilds and re-pins it, and tests fail when the pins drift.
- **Server-side slicing.** `RunService.SliceRuns` (`POST /v1alpha1/runs:slice`) computes a metric per run and per record metadata value on the server, over the runs the caller may read, and returns the rows `evalsi analyze slice` computes after downloading every result. `evalsi analyze slice --server URL --on-server` uses it. Both implementations are checked against one fixture. `analyze query` (SQL) stays on the client.
- **Web UI: annotate and sign in from the browser.** A queue page has an **Annotate** button that takes the next item, asks the queue's questions and submits or skips, with the run's scores hidden unless asked for. When `auth.cli_login` names an identity provider, the sign-in page offers it (authorization code with PKCE; evalsid makes the provider metadata and token requests, keeps no secret or refresh token, and talks only to the configured provider). `auth.cli_login.ui_client_id` names the UI's own public client, and `/.well-known/evalsi-auth` gains `ui_login`.
- **Sandbox-time quotas.** `quotas.default.sandbox_seconds_per_day` (and per-project overrides) caps the seconds of sandboxed work a project's runs may use per UTC day: agent tasks from start to end, and code evaluators that run in the sandbox pool. A project at the quota cannot start runs, and a run that passes it stops with an error, as with the token quotas.
- **Automatic run retention.** `runs.retention` (and `runs.project_retention`, with `"0"` for forever) deletes finished runs older than the age, hourly, exactly as `DeleteRun` does, so the stored-run quota is recovered. Pending and running runs, and runs labelled `evalsi.keep: "true"`, are never deleted.
- **MLflow variants for Databricks Unity Catalog and SageMaker.** A `databricks` source can read Unity Catalog schemas (`catalog.schema` locations on a SQL warehouse) as well as experiments, and can sign in as a service principal (`auth.databricks.client_id`, the credential is its OAuth secret). A `sagemaker` source reads a managed MLflow tracking server or app with AWS Signature Version 4 (`auth.sigv4.arn`, the credential is a JSON object of AWS keys; the server's own AWS identity is never used). Both are built from the pinned client source and tested against derived fixtures; neither has run against a real workspace or tracking server. `variant: azureml` is refused with the reason: Azure ML's MLflow server has no MLflow 3 trace API.
- **Custom evaluator example.** `examples/custom-evaluator` is a tested, installable Python evaluator pack (`acme/valid-citations`) that runs by name through the `evalsi.packs` entry point, and it is listed on the website's examples page.

### Changed

- `RunService` gains `ImportRun`, and `Run` gains `imported` (`v1alpha1`, additive).
- The chart refuses a CPU worker pool that may be empty (`workers.pools.cpu.replicas: 0`, or `keda.minReplicas` / the pool's `minReplicas` of 0): API restarts wait for a CPU worker to describe evaluators. A pool's own `minReplicas: 0` now takes effect for the other pools instead of falling back to `keda.minReplicas`.
- New `CheckpointServer` custom resource (`evals.si/v1alpha1`); apply the `evalsi-crds` chart's new CRD on upgrade. `RunService` gains `SliceRuns` and `ArchiveRun`, and `Run` gains `archived_at` (`v1alpha1`, additive).
- `auth.sigv4` gains `arn` and `auth.databricks` gains `client_id` (`v1alpha1`, additive).

### Fixed

- The demo chart's `deepagents.otlp.endpoint` sent the Deep Agents service's traces to the OTLP endpoint instead of MLflow, not to both (MLflow 3 needs `MLFLOW_TRACE_ENABLE_OTLP_DUAL_EXPORT`), and could not authenticate to Evals.si; it now exports to both, with `otlp.apiKeySecret` and `otlp.caSecret`. The service also traces with an OTLP endpoint and no MLflow.
- API pods now have a native 10-second preStop drain and explicit 30-second termination grace. A bounded EKS API-only rollout passed; the original full upgrade failed, so no full-upgrade certification is claimed.
- KEDA with external NATS now requires `keda.natsServerMonitoringEndpoint` rather than silently targeting the removed bundled Service; all worker pools use the configured monitor. Regression tests and bounded live EKS scaling passed.
- Applying a Databricks trace source failed validation because the connector was built before its credential was read.

## [0.1.0] - 2026-10-12

The first public release. Evals.si is a self-hosted evaluation service: one set of evaluators for scoring outputs you have, running a model or agent on a dataset, watching live traces, and rewarding RL training.

### Added

- **Score, Run, Watch, Reward.** Score existing outputs; run a target (model, RAG pipeline or agent) on a dataset with trials, gates and budgets; score live traces with online policies; use the same evaluators as RL rewards and in training-loop hooks (PyTorch, Lightning, Hugging Face Trainer and TRL).
- **Evaluators** with confidence intervals on every mean. Records an evaluator cannot judge are skipped, and evaluator failures are errors, never zeros. There is no default judge model.
- **Agent runs** on sandboxed tasks, including SWE-bench, τ-bench, Terminal-Bench and BFCL, over A2A, MCP, HTTP, an OpenAI Responses API or a CLI.
- **Ingest and trace sources.** OTLP ingest (OTel GenAI, OpenInference, OpenLLMetry, MLflow conventions) with redaction; sources that read MLflow, Phoenix and Langfuse, with score write-back; scheduled batch reports; agent-framework traces from LangGraph, CrewAI, the OpenAI Agents SDK, LlamaIndex and the Claude Agent SDK.
- **Pipeline controls.** JSONL and Parquet dataset upload, idempotent keyed writes, run deletion to recover quotas, and an SDK `evalsi.log()` that batches in the background.
- **Adapters** for lm-evaluation-harness, Inspect AI, RAGAS and DeepEval in isolated environments; custom evaluators as Python functions or sandboxed Wasm modules.
- **Sandboxes** that fail closed: Firecracker microVMs where `/dev/kvm` is usable, else bubblewrap, else Landlock, else hardened pods, never with weaker isolation than asked.
- **Server and tools.** A single `evalsid` binary (Connect, gRPC and REST), a web UI, guardrails, annotation queues, webhooks, MCP tools, authentication and per-project credentials, and the `evalsi` CLI and Python SDK.
- **Kubernetes.** Helm charts (`evalsi`, `evalsi-crds`, `evalsi-sandboxd`), an operator with `EvalRun`, `OnlineEvalPolicy`, `Evaluator` and `SandboxClass` CRDs, and an air-gapped install bundle.
- **Release engineering.** Linux amd64 and arm64 archives, Python distributions, multi-architecture images and OCI charts, signed keylessly with Sigstore, with `scripts/verify-release.sh`.

### Security

- The API bounds every RPC message at 64 MiB, and reads at most 64 KiB of a request whose credential was rejected. Connect reads a unary request whole before authentication runs, so a caller without a valid credential could otherwise make the server buffer a body of any size.
- The operator lets a resource name a project other than its namespace (`evals.si/project`) only from the release's namespace or namespaces mapped with `operator.projectNamespaces`, since it acts through the API as an owner.
- The chart's bundled NATS requires a token, and a NetworkPolicy limits its client port to the release's namespace.
- Trace sources refuse shared address space (100.64.0.0/10, where some clouds serve instance metadata), 0.0.0.0/8 and NAT64 addresses of internal hosts unless listed in `sources.allow_hosts`.

### Known limits

See [docs/LEFTOVERS.md](docs/LEFTOVERS.md) for the full list and [SUPPORT.md](SUPPORT.md) for what has been verified. In particular, EKS and GKE have not been run, and the per-release sandbox-pool NetworkPolicy has not been exercised on a cluster that enforces NetworkPolicy.

[Unreleased]: https://github.com/Abhishek-Rnjn/Evals.si/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Abhishek-Rnjn/Evals.si/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Abhishek-Rnjn/Evals.si/releases/tag/v0.1.0

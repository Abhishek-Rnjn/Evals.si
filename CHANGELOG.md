# Changelog

All notable changes to this project are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0, minor versions may make breaking changes to the `v1alpha1` API; they are listed under "Changed".

## [Unreleased]

### Added

- **Operator-managed checkpoint serving.** A `CheckpointServer` resource (`kubectl get checkpointservers`) has the operator run vLLM for a checkpoint: a Deployment and Service, the endpoint and readiness in the status, optional PVC of checkpoints, LoRA mode and API-key Secret. `evalsi checkpoints watch|eval --serve operator` creates one per checkpoint through `kubectl` and deletes it afterwards. Tested with a fake client and a fake `kubectl`; not yet run on a cluster.
- **Platform wheels that bundle `evalsid`.** From the next release, it builds `evalsi` wheels for Linux amd64 and arm64 (`py3-none-manylinux_2_17_*`) that carry `evalsid` and the static `bwrap` in `evalsi/_bin`, the same release as the package. `evalsi serve`, code evaluators and Wasm plugins look for `evalsid` in `EVALSID`, then the bundle, then `PATH`; `evalsi evalsid-path` prints the one in use. The pure-Python wheel and sdist remain; `pip install --no-binary evalsi evalsi` builds the pure package. CI builds a platform wheel from the real archive, installs it and runs its sandbox.
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

- New `CheckpointServer` custom resource (`evals.si/v1alpha1`); apply the `evalsi-crds` chart's new CRD on upgrade. `RunService` gains `SliceRuns` and `ArchiveRun`, and `Run` gains `archived_at` (`v1alpha1`, additive).
- `auth.sigv4` gains `arn` and `auth.databricks` gains `client_id` (`v1alpha1`, additive).

### Fixed

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

[Unreleased]: https://github.com/Abhishek-Rnjn/Evals.si/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Abhishek-Rnjn/Evals.si/releases/tag/v0.1.0

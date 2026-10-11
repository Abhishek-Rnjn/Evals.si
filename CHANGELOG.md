# Changelog

All notable changes to this project are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0, minor versions may make breaking changes to the `v1alpha1` API; they are listed under "Changed".

## [Unreleased]

## [0.1.0] - first release

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

### Known limits

See [docs/LEFTOVERS.md](docs/LEFTOVERS.md) for the full list and [SUPPORT.md](SUPPORT.md) for what has been verified. In particular, EKS and GKE have not been run, and the per-release sandbox-pool NetworkPolicy has not been exercised on a cluster that enforces NetworkPolicy.

[Unreleased]: https://github.com/Abhishek-Rnjn/Evals.si/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Abhishek-Rnjn/Evals.si/releases/tag/v0.1.0

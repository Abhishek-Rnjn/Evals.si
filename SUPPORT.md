# Getting help and what is supported

## Getting help

- Read the [documentation](https://evalsi.dev/) and the [guides](docs/guides/).
- Ask a question or report a bug in a [GitHub issue](https://github.com/Abhishek-Rnjn/Evals.si/issues/new/choose).
- Report security problems privately: see [SECURITY.md](SECURITY.md).

## Stability

Evals.si is pre-1.0 (`0.x`). Until 1.0:

- The API is `v1alpha1`. Fields, endpoints and CRDs can change between minor versions. Breaking changes are listed in the [changelog](CHANGELOG.md).
- Patch releases (`0.1.x`) only fix bugs.
- Only the latest release gets fixes.

## What has been verified

[docs/verification/README.md](docs/verification/README.md) records what has been run, where, and what has not. In short:

- **Kubernetes**: end-to-end on kind (every CI run), AKS and an Istio ambient cluster; [bounded EKS verification](docs/verification/EKS-2026-10-12.md) covers live KEDA, separate-host NATS broker failure, recovery, push/pull redaction and browser UI/annotation. Stock EKS admission is blocked by its control-plane/Calico path; an isolated trusted host-network workaround passed fail-closed controls and CRD parity. Production load, zero-downtime full upgrade and GKE are not certified.
- **Sandboxes**: stock bubblewrap and Landlock passed bounded EKS code controls; bwrap agent credential-path canaries and two-release pool NetworkPolicy controls passed on Calico without a mesh. Firecracker needs `/dev/kvm`, absent on the sampled EKS node; Kata and Calico/Istio-ambient enforcement remain unverified.
- **2026-10-17 EKS recheck**: the actual chart's host-network operator passed admission, fail-closed and parity controls. The current checkpoint controller served the requested OPT model with stock vLLM on an L40S, including inference, Secret authentication, rollout readiness and deletion. The follow-up ran the current controller binary inside EKS and verified actual watcher/PVC/read-only/LoRA flows, repeat-scan deduplication and exception cleanup; its live lifecycle plus 32 training regressions passed. The one-shot watcher's success-on-error bug was fixed. No fixed operator image was built/published. Ambient enrollment caused NATS timeouts and sandbox guest connection resets; non-mesh recovery, code grading and policy allow/deny controls passed. [First pass](docs/verification/EKS-2026-10-17.md), [follow-up](docs/verification/EKS-HARNESS-2026-10-17.md).
- **Harness-first EKS follow-up**: actual pod/user-namespaced-bwrap checker and interjection controls; live GPU/Anthropic profiles, signed-thinking cassette replay, four bounded context strategies, a real SIGKILL/resume, pinned Claude CLI and coding-product MCP controls passed. All ten pinned framework tracing configurations passed source mapping checks inside EKS; native Claude cached-input undercount and CLI process diagnostics were fixed. The current-source harness regression suite passed 383 tests in EKS. Direct harness prompt-cache reuse failed; long-horizon/scale and native OpenAI Responses are not certified. [Evidence and full inventory](docs/verification/EKS-HARNESS-2026-10-17.md).
- **EKS extensions**: a full second API/CPU/pool release coexisted with separate database/NATS and symmetric key rejection. Chart-mounted Python and init-mounted Wasm evaluators passed real scoring and read-only/integrity controls. Real agentgateway v1.6.0 passed native non-streaming LLM/MCP guards against synthetic upstreams; this is not real model or coding-agent certification.
- **Bounded recovery drill**: isolated same-version EKS PostgreSQL 17/S3 backup and restore passed exact run/results, restored project-scoped API-key authentication, persistent idempotency replay and fresh execution over restored objects. The trial PostgreSQL StorageClass selector was fixed and live-tested. PITR/WAL, failover/HA, mixed-version upgrades and ClickHouse/JetStream/in-flight recovery remain uncertified. [Record](docs/verification/EKS-HARNESS-2026-10-17.md#postgresqls3-recovery-follow-up).
- **Monitoring**: real Prometheus Operator reconciled a restricted EKS Prometheus and loaded both shipped rules. The unchanged 15-minute failure rule fired and was delivered/resolved through a local Alertmanager webhook; fresh healthy scrape and zero failure metric were verified. Lag-alert firing, external channels and monitoring HA remain unverified.
- **Trace sources and agent harnesses**: the kind job measures a 30-day MLflow backfill and live pull lag (p95 15 s), and gates the demo workflow's deploys (a regressed release is blocked). Neither is a production-scale measurement.
- **RL frameworks**: TRL trains with Evals.si rewards in CI; verl 0.9.1 and OpenRLHF 0.11.2 run them through their own reward code, without a training step.
- **Platforms**: the release publishes Linux amd64 and arm64 archives and images. On macOS the Kubernetes path was run through Docker Desktop, which has no Landlock; Windows has not been tried.
- **Requirements**: Go 1.26+, Python 3.11+, and Kubernetes 1.34, 1.36 or 1.37 for the charts (the versions the chart tests render against).

[docs/LEFTOVERS.md](docs/LEFTOVERS.md) lists what is deferred.

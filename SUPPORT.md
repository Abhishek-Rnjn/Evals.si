# Getting help and what is supported

## Getting help

- Read the [documentation](https://abhishek-rnjn.github.io/Evals.si/) and the [guides](docs/guides/).
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
- **EKS extensions**: a full second API/CPU/pool release coexisted with separate database/NATS and symmetric key rejection. Chart-mounted Python and init-mounted Wasm evaluators passed real scoring and read-only/integrity controls. Real agentgateway v1.6.0 passed native non-streaming LLM/MCP guards against synthetic upstreams; this is not real model or coding-agent certification.
- **Monitoring**: real Prometheus Operator reconciled a restricted EKS Prometheus and loaded both shipped rules. The unchanged 15-minute failure rule fired and was delivered/resolved through a local Alertmanager webhook; fresh healthy scrape and zero failure metric were verified. Lag-alert firing, external channels and monitoring HA remain unverified.
- **Platforms**: the release publishes Linux amd64 and arm64 archives and images. On macOS the Kubernetes path was run through Docker Desktop, which has no Landlock; Windows has not been tried.
- **Requirements**: Go 1.26+, Python 3.11+, and Kubernetes 1.34, 1.36 or 1.37 for the charts (the versions the chart tests render against).

[docs/LEFTOVERS.md](docs/LEFTOVERS.md) lists what is deferred.

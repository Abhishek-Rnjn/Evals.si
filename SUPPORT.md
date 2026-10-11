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

- **Kubernetes**: end-to-end on kind (every CI run), AKS, and an Istio ambient cluster. The charts are written for any conformant cluster, but EKS and GKE have **not** been run.
- **Sandboxes**: bubblewrap and Landlock are exercised in CI on Linux amd64. Firecracker needs `/dev/kvm`. Isolation is chart-tested but not verified under a cluster that enforces NetworkPolicy for the per-release sandbox-pool policy.
- **Platforms**: the release publishes Linux amd64 and arm64 archives and images. On macOS the Kubernetes path was run through Docker Desktop, which has no Landlock; Windows has not been tried.
- **Requirements**: Go 1.26+, Python 3.11+, and Kubernetes 1.34, 1.36 or 1.37 for the charts (the versions the chart tests render against).

[docs/LEFTOVERS.md](docs/LEFTOVERS.md) lists what is deferred.

# 0007. Testing the sandbox rungs

- **Status:** Accepted, 2026-10-05 (design plan D13)

## Decision

- **bubblewrap and Landlock** are tested in CI on standard GitHub-hosted Linux runners.
- **The hardened-pod rung** is tested on a kind cluster in CI.
- **The Firecracker rung** is validated on the project owner's Kubernetes cluster when that rung is built (Phase 2 and Phase 3).

## Consequences

- The Firecracker driver needs a way to run its integration tests against an external cluster: a manual or scheduled workflow, with credentials stored as repository secrets.
- **Open question for Phase 2:** do that cluster's nodes expose `/dev/kvm`, either bare metal or with nested virtualization? If not, the Firecracker rung is exercised through Kata Containers instead.

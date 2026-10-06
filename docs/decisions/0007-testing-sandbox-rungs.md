# 0007. Testing the sandbox rungs

- **Status:** Accepted, 2026-10-05 (design plan D13). As of Phase 4, CI tests bubblewrap and Landlock on GitHub runners, and the bubblewrap pool and the pod rung on kind (`deploy/e2e/kind-e2e.sh`). The Firecracker tests run only on hosts with a usable `/dev/kvm`, which CI does not have.

## Decision

- **bubblewrap and Landlock** are tested in CI on standard GitHub-hosted Linux runners.
- **The hardened-pod rung** is tested on a kind cluster in CI.
- **The VM rung runs through Kata Containers for now** (decided 2026-10-05). On the project owner's Kubernetes cluster, the `vm` isolation level is provided by a Kata `RuntimeClass` rather than by `sandboxd` driving Firecracker directly. Kata can itself use Firecracker or Cloud Hypervisor underneath. Direct Firecracker with warm snapshot pools stays in the design for clusters and hosts that expose `/dev/kvm` to us, and is built after the Kata path.

## Consequences

- Phase 3's `vm` rung is a `pod` driver with a Kata `runtimeClassName`, reported as level `vm`. This is simpler to operate, but sandboxes start in seconds rather than milliseconds, so it suits agent environments more than RL reward hot paths.
- Validating the Kata rung needs a manual or scheduled workflow against that cluster, with credentials stored as repository secrets.
- The direct Firecracker driver (`sandboxd`) moves after the Kata path. It is needed for millisecond-scale warm pools.

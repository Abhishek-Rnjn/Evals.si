# 0002. Go core, Python runtime

- **Status:** Accepted, 2026-10-05 (design plan D2)

## Decision

The long-lived services are written in Go: API server, scheduler, OTLP ingest, sandbox manager and Kubernetes operator. Evaluators, framework adapters, the agent harness and the SDK are written in Python. The two halves talk only over protobuf, via gRPC and NATS.

## Why

Every evaluation framework worth adapting is Python. The services that must be fast, long-lived and Kubernetes-native benefit from Go: static binaries, the OpenTelemetry Collector libraries, controller-runtime and the Firecracker Go SDK. Python alone would ship faster but tops out on ingest, the operator and VM management. Rust would slow iteration.

## Consequences

- `proto/` is the single source of truth. Go code is generated into `gen/go`, and CI fails if it is stale.
- Python types mirror the proto messages field for field (`evalsi.types`). Generated Python stubs arrive with the worker protocol in Phase 1.
- Contributors need both toolchains. The `Makefile` wraps the common commands.

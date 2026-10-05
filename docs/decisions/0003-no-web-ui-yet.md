# 0003. No web UI for now

- **Status:** Accepted, 2026-10-05 (design plan D3)

## Decision

There is no web UI in the early phases. Results are visible through:

- the CLI;
- static reports;
- Grafana dashboards;
- write-back into the tools teams already use (MLflow, Langfuse, Phoenix).

## Why

A UI can consume half of a small team. The users we target already have trace and experiment UIs, and they want scores to show up there.

## Consequences

- Every result must be machine-readable: JSON output, a stable result schema and OTel export.
- Write-back sinks are core features, not extras.
- A minimal UI is reconsidered in Phase 5.

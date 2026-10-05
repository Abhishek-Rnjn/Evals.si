# 0004. Self-hosted in the client's environment; hosted multi-tenant later

- **Status:** Accepted, 2026-10-05 (design plan D5)

## Decision

For now, Evals.si is installed and operated by clients inside their own infrastructure. A hosted multi-tenant offering comes later, when there is compute for it.

## Consequences

- **Nothing calls home.** Telemetry is off unless the operator turns it on.
- **Bring your own everything:**
  - models and judges, with no default judge;
  - storage (Postgres, ClickHouse, S3-compatible);
  - identity (OIDC);
  - secrets;
  - observability.
- **Air-gapped installs are supported.** An offline bundle and a dataset mirror tool ship in Phase 3.
- **Least-privilege install.** The main Helm chart is namespace-scoped, and cluster-scoped pieces live in a separate chart.
- **Single tenant per install, with projects inside it.** Every stored key carries `project_id` and a reserved `tenant_id`, so a hosted multi-tenant service can be added without a data migration.

# 0008. Agent platform builders consume Evals.si as a service

- **Status:** Accepted, 2026-10-05 (design plan D14)

## Decision

Agent platform builders run Evals.si beside their platform as a service. They integrate through:

- the gRPC and HTTP API;
- OTLP;
- the CRDs;
- their own OIDC.

Embedding Evals.si as a library inside their control plane, and white-labeling, are not goals for now.

## Consequences

- API stability matters early. Breaking changes to `evalsi.v1alpha1` are allowed but must be deliberate, and they become forbidden once `v1` exists.
- Authentication is pluggable from the start (OIDC, API keys), and authorization is project-scoped so a platform can map its teams onto projects.
- Everything a platform needs to automate goes through the API, never through the CLI alone.

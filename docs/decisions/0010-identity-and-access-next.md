# 0010. Identity and access come next, modeled on agentgateway

- **Status:** Accepted, 2026-10-05 (design plan D15)

## Context

Phase 1 left `evalsid` with no authentication. Its only protection is the default loopback listen address. Once it listens on a network, anyone who can reach it can:

- spend the server's model keys on runs and judges;
- read traces and results, which may hold PII;
- execute code through code evaluators;
- change online policies, and inject traces into any project.

Platform builders consume Evals.si as a shared service ([0008](0008-platform-builders-use-a-service.md)), which already called for pluggable authentication and project-scoped authorization early. The next planned phase, agent runs, adds agents with tools and sandbox leases, which raises the stakes further. The roadmap had placed OIDC and RBAC in the Kubernetes phase.

## Decision

- **Identity and access becomes Phase 2.** The later phases move up by one: agent runs become Phase 3, Kubernetes Phase 4, fine-tuning and RL Phase 5, and MCP, classic ML and ecosystem Phase 6. The Kubernetes phase keeps only the Kubernetes-specific parts: service-account tokens as an OIDC provider, mTLS, and an admission webhook that records who created each CR.
- **The model follows agentgateway, so one identity setup and one policy language serve both:**
  - authentication policies with modes `strict`, `optional` and `permissive`;
  - JWT providers with a JWKS from a URL, a file, inline JSON or discovery;
  - API keys stored as `sha256:` hashes;
  - a configurable token location;
  - CEL authorization rules of three kinds, `allow`, `deny` and `require`, with agentgateway's precedence;
  - external authorization through AuthZEN and Envoy `ext_authz`.
- **Project-scoped RBAC with custom roles is the base.** Clients describe access in roles that fit their application, not in a fixed rule set:
  - Permissions are the API's actions, which form a stable public list.
  - A role is a named set of permissions, with optional inheritance and an optional CEL condition over request and resource attributes, including labels such as `app` or `env`.
  - viewer, runner, editor, admin, ingest and an install-wide owner ship as built-in roles. Clients extend them, or define their own in config or through the API.
  - Roles can also be mapped directly from token claims, so the client's application keeps owning role assignment.
  - Role management cannot escalate privilege: a project admin can only grant permissions it holds.
- **Global CEL rules are optional** and hold whatever the role. A request is allowed only when no `deny` matches, every `require` holds, and either a role or an `allow` rule grants it.
- **Bring your own identity provider.** Evals.si keeps no user database or passwords. The CLI signs in through the OAuth device flow or PKCE. CI uses workload identity, such as GitHub Actions OIDC, instead of stored secrets.
- **Secure by default.** `evalsid` refuses to listen on a non-loopback address without an `auth` section unless `auth: {mode: none}` is set explicitly. With auth enabled, the default decision is deny. Every RPC must appear in the action table, which a test enforces.

DESIGN.md §17 has the full design, and §23 has the slices.

## Consequences

- Agent runs start one phase later. In exchange, the harness, MCP tools and sandbox leases are built on top of principals, projects and audit, instead of having them retrofitted.
- The protos gain a project on `EvaluateRequest` and on traces, `Run.created_by`, labels on runs, policies and traces, and an `AuthService` with role management. These are additive changes to `v1alpha1`.
- The permission list becomes part of the public API. Renaming or removing a permission is a breaking change, so new permissions are added rather than existing ones changed.
- Every store query takes a project scope. That makes the store contract stricter, which helps when Postgres and ClickHouse arrive in Phase 4.
- The CLI and the Python client gain credentials: `evalsi login`, `EVALSI_TOKEN` and `EVALSI_API_KEY`. Embedded mode stays unauthenticated, because it is a local library.
- Decision records written before this one were updated to the new phase numbers.

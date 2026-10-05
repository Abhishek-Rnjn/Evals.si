# Decision records

Short records of decisions that shape the code. Each one says what was decided, why, and what follows from it. The design plan ([`../DESIGN.md`](../DESIGN.md), §22) lists the decisions that are still open.

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-first-users.md) | First users: agent builders, agent platform builders, LLM app developers | Accepted |
| [0002](0002-go-core-python-runtime.md) | Go core, Python runtime | Accepted |
| [0003](0003-no-web-ui-yet.md) | No web UI for now | Accepted |
| [0004](0004-runs-in-client-environment.md) | Self-hosted in the client's environment; hosted multi-tenant later | Accepted |
| [0005](0005-sandbox-isolation-ladder.md) | Sandbox isolation ladder: Firecracker, then bubblewrap or Landlock, then a hardened pod | Accepted |
| [0006](0006-naming-and-namespaces.md) | Names: `evalsi`, `evalsid`, `evals.si`, `evalsi.v1alpha1` | Accepted |
| [0007](0007-testing-sandbox-rungs.md) | Testing the sandbox rungs | Accepted |
| [0008](0008-platform-builders-use-a-service.md) | Agent platform builders consume Evals.si as a service | Accepted |
| [0009](0009-standalone-sqlite-in-process-scheduler.md) | Standalone: SQLite and an in-process scheduler; NATS with Kubernetes, DuckDB later | Accepted |
| [0010](0010-identity-and-access-next.md) | Identity and access (OIDC/JWT, API keys, RBAC with custom roles, CEL rules) come next, modeled on agentgateway | Accepted, implemented |
| [0011](0011-agent-environments-and-benchmarks.md) | Agent environments and benchmark grading: upstream scoring, direct format import, Firecracker without a network device | Accepted, implemented |

To add one, copy the shape of an existing record, take the next number, and link it here.

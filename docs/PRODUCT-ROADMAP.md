# Product roadmap: production agents and enterprise AI teams

Status: proposed implementation backlog, 19 October 2026.

This roadmap turns the [product strategy and competitive research](PRODUCT-STRATEGY.md)
into work that can be scoped, assigned and implemented in subsequent PRs. It targets
application teams shipping production agents and enterprise AI/platform teams
standardizing evaluation across applications. Security, risk and procurement
reviewers have their own acceptance criteria within enterprise accounts.

The product direction to validate is: **turn real agent failures into reproducible
tests and produce reviewable evidence for every release, using the customer's
existing agent framework, trace store and deployment environment.**

## Status, scope and tracking

- Every item below remains **proposed** and **unassigned** unless scoped in the
  implementation progress table. An existing foundation is
  not a claim that the proposed workflow or enterprise guarantee is shipped.
- P0 means the recommended initial product/pilot scope; P1 follows validation;
  P2 is conditional on customer requirements. These are priorities, not release
  commitments. Revisit them after discovery.
- Stable IDs identify work across issues and implementation PRs. Record an owner,
  issue/PR link, dependencies and acceptance evidence when work is picked up.
  Only mark an item shipped after its acceptance criteria and support limits are
  recorded. No issues or owners have been created by this planning document.
- The [technical inventory](LEFTOVERS.md), [customer Kubernetes PRD](PRD.md),
  [harness roadmap](HARNESS-ROADMAP.md) and accepted decisions retain their
  implementation status. This proposal does not silently cancel their work or
  change the GA date. Resolve priority conflicts explicitly in follow-up planning.
- The [original thirteen-item implementation review](verification/STACK-101.md)
  separates shipped work from follow-ups. Do not advertise those follow-ups as
  completed features.

### Implementation progress

These are initial implementation slices, not completion of the acceptance
criteria below. Package commands are unreleased until the next Python release.

| Items | Implemented slice | Evidence | Remaining acceptance work |
|---|---|---|---|
| PROD-01 | `evalsi doctor` diagnoses requested capabilities; `evalsi init` creates recorded-output or agent starters with positive/negative controls. | [Setup guide](guides/onboarding.md), `python/evalsi/tests/test_doctor.py`, `python/evalsi/tests/test_init.py`. | Observe onboarding on customers' own agents; verify supported host/container paths and measure time to useful result. |
| PROD-02 | Composite GitHub Action: one run, gate exit propagation, metrics/identity summary, conservative advisory baseline comparison, opt-in raw artifacts. | [Action](../action.yml), `python/evalsi/tests/test_ci_action.py`, the `onboarding` CI job checks published 0.3.0 with both controls. | Customer PR evidence for business failures/cost/latency, baseline provenance/governance, stronger reproduction evidence and release-policy integration. |
| GTM-02 | Runnable synthetic examples and a website-imported setup/CI guide. | [Guide](guides/onboarding.md), template controls and action smoke checks. | Framework-specific customer walkthroughs, teammate usability and approved customer examples. |

## Application-team workflow

These items reuse the shipped evaluation engine, CI recipes, comparisons,
annotation, content hashes, promotion, recording and sandbox execution.

| ID | Priority | Deliverable and existing foundation | Acceptance evidence | Dependencies |
|---|---|---|---|---|
| PROD-01 | P0 | Guided setup and diagnostics around the CLI/serve/install paths; proposed initialization and capability-check commands. | A new developer evaluates their own agent without maintainer help; measure time to first useful result against an initial target of 15 minutes. Verify the documented Linux and macOS/Windows-to-Linux execution paths and clear sandbox capability reporting. | GTM-01 |
| PROD-02 | P0 | A maintained GitHub Action or App wrapping existing CI gates, paired comparisons, intervals and error-rate checks. | A real PR shows baseline/candidate identity, meaningful regressions, critical business failures, cost/latency changes, errors/skips and reproduction links. Insufficient sample/coverage evidence is explicit; retries cannot silently select the best result. | PROD-01, PROD-04 |
| PROD-03 | P0 | A production failure inbox using existing trace ingestion, policies and promotion. | An approved real incident can be inspected, redacted, deduplicated, assigned, reviewed and promoted into a recurring test with its source trace and permissions preserved. Suggestions and generated labels require review. | PROD-04, PROD-05; ENT-06 for sensitive data |
| PROD-04 | P0 | A curated dataset lifecycle extending existing upload paths and content hashes. | Named immutable versions, item-level diffs, owners/tags, source provenance, review history and held-out versus tuning splits can be retrieved and used by a run without an unnoticed data change. | Existing DatasetService/content hashing |
| PROD-05 | P0 | An actionable UI extending the current runs/comparisons/annotation interface. | A teammate can connect/import, create or rerun an experiment, compare, inspect model/tool trajectories, approve a test case and configure a gate without writing YAML. Replay/fork clearly identifies recorded/live calls and controls external side effects. | PROD-01, PROD-04; reuse existing API authorization |
| PROD-06 | P1 | Judge/rubric calibration extending existing annotation queues and agreement statistics. | Human labels, disagreements, rubric versions, slice-level false positives/negatives and held-out results are visible. Comparisons distinguish a changed judge from an improved agent under a fixed judge. | PROD-04, PROD-05 |
| PROD-07 | P1 | A supported integration matrix and a first-class TypeScript path when pilot usage requires it. | Selected framework/trace-store paths run compatibility checks on supported versions, preserve trace/results semantics and diagnose import/write-back errors. Customers keep their existing agent loop; portable results remain exportable. | GTM-01 selects initial integrations; PROD-01 |

### The first complete workflow

Use one tool-using support/business workflow, such as order changes and refunds.
Build an editable starter suite for authorization, correct amounts/recipients,
clarification, retry/timeouts, duplicate prevention and human handoff. Check final
business state in a controlled environment alongside language-quality scores.
Document task provenance and usage rights, and identify synthetic cases.

The user should be able to go from **production failure → reviewed expected
behavior → dataset version → candidate comparison → CI protection**. Measure
whether that flow is understandable and repeatedly used before expanding the
number of templates or frameworks.

## Enterprise AI/platform workflow

Use the same engine and application-team workflow. Existing OIDC, group claims,
project RBAC, service identities, mTLS, credential grants and Helm/operator paths
are foundations to administer and qualify, rather than features to recreate.

| ID | Priority | Deliverable and existing foundation | Acceptance evidence | Dependencies |
|---|---|---|---|---|
| ENT-01 | P0 pilot | Supported private-deployment reference architectures around Helm/operator and air-gap tooling. | Publish the supported topology, outbound dependencies, capacity limits and install diagnostics. Exercise disconnected operation where sold and document who operates each dependency. | GTM-01 |
| ENT-02 | P0 pilot | Enterprise identity administration around existing OIDC/group mapping, project RBAC, mTLS and service credentials. | Target IdP configurations and UI administration pass role/isolation/revocation tests across CLI, UI, API and service identities. Decide native SCIM/SAML only after recording an actual provisioning/federation requirement. | GTM-01; existing identity/authorization model |
| ENT-03 | P0 pilot | Strict audit semantics from proposed ADR 0026. | Fault injection proves an audit-store/outbox failure cannot commit a supposedly rejected mutation. Specify transaction/outbox and recovery semantics for RPC, REST, MCP, asynchronous jobs and service-owned writes; preserve denial behavior. | Contract/design review of ADR 0026 |
| ENT-04 | P0/P1 pilot | Read/configuration auditing, authentication and initiator/executor context, plus durable export. | Reads/streams record scoped access without private payloads; configuration changes are attributed; export outage/recovery has bounded backlog and no silent audit loss. Add tamper evidence, archival/checkpoints and legal holds when required, with measurable retention guarantees. | ENT-03; per-sink delivery/retention design |
| ENT-05 | P1 | Central evaluation standards using existing evaluators, policies, permissions and gates. | Teams can adopt approved versioned rubric/test packs, inspect effective inherited policy, identify owners and record approvals and time-bounded exceptions. Existing applications retain project isolation. | PROD-04, ENT-02; ENT-04 for auditable changes |
| ENT-06 | P0 pilot | Verified data and secret boundaries around existing redaction and project/host credential grants. | Test boundaries through ingestion, execution, results, exports and retention. Document deletion/backup behavior and external secret-management integration. Scope stronger per-task credential isolation and legal holds according to the supported deployment. | ENT-01, ENT-02; existing ADR 0015 limits |
| ENT-07 | P1 | Shared capacity, budgets and usage reporting around current quotas and worker pools. | Multi-project, multi-replica tests demonstrate the documented hard/soft limits, priority/fairness and isolation. Owners can explain usage/chargeback; a noisy team cannot exhaust another's promised capacity. | ENT-01, ENT-02; pilot workload/cost measurements |
| ENT-08 | P1 | An exportable release-evidence package extending manifests, hashes and run results. | A reviewer can independently identify agent commit, dataset/rubric/model/environment versions, result coverage, decisions, reviewers and exceptions/expiry; tampering and missing evidence are detectable according to the chosen integrity contract. | PROD-02, PROD-04, ENT-04, ENT-05 |
| ENT-09 | P0 pilot | Operating qualification and support boundaries using recovery tests and operations runbooks. | Exercise supported migrations, backup restore, failure recovery and upgrade/rollback paths; publish measured RPO/RTO, load limits and service objectives. Test N/N-1 against migrated data before claiming rolling compatibility. Record operational ownership and support commitments. | ENT-01; compatible release pair and test infrastructure |

### Enterprise implementation boundaries

[ADR 0026](decisions/0026-enterprise-audit-and-delegation.md) is proposed, not
implemented. Current mutating/denied-call audit is an existing foundation; strict
and read auditing are new work. An interceptor flag alone does not establish the
ENT-03 guarantee. Audit behavior must remain consistent with transaction,
authorization, filtered-operation, streaming and asynchronous execution contracts.

[Identity](guides/identity.md) already delegates authentication and roles to
customer identity providers. Test and administer those paths before building
another identity system. [Credential grants](decisions/0015-per-project-credentials.md)
currently scope secret names/hosts; they do not establish isolated per-task secret
values. [Operations](guides/operations.md) describes supported procedures and the
uncertified rolling-upgrade boundary. Preserve those distinctions in sales and docs.

An enterprise pilot should include two or three application teams, a platform
owner and a security reviewer. Start with synthetic or approved non-sensitive
data, then approved real cases under the customer's handling policy. Demonstrate
shared standards, project separation, coverage/cost visibility, a release-evidence
review, revocation, an audit/export failure, an upgrade and a restore within the
agreed pilot scope.

## Adoption, distribution and packaging

| ID | Priority | Work | Acceptance evidence | Dependencies |
|---|---|---|---|---|
| GTM-01 | P0 | Design-partner discovery and pilot definition for both audiences. | Observe onboarding for 3–5 production-agent teams; qualify an enterprise pilot with application/platform/security participants. Record current release decisions, integration/deployment needs, consent/data scope and measurable success criteria. | None; no outreach is performed by this document |
| GTM-02 | P0/P1 | Product documentation, demonstration and ecosystem distribution. | Framework-specific walkthroughs begin with an existing application and show a deliberately broken control caught by CI. A second teammate can understand a private shared report. Publish customer examples only with approval; maintain support/benchmark evidence. | PROD-01, PROD-02, PROD-05, PROD-07 |
| GTM-03 | P1/P2 | Packaging, managed-operation discovery and enterprise procurement evidence. | Validate willingness to pay and compute/support costs. Prepare architecture/data-flow, outbound-dependency, SBOM/vulnerability, support/incident and applicable DPA/security-questionnaire materials. Document subprocessors when applicable; claim certification/testing only when obtained. | GTM-01, ENT-01, ENT-06, ENT-09 |

A packaging hypothesis is a reliable open-source local/CI foundation, optional
team collaboration/managed execution, and enterprise administration, supported
private deployments and operating commitments. Keep one engine and dependable
security/correctness across editions. Hosted service operation, billing and
customer-data responsibilities require explicit scope; they do not exist merely
because a managed offering is proposed.

Self-hosting does not remove vendor procurement and security-evidence needs.
Extensive legal holds, specific residency architectures, native SCIM/SAML or
compliance certification should be prioritized from qualified buyer requirements.

## Sequencing and measures

This is a relative validation sequence, not a delivery estimate or a committed
calendar. Staffing and pilot evidence determine scope. Fund operating reliability
throughout; do not postpone it until a large customer deployment.

| Window | Product focus | Enterprise work in parallel | Evidence to seek |
|---|---|---|---|
| Days 1–14 | GTM-01; one agent workflow; onboarding blockers and business checks. | Identify platform/security owners, supported deployment and required controls. | At least three teams run their own cases and explain today's release decision. |
| Days 15–40 | PROD-01/02 and a focused failure-detail slice of PROD-05. | ENT-01/02/03/06/09 contracts and initial pilot qualification. | Actual PR use and failures reproduced without maintainer interpretation. |
| Days 41–65 | PROD-03/04; shared UI flow; initial PROD-06 calibration. | Advance pilot-required ENT-04/05/07/08; verify inherited standards and evidence. | Real incidents become recurring tests; measure noisy blocks and governance effort. |
| Days 66–90 | Improve observed setup/compatibility failures; GTM-02 case studies. | Complete agreed isolation, evidence, audit/export, upgrade and restore exercises; GTM-03 discovery. | Initial target: three of five partners use the product weekly for four weeks; assess enterprise expansion into another team. |

The north-star measure is **weekly teams making a real release decision using an
Evals.si baseline and regression suite**. Track first useful result, first real
repository gate, four-week team retention, human-confirmed false blocks,
investigation time, cost per decision and cases promoted from real incidents.
For enterprise accounts, also track retained application teams per organization,
relevant releases covered, shared-standard adoption and evidence-preparation time.
Targets above are validation hypotheses, not achieved adoption statistics.

## Conditional follow-ups and scope choices

Broader connectors/protocols, a full prompt-management product, voice/desktop
environments, more benchmark importers, autonomous optimization and additional
training-environment work remain possible follow-ups. Existing reward/training
features remain supported; prioritize expansion from customer evidence rather
than treating this list as a cancellation of the technical roadmap.

An optional triage assistant should summarize evidence and link suggested causes
or fixes to the trace/checker results. Keep generated expectations reviewable.
Synthetic tests supplement real cases; neither a generated answer nor a judge's
score automatically becomes business ground truth.

The [strategy](PRODUCT-STRATEGY.md#primary-online-sources-reviewed) retains the
competitive findings, primary-source links, rationale and proposed positioning.
Existing [benchmark methodology](../tests/benchmarks/README.md) measures local
framework overhead; it does not establish an overall hosted-platform winner.

## Taking an item into implementation

For each issue or PR, include its stable ID, owner, user outcome, existing
foundation, scoped behavior and dependencies. Describe authorization/data effects,
failure/recovery behavior and the exact acceptance evidence. Record the tested
versions/deployment and remaining limits. Link evidence before updating status;
retain the distinction between implemented, locally verified, pilot-verified and
supported in production. Planning approval alone establishes none of those states.

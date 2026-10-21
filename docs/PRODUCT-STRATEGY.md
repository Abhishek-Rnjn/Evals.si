# Evals.si product strategy for production-agent and enterprise AI teams

Status: proposed product direction, 19 October 2026. Target audiences: teams shipping production agents and enterprise AI/platform teams.

Track implementation in the [product roadmap](PRODUCT-ROADMAP.md). This proposal adds no product functionality or certification claims. Existing implementation and verification status remains in the [technical inventory](LEFTOVERS.md), [customer Kubernetes PRD](PRD.md), and [harness roadmap](HARNESS-ROADMAP.md).

The [pre-0.4 release plan](PRE-0.4-RELEASE-PLAN.md) turns the next release's
evaluation workflow and customer-adoption work into required acceptance criteria.
Its pilot, retention and willingness-to-pay targets remain unmet until evidence
is recorded; they are not claims of existing customer usage.

## Recommendation

Position Evals.si around one recurring decision: **Can we safely ship this agent change?**

A product promise to validate: **Connect the agent and trace store you already use. Turn real failures into reproducible tests. Get a reviewable release decision for every change.**

The product already spans local scoring, CI gates, sandboxed execution, online policies, trace-store write-back, annotation, comparisons and rewards. The next investment should make those capabilities form an accessible, dependable workflow for an application team, with enterprise AI/platform teams able to standardize and operate that workflow across departments. Treat advanced training features as an adjacent market until production teams repeatedly use the release workflow.

This is a recommendation based on current public product documentation and repository inspection. It is not a usability study, an independent platform performance benchmark, a pricing survey or evidence of customer demand. Customer outcomes below are hypotheses and proposed acceptance criteria.

## Enterprise AI teams: one engine, shared standards and operating controls

Enterprise AI teams are an explicit second audience. Application developers use the daily testing and debugging workflow; central AI/platform teams often operate shared services and define organization-wide evaluation requirements. Security, risk and procurement have distinct acceptance criteria. One account can contain all these roles.

A broader positioning to validate: **A shared evaluation and release-assurance platform for production agents, with reproducible evidence and deployment under the enterprise's control.**

The enterprise path should reuse the developer workflow and existing project/RBAC infrastructure. Avoid creating a separate evaluation engine or requiring an enterprise-wide installation before one application team gets value.

### What enterprise buyers need, and the shipped foundation

| Capability | Existing foundation | Required product extension or evidence |
|---|---|---|
| Private operation | Self-hosted server, Helm/operator, air-gap tooling | A small set of supported reference architectures, documented external dependencies, installation/upgrade diagnostics, and verified disconnected operation where sold. |
| Enterprise identity | OIDC/JWT, group/role claims, project RBAC, API keys, mTLS and service identities | A usable administration experience and certified configurations with target IdPs. Validate whether native SCIM or SAML is actually required; do not rebuild working identity controls. |
| Shared evaluation standards | Evaluators, policies, datasets/hashes, project permissions and gates | Approved versioned rubric/test packs, inheritance and explicit exceptions, owners, approval history and effective-policy visibility across teams. |
| Audit and traceability | Mutating/denied-call audit and optional OTel export | Strict audit semantics, read/configuration auditing, richer initiator/executor context, durable export and retention guarantees. ADR 0026 is proposed, not shipped. |
| Sensitive data control | Trace redaction and project/host credential grants | Verified boundaries throughout ingestion, execution, exports and retention; defined deletion/backup behavior and optional legal holds. Support external secret management and stronger per-task secret isolation where needed. |
| Shared capacity and budgets | Quotas, worker pools and Kubernetes integration | Reliable organization/project allocation, priority/fairness, usage/chargeback views, and verified enforcement under multiple replicas; prevent one team from exhausting another's capacity. |
| Release evidence | Manifests, dataset hashes, comparisons and results | An exportable evidence package linking agent commit, dataset/rubric/model/environment identities, decisions, reviewers, exceptions and expiry. Make it independently checkable and access-controlled. |
| Reliable operation | Durable work, recovery tests, operations/runbooks and monitoring | Supported upgrade/migration and restore drills, scale/failure evidence, RPO/RTO and service objectives, and ownership/support commitments appropriate to the offering. Rolling N/N-1 compatibility is not currently certified. |

Strict audit needs service-level guarantees, not just an interceptor flag: an audit-store failure must not permit a supposedly rejected mutation to commit. Reads/streaming, REST, MCP, asynchronous jobs and service-owned writes need an explicit and tested contract. Existing access enforcement should remain common to all product surfaces.

### Enterprise pilot

Select one enterprise with two or three application teams, a central platform owner and a security reviewer. Start on a supported private deployment with synthetic or approved non-sensitive cases, then introduce approved real cases under the customer's handling policy.

Pilot outcomes should demonstrate that teams share an evaluation standard while retaining project isolation; the platform owner can see adoption, coverage and cost; and a reviewer can recover the evidence and authorization history for a release. Exercise revocation, an upgrade, an audit/export failure and a backup restore in the agreed test scope.

Use measured pilot goals, such as onboarding time per application, weekly retained teams, rubric adoption, time to assemble release evidence, confirmed false blocks and cost per decision. A successful single install does not establish operating trust across the organization.

### Packaging and delivery order

Keep the same evaluation engine across local/CI, team and enterprise offerings. A possible packaging hypothesis is:

- **Open source / developer:** local scoring, execution, CI checks, portable results and a dependable self-hosted foundation.
- **Team:** managed execution if needed, shared history, collaborative triage and administration.
- **Enterprise:** supported private deployment, central policy administration, evidence/audit integrations, operational commitments and procurement support.

Existing security fundamentals should remain dependable across editions. Do not paywall fixes to isolation, correctness or data protection. Determine the commercial boundary from customer demand, hosting/support costs and deployment responsibilities; do not promise a managed service before it exists.

Run enterprise discovery alongside the first production-agent pilots. Prioritize identity administration, strict/read audit, deployment evidence and tenant/capacity controls early when required by a qualified pilot. Sequence extensive legal holds, native SCIM/SAML, specific residency architectures or compliance certification against actual buyer requirements.

Prepare a procurement evidence pack: architecture/data-flow and outbound-dependency diagrams, a subprocessor statement if applicable, software inventory/SBOM and vulnerability handling, incident/support policies, published deployment limits, DPA/security-questionnaire responses, and independent testing or certification when actually obtained. Customer operation of a self-hosted instance does not eliminate vendor procurement requirements.

The developer adoption measure remains weekly teams using Evals.si for real release decisions. Add enterprise measures: retained application teams per organization, percentage of relevant releases covered, shared-standard adoption, evidence preparation time and expansion from the initial pilot into another team.

Enterprise sources: the [identity guide](guides/identity.md), [operations guide](guides/operations.md), [ADR 0015](decisions/0015-per-project-credentials.md) (credential boundaries), [ADR 0026](decisions/0026-enterprise-audit-and-delegation.md) (proposed audit hardening), and [current verification inventory](LEFTOVERS.md). Enterprise market and packaging recommendations are hypotheses to validate with customers, not newly researched competitor feature or certification claims.

## Competitive research

| Product | Publicly documented capabilities reviewed | Implication for Evals.si |
|---|---|---|
| LangSmith | Evaluation through UI and SDK; experiment comparisons; dataset versions/splits; human annotation and judge alignment. | The complete workflow is competitive territory. Make evaluation creation, investigation and rubric calibration accessible to the team. |
| Langfuse | Traces/sessions, prompt versions, datasets, code/judge/manual scores, and trace-to-playground investigation; self-hosted deployment. | Observability already has capable incumbents. Let their users add dependable Evals.si execution and gates without moving their trace history. |
| Arize Phoenix | OpenTelemetry/OpenInference tracing, versioned datasets, experiments, playground replay, prompt management, and MCP access. | A basic trace viewer or an MCP endpoint alone will not distinguish the product. Trace-linked executable evidence could be more useful. |
| Braintrust | Python and TypeScript code-defined evaluations with tasks/scorers, tracing SDKs, CLI workflows and many framework integrations. | Integration quality, a small code footprint and developer-language reach matter. |
| Promptfoo | Simple init/eval/view commands, declarative comparisons, CI integrations, shareable reports and broad adversarial/red-team testing. | The first useful run and understandable results must be easy. Existing marker injection controls should not be marketed as equivalent to broad validated red teaming. |
| DeepEval / Confident AI | Pytest-oriented agent/LLM metrics; the public README also describes production failure discovery, clustering and conversion to evaluations in Confident AI. | Another list of evaluators is a weak reason to switch. Curated failures and rubric trust deserve attention. |
| LangWatch / Scenario | Simulated users, multi-turn agent tests, tool assertions, and simple agent integration; a broader production observability product. | Useful scenarios test state changes and conversations, not only final text. |
| W&B Weave | Agent-centric tracing, evaluation, Python function tracing and integrations with several agent SDKs. | Make complete sessions and tool behavior easy to inspect while fitting existing instrumentation. |

These are source-supported capabilities, not claims that the products are identical, independently verified at scale, or equally usable. Feature overlap is substantial; the proposed positioning is a hypothesis to prove with customers, not a claim that no competitor supports CI or agent testing.

## What is already in Evals.si

Reviewed the README; web UI, annotation, agent-runs and CI-gating guides; run/dataset protocol; implementation review of PR 101; verification inventory; and local benchmark methodology.

Already built: CI gates, paired comparisons and intervals, error-rate gates, dataset content hashes, promotion of failing records, annotation and agreement statistics, real sandbox execution, recording/replay/resume/fork, and existing framework/trace-source integrations.

The current UI primarily reads runs/comparisons/policies/catalog and writes annotation answers. Starting runs and applying policies generally require the CLI/API. It does not yet present the full product workflow described below. Uploaded dataset hashes are a foundation for versioning; they are not by themselves a complete curated dataset lifecycle.

The [PR 101 implementation review](verification/STACK-101.md) explicitly leaves several original stack items for follow-up: strict/read audit, Tempo, Databricks-name and ambient SageMaker credentials, event/CLI interjections, pass@k exclusion for reward hacking, A2A push, and portions of step-level training telemetry/Gymnasium compatibility. The strategy should not assume all thirteen were shipped.

## Prioritized work

### 1. Deliver the first useful result in one session (P0)

Extend the current CLI/serve path with a guided `init` experience and a capability `doctor`. These are proposed additions, not current commands.

The user should connect an existing Python callable, HTTP agent, agent SDK or trace store, select an opinionated template, run a small dataset, see a specific failing case, and add the generated CI configuration. Use customer examples as soon as permission is granted; the no-key demo should deliberately show a good agent and a regressed agent.

Provide documented macOS/Windows paths to Linux-backed execution. Keep ordinary SDK scoring easy on all supported Python platforms and make sandbox capability differences explicit. Preserve fail-closed isolation; do not silently substitute an unconfined runner.

Acceptance test: a new developer reaches an evaluation of their own agent in under 15 minutes without a maintainer call. Treat this as a target to test, not a measured result.

### 2. Make the PR check the daily entry point (P0)

Package the existing CI recipe into a maintained GitHub Action or GitHub App. Put its installation in the onboarding flow. Start with one SCM rather than three.

For a candidate and the currently deployed baseline, show:

- Task success and paired differences with intervals.
- Critical business-rule and permission failures.
- Cost and latency changes and the affected customer/task slices.
- Newly failing cases with baseline/candidate tool trajectories.
- Evaluation coverage, error/skip counts and insufficient evidence.
- A reproducible command, configuration identities and links to detailed evidence.

Reuse existing paired comparisons, confidence bounds and error-rate gates. Add or verify a first-class policy for insufficient coverage, sample counts, material regression thresholds and baseline identity. Pin the dataset, evaluator/rubric, model settings, agent commit and tool/environment versions. Explain the limits imposed by nondeterministic external systems.

Begin as an advisory check; make enforcement a team choice after measuring its false alarms. Preserve critical deterministic business checks even if a composite quality score improves. Reruns should have an explicit policy rather than selecting the best result.

Acceptance test: the developer can identify and reproduce a blocked case directly from the PR. A trial with only a small, selected subset of records must not look like complete evidence.

### 3. Build the failure-to-regression workflow (P0)

Extend existing trace sources, online policies and promotion into a visible inbox:

1. Select an actual failed or uncertain production execution.
2. Show the trace, tool errors, business outcome and evaluator evidence.
3. Redact and deduplicate the case, retaining its origin and permissions.
4. Let a human confirm the expected behavior.
5. Add it to a named, immutable dataset version.
6. Run a candidate against that version and add it to the CI gate.

Prioritize cases by recurrence and business impact. Start with transparent rules and grouping; model-assisted clustering can be advisory. Avoid automatically labeling generated answers as ground truth. Separate tuning examples from a held-out set.

Expose dataset ownership, tags, version history and change diffs. Preserve existing hashes and provenance. Keep private data private when sharing reports or promoting cases.

Acceptance test: a reviewed production failure can become a repeatable test in a few clicks, with the original trace and dataset version still traceable.

### 4. Build the UI around decisions and debugging (P0/P1)

A compact application should support: connect/import, create or rerun an experiment, compare with a baseline, inspect a failure, approve a test case, and configure a gate. Start with these tasks rather than exposing every service endpoint as a screen.

Add a task/session timeline with LLM calls, tool arguments/results, user interventions and the business-state checker outcome. Compare paths as well as answers. Make error versus skipped versus failed behavior visible. Add ownership and links colleagues can use inside the appropriate access controls.

Use existing replay/fork primitives to offer debugging from a recorded step. Clearly mark recorded versus live calls and state restoration requirements. A replay of an external side effect must not accidentally issue a real transaction.

Acceptance test: a teammate who did not write the YAML can investigate and explain a regression using the browser.

### 5. Make evaluator trust visible (P1)

Build on the annotation queues, agreement statistics and comparison capabilities already present. Add a judge-calibration workflow that captures a rubric version, labeled calibration examples, human disagreements, slice-level false positives/negatives and a separate held-out check.

Use explicit state-based assertions for actions such as refund authorization, amount, recipient, duplicate writes and human handoff. A judge can help with helpfulness and interpretation but should not be the sole source of evidence for transactional correctness.

Keep rubric/evaluator changes explicit in run comparisons. A score improvement caused by a changed judge is a different observation from improvement under a fixed judge.

Acceptance test: the team can explain why its evaluator is fit for the decision and inspect disagreements rather than treating a score as unquestionable ground truth.

### 6. Make a few integrations excellent (P1)

Maintain a tested compatibility matrix for the release paths used by design partners. Candidate starting points are OpenAI Agents/LangGraph plus existing Langfuse/Phoenix ingestion; select the final set from actual customer usage.

Add a first-class TypeScript path if the first customers use JavaScript agent applications. They should not need to rewrite their agent around Evals.si's harness. Maintain standard OpenTelemetry/OpenInference and machine-readable result export; make trace import and score write-back resilient and diagnosable.

The research found that LangSmith's former SDK repository was archived and split into Python and JavaScript repositories on 16 October 2026. This is a concrete example of why compatibility tests and maintained support boundaries matter more than an unqualified integration count.

### 7. Treat operating trust as product work (parallel track)

Prove restart/recovery behavior, backup restore, migrations, retention, bounded retry/idempotency, resource and spend limits, and tenant isolation on the configurations sold or recommended. Publish reproducible measurements with the deployment sizes and limits stated. Add progress/cancel/retry behavior people can understand.

Strict/read auditing remains a real implementation item for an enterprise offering; existing authentication/RBAC should be reused. Add compliance claims only when the evidence and organizational work exist. A hosted service would also need operational ownership, billing and customer-data controls beyond the self-hosted engine.

The existing SDK-overhead benchmark is not an overall platform comparison. Publish matched real workflows—time to integrate, time to diagnose, persisted-result correctness, test cost and end-to-end runtime—instead of extrapolating SDK microbenchmarks to competitor scale or judging quality.

## First product experience

A proposed initial customer is a team shipping a tool-using support/workflow agent. Start with one workflow such as order changes and refunds. Build an editable starter pack around policy rules, user clarification, tool failure/retry, duplicate actions and escalation. Measure the final database/action state in a controlled test environment.

The outcome for the user is: “This prompt/model/tool change broke these specific customer cases; here is the evidence and how to reproduce it.”

The engine's portable evaluators and executable sandbox/checker behavior can support that promise while the customer's existing observability system continues to collect traces.

## 90-day validation plan

Run an enterprise pilot and security/platform discovery in parallel with the application-team work below. Use shared product infrastructure and prioritize the enterprise controls demanded by qualified pilot customers.

This is a sequencing proposal, not a delivery estimate; actual staffing and pilot feedback should determine scope.

| Window | Deliverable | Evidence to seek |
|---|---|---|
| Days 1–14 | Recruit 3–5 design partners; watch onboarding; select one agent workflow; define the release decision and business checks. | At least three teams can run their own cases; document each blocker and how they decide whether to ship today. |
| Days 15–40 | Guided setup, a maintained GitHub integration, paired regression report and failure-detail UI. | Partners use the check on actual PRs; developers reproduce failures without a maintainer explaining the report. |
| Days 41–65 | Production failure inbox, human-reviewed promotion, versioned datasets and baseline identity; initial judge calibration. | At least one real incident per partner becomes a recurring regression test; measure repeated use and noisy blocks. |
| Days 66–90 | Improve setup and compatibility from observed failures; publish case studies and a verified support matrix; pilot managed execution only if installation is a recurring blocker. | Target three of five partners using the product weekly for four consecutive weeks and able to name an incident or debugging cost it helped avoid. |

Fund the reliability/operability track throughout. Do not wait until a large enterprise pilot to test backup restore and supported upgrades.

## Adoption and business

- Make the public positioning and demo about the agent release decision. Keep the broader four-door architecture in deeper docs.
- Publish short framework-specific walkthroughs that start from an existing app and end with a caught regression. Include a broken control so the result is checkable.
- Offer task packs with documented provenance, configurable business rules and usage rights. Keep synthetic cases identifiable; they supplement real failures.
- Distribute inside GitHub and the agent/observability ecosystems the customer already uses. Easy exports and coexistence lower adoption cost.
- Use shareable reports to help a second teammate understand and adopt the workflow. Default private data and access controls matter.
- Keep a reliable open-source local/CI foundation. Test charging for managed execution, shared team history, collaboration and enterprise deployment/support where customers value them. Choose units after measuring support and compute costs; do not invent pricing from competitors' public feature lists.
- Treat an AI triage assistant as an optional evidence summarizer. Proposed causes or fixes should link back to traces/checker evidence and remain reviewable.

A useful north-star measure is **weekly teams making a real release decision using an Evals.si baseline and regression suite**. Track first useful result, first gate on a real repository, four-week team retention, human-confirmed false blocks, triage time, cost per decision and cases promoted from actual incidents. Stars, downloads and evaluator count are supporting distribution signals.

## What to sequence later

Broader connector coverage, more harness protocols, a full prompt-management product, more general benchmark importers, autonomous agent optimization, voice/desktop environments, and training-environment expansion should follow evidence from the selected audience. Existing RL/reward capabilities remain useful, but broadening that market immediately would compete for the same product and support capacity.

## Primary online sources reviewed

1. LangSmith evaluation quickstart: https://github.com/langchain-ai/docs/blob/main/src/langsmith/evaluation-quickstart.mdx
2. LangSmith comparisons: https://github.com/langchain-ai/docs/blob/main/src/langsmith/compare-experiment-results.mdx
3. LangSmith dataset lifecycle: https://github.com/langchain-ai/docs/blob/main/src/langsmith/manage-datasets.mdx
4. LangSmith judge alignment: https://github.com/langchain-ai/docs/blob/main/src/langsmith/improve-judge-evaluator-feedback.mdx
5. Langfuse overview: https://github.com/langfuse/langfuse#readme
6. Phoenix overview: https://github.com/Arize-ai/phoenix#readme
7. Braintrust Python SDK: https://github.com/braintrustdata/braintrust-sdk-python#readme
8. Braintrust JavaScript SDK: https://github.com/braintrustdata/braintrust-sdk-javascript#readme
9. Promptfoo overview: https://github.com/promptfoo/promptfoo#readme
10. Promptfoo CI documentation: https://github.com/promptfoo/promptfoo/blob/main/site/docs/integrations/ci-cd.md
11. Promptfoo red-team documentation: https://github.com/promptfoo/promptfoo/blob/main/site/docs/red-team/index.md
12. DeepEval / Confident AI overview: https://github.com/confident-ai/deepeval#readme
13. LangWatch Scenario: https://github.com/langwatch/scenario#readme
14. Weave overview: https://github.com/wandb/weave#readme
15. Inspect AI overview (additional evaluation-framework context): https://github.com/UKGovernmentBEIS/inspect_ai#readme

Public repositories were fetched directly through GitHub's API, following documented moves to active repositories. Sources describe their own products; no independent adoption or performance rankings are inferred. The links above identify the reviewed public documentation; repository branches and product features can change after the research date. The public braintrustdata/docs repository contained starter-template content and was excluded as product evidence.

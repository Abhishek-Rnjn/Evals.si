# Remaining work

Status: inventory compiled 20 October 2026 from the [product strategy](PRODUCT-STRATEGY.md),
[product roadmap](PRODUCT-ROADMAP.md), [PRD](PRD.md), [harness roadmap](HARNESS-ROADMAP.md)
and [LEFTOVERS](LEFTOVERS.md). It lists what those documents record as not built,
not verified or not decided. They remain the source of truth: each row names the one
it comes from, and their acceptance criteria apply. Nothing here is a new commitment.

**Kind** says what unblocks a row:

- **Code**: can be built and tested in this repository.
- **Infra**: needs a real cluster, hardware, credentials, live models or scale.
- **People**: needs customers, a decision, an outside party or a maintainer.

**Priority** is the source's (P0, P1, P2, M-milestone or H-phase); a dash means none is given.

Not repeated below: [#117](https://github.com/Abhishek-Rnjn/Evals.si/pull/117)
(shared dataset catalog and pinned regression runs, PROD-04; merged) and
[#118](https://github.com/Abhishek-Rnjn/Evals.si/pull/118) (production failures to
baseline-aware release decisions, PROD-02/PROD-03/ENT-08 foundation; merged).

## Application-team product work

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| PROD-01 | P0 | Guided setup (`init`, `doctor`) | Observe onboarding on customers' own agents; measure time to a useful result against the 15-minute target; verify the documented macOS/Windows paths to Linux-backed execution | People, Infra | Roadmap, strategy §1 |
| PROD-02 | P0 | PR check as the daily entry point | Customer PR evidence; critical business-rule and permission failures; cost/latency changes by slice; newly failing cases with baseline/candidate trajectories; a first-class policy for insufficient coverage, sample counts and regression thresholds; an explicit rerun policy (no best-of selection); pinned agent commit and tool/environment versions; a reproduce command in the report | Code, People | Roadmap, strategy §2 |
| PROD-03 | P0 | Failure-to-regression inbox | Validate on real customer incidents; automatic policy-driven collection; recurrence/impact prioritization; semantic grouping (advisory); enforced independent review for inbox cases; assignment notifications; broader experiment/replay UI | Code, People | Roadmap, strategy §3 |
| PROD-04 | P0 | Dataset lifecycle | The shared catalog from #117 is built (unreleased). Left: customer validation, shared ownership administration, broader split governance, bulk storage, retention and erasure. Exact overlap checks do not rule out semantic leakage; imported provenance is publisher-supplied | Code, People | Roadmap |
| PROD-05 | P0 | Decision and debugging UI | Built unreleased: [release workbench](guides/release-workbench.md) for supported run configuration, policy forms, pinned exports and trial-specific paired event/checker inspection with access-controlled deep links. Left: advanced connectors/harness configuration, broader connect/import, live replay/fork with side-effect controls, and customer usability validation | Code | Roadmap, strategy §4 |
| PROD-06 | P1 | Evaluator trust and judge calibration | Rubric versions, labelled calibration sets, human disagreements, slice-level false positives/negatives, held-out checks; comparisons that tell a changed judge apart from a better agent | Code | Roadmap, strategy §5 |
| PROD-07 | P1 | Integration matrix and TypeScript | A tested compatibility matrix for partners' paths (candidates: OpenAI Agents, LangGraph, Langfuse, Phoenix); a TypeScript SDK if partners need it; resilient, diagnosable import and score write-back | Code, People | Roadmap, strategy §6 |
| STRAT-1 | P0 | First workflow starter pack | The support/refund suite is built (unreleased, [guide](guides/support-refund-suite.md)): twelve synthetic tasks, provenance recorded. Left: run it on a model-driven agent and on a customer's agent; more cases, from real incidents; approved customer examples and usage rights | Code, People | Strategy "First product experience", roadmap |
| STRAT-2 | P1 | State-based business assertions | Built for the refund workflow: `builtin/state-assertion` plus a checker reporting authorization, amount, recipient, duplicate-write, handoff, clarification and ledger assertions. Left: other workflows; a domain-neutral database tool environment (H4-d); tamper resistance beyond clean checking and the hash-chained ledger | Code | Strategy §5 |
| STRAT-3 | - | Optional triage assistant | Summarizes evidence and links suggested causes or fixes to traces and checker results; generated expectations stay reviewable | Code | Strategy "Adoption", roadmap |
| STRAT-4 | - | Matched-workflow benchmarks | Publish time to integrate, time to diagnose, persisted-result correctness, test cost and end-to-end runtime, instead of extrapolating SDK microbenchmarks | Code, Infra | Strategy §7 |
| STRAT-5 | - | Progress, cancel and retry UX | Run progress, cancel and retry behaviour people can follow | Code | Strategy §7 |

## Enterprise AI/platform work

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| ENT-01 | P0 pilot | Private-deployment reference architectures | Topologies and every outbound connection are now documented ([enterprise deployment](guides/enterprise-deployment.md)). Left: published capacity limits, install diagnostics, and disconnected operation exercised where sold | Infra, People | Roadmap, strategy |
| ENT-02 | P0 pilot | Identity administration | Revocation and isolation are now tested across Connect, gRPC, REST and MCP. Left: target IdP configurations, UI administration, the web UI surface; decide SCIM/SAML only from a recorded requirement | Code, Infra | Roadmap, strategy |
| ENT-03 | P0 pilot | Strict audit (ADR 0026) | Strict mode is built and on by default with authentication; a change that cannot be audited is refused before it runs. Left: a tested contract for asynchronous jobs other than run retention, and outbox semantics once export is guaranteed | Code | Roadmap, LEFTOVERS, ADR 0026 |
| ENT-04 | P0/P1 pilot | Read and configuration auditing, durable export | Built: read auditing by kind, configuration load/reload entries, authentication context with `subject`/`actor`, and a hash chain with `evalsi auth audit verify`. Left: read sampling and row counts, initiator/executor/grant fields, signed checkpoints, a durable export with bounded backlog, legal holds | Code | Roadmap, LEFTOVERS |
| ENT-05 | P1 | Central evaluation standards | Approved, versioned rubric/test packs; inheritance and effective-policy view; owners, approvals and time-bounded exceptions | Code | Roadmap, strategy |
| ENT-06 | P0 pilot | Data and secret boundaries | Data lifecycle and an erasure procedure are now documented ([enterprise deployment](guides/enterprise-deployment.md#data-lifecycle-and-erasure)). Left: boundaries tested through ingestion, execution, results, exports and retention; per-label retention rules; a delete grace period; an erasure API; external secret management; per-task credential isolation where needed | Code, Infra | Roadmap, LEFTOVERS |
| ENT-07 | P1 | Shared capacity and budgets | Multi-project, multi-replica tests of hard/soft limits and fairness; usage and chargeback views. Concurrency quotas and rate limits are per replica today | Code, Infra | Roadmap, LEFTOVERS |
| ENT-08 | P1 | Release-evidence package | Exportable, independently checkable evidence: agent commit, dataset/rubric/model/environment versions, coverage, decisions, reviewers, exceptions and expiry, with tamper detection | Code | Roadmap, strategy |
| ENT-09 | P0 pilot | Operating qualification | Versioned migrations with a minimum-reader guard are built. Left: the real N-1 release against a migrated database in CI; upgrade and rollback paths exercised; measured RPO/RTO and service objectives; managed/off-site PITR; ClickHouse and NATS recovery | Code, Infra | Roadmap, LEFTOVERS |

## Adoption, distribution and packaging

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| GTM-01 | P0 | Design partners and pilot | Observe 3 to 5 production-agent teams; qualify an enterprise pilot with application, platform and security participants; record their release decisions and success criteria | People | Roadmap, strategy |
| GTM-02 | P0/P1 | Docs, demos and distribution | Framework-specific walkthroughs from an existing app to a caught regression; a private shareable report a second teammate can follow; approved customer examples; a published support matrix | Code, People | Roadmap, strategy |
| GTM-03 | P1/P2 | Packaging and procurement evidence | Willingness to pay and cost validation; architecture/data-flow and outbound-dependency diagrams; SBOM and vulnerability handling; support and incident policies; DPA and security questionnaire; subprocessor statement if applicable | People, Code | Roadmap, strategy |
| GTM-04 | - | Penetration test | Commission an outside test before the first enterprise evaluation; keep answers and evidence in `docs/` | People | LEFTOVERS |
| GTM-05 | - | Second maintainer | A second owner for `/.github/`, `/deploy/` and `SECURITY.md` in `CODEOWNERS` | People | LEFTOVERS |

## PRD milestones

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| M2 | M2 | Agent harness online | Sign-off on the real agent harness, which needs access to its repository (open question) | People, Infra | PRD, LEFTOVERS |
| M3 | M3 | Pull connectors | Databricks and SageMaker against real workspaces; backfill and lag at production scale; scores checked in each store's UI; profiles against real model output; Langfuse Cloud/TLS/arm64 | Infra | PRD, LEFTOVERS |
| M3-b | M3 | Connector gaps | Databricks table-prefix and experiment-name locations; ambient AWS credentials for SageMaker | Code | LEFTOVERS |
| M4 | M4 | Real-infra hardening | 10k traces/min and 1k concurrent trials; a full zero-downtime upgrade and the N-1 rule; node failure; sandbox egress under a mesh (Calico with Istio ambient) | Infra | PRD, LEFTOVERS |
| M5 | M5 | More sources | LangSmith live verification; ClickHouse Cloud/TLS/distributed/scale; Tempo, Kafka and S3 sources | Code, Infra | PRD, LEFTOVERS |
| PRD-Q1 | - | Kafka and log sources | Decide built-in connectors or collector recipes only for v1 | People | PRD open questions |
| GA | GA | GA with agent harness | All P0 requirements met, runbooks and SLO alerts, a 4-week external pilot | People, Infra | PRD |

## Harness roadmap

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| H1-a | H1 | Frontier-model exit test | A recorded multi-step `fix-calc` run with thinking on Anthropic and an OpenAI reasoning model that replays exactly; native Responses and thinking-plus-tool roundtrip; direct prompt-cache reads failed in both modes | Infra | Harness roadmap, LEFTOVERS |
| H1-b | H1 | Frontier-model gaps | Moving reasoning between providers (dropped today by rule); a separate model for compaction or sub-agents; `pause_turn` with server-side tools; resuming a broken stream's partial output | Code | Harness roadmap |
| H2-a | H2 | Persistent background processes | A long-lived shell per sandbox in `evalsid`, so a server started in one call survives the next | Code | Harness roadmap, LEFTOVERS |
| H2-b | H2 | Long-horizon verification | Live-model 300-step and context-quality runs; external side-effect idempotency; `evidence-state` quote check is a text match | Infra | Harness roadmap, LEFTOVERS |
| H3-a | H3 | Agents-under-test gaps | ACP connector (needs interactive exec in the sandbox); A2A push notifications; a server-side sweep over harness configurations | Code | Harness roadmap |
| H3-b | H3 | Agents-under-test verification | Readers for Codex, Gemini CLI and pi against real output; profile comparison on a live model; the live CLI kind check with a key; transfer experiments | Infra | Harness roadmap, LEFTOVERS |
| H4-a | H4 | Clean checker from the agent's diff | Run the checker on a clean copy with only the agent's diff applied; clean checking on the pod and Firecracker rungs | Code, Infra | Harness roadmap, LEFTOVERS |
| H4-b | H4 | Multi-service environments | Service groups on bubblewrap and Firecracker; unrestricted Compose; multi-stage builds; a multi-service Terminal-Bench task on the bubblewrap and pod rungs | Code, Infra | Harness roadmap, LEFTOVERS |
| H4-c | H4 | Browser, desktop and long-document environments | Browser and desktop environments; WebArena/BrowserGym and GAIA importers; OfficeQA Pro and AA-LCR importers | Code | Harness roadmap, LEFTOVERS |
| H4-d | H4 | Other environment gaps | A domain-neutral database tool environment (a design question); interjections timed by event and for CLI agents | Code, People | Harness roadmap, LEFTOVERS |
| H5-a | H5 | Rigor signals on real data | False-positive rates of `trial-tampering`, `harness-edit-weakened-guardrail` and `contamination-fetch` on real runs; `sitecustomize.py` shim blind spot; variance advice on real multi-trial results | Infra | LEFTOVERS |
| H5-b | H5 | Judge calibration | Human-labelled checks for `evidence-stage-judge`, `rubric-grade` and `calibrate`; broader deployed-judge injection coverage | People, Infra | LEFTOVERS |
| H5-c | H5 | Determinism record | A per-run record of seeds, model snapshots, tool versions and image digests; a replay that names the step where it diverged | Code | LEFTOVERS |
| H6-a | H6 | RL gaps | A run-spec field for `token_trace`; SGLang token fields; cancel and resume of rollouts on the pod rung | Code | Harness roadmap |
| H6-b | H6 | RL verification | A trainer running a real step in CI; trainer-backed OpenEnv, verifiers, Gymnasium and NeMo Gym; current OpenRLHF 0.11.2; live vLLM token IDs; measured rollout and reward throughput at the §12 scale; whether step-level rewards help | Infra | Harness roadmap, LEFTOVERS |
| H7-a | H7 | Harness² gaps | OpenCode trajectory reader; a per-task significance test for ablation harm; a LAB/JobBench grader that sees workspace files; live scoring of executor telemetry | Code | Harness roadmap, LEFTOVERS |
| H7-b | H7 | Harness² verification | The Propose-Probe-Compose go/no-go spike; the `runs/` layout and `score.json` fields against a pinned release; a full Codex or OpenCode task under the egress allowlist | Infra | Harness roadmap, LEFTOVERS |

## Infrastructure verification

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| INF-1 | M4 | Firecracker with the stock image | Re-run PR #12's fixes with the stock image and `firecracker.defaultImage` on a KVM cluster | Infra | LEFTOVERS |
| INF-2 | M4 | PR #12 fixes on AKS and Istio | Re-run on those clusters | Infra | LEFTOVERS |
| INF-3 | M4 | User-namespaced bubblewrap | AKS and idmap host limits; nested kind preflight failed | Infra | LEFTOVERS |
| INF-4 | M4 | Admission webhooks on EKS with Calico overlay | Overlay pod routing not certified | Infra | LEFTOVERS |
| INF-5 | - | Reference demos | Deep-research and Terminal-Bench 2 demos with a real model and judge; dsh interactive web export | Infra | LEFTOVERS |
| INF-6 | - | Framework mapping profiles | Native Responses, retries and multi-agent coverage | Infra | LEFTOVERS |
| INF-7 | - | Real SWE-bench Verified images | Run at least one real task | Infra | LEFTOVERS |
| INF-8 | - | cgroup v2 limits unprivileged | Read-only cgroup mount in non-privileged pods falls back to rlimits | Infra | LEFTOVERS |
| INF-9 | - | Kata Containers as the `vm` level | Run on a cluster with Kata installed | Infra | LEFTOVERS |
| INF-10 | - | Pipeline and schedule certification | AWS S3, database failover/HA races, production load, MCP tool idempotency, scheduled source write-back | Infra | LEFTOVERS |
| INF-11 | - | EKS GPU resume list | Paused checks in the [EKS handoff](verification/EKS-PAUSED-2026-10-18.md) | Infra | LEFTOVERS |
| INF-12 | - | Checkpoint model serving | Serving strategy for state dictionaries and Lightning `.ckpt` files; fixed image release | Code, Infra | LEFTOVERS |
| INF-13 | - | Guardrails against agentgateway | Streaming and real model/agent products | Infra | LEFTOVERS |
| INF-14 | - | Coding agent over MCP in CI | GitHub `mcp-agent` secret and first CI run; other coding products; remote OAuth | Infra | LEFTOVERS |
| INF-15 | - | Web UI accessibility | A manual screen-reader audit | People | LEFTOVERS |

## Smaller code items

| ID | Priority | Item | What is left | Kind | Source |
|---|---|---|---|---|---|
| SM-1 | - | Webhook URLs to internal addresses | Apply the trace-source address guard with an allow list; decide the default | Code, People | LEFTOVERS |
| SM-2 | - | Rate limits | A limit shared across replicas; live reload of settings | Code | LEFTOVERS |
| SM-3 | - | Keyed dataset upload | Stop writing it to disk twice and taking two upload slots | Code | LEFTOVERS |
| SM-4 | - | `evalsi.log()` retries | Retry a failed batch, sending an `Idempotency-Key` | Code | LEFTOVERS |
| SM-5 | - | Scheduled-window partial pages | A connector returning a partial page without an error loses traces from that window | Code | LEFTOVERS |
| SM-6 | - | Keyless image signatures | Verify Fulcio/Rekor keyless signatures and the bundle-as-referrer format | Code | LEFTOVERS |
| SM-7 | - | Sandbox-time quota coverage | Meter online policy evaluation, guardrails, Wasm evaluators and reward rollouts | Code | LEFTOVERS |
| SM-8 | - | Feeding scores back into agentgateway | Quality-aware routing; depends on the gateway's extension points | Code, People | LEFTOVERS |
| SM-9 | - | BFCL categories | Memory and web-search categories | Code | LEFTOVERS |
| SM-10 | - | τ-bench user | tau2's own LiteLLM user instead of Evals.si's simulated user | Code | LEFTOVERS |

## Deliberately not planned

Recorded so they are not mistaken for gaps: a general agent framework, provider SDKs
as dependencies, automated harness search or evolver training, hosting agents
([harness roadmap](HARNESS-ROADMAP.md#not-planned)); arbitrary server-side SQL over
runs ([decision 0022](decisions/0022-server-side-slicing.md)); RFC 8693 on-behalf-of
token exchange ([0015](decisions/0015-per-project-credentials.md)); Azure ML trace
sources (no V3 trace API); FrontierCode and CursorBench (tasks not public). The
strategy sequences broader connectors, a prompt-management product, voice/desktop
environments and training-environment expansion after customer evidence.

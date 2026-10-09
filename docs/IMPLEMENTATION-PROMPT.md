# Implementation prompt: next phase

This is the brief to hand an implementation agent for the next phase: M1 and the reference demos from [PRD.md](PRD.md), plus the design for M3. Paste everything below the line.

---

You are implementing the next phase of Evals.si (repo: Abhishek-Rnjn/Evals.si, branch from main).

## Source of truth
Read these before writing code:
- [AGENTS.md](../AGENTS.md): repository orientation and checks.
- docs/PRD.md: the product requirements, with requirement IDs and milestones M1–M5.
- docs/verification/: a real end-to-end run on three clusters. Start with its README.md, which lists every finding and whether it is fixed or still open. The clusters are:
  - kind (kindnet, no NetworkPolicy enforcement);
  - AWC (Istio ambient with a forced waypoint and mesh-wide STRICT mTLS, Calico, KVM nodes);
  - AKS (Ubuntu 24.04, every node tainted, no NetworkPolicy engine).

  Call its findings VR-F1…, VR-D1…, VR-R1…, so they never clash with the PRD's own D1–D11.
- docs/DESIGN.md, docs/decisions/ (follow their format for new records), docs/LEFTOVERS.md, README.md, docs/guides/*.

When the PRD and the code disagree, the code is the current state and the PRD is the target. If a requirement is wrong or impossible, stop and report it; don't silently change scope.

## Fixed decisions (do not revisit)
- agent-studio-standalone traces to MLflow Tracing.
  - The first pull connector is MLflow: open-source 3.x, Databricks, SageMaker and Azure ML.
  - Langfuse and Phoenix come after it.
- Single tenant: one Evals.si project per studio install.
- Evals.si ships as an independent Helm chart beside other charts (like agent-sandbox), plus an integration guide. It is never a subchart.
- Agents are evaluated over HTTP, gRPC or A2A, or in-process from a PyPI package.
- No default judge model. Never bill a model by surprise.
- Demo agents:
  - Deep Agents (langchain-ai/deepagents) is the main walkthrough.
  - DeepSeek Harness (deepseek-ai/deepseek-harness, `dsh`) is the second example.
  - Both run on Anthropic or any OpenAI-compatible endpoint.
  - CI uses a deterministic mock model.
- "Runs anywhere" means:
  - any conformant cluster, with no cloud-specific dependency;
  - kind is the reference and CI target;
  - it must also work on the two real cluster shapes in docs/verification/: a mesh cluster (Istio ambient, waypoint, STRICT mTLS) and a tainted-node cluster with no NetworkPolicy engine.

## Step 0: verify what is already fixed (before new work)
PR #12 (commits 033b11c, 967fdd9, 8cf4c81) fixed most findings after the run. docs/verification/README.md lists which. Don't redo them. Do this instead:
1. On kind, re-run the rows these fixes touch and record the evidence:
   - judge `projects`;
   - `auth audit --denied` showing escalation refusals;
   - all-errors run fails;
   - OTLP partial success on dropped spans;
   - tolerations, nodeSelector and service/pod labels rendering;
   - sandboxd firecracker values rendering (no KVM on kind: render and config check only).
2. Write docs/verification/RECHECK.md: a short, copy-pasteable checklist for re-running the affected rows on AKS and on the mesh cluster. You cannot reach those clusters; the maintainer will run it.

Then handle the findings still marked Open:
- **Sandbox egress NetworkPolicy not enforced on Calico + Istio ambient (AWC).** The cause is unknown.
  - Investigate from the policy's selectors and ambient's known interaction with NetworkPolicy.
  - Fix it if it's ours. Otherwise document the limitation and make `evalsid sandbox probe` or the SandboxClass status say plainly when egress isolation is not enforced. Sandbox isolation must never be claimed when it isn't.
- **VR-D15 and the AWC idmap failure.** `mode: bwrap` fails on Ubuntu 24.04 / AKS (`RTM_NEWADDR: Operation not permitted`) and on hosts whose overlayfs lacks idmap mounts.
  - Make the failure message name the fallback (`mode: privileged`, or the pod rung).
  - Document both cases in docs/guides/kubernetes.md.
- **VR-R2.** Add an opt-in chart value that renders the Istio PeerAuthentication (webhook port 9443 PERMISSIVE). Leave it off by default.
- Never verified: KEDA on a live pool, several-node NATS, a real SWE-bench Verified task, vLLM checkpoint curves, agentgateway in front. Leave them in docs/LEFTOVERS.md unless your scope covers them.

## Scope, in order
1. M1
   - E2: `Client.evaluate()`, `evaluate_stream()` and `evaluate_async()` over EvaluationService. They return the same `EvaluationResult` as in-process `evalsi.evaluate()`. Add a parity test: same scores and intervals.
   - P4: webhooks when a run finishes or a gate fails, HMAC-signed, with retries.
     - Follow the policy-alert webhook code in internal/watch/engine.go.
     - Expose it as `POST /v1alpha1/webhooks`.
     - Define it in proto first.
   - D4 (PRD): a Helm bootstrap Job that creates projects, API keys, credential grants and default policies from values and writes keys to Secrets. It must be idempotent on upgrade and work with Helm 3 and Helm 4 (see VR-D3).
   - D2 (PRD): audit the charts so every name is prefixed by the release, and Postgres, NATS, S3 and ClickHouse can each be bundled or external.
   - D1 (PRD): make release.yml produce signed images and OCI charts. Do not cut a tag.
2. Demos (PRD R1–R5, R7–R9)
   - R1: images under examples/demo/.
     - Deep Agents image: pinned deepagents and deepagents-code, plus an HTTP wrapper.
     - dsh image: Node 22, dsh pinned to a commit.
     - Both must work as non-root on the pod rung (see VR-D8; set `runAsUser` where needed).
   - R2: trajectory mapping, tested against recorded fixtures.
     - dsh: `dsh --profile headless --json` events.
     - Deep Agents: LangGraph spans.
     - First verify `dcode`'s headless flags and that MLflow's LangChain autolog captures Deep Agents runs.
   - R3: one run spec per agent and suite. Suites:
     - small repo fixes: fix-calc plus about 10 SWE-bench-style fixtures;
     - Terminal-Bench 2 via harbor://;
     - deep research graded by llm-judge with a rubric.
   - R4: standalone walkthrough.
   - R5: Kubernetes walkthrough.
     - On Kubernetes, file datasets need S3 (VR-D2), so the demo installs S3-compatible storage (MinIO) or bakes datasets into images.
     - It includes an MLflow server and the MLflow sink.
     - Model keys go through the per-project credential grants (PR #11), never raw env vars on pods.
     - A namespace-only install must use the pod rung: agent runs refuse Landlock by design.
   - R7: extend deploy/e2e/kind-e2e.sh so CI runs the small-repo-fix suite for both agents against the mock model.
   - R8: every pod the demo adds (MLflow, MinIO, the Deep Agents service, `dsh web`) takes tolerations, nodeSelector, pod and service labels, image registry and storage class as values, like the evalsi chart after PR #12. Document a values overlay for each cluster shape: kind, tainted nodes, Istio ambient with waypoint.
   - R9: docs/guides/integrate-an-agent-studio.md.
3. Design only: docs/decisions/0016-trace-source-connectors.md for M3. It covers:
   - the `TraceSource` CRD and API;
   - the Go connector interface;
   - per-variant MLflow watermarks, auth, pagination and rate limits, cited from each variant's real API docs;
   - the mapping-profile format;
   - write-back as MLflow assessments;
   - egress NetworkPolicy for sources.

   Pulled records enter the policy engine through `watch.Engine.IngestBatch([]ingest.Trace)`.

## Rules
- Match the surrounding code's style, naming, comments and test patterns. Go core, Python runtime (see decision 0002).
- Add tests for every behaviour change, including tests/helm for chart values. Never skip, disable or weaken a test.
- Before each commit, run what CI runs and make it pass (see AGENTS.md).
- Run deploy/e2e/kind-e2e.sh for chart and demo changes when Docker is available. Say plainly when you could not run it.
- Keep docs/PRD.md status cells, docs/LEFTOVERS.md, docs/verification/README.md and README.md current as items land.
- Small commits with clear messages. One pull request per scope item group.
- No secrets, API keys, internal hostnames or private registry names in commits.

## Done means
- Step 0 evidence and docs/verification/RECHECK.md are written.
- Every P0 item in scope is implemented, tested and documented.
- CI is green.
- Both demos pass end to end in standalone mode and on kind against the mock model.
- Decision record 0016 is written.
- A short report lists:
  - what was built, by requirement ID;
  - what was verified, and how;
  - which verification rows now need a re-run on AKS or the mesh cluster;
  - anything deferred, or found wrong in the PRD.

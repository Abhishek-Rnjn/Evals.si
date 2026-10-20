# From production failure to release decision

This unreleased workflow connects **production failure → approved regression
suite → candidate comparison → release decision**. Use a matching source SDK,
server and worker; published 0.3.0 does not include these APIs or commands.

A release decision checks one explicit baseline and one candidate against a
reviewed policy and the complete approved held-out suite. It saves the result
and its evidence identities. The CLI and GitHub Action fail closed: insufficient
evidence cannot produce a passing check. Recording a decision does not deploy
software or authorize deployment by itself.

## 1. Capture and approve the regression suite

Use the [failure inbox](failure-inbox.md) to capture a trace revision, redact the
case, enter the expected behavior and approve that saved revision. Publish
explicit approved case revisions into the [shared catalog](dataset-catalog.md),
inspect the whole dataset, and approve its content digest. Authenticated dataset
approval requires a different principal from the publisher.

Use a **held-out** suite. Release checks require the full version: a limited run
cannot represent full-suite coverage. The policy must require at least two
independent records. Two synthetic cases are sufficient for the demo mechanics,
not evidence that a real agent is safe; set an appropriate sample requirement
for your task and risk tolerance. Repeated trials do not increase independent
record count. Existing overlap checks can detect exact tuning/held-out overlap;
the held-out label alone does not establish absence of contamination.

## 2. Establish an explicit baseline

Run the currently accepted agent against that approved catalog pin. Include the
declared agent commit in the run's metadata:

```yaml
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: accepted-refund-agent
  project: support
  labels:
    agent.commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
spec:
  dataset:
    catalog:
      project: support
      name: refunds
      version: v1
      sha256: REPLACE_WITH_APPROVED_DATASET_PIN
  target:
    connector: openai-compatible
    model: support-agent
    base_url: https://accepted-agent.example.com/v1
    api_key_env: SUPPORT_AGENT_KEY
  evaluators: [{ref: exact-match}]
  gates:
    - {metric: exact-match, min: 1}
    - {metric: exact-match, stat: GATE_STAT_ERROR_RATE, max: 0}
```

```bash
evalsi run -f baseline.yaml --server https://evals.example.com
evalsi release evidence "$BASELINE_RUN_ID" --server https://evals.example.com
```

Inspect the result, then save the returned reference's run ID, evidence SHA-256
and commit in reviewed configuration. There is no automatic `latest` baseline.
The commit label is a declaration supplied by the run creator; it is not proof
that a remote endpoint executed those Git bytes. An endpoint must actually serve
the intended candidate in the customer's test environment.

## 3. Review a release policy

For example, `release-policy.yaml`:

```yaml
name: production-refunds
version: v1
dataset:
  project: support
  name: refunds
  version: v1
  sha256: REPLACE_WITH_APPROVED_DATASET_PIN
min_records: 30
confidence_level: 0.95
rules:
  - metric: exact-match
    direction: METRIC_DIRECTION_HIGHER
    max_regression: 0.02
    min_mean: 0.95
    min_case: 1
    case_ids: [refund-authorization, refund-recipient]
  - metric: usage.latency_ms
    direction: METRIC_DIRECTION_LOWER
    max_regression: 100
    max_mean: 2000
```

Choose case IDs from the published version (inbox-derived IDs are
`failure/<case-id>`). `min_case`/`max_case` apply to **every trial** of each named
case; omitting `case_ids` applies them to all cases. A critical failure cannot
be hidden by a passing mean. Missing selected cases make evidence insufficient.
Rules need an explicit direction and at least one bound. `max_regression` is
optional for absolute/critical-only checks.

A positive regression means worse in either direction. `max_regression` checks
the **upper end of the paired confidence interval**, using per-record trial
means and a Student t interval; it does not compare means alone. The confidence
level defaults to 0.95. Wide intervals may block even when the observed mean
looks acceptable. Absolute bounds compare the candidate mean; critical bounds
check individual trials. These intervals are not simultaneous guarantees across
many rules, and do not remove selection bias or external-system nondeterminism.

`usage.cost_usd` and `usage.latency_ms` use each output record's reported target
usage (or recorded source usage when no target runs). They are reserved metric
names. Missing, invalid or incomplete usage cannot be treated as zero. Other
numeric/pass-fail metrics use their evaluator/score names, as in run summaries.

```bash
evalsi release policy-hash -f release-policy.yaml
```

Review the parsed policy and save its returned digest independently. Formatting
and protobuf JSON field-name style do not change the digest. The exact policy
values and rule order do. JSON/YAML with duplicate mapping keys is rejected.

## 4. Execute the candidate and decide once

Create `candidate.yaml` with the same catalog reference and evaluation/checker
settings, pointing at the candidate endpoint. Then:

```bash
evalsi release gate -f release-policy.yaml --policy-sha256 "$REVIEWED_POLICY_SHA256" \
  --server https://evals.example.com --project support --id release-2026-10-20-attempt-1 \
  --baseline "$BASELINE_RUN_ID" --baseline-sha256 "$BASELINE_EVIDENCE_SHA256" \
  --baseline-commit "$BASELINE_COMMIT" --spec candidate.yaml \
  --commit "$CANDIDATE_COMMIT" -o release-attempt-1.json
```

The command validates the policy and baseline pins before candidate execution,
creates one candidate run, waits for its results and saves one immutable
server decision. It records the candidate commit in `agent.commit`; this is
still declared metadata. It does not automatically retry trials or select the
best attempt. Repeating an identical candidate submission uses a deterministic
run idempotency key; an existing decision ID cannot be overwritten. Retrieve
the previous decision or choose an explicit new attempt ID.

| Exit | Decision | Meaning |
|---|---|---|
| 0 | PASS | All required evidence and policy checks passed |
| 3 | BLOCK | A regression, absolute/critical bound or existing candidate gate failed |
| 1 | INSUFFICIENT/error | Evidence is incomplete/incompatible, too few records, or the request/execution failed |

Insufficient evidence takes precedence if another metric also blocks; metric
results retain both findings. Configuration, permission and pin errors fail
without creating a decision. Valid comparisons with insufficient evidence are
saved with that outcome. A failed original candidate gate always prevents a pass.

To compare existing completed runs, use `evalsi release decide` with the same
policy/baseline arguments and explicit `--candidate`, `--candidate-sha256` and
`--candidate-commit` instead of `--spec`/`--commit`. `release evidence RUN_ID`
provides the reference to inspect. The synchronous SDK offers
`inspect_release_policy`, `release_run_evidence`, `create_release_decision`,
`get_release_decision` and `list_release_decisions`; `gate_candidate` orchestrates
the one-candidate workflow.

## 5. Inspect and verify the saved decision

In the browser, **Releases** accepts explicit baseline/candidate run IDs. Inspect
both identities, review the policy JSON and its digest, confirm that review,
then evaluate. Editing identities or policy revokes that confirmation. Reports
link to the approved dataset, run details and changed inbox cases, show critical
failures and paired intervals, and download as portable JSON. The dataset run
starter accepts a declared commit for release comparisons.

```bash
evalsi release get release-2026-10-20-attempt-1 --project support \
  --server https://evals.example.com --sha256 "$REVIEWED_DECISION_SHA256"
evalsi release verify release-attempt-1.json --sha256 "$REVIEWED_DECISION_SHA256"
```

Portable verification is offline and checks content against an independently
trusted decision pin; it returns the decision's 0/3/1 outcome. Reports contain
policy, evidence pins, declared commit/model/evaluator identities, settings
hashes, dataset reviewer, decision actor/time, metrics and changed case IDs/values.
They omit raw record content, prompts, evaluator parameter values and environment
values. A portable report can still contain sensitive identities and metrics.
CLI files are private, atomically published and never replace existing paths.

The API saves decisions under explicit project/ID names. Listing is lexical by
ID, default page size 20, maximum 50; summaries omit metric details and policy
rules. Get the full decision to inspect or verify its digest. Historical
decisions retain their outcome after dataset retirement; they describe their
recorded point in time, not a current deployment approval.

## GitHub Action

The maintained Action supports server release mode in addition to its existing
embedded/advisory mode. Use a reviewed source commit until compatible packages
are published. A customer repository can check out the tool separately:

```yaml
steps:
  - uses: actions/checkout@v4
  - uses: actions/setup-python@v5
    with: {python-version: '3.12'}
  - uses: actions/checkout@v4
    with:
      repository: Abhishek-Rnjn/Evals.si
      ref: REPLACE_WITH_REVIEWED_TOOL_COMMIT
      path: .evalsi-tool
  - run: pip install './.evalsi-tool/python/evalsi[server]' './.evalsi-tool/python/evalsi-harness'
  - uses: ./.evalsi-tool
    env:
      EVALSI_API_KEY: ${{ secrets.EVALSI_API_KEY }}
    with:
      install: 'false'
      spec: .evalsi/candidate.yaml
      server: ${{ vars.EVALSI_SERVER }}
      release-policy: .evalsi/release-policy.yaml
      release-policy-sha256: ${{ vars.EVALSI_RELEASE_POLICY_SHA256 }}
      baseline-run: ${{ vars.EVALSI_BASELINE_RUN }}
      baseline-sha256: ${{ vars.EVALSI_BASELINE_EVIDENCE_SHA256 }}
      baseline-commit: ${{ vars.EVALSI_BASELINE_COMMIT }}
```

The runner must reach the server and use a credential permitted to create/read
runs and release decisions and read the dataset/sources. Configure target
credentials on the server as usual. The Action uses `GITHUB_SHA` as a declared
candidate commit and `ci-GITHUB_RUN_ID-GITHUB_RUN_ATTEMPT` as the default decision
ID. Keep baseline/policy pins in reviewed/protected configuration; automatically
accepting new pins from the candidate defeats the review. The Action does not
select or approve the organization's baseline or policy for you.

The check publishes outcome, coverage, aggregate metrics, generic decision
reasons and identity hashes. Case IDs/content remain in the authorized server
report. Raw artifacts are uploaded only when explicitly requested. Its final
step propagates 0/3/1, so branch protection can enforce the chosen policy.
Published 0.3.0 lacks server release mode; a mismatched installation fails
instead of falling back to an advisory comparison.

## Integrity, access and supported limits

At admission the server records bound evaluator manifests/versions, parameter
hash inputs, effective judge settings, summary/trial/checker settings and server
version. It rechecks that contract before queued, adopted or resumed execution;
changed settings require a new run. Older runs without a captured contract and
imported runs cannot pass release checks. Supported comparisons currently need
record-scoped evaluators with fully matched identities and scored results for
every record/trial/evaluator. Session, span, dataset and comparative scopes are
not qualified by this initial decision workflow.

Source records must match the complete catalog content. Target outputs cannot
change the approved input/reference/context or reserved source identities.
Missing target outputs, errors/skips, wrong metric coordinates, nonfinite data,
differing contracts and unapproved/tuning datasets prevent a pass. Actual remote
model weights, worker/plugin binary bytes, mutable image tags and declared agent
commits are not attested; the report records their available identities/hashes.

Existing viewer roles gain `releases.read`; runners gain `releases.create`.
Decision reads also require `runs.read` on both captured run resources and
`datasets.read` plus captured source `traces.read` boundaries. Current
permissions are rechecked even after run deletion. Retirement/deletion of source
runs does not erase the saved report or its access metadata. There is no release
decision deletion API in this slice; plan retention before using sensitive IDs.

Versioned domains distinguish policy, run evidence and decision hashes. Policy
and decision pins hash deterministic protobuf with a domain plus NUL prefix.
Run evidence additionally length-frames the stored run, source records, ordered
outputs and result coordinates/messages. The store verifies decision and access
envelope integrity on read. These are tamper checks relative to trusted pins,
not signatures or protection against an administrator replacing data and hashes.
The portable report is a saved decision record; independently recomputing its
outcome needs the original run data. It is not the complete signed evidence
package proposed under ENT-08.

Limits are 2 MiB per policy, 32 rules, 100,000 record/trial/evaluator cells and
4 MiB per saved report/envelope. Changed cases are capped at 200 per metric with
an explicit omitted count. Dataset catalog limits still apply. Organization-wide
policy approval/inheritance, signed attestations, expiry/exception governance,
transactional audit export and erasure/backup semantics remain roadmap work.

Run the [synthetic example](../../examples/release-decisions/README.md) and see
[verification](../verification/RELEASE-DECISIONS.md) for concrete controls.

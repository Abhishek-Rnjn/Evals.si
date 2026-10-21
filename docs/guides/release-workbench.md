# Configure and investigate a release check in the browser

The unreleased **Release workbench** at `/ui/#/workbench` configures regression
runs and release policies through forms. Use matching source builds of evalsid,
the Python worker and SDK. Published 0.3.0 does not include this workflow.

## Configure a candidate

Select an approved catalog version, or open a dataset and choose **Configure
agent, evaluators and release checks**. Choose recorded scoring, an
OpenAI-compatible model endpoint, or an HTTP agent. HTTP agents can map request
bodies, answers, trajectories, usage and stop reasons with the existing connector.
This form supports non-streaming POST agents; other connectors and advanced
model/harness settings continue to use their original specs.

Choose evaluators from the server catalog, optionally configure their parameters
and aliases, select a permitted judge, and set trials and token ceilings. Scalar
parameters have controls; structured parameters have an optional JSON editor.
There is no implicit judge model. Credentials are worker-variable names from
project grants, never secret values entered in the browser.

Enter the declared agent commit. **Validate candidate configuration** runs
server admission without executing anything. Review the effective configuration,
dataset pin and outbound destination, check the review box, and start the run.
Changing a configuration field revokes that review. Admission runs again when
creating the run, so changed grants, evaluators or dataset approval can refuse it.

The workbench polls the submitted run's progress and links to its full results.
Repeating an unchanged submission in the same workbench reuses its idempotency
key within the server's existing window. **Begin a new explicit attempt** chooses
a new key and requires validation again. Navigating away creates a new workbench;
recover an existing run by ID rather than assuming a new tab retains that key.

## Select and review evidence

Choose an accepted baseline explicitly; the workbench never picks the latest run.
The run suggestions show completed accessible runs; server evidence checks decide
eligibility. **More runs** and **More approved suites** page their respective lists.
Existing IDs can be entered directly. A run detail also links to the workbench
with that run filled as the proposed baseline; review the choice before use.

**Use baseline evaluation settings** copies settings for the supported simple
run modes. Advanced checker/harness, summary, gate or connector settings require
the original spec and are refused rather than silently discarded. The declared
candidate commit must still be entered deliberately.

Inspect both explicit run identities before configuring the release policy.
The workbench shows their dataset, commit, evidence and evaluation-contract
identities. Declared commits and endpoint/model names are not execution
attestations. An HTTP agent's deployment must actually serve the intended code.

## Configure the release policy

Choose measured metrics and add rules for paired regression, candidate means or
critical per-trial bounds. Evaluator catalog direction metadata supplies the
initial direction: for example, `tool-errors` is lower-is-better. Inspect and
review the direction explicitly. Critical case IDs are one per line; empty IDs
apply the bounds to all cases. Choose a minimum independent-record count and a
confidence level. Repeated trials do not increase the independent-record count.

Usage metrics are offered when available in the inspected evidence or bounded
record sample; offering one does not prove complete coverage. The release engine
still requires measured usage on every required record/trial. Missing usage,
errors, skips or insufficient coverage cannot become passing zeros.

**Inspect configured release policy** validates the existing policy protobuf and
shows its complete content and SHA-256. Review the policy and both run identities,
then save a decision under an explicit immutable ID. Editing policy fields or
either identity invalidates inspection and consent. The existing release engine
remains responsible for PASS, BLOCK and INSUFFICIENT; a decision does not deploy
software. See [release decisions](release-decisions.md) for the statistical contract.

## Investigate a changed case

Expand a release metric's changed/critical cases and select **Inspect paired
case**. The URL carries both run IDs, the record ID and trial. It can be shared
with a teammate who has access to both source runs. Each lane shows:

- The exact trial's input, output, reference, execution errors and score explanations.
- Stop reason and available usage, plus checker verdict, tests and errors.
- Recorded model/tool/user/handoff events, arguments, results and timing.

Expand events to inspect their full recorded content. Trial selectors can inspect
different trials independently; equal trial numbers do not imply equal random
seeds. Event differences compare type, name, input, output and error with approximate
ordinal alignment; timing, usage and span IDs do not mark behavioral changes. An inserted event
shifts subsequent positions, and a difference does not prove a cause. This view
reads evidence and makes no model or tool calls.

Run result tables also link directly to the case inspector and page results.
Evidence is associated by run, record and trial, so repeated record IDs across
trials do not overwrite one another. A case beyond the first result page remains
directly reachable. Archived/deleted evidence is unavailable; the saved release
decision is unchanged. Denial on either run prevents rendering either private lane.

The API is `RunService.GetRecordEvidence`, or
`GET /v1alpha1/runs/{run_id}/record?record_id=ID&trial=0`, with `runs.read` and
the same resource rules as run-result reads. Older clients retain the existing
`ListRunResults.records`; newer clients use its additive `records_by_trial`.
The legacy snapshot schema requires a streaming scan to locate a record ID;
only that record's output and evaluator results are then queried. There is no
new index, schema migration or claim of constant-time lookup for very large suites.

## Export the check to CI

After policy review, download the candidate spec, release policy, CLI command and
Action step. The spec is JSON, accepted by the YAML/JSON run loader. Exports retain
the approved dataset digest, explicit baseline evidence and reviewed policy pin.
They can contain private configuration; handle them under your project's rules.

The CLI command executes a **new full-suite attempt** using explicit
`CANDIDATE_COMMIT` and `NEW_ATTEMPT_ID` values. Provision the intended candidate
endpoint before running it. The Action step requires replacing its source-commit
placeholder with a reviewed matching commit and provisioning matching packages
(`install: false`); its server credential comes from `secrets.EVALSI_API_KEY`.
Credential values are not downloaded. Neither export claims deterministic replay
of a remote endpoint or turns a single-case inspection into release evidence.

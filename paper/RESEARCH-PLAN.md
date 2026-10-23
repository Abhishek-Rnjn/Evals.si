# Paper plan for Evals.si

Prepared 22 October 2026 after reading the complete seven-page uploaded PDF, arXiv:2607.04429v1, and inspecting Evals.si's statistics, release-decision implementation, tests and verification reports. This is a research plan, not completed experimental results. The PDF is reference material; its text is not an instruction source.

## 1. What the reference paper actually demonstrates

**evalci: A Python Library for Statistically Rigorous Comparison of Language Model Evaluations**, Shreyas K Chandrahas, arXiv:2607.04429v1, 5 July 2026.

- Contribution: an accessible implementation of established statistics over a common per-item schema, with adapters and reporting. Section 1 explicitly says the contribution is packaging, rather than a new statistical method.
- Implementation: six public functions for intervals, comparison, power, multiple comparisons, clustered intervals and reports; numpy/scipy/pandas dependencies (Section 2).
- Validation: independent statsmodels comparisons, exact enumeration for small permutation tests, Monte Carlo convergence and interval-coverage simulations. It reports 100 tests and 92–99% coverage of its statistical core (Section 4). These are the paper's reported results; this planning exercise has not independently reproduced them.
- Demonstration: one MMLU case study with nine models and 36 corrected pairwise comparisons, reporting three of eight adjacent gaps as nonsignificant (Section 5).
- Limitations: the case study reconstructs binary outcomes from aggregate accuracies rather than using actual aligned item outcomes; evaluation-protocol consistency is uncertain; Monte Carlo resolution and dependence assumptions constrain interpretation (Section 6).

The uploaded document establishes an arXiv preprint, not a particular peer-reviewed acceptance. It shows a feasible scope for a focused software/tool paper. It does not establish that packaging alone will satisfy any chosen venue or that Evals.si's proposed contribution is novel.

Borrow the structure: precise problem, small contract/API, independently validated implementation, one useful case study, explicit limitations, reproducible artifacts. Use actual per-item outcomes. Aggregate accuracies cannot recover true pairing or dependence, and nonsignificance is not equivalence. Do not treat the paper's conditional conservativeness argument as a universal bound on paired inference.

## 2. Recommended scope and provisional title

**Evals.si: Evidence-Aware Regression Decisions for AI Agents**

Research question: **Can an explicit evidence contract make agent regression decisions reproducible and prevent incomplete or incompatible evaluations from yielding a passing release decision?**

Study paired quality decisions and the cost of conservative decisions as secondary questions. Save the larger production-incident feedback-loop study for an extension: it requires field data and more evidence than the short tool paper needs.

Three proposed contributions, subject to related-work review:

1. A documented decision contract connecting aligned record/trial outcomes, evaluation settings, critical-case requirements and evidence identities to PASS, BLOCK and INSUFFICIENT outcomes.
2. An open implementation spanning local Python, the server and CLI/CI reports, with demonstrated agreement across supported paths.
3. Independent statistical validation, controlled evidence-failure experiments and a case study using actual agent outputs.

Keep the paper centered on this path. Treat Kubernetes, sandbox implementation, RL training, every adapter and the complete browser product as context or optional appendices. Do not claim a new statistical method, universal agent safety, execution attestation or production effectiveness without evidence.

## 3. What exists and what needs research

| Component | Existing evidence | Work needed for the paper |
| --- | --- | --- |
| Basic statistics | Wilson, Student t, clustered intervals and bootstrap; table checks and shared Go/Python vectors | Broader independent-reference tests, coverage and decision-error simulations |
| Release decisions | Per-record trial means, paired Student t regression interval, explicit margins, completeness checks, critical cases and pins | Precise specification, external behavioral oracle, false-approval/denial analysis |
| Regression workflows | Synthetic positive/negative controls and integration/browser tests | Actual agent executions and independently checked outcomes |
| Judge calibration | Synthetic labels and scripted HTTP judges | Optional real human labels; required only if claiming judge quality or calibration efficacy |
| Performance | 112 bounded local SDK overhead trials in an older revision | Fresh, comparable measurements of the paper's decision workflow; existing numbers are background only |
| Related work | A preliminary study on the paper/literature-study branch | Read primary sources, verify versions and replace unsupported novelty assertions |
| Reproducibility | Version pins, reports, CLI and tests | Frozen research release, public experiment artifacts and independent rerun |

The inspected release gate averages repeated trials within each record, then applies a Student t interval to record-level paired differences. It does not automatically account for dependence between different records in one scenario family. Wide intervals can produce BLOCK even without an observed material regression. Current documentation disclaims simultaneous guarantees across multiple metric rules. Describe these behaviors accurately; do not silently give the paper a stronger algorithm than the released implementation.

## 4. Six-week core plan, assuming one primary contributor

The schedule is an estimate for a focused tool paper. A field deployment, broad human study or new statistical method requires additional time. Choose a model budget after the pilot; no credentials or paid runs are required merely to write the protocol or run most validation.

### Week 1 — Define the claim and audit related work

Tasks:
- Read evalci and primary material for Inspect AI, lm-evaluation-harness, HELM, relevant statistical evaluation work, and evaluation/release workflows in MLflow, LangSmith, Phoenix and DeepEval. Treat this as a candidate reading list, not a verified list of equivalent capabilities or current publications.
- Make a claim-to-evidence table: proposed claim, closest prior capability, exact difference, experiment needed, limitation.
- Freeze the experimental software revision and environment. Start from v0.3.1 only if that is the intended object of study; any fixes create a new explicitly pinned research revision. Do not mix main, release and modified code silently.
- Specify record identity, trial alignment, independence unit, metric direction, regression margin, error/skip handling, critical cases, policy identity and the meaning of every decision state.
- Distinguish a release being denied from statistical evidence proving the candidate is worse.

Deliverables: a one-page contribution statement; a checked related-work matrix; a decision-contract specification; a frozen experiment protocol.

Decision gate: if prior work already supplies the same contract and validation, narrow to a demonstrated usability/integration contribution or find a different measured problem. Do not invent a novelty claim.

### Week 2 — Independently validate the statistical and decision core

Tasks:
- Compare Wilson intervals against statsmodels on boundary and interior counts, several sample sizes and confidence levels.
- Compare Student t quantiles/intervals against SciPy. Independently validate cluster-robust calculations using a documented matching estimator and weighting scheme.
- Validate bootstrap behavior against an independent implementation with the same estimand and resampling unit. Equal numeric output from different bootstrap algorithms is not the correctness criterion; check distributional properties and coverage.
- For very small cases, enumerate outcomes or resamples where feasible to check decision calculations and alignment behavior.
- Check invalid/nonfinite inputs, no observations, all-equal scores, one independent unit, duplicated IDs, swapped trial order, and positive/negative metric directions.
- Use shared Go/Python vectors for consistency, while recognizing that both implementations can share the same mistake. Freeze externally generated reference values separately.
- Run simulations where the true mean difference is known: no change, beneficial change, differences just inside/outside the declared margin, rare failures, skewed metrics, within-item repeats and between-record correlation.
- A reasonable starting target is 2,000 independent simulation replicates per selected scenario; at 95% coverage this gives roughly one percentage point of Monte Carlo uncertainty for a 95% interval on the coverage estimate. Select and freeze a manageable scenario grid first.

Report coverage, harmful-candidate acceptance probability, nonregressing-candidate denial probability, insufficient-evidence rate and their uncertainty. Investigate undercoverage; document assumptions or repair the method before claiming reliable error control.

Deliverables: independent validation suite; simulation data; first coverage/error plots; a list of justified assumptions and discovered limitations.

### Week 3 — Pilot actual agent tasks and freeze the case study

Start with two applications: support/refund tool use and document-grounded retrieval/tool-use tasks. They connect naturally to existing examples and allow objective business-state or answer checks. Add coding tasks only if isolation and checker execution are available and reproducible.

Tasks:
- Pilot approximately 40–60 independent task scenarios across the two applications, with 2–3 trials per configuration where stochasticity matters. These are pilot targets, not evidence of adequate final power.
- Define one baseline and several controlled candidate changes: a plausible improvement, a harmless refactor, a tool/parameter defect and a critical-policy violation. Verify that a planted defect actually changes outcomes; intent alone does not establish a regression.
- Keep development/tuning scenarios separate from the final held-out set, grouping related variants into the same split. Deduplicate semantic variants as well as checking exact matches where possible.
- Store actual inputs, outputs, trajectories, checker results, configuration identity, model identifier, time and usage. Redact private data before artifact publication.
- Prefer objective final-state checks. Independently inspect their correctness. For subjective checks, collect blinded human ratings with an adjudication protocol and retain disagreement.
- Use pilot variance, discordant-pair frequency and dependence to choose the final number of independent scenarios for a declared effect size and power target. Rare failures or small effects may require hundreds or thousands of cases. Additional trials do not create additional independent tasks.

Budget estimate: independent scenarios × configurations × trials × mean cost per complete agent run, including tool/model turns. Set a hard spending ceiling and stopping rule. A recorded-output experiment can make most contract tests inexpensive, but does not replace the actual-agent case study.

Deliverables: pilot dataset; outcome-checking protocol; power/sample-size calculation; cost estimate; frozen final task list and candidate definitions.

### Week 4 — Run the main comparison and evidence-failure study

Run methods over identical records, scorers and frozen candidate outputs wherever possible. Separate statistical-method effects from evidence-contract effects.

Comparators:
1. A clearly specified mean-threshold release policy as a simple baseline.
2. A proper paired statistical comparison on valid aligned data, using evalci where applicable or an independently specified SciPy/statsmodels implementation. evalci's inference is not itself a release-provenance system; do not characterize absent provenance features as statistical bugs.
3. Evals.si's published decision rule and evidence checks.
4. An independently implemented reference policy with equivalent checks, where feasible. If it matches Evals.si, report the agreement; implementation convenience can still be a tool contribution.

Controlled evidence perturbations:
- Drop or duplicate result rows; remove an entire trial; change trial identity.
- Turn scorer failures into missing results, particularly on harder cases.
- Change dataset, evaluator/checker configuration or the baseline identity.
- Introduce a critical failure that an improved overall average can hide.
- Alter a saved report or its trusted expected digest.

Use a clean copy of the same run for each perturbation. Record the expected evidence-validity outcome before running methods. An appropriate INSUFFICIENT response is different from proving that an agent is bad. A file hash establishes integrity relative to a trusted identity, not which remote model weights or source bytes actually executed.

Ablations: paired versus unpaired analysis, per-record means versus incorrectly pooled trials, completeness checking enabled/disabled, contract verification enabled/disabled and critical-case checking enabled/disabled. Keep the statistical method fixed when isolating contract effects.

Primary measures: invalid-evidence acceptance rate; acceptance of materially harmful candidates under independently defined truth; denial of acceptable candidates; insufficient-evidence rate; decision agreement with the reference implementation. Also report task coverage, total latency/cost and decision overhead.

For real tasks, independently adjudicated per-case outcomes or a larger independent reference set provide evidence about quality; finite case-study data do not reveal population truth without uncertainty. Do not label correctness using the same gate being evaluated.

Deliverables: immutable raw outputs; decision tables; perturbation matrix; main case-study narrative; cost/performance measurements.

### Week 5 — Analyze, stress-test the claims and package artifacts

- Analyze paired outcomes at the declared independent task/scenario unit. Do not treat related task variants, shared baselines, candidate mutations or repeated decodes as independent observations merely to narrow intervals.
- Use a prespecified primary metric/hypothesis for the first paper. For additional formal comparisons, define the comparison family and appropriate multiplicity treatment. Label exploratory analyses accordingly; Evals.si's existing per-rule intervals do not create a joint confidence guarantee.
- Specify whether intervals are one- or two-sided. The upper endpoint of a two-sided 95% interval is not the same cutoff as a one-sided 95% bound.
- Report every prespecified experiment, failed run, exclusion, missing artifact and limitation. Fix infrastructure failures without cherry-picking favorable agent trials.
- Measure server/local agreement and the cost of the decision workflow on both cheap recorded scores and at least one realistic workload. Keep setup, model execution, persistence and statistical processing times separate.
- Ask someone other than the author to install from the pinned artifact and reproduce a main figure/table. Record the commands, environment and discrepancies.

Deliverables: final figures/tables; claim-to-result audit; reproducibility checklist; independent rerun notes; research tag and archived artifact plan.

### Week 6 — Write and review a focused manuscript

Suggested 6–8 pages of main text, adjusted to the selected venue's current rules:
1. Introduction: a concrete misleading release decision and the precise problem.
2. Related work and scope: what existing inference and evaluation tools already do.
3. Evidence/decision contract: definitions, supported assumptions and decision semantics.
4. Implementation: local/server/CLI paths, identity checks and key design choices.
5. Validation and case study: independent references, simulations, perturbations and real outcomes.
6. Limitations, availability and conclusion.

Draft the methods before the main experiment; write numerical abstract claims only after results are frozen. Target a software/tool paper or relevant workshop first, subject to the venue's scope and criteria. An arXiv preprint is a dissemination step and should not be described as peer-reviewed acceptance.

Deliverables: manuscript; code/data availability statement; machine-readable artifact manifest; submission checklist.

## 5. Planned figures and tables

1. A small architecture/contract diagram: runs → validated evidence → paired analysis/policy → decision/report.
2. A behavioral matrix for clean, missing, incompatible and tampered evidence.
3. Coverage and harmful-acceptance plots versus independent sample size, with Monte Carlo uncertainty.
4. A real-task candidate table showing quality deltas, uncertainty, critical failures, decisions, incomplete cases and cost.
5. A performance/reproduction table separating model time from evaluation/decision overhead.

Limit the central story to three contributions and these directly relevant exhibits. A broad product feature matrix and old exact-match speed numbers cannot substitute for these results.

## 6. Ready-to-submit checklist

- The closest prior tools and the exact contribution have been checked against primary sources.
- Every statistical statement has a specified estimand, independent unit, assumptions, method and confidence convention.
- Statistical routines agree with independent references where exact agreement is appropriate; coverage and decision-error behavior are measured separately.
- Main experiments use actual per-item outputs and fixed split/configuration identities.
- Missing results, evaluator failures and repeated trials are reported explicitly.
- The real-agent study has outcome evidence independent of the gate being evaluated.
- Negative findings and limits appear in the manuscript; no broad safety, simultaneous-confidence or production-efficacy claim exceeds the data.
- Raw permitted data, scripts, versions, seeds, exclusions and reproduction instructions are released, with licensing and redaction checked.
- An independent reader has reproduced at least one main result.
- No invented results appear in the abstract, figures or submission materials.

## 7. First five concrete actions

1. Write the one-page contribution and decision-contract specification.
2. Build and verify the related-work matrix, including evalci as complementary statistical infrastructure.
3. Add independent-reference statistical validation and run the first coverage simulations in a separate research/coding task.
4. Pilot the two selected applications, then calculate sample size and spending from measured pilot behavior.
5. Freeze the protocol before collecting and judging the final held-out results.

Optional extension after the core paper: collect genuine production incidents and test whether reviewed incident-derived cases improve held-out regression detection. This needs real operational data, appropriate permission/redaction, prospective splits and stronger deployment evidence; it is not a prerequisite for the focused tool-paper plan above.

# Work required before the 0.4 release

Status: open checklist, 21 October 2026. All owners are unassigned and all
evidence is pending. This document records required work; it does not establish
that product readiness, customer adoption or revenue has been achieved.

Stable 0.4 is gated on **product readiness** (R04-01 to R04-05), which a
maintainer can build and verify without outside teams. **Customer validation**
(CV-01 to CV-05: pilots, retention, willingness to pay) starts alongside the
release candidate but gates calling the project beta, not 0.4. Version numbers
should not wait on a business milestone while security and reliability fixes sit
on `main`.

The latest published release at preparation is v0.3.0. Guided onboarding, dataset
workflows, enforced release decisions, the browser workbench and evaluator
calibration ([guide](guides/judge-calibration.md)) are merged and unreleased. A
feature's presence in the repository is not evidence that a customer can install
and adopt it.

This checklist narrows the next release's scope within the
[product roadmap](PRODUCT-ROADMAP.md) and [strategy](PRODUCT-STRATEGY.md); the
[technical release gates](RELEASING.md) also apply. The roadmap's item IDs are
the long-term tracker; the crosswalk column below says which item each R04 row
advances, so there is one set of definitions, not two.

## Product focus and first customer

The recurring decision is the strategy's: **can we safely ship this agent
change?** Use this promise for it in the README, website, demonstrations and
outreach:

> Know whether your prompt, model or agent change actually improves performance,
> using your own examples and evaluators you can inspect.

Datasets, trustworthy scoring, comparisons and explaining failures are the core
product. CI gates consume that evidence. The support/refund suite demonstrates
state-based evaluation of tool actions; it is one example of the broader product.

The initial customer hypothesis is a small team already shipping a Python-based
RAG or support agent, changing prompts or models regularly and manually reviewing
outputs. A first evaluation must not require a Kubernetes installation or a
rewrite around Evals.si's harness. The in-cluster service in the
[PRD](PRD.md) remains the team and enterprise deployment path; this plan changes
where a new user starts, not what is built.

## Required before stable 0.4: product readiness

Each completion record names an owner, implementation PRs, tested commit/artifact
versions, evidence and limits. Evidence comes from clean-environment runs,
maintainer-run walkthroughs, an unfamiliar reviewer who is not a maintainer, and
public or synthetic labelled data identified as such. Real customer data is not
required for 0.4.

| ID | Advances | Work and existing foundation | Acceptance evidence | Status / owner |
|---|---|---|---|---|
| R04-01 | PROD-01, GTM-02 | Dependable installed experience, using existing packaging, `doctor`, `init` and release automation, and a release candidate that does not disturb stable users. | Fresh environments install candidate artifacts with matching SDK, harness and server versions. Every advertised quickstart and Action example works without a source checkout. Exercise recorded scoring on supported developer platforms and the advertised Linux execution paths, including documented macOS/Windows-to-Linux paths. Record exact versions, available sandbox capabilities and unsupported paths. A prerelease tag (for example `v0.4.0-rc.1`) is marked a GitHub prerelease, leaves the `latest` image and chart tags alone and builds Python distributions with PEP 440 versions, shown by a release dry run. | Open / unassigned |
| R04-02 | PROD-01 | Bring-your-own-application onboarding through recorded outputs, a Python callable or an HTTP endpoint. | A developer who did not write Evals.si evaluates their own cases (or a documented public sample application) from the artifacts, finds a useful result and repeats the run without help. Measure elapsed time against the under-15-minute target; record assistance and blockers. Document how recorded evaluation progresses to live execution. | Open / unassigned |
| R04-03 | PROD-02, PROD-05 | Understandable comparison reports, extending current reports and the browser workbench. | A second reader who is not a maintainer explains what improved, what regressed and which examples account for the change. Reports identify baseline/candidate and dataset/evaluator versions, paired differences and uncertainty, applicable task/business checks, available cost/latency evidence, denominators, errors/skips and incomplete evidence. Reproduction commands and case links work with the intended access controls. | Open / unassigned |
| R04-04 | PROD-06 | Evaluator validation workflow, building on annotations and the merged calibration commands. | The documented workflow runs end to end on a labelled set whose provenance is stated (public, or labelled by the maintainers). Tuning and held-out cases are separate, disagreements are retained, false positives/negatives are reported by slice with uncertainty, and a fixed-output control tells judge drift apart from application improvement. Scripted transport tests and synthetic labels stay separately identified; customer-labelled validation is CV-02. | Open / unassigned |
| R04-05 | PROD-07 | One maintained end-to-end integration. The maintainers choose it from LangGraph, OpenAI Agents and the existing trace-store paths and record the reason; pilot demand can add more later. | Record supported framework/trace-store versions and exercise a real application, including a model-driven path where claimed. Show a passing control and a deliberately regressed control, inspect the affected case and reproduce the result through the GitHub Action. Preserve result/trace semantics and diagnose integration failures. | Open / unassigned |

Do not infer evaluator quality from a passing transport test, statistical
precision from a small starter set, or live-model effectiveness from the
rule-based refund agents. Use deterministic state/tool checks where they express
the expected outcome; review judge-based scoring against human judgements.

## Interim releases

Stable 0.4 may be months away. Until then:

- Cut release candidates (`v0.4.0-rc.N`) once R04-01's prerelease handling
  passes a dry run, the technical gates pass and artifact-based installation is
  verified. Candidates can support pilots; they are not stable releases and must
  say so in their notes.
- Ship security and reliability fixes that cannot wait as `0.3.x` patches from a
  `release-0.3` branch, as [RELEASING](RELEASING.md) describes, or as a new
  candidate when the fix is already on `main`. Record which fixes need a backport
  (for example dependency advisories) when they merge.
- Set a date to review this gate. If R04-01 to R04-05 are not met by then, decide
  openly whether to narrow them or cut 0.4 with the unmet rows listed as known
  limits. Propose 15 December 2026; the release owner decides.

## Customer validation: beta and GA criteria, not 0.4

These rows start with the first release candidate and use real teams. They are
the exit criteria for calling Evals.si beta (the API-stability and compatibility
criteria for beta are separate and not defined here). Numbers are planning
targets, not forecasts or observed adoption; record actual results and leave an
unmet criterion open. A missed target is not a reason to hold a security patch or
a stable 0.4 whose product rows are met.

| ID | Advances | Work | Acceptance evidence | Status / owner |
|---|---|---|---|---|
| CV-01 | GTM-01 | Recruit and qualify design partners. | Align the product entry points with the promise above. Build a named prospect list of roughly 30 relevant teams, interview them about a recent evaluation decision and recruit an initial target of five qualified pilots. Record current workflow, developer owner, integration/deployment needs, approved data scope and the next real change to evaluate. | Open / unassigned |
| CV-02 | PROD-04, PROD-06 | Onboard pilots on their own applications, validate evaluators on their labels and establish repeat use. | Start each pilot with 20–50 approved representative examples, a human-reviewed scoring approach with a recorded rubric version, an explicit baseline and a proposed prompt/model/agent change. Keep tuning and held-out cases separate. Inspect disagreements together, run in their repository and add recurring execution. Seek a second real use within a week and have a second teammate interpret the report. Record and fix the blockers to independent use. | Open / unassigned |
| CV-03 | GTM-02 | Publish demonstrations and ecosystem-specific material. | Two practical tutorials for the integration chosen in R04-05 (or a pilot-demanded one), a short failure-to-evaluation demo, and at least one permissioned pilot example explaining the failure, the evidence and the decision. Include passing/regressed controls and reproducible commands. | Open / unassigned |
| CV-04 | GTM-03 | Validate a paid continuation. | Present a fixed-scope continuation for the need observed in the pilot: evaluation setup/support, supported private deployment or collaboration. Record scope, proposed price, delivery/support cost and buyer response. The gate is at least one written commitment or stronger; stated interest alone does not count. Keep stated interest, written commitment, signed agreement and collected revenue distinct. | Open / unassigned |
| CV-05 | GTM-01 | Adoption scorecard and retention. | Track the definitions below. Reach three external teams evaluating real application changes weekly for four consecutive weeks, with CV-04 met. Link the dated evidence and document confirmed false blocks, assistance required and remaining friction. | Open / unassigned |

With one maintainer, run CV-01 and CV-02 with a small number of pilots first and
let CV-03 to CV-05 follow what they show. Twenty to fifty cases is an onboarding
starting point, not a reliability or statistical-significance guarantee. Grow the
dataset according to the actual decision, relevant failure slices and measured
uncertainty, and do not let pilot examples silently become the held-out set after
tuning on them.

Customer records stay within the agreed handling and access boundaries. Public
tutorials and case studies need permission for the material they disclose;
synthetic illustrations must be labelled. Keep raw customer data and prospect
contact details outside the repository, which is public. This plan is not
published on the website, but it is visible in the repository, so the outreach
text below is a template and holds no prospect information.

## Acquisition and pilot playbook

Begin with the maintainer's network and communities around the selected agent
framework. Ask how a team evaluated its last prompt/model change, what failed
after deployment, what examples it saved and who decides whether to ship. Qualify
the recurring need before choosing an integration or offering a deployment.

An outreach starting point:

> How do you check whether a prompt or model change improves your agent today?
> I'm building Evals.si and looking for a few teams to try it on their existing
> examples. We'll help set up a repeatable comparison in your repo, without
> changing your agent framework.

Offer a concrete pilot:

> Bring 20–50 representative examples and one proposed prompt/model change.
> We'll help build a reviewed evaluation set, compare the versions and make
> the evaluation repeatable in your repository.

Run each pilot in this order:

1. Record the current release/evaluation process, owner, next change, handling
   boundaries and a measurable success criterion. Agree on supported deployment
   and who pays any execution/model costs.
2. Evaluate their existing application and examples. Record setup time and every
   intervention. Agree on expected outcomes, inspect scoring disagreements and
   freeze the baseline and dataset revisions.
3. Compare the proposed change, inspect affected examples and record the team's
   actual decision. Connect recurring execution and the Action where appropriate.
4. Within a week, seek another real evaluation. Ask a second teammate to interpret
   the evidence without the maintainer explaining it. Prioritize the observed
   blockers to repeated independent use.
5. Review weekly use for four weeks, ask for a paid continuation and request
   permission for a concrete case study. Use the result to choose subsequent work.

Four-week retention runs from each team's actual first use. This is a sequence,
not a promise about dates. Wider launch promotion should point to a working
adoption path and demonstrated outcomes.

## Adoption scorecard

The primary measure is weekly external teams evaluating real application changes
with an identified baseline, dataset and scoring configuration. Record the actual
decision and evidence for each qualifying team-week; downloads, synthetic demo
runs and repeated CI retries do not establish another adopted team or change.

| Measure | Definition and evidence to retain |
|---|---|
| First useful result | Time from beginning the documented setup to a result the developer can explain on their own examples; record environment, help and blockers. Target under 15 minutes. |
| First repository evaluation | First repeatable run on the team's application repository; record candidate/baseline/config identities and the command or Action run. |
| Second-week usage | Whether an activated team evaluates a subsequent real change in the following week; count teams rather than individual runs. |
| Four-week retention | Dated qualifying team-weeks for four consecutive weeks after activation; report the retained and activated team counts, including dropouts. Initial gate: three retained external teams. |
| Human-confirmed false blocks | Decisions the customer reviewed and determined were incorrectly blocked; retain the disposition and the total reviewed blocks. Separate evaluator mistakes from configuration/execution failures. |
| Investigation effort | Time to explain the changed result and the evidence used; record unresolved investigations. |
| Cost per evaluation | Available execution/model cost and its coverage; identify missing costs instead of treating them as zero. |
| Maintainer assistance | Number and nature of interventions per onboarding and subsequent evaluation; distinguish guided from independent use. |
| Willingness to pay | Buyer, offered scope/price, response and commitment stage; keep actual revenue separate. Initial gate: at least one written commitment or stronger. |

Three of five recruited pilots retained is a 60% bar, so one dropout among five
fails it; recruit beyond five if the bar matters. A manually maintained private
scorecard is sufficient. Use approved or opt-in usage collection where available;
a new analytics service is not a prerequisite. Stars and downloads are
distribution signals, not completion evidence for retention.

## Packaging and scope decisions

Keep the open-source local/CI path dependable. Test paid continuation against the
customer need and the cost of delivering it. Managed hosting and billing should
be built only after validating that offer and its operating responsibilities.

Maintain the existing capabilities while prioritizing this checklist. Additional
frameworks, training features and enterprise administration follow specific
customer requirements. The broader [roadmap](PRODUCT-ROADMAP.md) retains those
items and their evidence.

## Stable-release decision

Before stable 0.4, the release owner completes this record:

- [ ] R04-01 through R04-05: acceptance evidence reviewed against the final
  candidate artifacts, supported versions and documented limits.
- [ ] Implementation owners, PRs, commits/artifacts and evidence references filled
  in for every R04 row; unmet targets and unresolved defects remain visible.
- [ ] Existing technical checks, security scans, upgrade/restore checks applicable
  to the supported deployment, release notes and artifact verification completed
  under the release procedure.
- [ ] Final readiness review records the release owner, date, exact commit and
  disposition of every criterion before publication is authorized.

CV-01 to CV-05 are recorded in the same review, with their current status, but do
not block stable 0.4. The decision remains blocked while an R04 criterion is open.
Changing a criterion requires an explicit reviewed update to this plan and its
reason; a missed target cannot be silently relabelled as completed. Merging this
planning document records the work and does not complete any gate.

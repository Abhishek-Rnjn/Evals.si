# Work required before the 0.4 release

Status: open pre-0.4 stable-release checklist, 21 October 2026. All owners are
unassigned and all evidence is pending. This document records required work;
it does not establish that product readiness, customer adoption or revenue has
been achieved.

The next release must make one complete evaluation workflow useful to real
teams: bring their existing application and examples, establish trusted scoring,
compare a real change, explain the results and repeat the evaluation in their
repository. Packaging, integration, usability and customer evidence take priority
over another major subsystem.

The latest published release at preparation is v0.3.0. Guided onboarding, dataset
workflows, enforced release decisions and the browser workbench have unreleased
implementation slices. Evaluator calibration is tracked in
[PR #126](https://github.com/Abhishek-Rnjn/Evals.si/pull/126). Review and merge
required implementation PRs, then validate the actual release artifacts. A
feature's presence in the repository is not evidence that a customer can install
and adopt it.

This checklist narrows the next release's scope within the
[product roadmap](PRODUCT-ROADMAP.md) and [strategy](PRODUCT-STRATEGY.md).
The existing [technical release gates](RELEASING.md) also apply.

## Product focus and first customer

Use this promise consistently in the README, website, demonstrations and outreach:

> Know whether your prompt, model or agent change actually improves performance,
> using your own examples and evaluators you can inspect.

Datasets, trustworthy scoring, comparisons and explaining failures are the core
product. CI gates consume that evidence. The support/refund suite demonstrates
state-based evaluation of tool actions; it is one example of the broader product.

The initial customer hypothesis is a small team already shipping a Python-based
RAG or support agent, changing prompts or models regularly and manually reviewing
outputs. Qualify teams with representative examples, an application developer who
can integrate the tool, and a current evaluation decision. Preserve their agent
framework and trace store. An initial evaluation must not require a Kubernetes
installation or a rewrite around Evals.si's harness.

Choose the first supported integration from pilot demand. Existing LangGraph,
OpenAI Agents and trace-store paths are candidates, not commitments to expand
every integration before release.

## Required product work

Every row below is required before stable 0.4. Each completion record must name
an owner, implementation PRs, tested commit/artifact versions, evidence and limits.

| ID | Work and existing foundation | Acceptance evidence | Status / owner |
|---|---|---|---|
| R04-01 | Dependable installed experience, using existing packaging, `doctor`, `init` and release automation. | Fresh environments install candidate artifacts with matching SDK, harness and server versions. Every advertised quickstart and Action example works without a source checkout. Exercise recorded scoring on supported developer platforms and the advertised Linux execution paths, including documented macOS/Windows-to-Linux paths. Record exact versions, available sandbox capabilities and unsupported paths. | Open / unassigned |
| R04-02 | Bring-your-own-application onboarding through recorded outputs, a Python callable or an HTTP endpoint. | A new developer evaluates their own representative cases, finds a useful result and can repeat the run without maintainer intervention. Measure elapsed time against the under-15-minute target; record assistance and blockers. Document how recorded evaluation progresses to live execution when needed. | Open / unassigned |
| R04-03 | Understandable comparison reports, extending current reports and the browser workbench. | A second teammate explains what improved, what regressed and which examples account for the change. Reports identify baseline/candidate and dataset/evaluator versions, paired differences and uncertainty, applicable task/business checks, available cost/latency evidence, denominators, errors/skips and incomplete evidence. Reproduction commands and case links work with the intended access controls. A changed judge remains distinct from an improved application. | Open / unassigned |
| R04-04 | Customer-backed evaluator validation, building on annotations and calibration. | Use approved representative customer examples with human-reviewed labels and a recorded rubric/version. Keep tuning and held-out cases separate, retain disagreements, inspect false positives/negatives by relevant slice and report coverage/uncertainty. A fixed-output control distinguishes judge drift from application improvement. Scripted transport tests and synthetic labels remain separately identified. | Open / unassigned |
| R04-05 | One maintained end-to-end integration selected from actual pilot usage. | Record supported framework/trace-store versions and exercise a real application, including a model-driven path where claimed. Show a passing control and a deliberately regressed control, inspect the affected case and reproduce the result through the GitHub Action. Preserve result/trace semantics and diagnose integration failures. | Open / unassigned |

Do not infer evaluator quality from a passing transport test, statistical
precision from a small starter set, or live-model effectiveness from the
rule-based refund agents. Use deterministic state/tool checks where they express
the expected outcome; review judge-based scoring against human judgements.

## Required adoption and commercial work

These rows are pre-0.4 stable-release work alongside the product checklist.
Numbers are explicit planning targets, not forecasts or observed adoption.
Record actual results and leave an unmet criterion open.

| ID | Work | Acceptance evidence | Status / owner |
|---|---|---|---|
| R04-06 | Focus positioning, recruit and qualify design partners. | Align the product entry points with the promise above. Build a named prospect list targeting roughly 30 relevant teams, interview them about a recent evaluation decision and recruit an initial target of five qualified pilots. Record current workflow, developer owner, integration/deployment needs, approved data scope and the next real change to evaluate. | Open / unassigned |
| R04-07 | Onboard pilots on their own applications and establish repeat use. | Start each pilot with 20–50 approved representative examples, a reviewed scoring approach, an explicit baseline and a proposed prompt/model/agent change. Inspect disagreements together, run in their repository and add recurring execution. Seek a second real use within a week and have a second teammate interpret the report. Record and fix the blockers to independent use. | Open / unassigned |
| R04-08 | Publish useful demonstrations and ecosystem-specific material. | Publish two practical tutorials for the selected integration, a short failure-to-evaluation demo, and at least one permissioned pilot example explaining the failure, the evaluation evidence and the resulting decision. Include working passing/regressed controls and reproducible commands. Participate helpfully in the selected ecosystem and link these materials from onboarding. | Open / unassigned |
| R04-09 | Validate a paid continuation. | Present a fixed-scope continuation for the need observed in the pilot: evaluation setup/support, supported private deployment or collaboration. Record the scope, proposed price, delivery/support cost and buyer response. Obtain at least one explicit willingness-to-pay commitment; distinguish stated interest, written commitment, signed agreement and collected revenue. | Open / unassigned |
| R04-10 | Establish an adoption scorecard and demonstrate retention. | Track the definitions below. Reach three external teams evaluating real application changes weekly for four consecutive weeks, with at least one willing to pay as evidenced under R04-09. Link the dated evidence and document confirmed false blocks, assistance required and remaining friction. | Open / unassigned |

Twenty to fifty cases is an onboarding starting point, not a reliability or
statistical-significance guarantee. Grow the dataset according to the actual
decision, relevant failure slices and measured uncertainty. Do not let pilot
examples silently become the held-out set after tuning on them.

Customer records stay within the agreed handling and access boundaries. Public
tutorials and case studies need permission for the material they disclose;
synthetic illustrations must be labelled. Keep raw customer data and prospect
contact details outside the public repository. Redacted summaries or controlled
evidence references can substantiate the checklist.

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

Use the first month for recruitment, initial onboarding, second use and useful
distribution material. Four-week retention runs from each team's actual first
use and can extend beyond that month. This is a sequence, not a promise that
stable 0.4 will be ready in 30 days. Wider launch promotion should point to a
working adoption path and demonstrated outcomes.

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
| Willingness to pay | Buyer, offered scope/price, response and commitment stage; keep actual revenue separate. Initial gate: at least one explicit commitment. |

A manually maintained private scorecard is sufficient for the pilot. Use approved
or opt-in usage collection where available; a new analytics service is not a
prerequisite. Stars and downloads are distribution signals alongside these
measures, not completion evidence for retention.

## Packaging and scope decisions

Keep the open-source local/CI path dependable. Test paid continuation against the
customer need and the cost of delivering it. Managed hosting and billing should
be built only after validating that offer and its operating responsibilities.

Maintain the existing capabilities while prioritizing this checklist. Additional
frameworks, training features and enterprise administration follow specific
customer requirements. The broader [roadmap](PRODUCT-ROADMAP.md) retains those
items and their evidence; this release plan assigns priority to completing the
adoption workflow.

## Release candidate and stable-release decision

Cut a release candidate once it can safely support the bounded pilot, has passing
applicable technical checks and has artifact-based installation evidence. Use it
to collect the real-customer evidence above and fix observed blockers. Follow the
maintainer authorization and publication procedure in [RELEASING](RELEASING.md).

Before stable 0.4, the release owner must complete this record:

- [ ] R04-01 through R04-05: product acceptance evidence reviewed against the
  final candidate artifacts, supported versions and documented limits.
- [ ] R04-06 through R04-09: recruitment, pilot repeat-use, distribution and paid
  continuation evidence recorded with appropriate disclosure permissions.
- [ ] R04-10: three external teams retained weekly for four consecutive weeks
  and at least one explicit willingness-to-pay commitment, with dated evidence.
- [ ] Implementation owners, PRs, commits/artifacts and evidence references filled
  in for every row; unmet targets and unresolved defects remain visible.
- [ ] Existing technical checks, security scans, upgrade/restore checks applicable
  to the supported deployment, release notes and artifact verification completed
  under the release procedure.
- [ ] Final readiness review records the release owner, date, exact commit and
  disposition of every criterion before publication is authorized.

The stable-release decision remains blocked while a required criterion is open.
Changing a criterion requires an explicit reviewed update to this plan and its
reason; a missed adoption target cannot be silently relabelled as completed.
Merging this planning PR records the work and does not complete these gates.

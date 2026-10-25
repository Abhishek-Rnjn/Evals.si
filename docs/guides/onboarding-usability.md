# Validate onboarding with unfamiliar developers

This is a repeatable human study protocol, **not evidence that users have
completed it**. Automated wheel-install controls establish packaging and
functional behavior; they cannot establish usability, evaluator validity,
customer adoption or willingness to pay.

## The task

Give a developer who did not build Evals.si this goal:

> Install the supplied release candidate, evaluate a trusted application on
> examples you understand, identify a meaningful failing case, make a change,
> and reproduce the result. Explain what the report does and does not establish.

Supply one supported starting link, the exact candidate version/artifact, and
an agreed scope for application execution and provider spending. Do not supply
a maintainer's sequence of commands. Start with an approved local application
and non-sensitive cases; users may bring their own application if its handling
policy allows it. Record which kind of application they used. A bundled
demonstration alone does not establish bring-your-own-application success.

## Before the session

- Recruit five unfamiliar developers for a first qualitative round. Include
  different levels of Python experience and supported operating systems.
- Record the package version, artifact hash, OS, Python version and task.
  Do not mix unreleased source behavior with a published-package study.
- Ask permission before recording a screen or collecting feedback. Keep raw
  prompts, application code, outputs and credentials out of shared study data.
- Agree on the duration and maximum provider spending. No model call is
  required for the synthetic local application.
- Freeze the candidate artifact during a round. Report a new round separately
  after fixes; do not erase failed sessions.

## During the session

Start the clock at the first installation attempt. Keep installation and
download time in the record; separately annotate external network outages.
Observe without coaching. If the participant is blocked, offer help only after
recording the blocker and elapsed time. An assisted completion is useful
feedback but is **not an unaided success**.

Record these milestones:

1. Installation completed.
2. One application case executed successfully.
3. A relevant failure was identified and correctly explained.
4. A change was evaluated and the result reproduced.
5. A reusable command or CI configuration was saved.

Ask the participant to explain whether an execution error, a failed check, a
skip and insufficient evidence mean the same thing. Do not prompt the answer.
A technically successful run is not enough if the user misreads its result as
a production-release certificate.

Use aggregate observations, not application contents:

```csv
session_id,artifact_version,os,python,application_type,install_seconds,first_case_seconds,useful_finding_seconds,reproduction_seconds,assistance_count,completed_unaided,result_explained_correctly,blocker_category
```

Missing milestones stay missing. Do not replace abandonment with a zero,
exclude failed participants, or count repeated trials as independent users.

## After the session

- Ask what the user expected at each blocker and which instruction was unclear.
- Fix the highest-impact recurrent blockers, then test with new participants.
- Ask participants to repeat the workflow on another change later. Record
  whether they returned without prompting separately from study completion.
- Publish only consented, sanitized aggregates and the limitations of the
  sample. There is no automatic telemetry collection in this protocol.

## Targets and honest interpretation

The product target is at least 90% of an appropriately recruited cohort
completing the core task unaided within 15 minutes and correctly interpreting
the result. That is a **target, not a measured outcome or guarantee**.

Five exploratory sessions can reveal serious friction; they cannot certify
a general 90% success rate. For larger rounds, report the actual denominator,
assisted/abandoned sessions, time distribution, platform breakdown and
uncertainty. Report synthetic sample tasks separately from participants' own
applications.

External recruitment, consent, independent outcome interpretation, representative
human labels, repeat use, willingness to pay and release approval require people.
Passing automated package checks does not close those requirements.

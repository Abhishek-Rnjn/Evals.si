# 0031: Human-labelled evaluator validation uses pinned portable sets and isolated tuning

Status: Accepted, implemented (unreleased)

Date: 2026-10-21

## Context

Annotation queues already collect individual judgements, compare source-run
scores with them and report inter-rater agreement. Harness² calibration addresses
candidate selection. Neither freezes human labels and splits for reproducible
comparisons of evaluator configurations, fresh-call repeatability or independent
held-out validity. Scores used across evaluation and training need that evidence.

## Decision

Add `evalsi calibrate` and a Python SDK workflow over fixed recorded outputs.
Version human labels in an immutable content-addressed artifact that retains all
votes and explicit tuning/held-out assignments. Import existing annotation
exports rather than create a second human-review service. Keep human labels
outside evaluator-facing records and preserve task references.

Select thresholds and the candidate on tuning labels only, before held-out
evaluation. Refuse cross-split independent groups and exact duplicate inputs.
Retain missing labels, skips and errors without imputing failure. Start with
binary human judgements and numeric/pass-fail evaluator outputs.

Disable the local judge cache unconditionally; configure judge models explicitly
and audit fresh calls. Estimate single-call validity by averaging repeats within
cases, and bootstrap independent groups for uncertainty. Paired comparisons
require the exact same frozen targets and label revision. Reports identify
evaluator drift without attributing it to target improvement.

Use bounded private JSON artifacts and escaped, standalone HTML reports. Pin
resolved evaluator/default parameters, source hashes, judge settings and prompt
hashes. This first version needs no new server APIs, database schema or production
failure workflow. Content pins are integrity checks, not signatures, label
authorship attestations or proof of model weights.

## Consequences

The workflow is usable locally and with installed custom evaluators, independently
of the server. Real human-labelled deployments still require representative data,
enough independent groups, sufficient class support, careful split design and
fresh held-out data after iterative tuning. Human score/multiclass/preference
calibration and an integrated browser workspace are later extensions. Provider
caching and external dependency/model identity remain explicit limitations.

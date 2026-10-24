# Onboarding study: a clean install and tool-calling agent lab

This is a single technical walkthrough performed on 24 October 2026 in a fresh
managed Linux x86_64 cloud workspace with Python 3.12.14. It is not a user study
with recruited participants, a quality benchmark, or a production certification.
The intent was to install Evals.si as a newcomer would, run it, and turn observed
setup and interpretation problems into reproducible tutorials.

## Starting conditions and method

The cloud connection reported connected, and `pwd`, `git --version` and
`python --version` executed successfully. The executor's network policy allowed
HTTP destinations through its configured proxy. The initial `https://evals.si`
request returned HTTP 503; this did not block GitHub or PyPI. The repository's
README points to `https://evalsi.dev/` for documentation. The 503 observation is
about the requested hostname at that moment, not a diagnosis of the docs host.

The repository was cloned at source base
`dfb3e7f3f97c91a8ced99b93b0ca0d36c56dc4fa`. We used separate environments:

| Path through installation | Versions | Purpose |
|---|---|---|
| Fresh PyPI install | evalsi 0.3.1, evalsi-harness 0.3.1, grpcio 1.84.1, protobuf 7.36.2 | Published customer path |
| `cd python && uv sync --all-packages` | evalsi 0.3.1.dev0, evalsi-harness 0.3.1.dev0 | Checkout behavior, including unreleased SQL tools |

No model/provider secret was needed. The fake model was a real loopback HTTP
server with deterministic responses computed from the conversation prefix.
The harness and evaluators were the installed packages, not substitutes. The
published Linux wheel supplied a matching evalsid; strict agent doctor reported
the bwrap sandbox available. No test-only local sandbox was used.

## Observed results

Published scoring quickstart: six records; exact-match 0.333; numeric-match 0.667
over three scored records with three skips; latency mean 517.5 from fixture data.
These are properties of recorded fixtures, not measured network latency.

The published `init --template agent` starter passed with task-success 1.0 and
exit 0. Its regressed spec returned task-success 0.0 and exit 3. Thus the installed
binary and actual sandbox executed both controls in this host.

The following tool-loop matrix passed against **both** installations. Each row
uses three synthetic records and one trial. Evaluator errors were zero throughout.

| Scenario | Exit | Recall | Precision | Tool error fraction | Loop pass | Phrase pass |
|---|---:|---:|---:|---:|---:|---:|
| Careful baseline | 0 | 1 | 1 | 0 | 1 | 1 |
| Omitted refund | 3 | 0.5 | 1 | 0 | 1 | 1 |
| Repeated lookup | 3 | 0.5 | 0.167 | 0 | 0 | skipped |
| Missing lookup ID | 3 | 0 | 0 | 0 | 1 | 1 |
| Persistent refund fault | 3 | 1 | 0.667 | 0.667 | 1 | 0 |
| Record | 0 | 1 | 1 | 0 | 1 | 1 |
| Replay, model endpoint unreachable | 0 | 1 | 1 | 0 | 1 | 1 |

The loop hits the six-step cap and produces no final text; the phrase evaluator
skips instead of scoring that missing output as zero. Other explicit gates still
fail. This is a useful example of why coverage matters beside means.

The source-only database extension additionally passed with task-success 1.0
(exit 0); skipping the refund produced task-success 0.0 (exit 3). Each task starts
from fresh in-memory SQL state. These two cases were not run against release
0.3.1 because its schema predates `environment.database`.

Reproduce the matrix with [verify.py](../../examples/first-time-onboarding/verify.py):

```bash
# In the published environment, from the repository root:
python examples/first-time-onboarding/verify.py
# In the source environment:
python examples/first-time-onboarding/verify.py --database
```

The checked-in [published matrix](../verification/data/onboarding-2026-10-24/published.jsonl) and [source matrix](../verification/data/onboarding-2026-10-24/source.jsonl) contain the observed exit codes and metric means, rounded only in the table above.

The script asserts expected exit codes, three returned records, zero evaluator
errors and selected metric invariants, including the misleading success phrase
and failed SQL checker. It uses temporary artifacts and a free loopback port.
For inspectable raw JSON, use the manual lesson commands and `--output`.

## Friction found and tutorial decisions

**Published and source versions are easy to confuse.** The development version
string looks older than 0.3.1 while including newer fields. The install lesson
separates environments and requires a source SHA for development runs.

**A passing doctor has a scope.** Plain doctor establishes SDK readiness;
`--require agent` establishes the stronger local prerequisite check. The course
introduces the probes before model setup and runs a real positive/negative
sandbox starter rather than treating a probe as complete execution evidence.

**One reference field does not automatically encode every scoring intention.**
An early lab draft used a structured tool-call reference with exact-match and
received zero answer matches: exact-match compares against that structured
reference, not an embedded natural-language answer selected by the author.
The final lab uses explicit tool-call references and a separately configured
`contains` phrase check, then demonstrates its weakness with a negative control.
For real answer correctness, use an evaluator appropriate to the reference shape.

**Tool success, expected calls and business success differ.** The persistent
fault attempts all expected calls, so recall stays perfect while tools fail.
The omitted-action case passes the output phrase check. The state lesson shows
why final-state assertions need to accompany trajectory checks.

**Mock schemas are not business validation.** The missing-ID fixture produced
no tool errors with canned mocks but zero tool-call accuracy. The course avoids
claiming that a mock tool enforces the whole advertised schema or policy.

**Replay is easier to trust with a negative connectivity control.** The verifier
points replay at an unreachable model endpoint and still expects the baseline
results. This is stronger than replaying while an unnoticed live model remains
available. It does not validate every replay mode or sandbox side effect.

## Scope of the evidence

Verified: fresh published install, matching package/server diagnostics, recorded
scoring, actual sandboxed CLI starter controls, non-streaming chat-completions
HTTP transport, tool observations, loop limits, retry after injected error,
record/replay, gate statuses, and source-only SQL final-state grading.

Not exercised here: real neural token generation, GPU inference, streaming,
hosted provider authentication, model quality, production payment services,
remote Kubernetes deployments, Firecracker/KVM, multi-agent behavior, or statistical
reliability at meaningful scale. The fake usage counters and tiny dataset cannot
support throughput, cost or model-quality claims.

The deliverable is a reproducible onboarding course and local integration lab.
Production adoption requires representative reviewed cases and controlled real
integrations, as described in [the handoff lesson](05-state-and-handoff.md).

## Repository validation for this change

The example's Ruff lint/format checks passed. The documentation build generated
106 pages, and the website link checker passed. The recorded-output lesson's
shell/Python snippets and HTML report generation were exercised successfully.
The matrix summaries linked above were regenerated after the final dataset edit.
Both source and published matrix commands completed successfully.

`make check` could not complete: this host's `/usr/bin/go` is not a usable Go
toolchain (`Go: Unknown option: vet`), and `gofmt` is absent. Running `make py-check`
separately passed lint, formatting and mypy, then reported **1,761 passed,
35 skipped, 11 errors**. All 11 errors came from the existing
`test_client_evaluate.py` server fixture invoking `go build`, which returned
`Go: Unknown option: build`. These server-backed checks still require a host with
the repository's Go toolchain. No checks were disabled or weakened.

# Budgeted Claude horizon and multi-service verification

## Scope and accounting

This follow-up uses the user's terminal Claude gateway and the supplied **AKS**
kubeconfig, not EKS. The initial maximum was $15; the user subsequently
authorized **$20 total for full testing**, not $20 of additional spending.
The gateway tariff and a
provider-enforced cap were unavailable; the user explicitly accepted conservative
published-Anthropic-price estimates instead of an invoice guarantee.

Every real HTTP attempt, including summaries and retries, reserves estimated
spend before networking. The initial refusal threshold was $14.50; after the
explicit increase it is **$19.50**, leaving margin below $20.
Earlier attempts are not given fresh allowances. Failed or
ambiguous requests keep their reservations. An initial $0.001 smoke allowance
is included once.

| Family | All input, including cache, per million | Output per million |
|--------|-----------------------------------------|--------------------|
| Haiku | $2.20 | $5.50 |
| Sonnet 5.5 | $4.40 | $11.00 |

These rates conservatively exceed the applicable published base/cache rates at
the time of execution. See [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing).
Pre-request estimation uses serialized UTF-8 request bytes plus 4,096 overhead,
not an attested provider tokenizer. **This is not a gateway invoice guarantee.**
The token and private gateway address remain client-side and are not reported.
The Haiku alias does not attest a particular upstream model version.

## Task and acceptance

The [stdlib task/checker](../../tests/verification/horizon_task.py) presents
300 deterministic synthetic incidents covering API, database and queue signals.
The agent applies an ordered runbook, quotes the exact observation and advances
one incident per tool call. Historical distraction logs include injected
instructions. A release code revealed only at incident zero must be returned
with the last decision.

The [runner](../../tests/verification/long_horizon.py) uses the actual
`AgentLoop`, model cassette and compaction, with state persisted in real AKS
pods. Acceptance requires all 300 incidents, at least 95% correct actions
against the **full 300 denominator**, every observation quoted exactly, retained
release code, at least 300 model steps and normal completion. A model's own
normal stop is insufficient if the deterministic checker fails.

This is synthetic operational triage, not a public coding benchmark, actual
production remediation or representative customer-quality certification.
Thousands of cumulative input tokens do not establish a single 200k-token
context test. Configured 8k/6k compaction windows are approximate management
targets rather than hard tokenizer limits. Pod enforcement on AKS remains
`partial`: the known missing NetworkPolicy enforcement was not disguised.

## Haiku controls

| Attempt | Outcome | Aggregate conservative estimate |
|---------|---------|---------------------------------|
| Unconstrained summaries, initial 12k context | Failed control; interrupted after 117 model/tool events and 102 truncated 512-token summary attempts, no successful compaction | $7.31022940 |
| Concise summaries, 8k context | 80/300 submitted; all submitted actions and quotes correct; output truncation stopped the task | $8.46688270 |
| Cassette resume, 1,024-token output | First 80 decisions rebuilt without new provider calls; reached 81/300 correct submissions, then stopped normally with work incomplete | $8.49684560 |

The concise profile requests at most 120 words, preserving the release code and
current incident rather than enumerating history. It produced 77 successful
compactions and zero summary failures in the second attempt. That attempt had
81 model turns, 80 tool calls, 435,139 cumulative input tokens and 36,245 output
tokens; elapsed 334.833 seconds.

The resume had 83 model turns, 81 tools, 78 compactions and zero summary failures;
elapsed 61.217 seconds. Its reported 445,626 input / 37,498 output usage includes
replayed historical model responses and must **not** be added again to charges.
The incremental conservative charge was $0.02996290. Neither incomplete attempt
establishes final release-code retention.

These failures are preserved, not replaced by later attempts. Repeated
observations are legitimate fixture inputs; the gold checker was not weakened
to accommodate the model's uncertainty.

## Final Sonnet profile

The fresh [Sonnet configuration](../../tests/verification/long-horizon-sonnet.yaml)
uses the same seed, 300 incidents and acceptance criteria, a 6k compaction
target, concise summaries and a standard bounded completion hook. If the model
stops early, the hook reminds it of the current incident without providing gold
answers; the framework permits at most three continuations. This is an explicit
profile change, not a claim that the unguarded Haiku attempts passed.

All $8.49684560 of previous estimated spending/reservations was carried into the
fresh Sonnet attempt. After the user increased the total allowance, that worker
was gracefully interrupted and its cassette resumed in a fresh real pod with
the same profile and hook identity. Its complete ledger, including the uncertain
in-flight request reservation, was carried forward at **$9.87619940**.
Replayed decisions make no new provider calls. There is no reset of spending.

### Final result: PASS

| Measurement | Result |
|-------------|--------|
| Required/submitted decisions | **300/300** |
| Correct actions / exact observation evidence | **300/300 / 300/300** |
| First-incident release code retained at the end | **Yes** |
| Injected-log incidents | 8, all correct |
| Model turns / tool calls | 301 / 300 |
| Successful compactions / summary failures | 112 / 0 |
| Completion-hook continuations | 0 (enabled, but not needed) |
| Stop reason | `completed`, no error |
| Resumed invocation duration, including replay of first 50 decisions | 1,537.493 seconds |
| Cumulative trajectory input / output tokens | 1,368,513 / 174,316 |
| Peak reported Sonnet request input, including cache | **4,981 tokens**, not 200k |
| Final aggregate conservative estimate, all attempts | **$16.51619320** |
| Refusal cutoff / authorized maximum | $19.50 / $20 |

The Sonnet phase added $8.01934760 to the earlier $8.49684560 Haiku total.
All 795 HTTP attempts remain in the accounting: 793 settled responses and two
uncertain in-flight reservations ($0.08233720 and $0.08041440). The final resume
carried 450 attempts and made 345 new ones; the 50 replayed decisions and their
summaries were not charged again.

The final trajectory has 413 recorded model responses: 301 main turns and
112 summaries. Its $7.93893320 historical trajectory usage estimate is **not**
an additional charge on top of the aggregate ledger.

Fresh-pod replay with all `ANTHROPIC_*` environment variables removed also
passed all 300 decisions, exact evidence and memory retention, reproducing
301 turns and 112 compactions. No live model/client was allocated: the replay
ledger has zero HTTP attempts and **$0 new provider charges**. A replay
accounting correction ensures historical usage is priced with Sonnet rates
rather than the default Haiku rates; historical usage is not new spend.
The final replay took 190.787 seconds; its persisted decisions are byte-identical
to the live run.

The [sanitized structured evidence](data/horizon-services-2026-10-21.json)
contains these results and hashes of the retained private session evidence.
The raw synthetic cassette/decisions remain in session storage, not committed
with provider/environment settings.

## Real multi-service failure tests

**Eight groups passed** using the existing
[service runner](../../deploy/e2e/services.py) with `--cases basic,lifecycle`:

1. Readiness, API/database grading, fresh state, cross-trial isolation and service
   crash errors.
2. Multi-service Terminal-Bench-format import and execution.
3. Destroy and failed-startup cleanup.
4. Startup cancellation removes the pending pod and frees its admission slot.
5. Setup cancellation removes the complete service pod.
6. Deadline expiry stops containers/execution: **25.593 seconds**.
7. SDK-worker SIGKILL reclaims the service pod: **10.498 seconds**.
8. Pool-process SIGKILL preserves its UID, reaps orphan pods after restart and
   accepts fresh work.

These destructive lifecycle checks ran on a uniquely owned **local kind**
cluster, not shared AKS: Kubernetes v1.35.0 arm64 with Calico 3.30.3 and default
CNI disabled. Real NetworkPolicy isolation was checked; AKS policy declarations
were not changed to pretend enforcement. The pool image contained cross-built
Linux/arm64 binaries from this checkout. The dedicated cluster, forward,
temporary TLS, binaries and uniquely tagged fixture image were removed.
Other users' clusters/containers were untouched.

The checks do not certify AKS networking, unsupported Compose features,
bubblewrap/Firecracker service groups or production failure recovery.

## Reproducible files and validation

- [Runbook](../../tests/verification/README.md): real-pool setup, metering,
  paid continuation, zero-provider-call replay and service test instructions.
- [Haiku task configuration](../../tests/verification/long-horizon.yaml) and
  [Sonnet task configuration](../../tests/verification/long-horizon-sonnet.yaml).
- [Owned kind configuration](../../tests/verification/services-kind.yaml) and
  [source-built scratch image](../../tests/verification/services.Dockerfile).
- Maintained [API/Redis task](../../examples/multi-service/run.yaml) and
  [Terminal-Bench-format task](../../examples/multi-service/terminal-bench).

Validation:

```text
python/.venv/bin/python -m ruff check tests/verification/horizon_task.py tests/verification/long_horizon.py tests/verification/test_long_horizon.py
  PASS
python/.venv/bin/python -m mypy --follow-imports=silent tests/verification/horizon_task.py tests/verification/long_horizon.py
  PASS (2 source files)
python/.venv/bin/python -m pytest tests/verification/test_long_horizon.py tests/verification/test_preflight.py -q
  PASS (23 tests)
```

Tests cover rule precedence/boundaries, exact 95% threshold, complete denominator,
evidence and memory checks, ordered integer IDs, pre-network budget refusal,
failed-attempt reservations, malformed/missing usage, over-reservation reported
costs, legacy/cross-family ledger carry, and explicit supported allowance
increases without resetting historical charges.

Live invocation (pool mTLS and terminal gateway settings supplied privately):

```sh
python/.venv/bin/python tests/verification/long_horizon.py \
  --config tests/verification/long-horizon-sonnet.yaml \
  --model-family sonnet --completion-guard --budget-limit 19.5 \
  --prior-budget "$PREVIOUS_OUTPUT/budget.json" \
  --resume "$PREVIOUS_OUTPUT/cassette.jsonl" --output "$NEW_OUTPUT"
```

Result: **PASS**. For zero-provider replay, replace `--prior-budget` and
`--resume` with `--replay "$NEW_OUTPUT/cassette.jsonl"` and use a new output
directory. The model family, context configuration and hook identity must match.

## Cleanup

After replay, the AKS namespace contained only the owned pool pod, no trial pods.
Namespace `evalsi-horizon-20261021` was deleted and a subsequent
`get namespace --ignore-not-found -o name` returned no resource. The loopback
forward was stopped and client temporary TLS directories were removed.
The owned local-kind service cluster was absent from `kind get clusters`;
unrelated existing clusters were left intact. The user's kubeconfig remains
local and ignored by Git.

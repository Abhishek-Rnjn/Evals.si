# Evals.si — additional bug findings

Repository: `Abhishek-Rnjn/Evals.si`  
Branch: `main`  
Reviewed commit: `7188e045a06bfb2b9954b3a5ff150d30c585a49a`  
Date: 16 October 2026

Publication recheck: all four evidence tests also passed on the newer `main` commit `69519313f68fc1b098f35d45a273a9908d697d38` (4 passed in 0.68s; see `current-main-results.txt`). Source links below retain the original reviewed commit.

This supplements the supplied initial review of commit `13e09193f0fa0da49c394b28972bde219628f988`. The four findings below are distinct from R1–R6. Numbering continues from that report. This pass concentrated on the Python agent harness, connectors, code-mode interpreter, and recording/replay. It does not claim a complete repository audit or that these bugs were introduced after the earlier commit.

| ID | Priority | Finding | Evidence |
| --- | --- | --- | --- |
| R7 | P1 | Code-mode slice assignment bypasses worker memory limits | Bounded runtime reproduction |
| R8 | P1 | Malformed agent responses can strand the harness event consumer | Actual harness/runner with mocked HTTP transport |
| R9 | P2 | HTTP template expansion interprets placeholders inside user input | Actual connector reproduction |
| R10 | P2 | Recording omits simulated-user turns, so replay changes outcomes | Actual harness record/replay with scripted model and judges |

## R7 — Enforce size limits before slice assignment

**Location:** [codemode.py:296–298](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/codemode.py#L296-L298).

The interpreter caps sequence arithmetic at `MAX_SEQUENCE = 1_000_000`, but subscript assignment writes directly to the underlying Python container. Slice insertion can grow a list without any size check. A model using `run_code` can therefore exceed the limit with a few operations:

```python
values = [0] * 1000000
values[:0] = values
print(len(values))
```

**Observed:** The actual interpreter returned `2000000` without raising `Limit`. The test deliberately stopped at two million elements; it did not attempt an out-of-memory crash.

**Impact:** Repeating the insertion doubles the list on each iteration, well below the operation cap. Code mode runs in the worker process, rather than the task sandbox. Large allocations can exhaust or terminate that worker and affect its other trials. These synchronous allocations are not bounded by an asyncio wall-clock timeout. The demonstrated defect is the size-limit bypass; worker termination is the inferred consequence of scaling it.

**Fix:** Compute the resulting list length before slice assignment and reject oversized mutations. Audit other mutation and native allocation paths for the same pre-allocation requirement. Consider an independently enforceable resource boundary for model-controlled interpretation.

**Regression:** Exercise direct slice insertion, slice replacement and repeated growth through `RunCodeTool`; reject the operation before retaining more than the configured limit.

## R8 — Always terminate the event stream when the producer fails

**Location:** [harness.py:288–305](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/harness.py#L288-L305). Concrete trigger: [connectors.py:509](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/connectors.py#L509).

`BuiltinHarness.run` launches `drive` as a background task and consumes a queue. `drive` catches a selected set of exception types and only puts the final event and sentinel on the queue after that handling. An unhandled `ValueError`, `TypeError`, or another exception exits the producer without waking the consumer. The consumer waits only on `queue.get()`, not on producer completion.

**Trigger:** A configured HTTP agent returns HTTP 200 with an HTML page or malformed JSON. `response.json()` raises `JSONDecodeError`, a `ValueError`, which is not caught by the producer. Responses and A2A decoding have similar exposure.

**Observed:** With an HTTP mock returning `<html>upstream unavailable</html>`, the actual producer task completed with `JSONDecodeError`, while the actual `run_task` remained pending and required external cancellation at 250 ms. The test explicitly checked the producer's exception. No live endpoint was contacted.

**Impact:** A single malformed upstream response strands a trial rather than producing a task error. It can hold a run/concurrency slot until an outer cancellation or deadline occurs. The connector's own timeout cannot recover this case because its execution has already exited.

**Fix:** Make producer completion observable to the consumer and ensure cleanup signals the end of the stream on every exit. Convert decoding/schema errors to `TaskError` at connector boundaries, but also fix the generic producer/consumer lifecycle so another unexpected exception cannot recreate the hang. Preserve cancellation semantics.

**Regression:** Return invalid JSON and unexpected JSON shapes from each connector, and inject an unexpected producer exception. Assert `run_task` terminates promptly with an error and cleans up resources without needing external cancellation.

## R9 — Expand HTTP placeholders once, in the original template only

**Location:** [connectors.py:489–493](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/connectors.py#L489-L493).

HTTP request construction chains three string replacements. After inserting the JSON-encoded input, the remaining replacements also operate on that inserted input. Literal `{{task_id}}` or `{{messages}}` in a dataset instruction is interpreted as template syntax, although it is data.

**Trigger and observed result:** With the default body template and task ID `task-1`, calling the actual connector with `Explain {{task_id}}` raised `TaskError: body_template is not JSON after substitution`. The second replacement inserts a quoted JSON string inside an already quoted string. No HTTP request is made.

**Impact:** Valid tasks that contain these markers—such as instructions about templates or prompt construction—fail before reaching the agent. The user is incorrectly told that the configured body template is invalid.

**Fix:** Use a single substitution pass over the original template, selecting the encoded value from a fixed mapping, so inserted content is never scanned again. Alternatively, substitute structured JSON values with explicit placeholder semantics.

**Regression:** Send literal instances of every supported marker within input text, including quotes and backslashes. Assert the outgoing body preserves the exact input while genuine placeholders in the configured template still expand.

## R10 — Record and replay simulated-user turns

**Location:** [harness.py:656–672](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/harness.py#L656-L672), [loop.py:407–428](https://github.com/Abhishek-Rnjn/Evals.si/blob/7188e045a06bfb2b9954b3a5ff150d30c585a49a/python/evalsi-harness/src/evalsi_harness/loop.py#L407-L428).

Recording wraps the agent model and tools, but passes the simulated user directly into `AgentLoop`. On replay, `user.reply` still invokes the judge. Its message and `done` decision are not recovered from the cassette, despite controlling the conversation and when evaluation stops.

**Trigger:** Record a multi-turn task with a simulated user, then replay it when the simulator returns a different reply or termination decision. This can happen with ordinary model nondeterminism, an updated judge, or a fresh judge cache. Replay also requires a working judge even though the agent turns are recorded.

**Observed:** A complete recording ended with `Order 42 ships tomorrow.`. Replaying the same task and cassette with a judge that ended the conversation after the first answer invoked that live judge and returned `What is your order number?` as the final output instead. Both went through the actual harness and returned records; only model and judge responses were scripted.

**Impact:** A replay can silently produce a different answer, trajectory, and score, or fail when the judge is unavailable. This undermines deterministic re-scoring and also affects replayed prefixes used for resume/fork.

**Fix:** Record simulated-user messages, termination decisions and usage in their original sequence, and replay them without live judge calls until the run transitions to live execution. Include simulator configuration in compatibility checks and define an explicit policy for older logs that lack these events.

**Regression:** Record and replay a multi-turn task with live judge calls forbidden during replay. Assert identical final output, user messages and stopping point. Cover resumed and forked prefixes as well.

## Reproduction artifacts and limits

- `test_additional_findings.py`: four focused tests asserting the current defective behavior. These are evidence tests, not patched-behavior regression tests.
- `reproduction-results.txt`: retained terminal output, **4 passed in 0.57s**.
- `existing-tests.txt`: existing code-mode, session, and connector suites completed with **70 passed in 6.68s**. Their passing status does not cover the four reproduced gaps. Command: `/workspace/review-venv/bin/python -m pytest -q python/evalsi-harness/tests/test_codemode.py python/evalsi-harness/tests/test_sessions.py python/evalsi-harness/tests/test_harness_connectors.py`.
- Python dependencies were installed in `/workspace/review-venv`; repository code was imported from the pinned checkout.
- Network responses and model/judge outputs were faked where described. No GPU, real remote agent, Kubernetes cluster or production workload was needed.
- Fixes for R7-R10 are now implemented on this branch, with fixed-behavior regressions in `python/evalsi-harness/tests/test_review_regressions.py`. The captured logs above are historical evidence from before the fixes.

Prioritize R7 and R8, then R10 and R9. This order is independent of the initial report's R1–R6 recommendations.

## Running the regression tests

From the repository root, with the local SDK and harness packages and pytest installed:

```sh
python -m pytest -q python/evalsi-harness/tests/test_review_regressions.py
```

The regressions now assert bounded slice assignment, literal HTTP input, prompt failure and cleanup after malformed responses, and deterministic user-turn replay without a judge. They also cover resume/fork budgets, old-log compatibility, valid slice operations and cancellation. Validation: 80 targeted tests passed; Ruff checks and formatting passed; mypy passed for all five changed Python source/test files. Cassette v4 records simulator identity, decisions, state and tool-invocation ownership; older logs without these fields are rejected for simulated-user replay. Logs without a simulator retain v1/v2 compatibility.


### Follow-up replay review

- Adapter simulators declare their effective identity even when their builtin user config is empty. TauBench defaults now use the same replay wrapper and compatibility checks as explicit simulators.
- Stateful simulators can snapshot and restore each reply. TauBench records user actions, transcript position, call IDs and grading exchanges; replay reapplies actions to the fresh environment without resolving a judge. Domain agent tools also rebuild environment state before a resumed or forked continuation.
- Elicitation decisions include the owning tool invocation and requested schema. Cached tool replay advances its decisions and elicitation allowance, including when creating a fork. An unfinished invocation cannot consume a completed invocation's answer, even when their arguments and question match.

Regression coverage includes default mock/telecom replay without a judge, telecom device actions and history across resume/fork, and interruption immediately before or after a repeated elicitation question.

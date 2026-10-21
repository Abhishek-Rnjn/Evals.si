# The built-in agent harness

Evals.si ships a harness for evaluating agents: `evalsi-harness`. It runs an
agent on a task in a sandbox, records everything, and grades the final state.
Use it to evaluate a model with a known, versioned agent, or to evaluate your
own agent with the same checkers, trials and gates. It is built for evaluation:
it does not serve agents to users.

```bash
pip install evalsi-harness
evalsi run -f agent-run.yaml
```

## What is built

| Capability | What you get | Details |
|---|---|---|
| Tool-calling agent | OpenAI-compatible and Anthropic models; thinking, streaming, prompt caching; `bash`, file tools, `edit_file`, `glob`, `grep`, `apply_patch`; MCP over HTTP and stdio; mocked tools | [Agents](agent-runs.md#agents) |
| Versioned profiles | `minimal`, `standard`, `code-mode`, so a score names the harness it was measured on | [Profiles](agent-runs.md#profiles-a-harness-a-score-can-name) |
| Multi-agent runs | A `delegate` tool starts a sub-agent with a clean context; its steps hang under the call as a tree, and its spend comes out of the parent's budget. One level, same model | [Sub-agents](agent-runs.md#sub-agents) |
| Delegation checks | An evaluator that checks delegation links in A2A and sub-agent trajectories | [Checking delegation links](agent-runs.md#checking-delegation-links) |
| Approval gates | The `ApprovalGate` hook asks before matching tool calls, a scripted approver answers (approve, edit, deny, no-answer), and `hitl_request` / `hitl_resolved` steps are recorded and replay exactly. `denial-respected` and `approval-coverage` score the result. A pattern gate, not a sandbox | [Approval gates](agent-runs.md#approval-gates) |
| Escalation requests | `request_escalation` asks for a wider policy; the harness grants `network` only if configured and records every request for safety evaluators | [Harness options](agent-runs.md#harness-options) |
| Messages from the user mid-run | Scripted corrections or new constraints at a chosen step | [Messages](agent-runs.md#messages-from-the-user-mid-run) |
| Simulated user | An LLM user with a persona and goal for multi-turn tasks | [Harness options](agent-runs.md#harness-options) |
| Long runs | Context strategies (`compact`, `clear-tool-results`, `fresh-rounds`, `evidence-state`) that can be compared like any other setting | [Context management](agent-runs.md#long-runs-context-management) |
| Record, replay, resume, fork | Every model call and tool result is logged as it happens; replay, resume a killed trial, or fork from step N | [Sessions](agent-runs.md#sessions-resume-and-fork) |
| Hooks | Python classes at the loop's lifecycle points, instead of forking the loop | [Hooks](agent-runs.md#hooks) |
| Fault injection | Errors, timeouts, malformed output and prompt injection per tool | [Harness options](agent-runs.md#harness-options) |
| Grading | Checkers (exit code, JSON, JUnit, Python), clean checker isolation, reward-hacking signals, state assertions, citation grounding | [Grading](agent-runs.md#grading) |
| Bring your own agent | A2A, MCP, OpenAI Responses, HTTP, or a CLI inside the sandbox, with readers for Claude Code, Codex, Gemini CLI and pi trajectories | [Agents](agent-runs.md#agents) |
| Comparison | Harness × model matrices with cost beside the score and paired intervals | [Comparing harnesses](agent-runs.md#comparing-harnesses) |
| Benchmarks | SWE-bench, τ-bench, Terminal-Bench, Harbor, BFCL | [Benchmarks](agent-runs.md#benchmarks) |
| Training | The same tasks as OpenEnv, verifiers, Gymnasium and NeMo Gym environments, with step-level rewards | [Agent runs](agent-runs.md) |

## Not built yet

These are on the [harness roadmap](../HARNESS-ROADMAP.md#h8-oversight-topologies-and-conversations-proposed-after-h3) (H8) and are
proposals, not features:

- **A model or human approver, and an agent that asks on its own.** The approval gate
  answers from a script, and the agent is asked rather than choosing to ask. Planned:
  approver decisions recorded like the simulated user's, and a `request_approval`
  tool scored by "asked before the irreversible action".
- **Deeper multi-agent topologies.** Today sub-agents are one level and run one
  after another. Planned: specialists with their own prompt, tools, budget and
  model; nesting; parallel delegation with exact replay; and topology as a
  variable in `evalsi matrix`.
- **Richer conversations.** Planned: scripted and generated user types, goals
  that end a conversation, and conversation-level scoring.

Serving agents to users, a multi-user session store and a chat UI are not planned:
bring an agent that does that over A2A or HTTP and Evals.si evaluates it.

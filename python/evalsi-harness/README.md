# evalsi-harness

**The built-in agent harness for [Evals.si](https://github.com/Abhishek-Rnjn/Evals.si), which evaluates AI before you ship, while it runs and as you train it.** It drives an agent through a task in an Evals.si sandbox and grades the end state, so you can evaluate and gate agents in CI, and use the same tasks as RL environments.

[Documentation](https://evalsi.dev/) ·
[Agent runs guide](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/docs/guides/agent-runs.md) ·
[GitHub](https://github.com/Abhishek-Rnjn/Evals.si) ·
[Changelog](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/CHANGELOG.md)

It runs either of these:
- the built-in tool-calling agent on any OpenAI-compatible or Anthropic model;
- your own agent over A2A, MCP, OpenAI Responses or HTTP, or as a CLI run inside the sandbox.

## Install

```bash
pip install evalsi-harness       # installs the matching evalsi release
evalsi run -f agent-run.yaml
```

`evalsi-harness` pins `evalsi` to the same release; upgrade both together. Python 3.11 or newer.

## What it provides

- **A tool-calling agent:** OpenAI-compatible and Anthropic models with thinking, streaming and prompt caching; `bash`, file, `edit_file`, `glob`, `grep` and `apply_patch` tools; MCP tools over HTTP and stdio; mocked tools; versioned `minimal`, `standard` and `code-mode` profiles so a score names the harness it was measured on.
- **Multi-agent runs:** a `delegate` tool starts a sub-agent with a clean context; its steps hang under the call as a tree and its spend comes out of the parent's budget (one level today).
- **Escalation requests:** `request_escalation` asks for a wider policy; the harness decides, and every request is recorded for safety evaluators. Approval gates with human-in-the-loop events are on the [roadmap](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/docs/HARNESS-ROADMAP.md).
- **Users:** an LLM-simulated user for multi-turn tasks, and scripted mid-run messages that test whether the agent takes a correction into account.
- **Long runs and budgets:** context strategies (`compact`, `clear-tool-results`, `fresh-rounds`, `evidence-state`) and caps on steps, tokens, spend and wall-clock time.
- **Record and replay:** model calls and tool I/O are logged as they happen; replay, resume a killed trial, or fork from step N.
- **Faults and hooks:** deterministic fault injection (errors, timeouts, malformed output, prompt injection) and Python hooks at the loop's lifecycle points.
- **Grading:** exit-code, JSON, JUnit or Python checkers, clean checker isolation, reward-hacking signals and state-based assertions.
- **Your own agent:** A2A, MCP, OpenAI Responses, HTTP, or a CLI inside the sandbox, with trajectory readers for Claude Code, Codex, Gemini CLI and pi.
- **Observability:** every step becomes an OTel span, and the agent's diff is recorded for the `code-quality` evaluator when the workdir is a git repository.
- **Benchmarks:** SWE-bench, τ-bench, Terminal-Bench, Harbor datasets and BFCL.

See [The built-in agent harness](https://github.com/Abhishek-Rnjn/Evals.si/blob/main/docs/guides/built-in-harness.md) for what is built and what is not.

## Tasks as RL environments

The same tasks can be handed to trainers that speak other environment specs (install `openenv-core` or `verifiers` 0.3.x yourself):

```python
from evalsi_harness.envspecs.openenv import TaskOpenEnv, TaskAction, create_task_app

env = TaskOpenEnv([(record, spec)])  # an OpenEnv Environment
obs = env.step(TaskAction(tool="submit"))  # obs.done, obs.reward

from evalsi_harness.envspecs.verifiers import load_environment

vf_env = load_environment([(record, spec)])  # a verifiers StatefulToolEnv
```

Apache-2.0.

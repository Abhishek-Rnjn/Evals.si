# evalsi-harness

**The built-in agent harness for [Evals.si](https://github.com/Abhishek-Rnjn/Evals.si).** It drives an agent through a task in an Evals.si sandbox and grades the end state, for evaluation and for RL environments.

[Documentation](https://abhishek-rnjn.github.io/Evals.si/) ·
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

- **Sandbox tools:** `bash`, `read_file` and `write_file`, plus an escalation request the harness policy decides on.
- **Other tools:** MCP tools over streamable HTTP or stdio; mocked tools; deterministic fault injection.
- **Budgets:** steps, tokens, spend and wall-clock time.
- **Simulated user:** an LLM user for multi-turn tasks.
- **Record and replay:** model calls and tool I/O are captured, replay can branch from step N, and sandbox commands are re-executed so the environment is rebuilt.
- **Environment checkers:** exit code, JSON, JUnit or a Python function.
- **OTel spans:** every step becomes a span.
- **The agent's diff:** recorded for the `code-quality` evaluator when the workdir is a git repository.
- **Benchmark importers:** `harbor://` (Terminal-Bench 2 and other Harbor datasets) and `terminal-bench://` (Terminal-Bench 1) task directories, with their Dockerfiles translated into environments.

## Tasks as RL environments

The same tasks can be handed to trainers that speak other environment specs (install `openenv-core` or `verifiers` 0.3.x yourself):

```python
from evalsi_harness.envspecs.openenv import TaskOpenEnv, TaskAction, create_task_app
env = TaskOpenEnv([(record, spec)])              # an OpenEnv Environment
obs = env.step(TaskAction(tool="submit"))        # obs.done, obs.reward

from evalsi_harness.envspecs.verifiers import load_environment
vf_env = load_environment([(record, spec)])      # a verifiers StatefulToolEnv
```

Apache-2.0.

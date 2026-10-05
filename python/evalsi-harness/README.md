# evalsi-harness

The light built-in agent harness for [Evals.si](https://github.com/abhishek-rnjn/evals.si).
It drives an agent through a task in an Evals.si sandbox and grades the end state.

It runs either of these:
- the built-in tool-calling agent on any OpenAI-compatible or Anthropic model;
- a bring-your-own agent over A2A, MCP, OpenAI Responses or HTTP, or as a CLI run inside the sandbox.

```bash
pip install evalsi-harness
evalsi run -f agent-run.yaml
```

What it provides:
- **Sandbox tools:** `bash`, `read_file` and `write_file`, plus an escalation request the harness policy decides on.
- **Other tools:** MCP tools over streamable HTTP or stdio; mocked tools; deterministic fault injection.
- **Budgets:** steps, tokens, spend and wall-clock time.
- **Simulated user:** an LLM user for multi-turn tasks.
- **Record and replay:** model calls and tool I/O are captured, replay can branch from step N, and sandbox commands are re-executed so the environment is rebuilt.
- **Environment checkers:** exit code, JSON, JUnit or a Python function.
- **OTel spans:** every step becomes a span.
- **The agent's diff:** recorded for the `code-quality` evaluator when the workdir is a git repository.
- **Benchmark importers:** `harbor://` (Terminal-Bench 2 and other Harbor datasets) and `terminal-bench://` (Terminal-Bench 1) task directories, with their Dockerfiles translated into environments.

See [docs/guides/agent-runs.md](../../docs/guides/agent-runs.md).

# Evals.si over MCP

`evalsi mcp` serves Evals.si to MCP clients, such as a coding agent, so the agent can check its own changes against your evaluation suites.

## Set it up

The server speaks MCP over stdio. Add it to your agent's MCP configuration, for example a project's `.mcp.json`:

```json
{
  "mcpServers": {
    "evalsi": {"command": "evalsi", "args": ["mcp"]}
  }
}
```

Options:

| Option | Meaning |
|---|---|
| `--root DIR` | The workspace (default: the working directory). Run specs and results files must be inside it, whatever path a client sends. |
| `--results-dir DIR` | Where `run` saves results, inside the root (default `.evalsi/results`). |
| `--server URL` | Run on an evalsid server instead of in-process; `compare` then takes run ids. Credentials as for other commands (`--token`, `--api-key`, `EVALSI_*`, `evalsi login`). |
| `--judge-*` | The judge model, for evaluators that need one (or `EVALSI_JUDGE_*`). |

## Tools

| Tool | What it does |
|---|---|
| `list_evaluators` | The installed evaluators by pack, with their params and whether they need a judge. |
| `evaluate` | Scores records the agent already has (`input`, `output`, `reference`, `metadata`) with evaluators. Returns each metric's mean and interval, and per-record scores. |
| `run` | Runs a run spec from the workspace and saves its results. By default (`baseline: previous`) it compares them, record by record, with the previous results of the same run, and reports significant changes, regressions and gates. `baseline` can also be `none`, a results file, or a server run id. |
| `compare` | Paired comparison of two results files (or two server runs). |

A typical loop: the agent runs the suite before its change, makes the change, runs it again, and reads whether any metric regressed significantly and whether the gates still pass.

Results come back as text for the model and as `structuredContent` for clients that read it. Long runs send `notifications/progress` when the client asks for progress, and a cancelled request stops its run.

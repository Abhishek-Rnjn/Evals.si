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

## On a server: evalsid's /mcp

`evalsid` serves MCP itself at `/mcp`, over streamable HTTP, for agents that should evaluate on the shared server with their own identity. Its tools are `list_evaluators`, `evaluate`, `run` (a run spec as an object or YAML text; by default compared with the previous finished run of the same name in the project), `get_run` and `compare_runs`.

```json
{
  "mcpServers": {
    "evalsi": {"type": "http", "url": "https://evalsi.example.com/mcp"}
  }
}
```

It follows the MCP authorization specification:

- **Discovery.** Unauthenticated requests get `401` with `WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource/mcp"`. The metadata (RFC 9728) names the issuers of your JWT providers (kind `user`) as authorization servers, so an MCP client signs the user in with your identity provider.
- **Audience-bound tokens (RFC 8707).** With `mcp.resource` set, a bearer token must carry that URI in its audience; a token minted for another service is refused with `error="invalid_token"`. Add the URI to the provider's `audiences` too.
- **Same access as everything else.** A tool call is a call to evalsid's own API with the caller's credential, so roles, rules, quotas and the audit log apply unchanged. API keys and mutual TLS work as for other clients. Per-tool rules use `mcp.tool.name` (see the [identity guide](identity.md#4-global-rules)).
- **Browsers.** Requests carrying an `Origin` header are refused unless the origin is listed in `mcp.allowed_origins`.

```yaml
mcp:
  resource: https://evalsi.example.com/mcp   # recommended; enables the audience check
  # allowed_origins: [https://studio.example.com]
  # run_wait_s: 600       # how long `run` waits before returning the run id to poll
  # disabled: true
```

Alternatively, put `/mcp` behind agentgateway, which implements the same specification at the gateway.

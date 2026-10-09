# 0015. Worker secrets are granted per project, and optionally per host

- **Status:** Accepted, 2026-10-07

## Context

Specs name secrets by environment variable (`api_key_env`, `headers_env`, `env_from`, evaluator params ending in `_env`), and the worker reads the value and sends it to a URL the same spec names. Authorization decided *who* may create a run in a project, but not *which* variables that run may name or where they may go. On a shared server, a caller allowed to create runs in one project could have the worker send any variable it holds (another team's key, a database DSN) to a host of their choosing. An empty `api_key_env` was no safer: the worker falls back to `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` and sends it to the spec's `base_url`.

On-behalf-of token exchange does not fit here: runs are asynchronous and outlive a caller's token, and evaluation calls test targets with a fixed identity so that scores do not depend on who started the run.

## Decision

1. **Grants in the server config.** `credentials.grants` lists, per variable, the projects that may name it (`"*"` for all) and optionally the hosts its value may be sent to (exact, or `*.domain`). It is the same trust model as `agents.trusted_commands`: the config is trusted, specs are not.
2. **Checked at the API, everywhere evaluators or targets arrive.** Runs and shadow replays, `Evaluate`/`EvaluateStream`, rewards, online policies and guardrails. Refusals are `permission_denied`, before anything is stored. Policies and guardrails are re-checked when loaded.
3. **Defaults are references.** A target with no `api_key_env` is checked as naming the connector's default variable. `api_key_env: none` sends no key.
4. **On whenever authentication is.** Enforcement defaults to on with authentication (or any grant), off for an unauthenticated local server; `credentials.enforce: false` opts out explicitly.
5. **Sandboxes get only unrestricted grants.** A value copied into a sandbox (`env_from`) leaves evalsid's view, so a host-limited grant cannot be used there.
6. **Evaluator params are declared.** An evaluator declares the params that name variables, and where each value goes, in its manifest's params schema (`x-evalsi-secret: {sent_to: <param>}`); `@evaluator(secrets=...)` writes it, and params ending in `_env` are declared automatically. The server checks declared params (and any `_env` param) against the grants. The worker refuses a call in which an undeclared param's value names one of its variables, so a plugin cannot read a secret through a param it did not declare.
7. **HTTPS for host-limited grants.** A value limited to hosts goes only over HTTPS (or to loopback); `allow_http` opts a grant out for a trusted network.
8. **Judges are scoped too.** `judges.*.projects` keeps a judge, whose key and budget are the server's, to the projects listed; it applies whether or not grants are enforced.
9. **Refusals are visible.** Each one is audited (`credentials.use`, `env:NAME` or `judge:NAME`, with the caller) and counted in `evalsi_credential_denied_total`. `ListCredentials` (CLI `evalsi credentials list`, the web UI's catalog) shows a project its grants and judges, names and hosts only.
10. **Admission asks the server.** `CreateRun` and `ApplyPolicy` take `validate_only`, and the operator's validating webhook uses it, so Kubernetes resources that the server would refuse fail at `kubectl apply`. When the server cannot answer, the resource is admitted with a warning rather than blocking the cluster on evalsid.
11. **Grants reload.** evalsid re-reads its config file every 15 seconds and on SIGHUP and applies changed grants and judge projects without a restart; online policies are re-checked at once, guardrails on their next use.

## Consequences

- An authenticated server that relied on specs naming any variable must now list grants; the error names the variable, the project and the config key.
- Secrets stay in the worker's environment; nothing secret is stored in specs or the database.
- Not covered: runs already started are not re-checked when grants change; the worker process still holds every variable (a scrubbed worker environment, with values passed per task, is the stronger design and a follow-up).
- Not covered: per-project secret *values* (a secret store, or different values of one name per project). Grants scope names; a deployment that needs a distinct key per project gives each its own variable.

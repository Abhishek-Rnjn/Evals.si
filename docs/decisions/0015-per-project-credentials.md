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
6. **Evaluator params by convention.** A param ending in `_env` holds a variable name (or headers mapped to names), and its destination is the `url` or `base_url` param beside it. Evaluators that read secrets follow this convention.

## Consequences

- An authenticated server that relied on specs naming any variable must now list grants; the error names the variable, the project and the config key.
- Secrets stay in the worker's environment; nothing secret is stored in specs or the database.
- Not covered: per-project secret *values* (a secret store, or different values of one name per project). Grants scope names; a deployment that needs a distinct key per project gives each its own variable.

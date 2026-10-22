# 0032. A database-backed tool environment in the task spec

- **Status:** Accepted, implemented on this branch, 2026-10-22.

## Context

τ-bench-style tasks give the agent tools over a stateful backend and grade the
final state of that backend. The tau2 adapter does this for its own domains, through
tau2's environment. The roadmap asked for a domain-neutral version and called it a
design question, because the obvious shapes pull in different directions: a service
in the sandbox (needs a rung with services and a guest image), an MCP server the
task ships (needs code the spec cannot carry), or Python tool functions (a trusted
code path, and not data).

## Decision

The environment declares the backend as data, in `environment.database`, beside the
fields that already describe a task:

- `schema` and `seed`: SQL run on an empty SQLite database. The database is built in
  the harness process for each trial, in memory. Trials share nothing, and no sandbox
  is created for a task that has only a database (`EnvironmentConfig.needs_sandbox`).
  With an image, setup, files or a command checker, the sandbox exists too and the
  two checks are combined.
- `tools`: each one a single parameterized statement with named parameters (`:id`) and
  a JSON Schema for them. `mode: read` (the default) runs under a SQLite authorizer that
  allows only SELECT, so a read tool that could change anything is refused when the
  task is read. `mode: write` may insert, update and delete, never change the schema,
  attach a database or run a pragma. `min_rows` and `max_rows` bound the rows a write
  changes; outside the bound the write is rolled back and the agent is told.
- `assertions`: named read-only queries and the rows they must return (or one value).
  The check is one test per assertion; the score is the fraction that held.

Arguments are checked against the tool's schema before the statement runs (type,
enum, required, bounds, `additionalProperties`), by a small checker in the harness, not
a schema library. Statements are bound, never interpolated, so an argument cannot become
SQL. A statement that runs past a step limit is interrupted.

Every write attempt, applied or rejected, is recorded: on the tool's step
(`evalsi.tool.db_write`, `db_rows_changed`, `db_applied`) and in `metadata.database`
(the writes with their arguments, and digests of the initial and final database). A
write tool is re-executed when a recording is replayed, so the end state is rebuilt.

## Why this shape

- It is data, so it travels in a run spec, a dataset record's `metadata.environment`
  and the Kubernetes CRD like every other environment field, and needs no trusted code
  on a server's worker.
- SQLite is in the standard library: no new dependency, no service to start, nothing
  on the sandbox's network, deterministic and fast to reset.
- Writes are explicit statements, so "every write" is a complete and reviewable
  record, and an evaluator can compare the sequence of writes with a reference, not
  only the end state.

## Limits

- One statement per tool. A multi-step business rule is several tools or a trigger in
  the schema. There is no procedural logic and no joins across tools.
- SQLite only, in memory, 100 rows returned per read. Large datasets belong in a
  service, which needs a rung with services.
- The RL gym (`TaskEnv`) does not expose database tools yet.
- Not a replacement for tau2's environment, which has domain logic in Python.

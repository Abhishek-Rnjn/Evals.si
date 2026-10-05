# 0006. Names and namespaces

- **Status:** Accepted, 2026-10-05 (design plan D10)

## Decision

| Thing | Name |
|-------|------|
| Python package (PyPI) and import | `evalsi` |
| User-facing CLI | `evalsi` (ships with the Python package) |
| Go daemon (API, scheduler, ingest) | `evalsid`, started by `evalsi serve` |
| Go module | `github.com/abhishek-rnjn/evals.si` |
| Protobuf packages | `evalsi.v1alpha1`, `evalsi.plugin.v1alpha1` |
| Kubernetes API group | `evals.si`, starting at `v1alpha1` |
| Evaluator namespaces | `builtin/…` for first-party; `<org>/…` for everyone else |

## Why

One user-facing command, `evalsi`, covers both the embedded and the server workflows. Evaluators are Python, so the Python package is present in every form factor and is the natural home for the CLI. The daemon gets its own name, `evalsid`, so the two binaries never collide on `PATH`.

The API starts at `v1alpha1` because it will change. `v1` is reserved for when we commit to compatibility, and `buf breaking` will then guard it.

## Consequences

- The design plan's references to a Go binary called `evalsi` now mean `evalsid`.
- Platform wheels may bundle `evalsid` later, the way some Python tools ship native binaries.

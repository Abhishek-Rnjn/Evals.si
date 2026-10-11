# 0018. The isolated dsh demo uses Evals.si's outer sandbox

- **Status:** Accepted, 2026-10-10

## Context

The pinned dsh demo requests its own `workspace-write` bash sandbox. Inside
Evals.si's hardened pod rung, no dsh confinement backend is usable, so dsh
correctly refuses the tool call. The live small-repo suite left every bug
unchanged despite making the expected bash calls. Evals.si already confines
each task, requires `min_isolation: namespaced`, and denies network access
except the explicitly allowed model endpoint.

## Decision

The small-repo dsh spec explicitly sets `DEMO_OUTER_SANDBOX=true`. The shared
launcher then patches only its headless profile's `sandbox-policy.mode` to
`danger-full-access`, a supported setting in the pinned dsh package. This
removes nested confinement only; Evals.si's outer sandbox is unchanged and
still fails closed if the requested isolation cannot be provided.

The launcher defaults this opt-in off. Web and TUI profiles never receive
the patch. Do not use the opt-in outside an already isolated Evals.si task.

## Consequences

- Tests check the default, opt-in, profile scope and the spec's retained
  `namespaced` minimum and deny-network policy.
- dsh can modify files in its task sandbox without requiring a nested
  bubblewrap or Landlock backend. No weaker Evals.si isolation is requested.
- The mock model's fixed final text is not proof a tool succeeded; hidden
  checkers and tool trajectories remain the evidence.
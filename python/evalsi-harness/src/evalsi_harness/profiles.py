"""Versioned profiles of the built-in agent (H3).

A profile fixes the parts of the harness that change what a model's score
means: the system prompt, the tools and, for ``standard``, the default context
strategy. A score that names its profile and version means the same thing
across releases; changing a profile's behaviour means a new version, and the old
one stays available so earlier results stay comparable.

* ``minimal`` (pi-style): a system prompt of well under 1,000 tokens and four
  tools (``bash``, ``read_file``, ``write_file``, ``edit_file``). The
  escalation request tool is **not** offered, so a safety evaluator that
  depends on seeing the agent ask for more access cannot use this profile.
* ``standard``: the default prompt, the H2 toolset (``edit_file``, ``glob``,
  ``grep``, ``apply_patch``, ``memory``, ``session_search``) and, when the spec
  gives the model's ``window_tokens`` but no strategy, ``compact``.
* ``code-mode``: the agent acts through ``run_code`` scripts over ``edit_file``,
  ``glob`` and ``grep``.

A profile owns the toolset, so a spec that names a profile and also lists
``tools.builtin`` is an error rather than a silent merge. Everything else
(budgets, hooks, extra ``instructions``, MCP servers, context) is the spec's.
"""

from __future__ import annotations

from dataclasses import dataclass

MINIMAL_SYSTEM = (
    "You are a coding agent. You work in a sandbox with four tools: bash, read_file, "
    "write_file and edit_file. Read before you change, make small edits with edit_file, "
    "run the project's tests or the task's own check to confirm, and when the work is "
    "done reply with a short summary and call no more tools."
)


@dataclass(frozen=True)
class Profile:
    name: str
    version: int
    description: str
    builtin: tuple[str, ...]
    system: str = ""  # empty: the default prompt with its notes
    escalation: bool = True
    # Use ``compact`` when the spec gives a window but no strategy.
    compact_with_window: bool = False

    @property
    def ident(self) -> str:
        return f"{self.name}@{self.version}"


PROFILES: dict[str, dict[int, Profile]] = {
    "minimal": {
        1: Profile(
            "minimal",
            1,
            "pi-style baseline: a short prompt and four tools",
            builtin=("edit_file",),
            system=MINIMAL_SYSTEM,
            escalation=False,
        )
    },
    "standard": {
        1: Profile(
            "standard",
            1,
            "the H2 toolset, with compaction when a window is given",
            builtin=("edit_file", "glob", "grep", "apply_patch", "memory", "session_search"),
            compact_with_window=True,
        )
    },
    "code-mode": {
        1: Profile(
            "code-mode",
            1,
            "act through run_code scripts over edit_file, glob and grep",
            builtin=("code_mode", "edit_file", "glob", "grep"),
        )
    },
}


def resolve(spec: str, tools_builtin: list[str]) -> Profile | None:
    """The profile a spec names (``name`` for the latest version, ``name@N`` for one),
    or None for no profile. Raises ValueError for an unknown name or a conflict."""
    if not spec:
        return None
    name, _, ver = spec.partition("@")
    versions = PROFILES.get(name)
    if versions is None:
        raise ValueError(f"unknown profile {name!r}; use one of {', '.join(sorted(PROFILES))}")
    if ver:
        if not ver.isdigit() or int(ver) not in versions:
            have = ", ".join(str(v) for v in sorted(versions))
            raise ValueError(f"profile {name} has no version {ver!r}; versions: {have}")
        profile = versions[int(ver)]
    else:
        profile = versions[max(versions)]
    if tools_builtin:
        raise ValueError(
            f"profile {profile.ident} sets the toolset; remove tools.builtin or the profile"
        )
    return profile

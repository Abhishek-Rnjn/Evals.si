# 0032. Browser and desktop toolsets, and images on tool results

- **Status:** Accepted, implemented on this branch, 2026-10-22.

## Context

H4 asks for a browser environment and a desktop environment with screenshot steps, and importers
for web and long-document benchmarks. Steps carried only text, and a tool result was a string.

## Decision

- **Images.** A `ToolResult` can carry images. The model sees them: as a following user message
  with `image_url` parts for chat-completions, as image blocks inside the `tool_result` for
  Anthropic. The trajectory records each as a child step (`generic`, name `image`) whose output is
  the new `Content.media` kind (the proto's `MediaRef`, which already existed). Evaluators' text
  view of it is `[image/png]`, never the bytes. Images over 1 MB are noted, not stored.
- **Where the tools run.** Both toolsets run in the harness process's host, not the sandbox:
  Chromium for the browser, Xvfb for the desktop. A sandbox keeps no background processes, so a
  display cannot live in one. The consequence is that the sandbox network policy does not apply, so
  the browser has its own host allowlist, and a restricted (server) worker refuses both toolsets
  and any desktop launch command unless its operator trusts them, through the existing
  `agents.trusted_commands` list (`["evalsi-browser"]`, `["evalsi-desktop"]`).
- **Optional dependency.** Playwright is the `evalsi-harness[browser]` extra; no browser binary is
  installed by the package. `uv.lock` changes with it.
- **Corpora.** Importers copy files into the record, so a large corpus is offered only as the
  per-question oracle documents, or as an image the user builds.
- **Scorers.** Benchmark scorers whose source was readable (WebArena) are reimplemented with the
  pin in the description; GAIA's, which was not, is reimplemented from memory and says so; OfficeQA's
  is run from the user's copy of `reward.py` only if its SHA-256 is the pinned one.

## Consequences

The H4 exit criterion (a WebArena-style fixture on the bubblewrap and pod rungs) is not met: the
fixture runs against a local site in a real Chromium, not under those rungs. A browser that lives
in the task's sandbox, or a sandbox-resident desktop, is future work.

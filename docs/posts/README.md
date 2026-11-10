# Post drafts

Drafts of blog and forum posts about Evals.si, for the maintainer to edit and publish elsewhere (a blog, dev.to, Hacker News). They are unpublished: the website does not import this folder. Every command and output in them was run, on the synthetic examples that ship in this repository.

- [a-gate-that-fails-the-build.md](a-gate-that-fails-the-build.md): a regression in recorded answers fails the build with exit code 3. Works with the published release.
- [a-gate-that-passes-on-missing-outputs.md](a-gate-that-passes-on-missing-outputs.md): a gate that passes while most records were skipped, and the gates that catch it. Needs features on `main`, so publish after the next release.
- [scoring-recorded-agent-tool-calls.md](scoring-recorded-agent-tool-calls.md): scoring recorded agent runs on their tool calls with no framework and no model call. Works with the published release.

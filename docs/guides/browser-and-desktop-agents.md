# Browser, desktop and long-document tasks

The built-in agent can drive a web browser or a virtual desktop, and importers turn four
public benchmarks into tasks for them. Everything below is built and tested against
fixtures and a local site. **None of the importers has run against a real benchmark's
questions with a model, and no real WebArena site, GAIA attachment, OfficeQA corpus or
AA-LCR document set was served.**

## The `browser` toolset

```yaml
spec:
  target: {connector: anthropic, model: claude-sonnet-5-5}
  harness:
    builtin:
      tools:
        sandbox: false          # the browser needs no sandbox
        builtin: [browser]
```

The agent gets `browser_navigate`, `browser_snapshot`, `browser_screenshot`, `browser_click`,
`browser_type`, `browser_select`, `browser_back` and `browser_tabs`. A snapshot is a tree of
elements with a `ref` on each interactive one; the agent acts by ref. A screenshot comes back
as an image the model sees, and the trajectory keeps it as a `media` step under the tool's step
(a `data:` URI, or only a note when the PNG is over 1 MB).

- **Install.** Playwright is the optional `evalsi-harness[browser]` extra. Install Chromium
  with `playwright install chromium`, or set `PLAYWRIGHT_BROWSERS_PATH` or
  `EVALSI_BROWSER_EXECUTABLE`. Without them the first call returns a clear tool error, and the
  tests skip.
- **Where it runs, and what it may reach.** In the harness process's host, not the task's
  sandbox, so the sandbox network policy does not apply. The browser has its own: only the
  hosts in the record's `metadata.browser.allow_hosts` (default: the start URL's host) load, and
  every other request, including redirects and sub-resources, is aborted. Websockets and
  cross-origin frames are not filtered. A server worker refuses the toolset unless its config
  lists `["evalsi-browser"]` under `agents.trusted_commands`.
- **Options** come from `metadata.browser`: `start_url`, `allow_hosts`, `viewport`,
  `timeout_s`, `max_snapshot_chars`, `storage_state`.
- **Replay.** A recorded cassette keeps a screenshot tool's text, not its image.

## The `computer` toolset

`builtin: [computer]` gives one tool, `computer`, with Anthropic's computer-use action and
parameter names (`screenshot`, `left_click`, `type`, `key`, `scroll`, `left_click_drag`,
`cursor_position` and the rest, with `coordinate`, `text`, `scroll_direction`, ...). It is an
ordinary function tool, not the native `computer_20250124` type; the zoom action is not built.
It drives an Xvfb display with `xdotool` and captures it with ImageMagick's `import`; all three
must be on the host (`apt install xvfb xdotool imagemagick`).

The display runs on the harness's host, not in the sandbox: the sandbox keeps no background
process between calls. Apps the task launches (`metadata.desktop.launch`, a list of argument
lists) therefore run unconfined on the host. A server worker refuses the toolset and each launch
command unless `agents.trusted_commands` lists them (and `["evalsi-desktop"]`). A desktop whose
apps live in the task's image is not built.

## WebArena and BrowserGym: `webarena://`

```
webarena://config_files/test.raw.json?shopping=http://host:7770&gitlab=http://host:8023&tasks=0-20
```

Reads WebArena's `config_files/test.raw.json` (or a directory of `<id>.json` files). Record ids are
`webarena.<id>`, the numbering BrowserGym's `browsergym/webarena.<id>` uses; no BrowserGym code is
used. You deploy the sites as WebArena's README describes and give each base URL; a task whose
sites are missing is an error. Every real task requires a login: pass `auth=<dir>` of Playwright
storage states (WebArena's `auto_login.py` makes them) or the agent starts logged out.

Score with `builtin/webarena-string-match` (`exact_match`, `must_include`) and
`builtin/webarena-url-match` (`GOLD in PRED`, from the last page the browser reported). Run over
the real file: 812 tasks parse, and **283 of them (35%) are fully checked by those two**; 118 need
`fuzzy_match` (an LLM judge) and 411 need `program_html` (the live page's DOM), which are not
reproduced. `metadata.grading.not_reproduced` says which, per task.

## GAIA: `gaia://`

```
gaia://GAIA/2023/validation?level=1,2&web=allow
```

Reads a directory holding `metadata.jsonl` and its attachments. The dataset is gated and asks not to
be reshared: nothing is vendored, the annotators' solution steps are dropped, and the records carry
the reference answers, so keep results private. Score with `builtin/gaia-match` (quasi-exact match,
reimplemented, not diffed against the gated scorer). The format is written from the dataset card as
remembered and has not been checked against a copy.

## OfficeQA and AA-LCR: `officeqa://`, `aa-lcr://`

Long documents are not copied into every record. `officeqa://officeqa_pro.csv?corpus=<dir>` gives
each question only its own source documents (the **oracle-document** setting, easier than the
benchmark's full-corpus one); `docs=image&image=<image with the corpus at /corpus>` is the
full-corpus setting and is unbuilt beyond the loader. AA-LCR comes with its document set by design
(`aa-lcr://questions.csv?docs=<dir>`); its columns are reconstructed from memory and unchecked.

Score OfficeQA with `builtin/officeqa-reward`, which runs your copy of Databricks's `reward.py`
if its SHA-256 matches the pinned one (or the `sha256` you pass); AA-LCR with `builtin/llm-judge`
(`correctness`), uncalibrated for it.

## Pins

| Target | Version |
|---|---|
| WebArena configs | `web-arena-x/webarena` `config_files/test.raw.json`, `main`, 2026-10-22 |
| WebArena scorers | `evaluation_harness/evaluators.py`, same date |
| OfficeQA | `databricks/officeqa` README and `reward.py` (SHA-256 `0d91698c...`), `main`, 2026-10-22 |
| GAIA, AA-LCR | the dataset cards as remembered; unchecked |

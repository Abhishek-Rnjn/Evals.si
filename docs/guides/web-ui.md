# The web UI

evalsid serves a small UI at `/ui/`, for example
`http://127.0.0.1:8080/ui/`. It shows:

- **runs:** their metrics with confidence intervals, gates, and the
  records sorted by any metric, lowest scores first;
- **paired comparisons** of two runs;
- **online policies**, with their counters, windows and alerts;
- **production traces** and the [failure inbox](failure-inbox.md), including case editing and review;
- **dataset versions**, with [publication, review, diffs and pinned regression runs](dataset-catalog.md);
- **annotation queues**, with progress and agreement, and a form to
  [annotate](#annotating);
- **guardrails;**
- **the evaluator catalog**, with each evaluator's tier and runtime.

The unreleased inbox and dataset screens also write through the same authorized
API as the CLI. Other run configurations and applying policies or guardrails go
through the CLI or API.

## Signing in

When authentication is on, the UI asks for a credential.

**With your identity provider.** If the server names a provider
(`auth.cli_login`), the page offers **Sign in with your identity
provider**. It sends you to the provider (the authorization code flow
with PKCE), and on return exchanges the code for a token through
evalsid. The token is the ID token, as for `evalsi login`, and the
server checks it like any other, so it needs an audience listed in
`audiences`. A tab asks for no refresh token: when the token expires, sign
in again. Signing in needs `https`, or `localhost`, because the browser
only offers PKCE's hashing there.

Register a **public** client for the UI with the redirect URI
`https://<your evalsid>/ui/` (exactly that path), and name it:

```yaml
auth:
  cli_login:
    issuer: https://sso.example.com/realms/eng
    client_id: evalsi-cli
    ui_client_id: evalsi-ui     # the UI's own client; defaults to client_id
```

evalsid reads the provider's metadata and makes the token request for the
browser, because the page's Content-Security-Policy allows only its own
origin. It keeps no secret and no refresh token: the client is public, and
the code verifier stays in the tab until that one exchange. The provider
must be `https` (or a loopback address, for one run on your own machine).

**With a key or token.** Or paste either of these:

- an API key (`evk_...`);
- the token of your CLI login:

  ```bash
  evalsi login --server https://evals.example.com
  evalsi auth token --server https://evals.example.com
  ```

Either credential is kept for the browser tab only (`sessionStorage`).
Every request goes through the API with it, so the UI shows exactly what
your roles allow, and the project selector lists the projects you can see.
Locally, with `--no-auth`, there is nothing to sign in with.

## Annotating

Open a queue and choose **Annotate**. The page takes the next item that
needs your answer (held for you under a lease, as for `evalsi annotate
start`), shows its input, output, reference, context and trajectory, and
asks the queue's questions: pass or fail, a score in range, a label, or
text. You can add a comment. **Submit** records the answer and brings the
next item; **Skip this item** records a skip, which does not count as an
answer. The source run's scores are hidden unless you ask for them, since
they can bias answers. It needs the annotate permission, like the CLI.

## Security

- **Data:** the UI's files hold no data, so they are served without a
  credential. Everything they show comes from `/v1alpha1/...`, which
  requires one.
- **Rendering:** values from the API are rendered as text, never as
  HTML, so record content cannot turn into markup.
- **Content-Security-Policy:** only the UI's own files and same-origin
  API calls are allowed. That means no inline script, no third-party
  content, and no framing.
- **Turning it off:** set `ui: {disabled: true}` in `evalsi.yaml`.

## Testing it

The Go tests cover the files, the headers and the text-only rendering. A browser
test runs the real `evalsid` against a real Chromium (Playwright) and checks
what only a browser does: the script runs under the Content-Security-Policy and
a script the page did not ship does not, every section renders, the catalog
filter works, sign-in accepts only a valid key and keeps it in the tab's
`sessionStorage`, an annotator answers and skips items, and a fake identity
provider signs a person in with PKCE (a refusal and a forged response included). CI runs it as the `ui-browser` job. Locally:

```bash
cd tests/ui && npm ci && npx playwright-core install chromium
EVALSI_UI_WORKER="$PWD/../../python/.venv/bin/python -m evalsi" npm test
```

Set `CHROMIUM_PATH` to use an installed Chromium instead.

## Accessibility checks

The browser suite runs axe-core 4.10.3 with WCAG 2 A/AA and 2.1 A/AA tags
against the main sections in light/dark palettes, sign-in and annotation.
It also checks navigation using Tab and Enter. These checks run in the existing
`ui-browser` CI job; a [local run is recorded](../verification/LEFTOVERS-2026-10-17.md).
Automated rules do not replace a complete manual screen-reader audit.

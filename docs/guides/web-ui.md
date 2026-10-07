# The web UI

evalsid serves a small, read-only UI at `/ui/`, for example
`http://127.0.0.1:8080/ui/`. It shows:

- **runs:** their metrics with confidence intervals, gates, and the
  records sorted by any metric, lowest scores first;
- **paired comparisons** of two runs;
- **online policies**, with their counters, windows and alerts;
- **annotation queues**, with progress and agreement;
- **guardrails;**
- **the evaluator catalog**, with each evaluator's tier and runtime.

Changing anything (starting runs, applying policies or guardrails,
annotating) goes through the CLI or the API.

## Signing in

When authentication is on, the UI asks for a credential. You can paste
either of these:

- an API key (`evk_...`);
- the token of your CLI login:

  ```bash
  evalsi login --server https://evals.example.com
  evalsi auth token --server https://evals.example.com
  ```

The credential is kept for the browser tab only (`sessionStorage`). Every
request goes through the API with it, so the UI shows exactly what your
roles allow, and the project selector lists the projects you can see.
Locally, with `--no-auth`, there is nothing to paste.

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

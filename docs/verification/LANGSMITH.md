# LangSmith verification

PR #68 built the LangSmith connector from the pinned `langsmith` 0.4.37
client. The next step for PRD S2 / M5 is verifying it through the server and
worker, then against a real LangSmith deployment.

`tests/e2e/langsmith_test.go` runs the same exercise in two modes:

| Test | Backend | When it runs |
| --- | --- | --- |
| `TestTraceSourceLangSmith` | In-process derived fixture, including pagination, workspace authentication and a self-hosted URL prefix | Ordinary e2e CI with `EVALSI_E2E_WORKER` |
| `TestLiveTraceSourceLangSmith` | The API at `EVALSI_TEST_LANGSMITH_URL` | Opt-in only, with credentials and a worker command; also available in the manually dispatched `LangSmith live verification` workflow |

Both create a uniquely named disposable project, seed a completed trace
90 minutes old and an unfinished recent trace, and apply a two-hour-backfill
`TraceSource` and `tool-errors` policy to the local server. The real Python
worker scores the historical trace; the recent trace must remain deferred
until its root run is completed. Both scores must be readable as feedback,
and Evals.si's trace API must retain input, output and both trajectory steps.
Three seconds of repeated polling must leave pulled and written counts at two.

A separate synthetic metric checks feedback creation, replacement that clears
the comment, replay without the local write receipt, and recreation after
deletion. Every step reads the feedback back and checks its ID and value;
the final list must contain one feedback per metric. Cleanup stops the server
before deleting the disposable project, including when assertions fail.

The fixture follows the pinned client's project/run/feedback request shapes.
It deliberately forces fetch pagination and checks the window bounds. It is
not a recording from LangSmith and cannot prove that the remote API accepts
the filter or responds as assumed when a feedback ID is posted twice.

The live test has not been run in the implementation environment: no
LangSmith credentials were configured. The PRD and LEFTOVERS therefore keep
live verification pending. The local server/worker exercise against the
derived fixture passed on 2026-10-17. Follow the [run instructions](../guides/trace-sources.md#verify-your-langsmith-deployment)
and record the tested endpoint region, deployment/client version, commit and
result when credentials are available. Do not record keys or private hostnames.

This bounded check does not measure a production-scale backfill or p95 lag,
inspect LangSmith's UI, test a Kubernetes Secret/egress configuration, exercise
real model output, or certify all provider variants.
